package broker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/doctor"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// WriteEventsToRepo writes broker-routed events into the target repo's
// lineage DB. Groups events by source and session, upserts the necessary
// records, and inserts events with INSERT OR IGNORE for idempotency.
//
// If srcBlobStore is non-nil, payload blobs are copied from it into the
// target repo's blob store so that routed events retain their payload_hash
// and participate in line-level attribution.
//
// Returns the session IDs that were created or updated in this repo.
func WriteEventsToRepo(ctx context.Context, repoPath string, events []RawEvent, srcBlobStore *blobs.Store) ([]string, error) {
	if len(events) == 0 {
		return nil, nil
	}

	semDir := filepath.Join(repoPath, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return nil, fmt.Errorf("open lineage db %s: %w", repoPath, err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	// Resolve repository_id.
	repo, err := h.Queries.GetRepositoryByRootPath(ctx, repoPath)
	if err != nil {
		return nil, fmt.Errorf("get repo %s: %w", repoPath, err)
	}

	// Open target repo's blob store for copying payload blobs.
	var targetBlobStore *blobs.Store
	if srcBlobStore != nil {
		objectsDir := filepath.Join(semDir, "objects")
		targetBlobStore, err = blobs.NewStore(objectsDir)
		if err != nil {
			return nil, fmt.Errorf("init target blob store %s: %w", repoPath, err)
		}
	}

	now := time.Now().UnixMilli()

	// Group events by (provider, source_key) - one group per source/session.
	type sourceGroup struct {
		events            []RawEvent
		providerSessionID string
		parentSessionID   string
		sessionStartedAt  int64
		sessionMetaJSON   string
		sourceRepoPath    string // set when session originated from a different repo
		sourceProjectPath string // raw SourceProjectPath (always set, for implementation observations)
		model             string // LLM model name
		latestEventTs     int64  // most recent event timestamp in this group
	}

	groups := make(map[string]*sourceGroup) // key: provider + "|" + source_key + "|" + provider_session_id
	var groupOrder []string                 // preserve insertion order

	for i := range events {
		ev := &events[i]
		key := ev.Provider + "|" + ev.SourceKey + "|" + ev.ProviderSessionID
		g, ok := groups[key]
		if !ok {
			// Record provenance: if the session originated from a different
			// repo, store the source path so cross-repo routing is visible.
			// A session launched from a subdirectory inside this repo (e.g.,
			// /repo/subdir) is same-repo - not cross-repo provenance.
			var sourceRepoPath string
			if ev.SourceProjectPath != "" && !PathBelongsToRepo(ev.SourceProjectPath, repoPath) {
				sourceRepoPath = ev.SourceProjectPath
			}

			g = &sourceGroup{
				providerSessionID: ev.ProviderSessionID,
				parentSessionID:   ev.ParentSessionID,
				sessionStartedAt:  ev.SessionStartedAt,
				sessionMetaJSON:   ev.SessionMetaJSON,
				sourceRepoPath:    sourceRepoPath,
				sourceProjectPath: ev.SourceProjectPath,
				model:             ev.Model,
			}
			groups[key] = g
			groupOrder = append(groupOrder, key)
		}
		g.events = append(g.events, *ev)
		if ev.Timestamp > g.latestEventTs {
			g.latestEventTs = ev.Timestamp
		}
	}

	// Propagate payload and provenance blobs from the broker store into the
	// target repo store.
	// Uses hardlink when possible, falling back to raw compressed file copy.
	// Track which hashes were successfully propagated so we can clear
	// references on events whose blobs failed to transfer, avoiding
	// dangling references that would silently break attribution or provenance.
	blobStart := time.Now()
	copiedBlobs := make(map[string]bool)
	blobCount := 0
	var blobBytes int64
	if srcBlobStore != nil && targetBlobStore != nil {
		attempted := make(map[string]bool)
		for _, ev := range events {
			for _, hash := range []string{ev.PayloadHash, ev.ProvenanceHash} {
				if hash == "" || attempted[hash] {
					continue
				}
				attempted[hash] = true

				alreadyPresent := targetBlobStore.Exists(hash)
				if err := targetBlobStore.Propagate(ctx, hash, srcBlobStore); err != nil {
					continue
				}
				copiedBlobs[hash] = true
				if alreadyPresent {
					continue
				}
				blobCount++
				if size, err := srcBlobStore.StoredSize(hash); err == nil {
					blobBytes += size
				}
			}
		}
	}
	blobDuration := time.Since(blobStart)

	// Batch all DB writes in a single transaction to reduce fsync overhead.
	dbStart := time.Now()
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer sqlstore.RollbackTx(tx)

	txq := h.Queries.WithTx(tx)

	var sessionIDs []string
	rowsWritten := 0

	for _, key := range groupOrder {
		g := groups[key]
		first := g.events[0]

		// Upsert source metadata.
		sourceRow, err := txq.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
			SourceID:     uuid.NewString(),
			RepositoryID: repo.RepositoryID,
			Provider:     first.Provider,
			SourceKey:    first.SourceKey,
			LastSeenAt:   now,
			CreatedAt:    now,
		})
		if err != nil {
			return sessionIDs, fmt.Errorf("upsert source: %w", err)
		}

		// Resolve parent session if present.
		var parentSessID sql.NullString
		if g.parentSessionID != "" {
			if parent, err := txq.GetAgentSessionByProviderID(ctx, sqldb.GetAgentSessionByProviderIDParams{
				RepositoryID:      repo.RepositoryID,
				Provider:          first.Provider,
				ProviderSessionID: g.parentSessionID,
			}); err == nil {
				parentSessID = sql.NullString{String: parent.SessionID, Valid: true}
			}
		}

		// Upsert session.
		sessRow, err := txq.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
			SessionID:         uuid.NewString(),
			ProviderSessionID: g.providerSessionID,
			ParentSessionID:   parentSessID,
			RepositoryID:      repo.RepositoryID,
			Provider:          first.Provider,
			SourceID:          sourceRow.SourceID,
			StartedAt:         g.sessionStartedAt,
			LastSeenAt:        now,
			MetadataJson:      g.sessionMetaJSON,
			SourceRepoPath:    sqlstore.NullStr(g.sourceRepoPath),
			Model:             sqlstore.NullStr(g.model),
		})
		if err != nil {
			return sessionIDs, fmt.Errorf("upsert session: %w", err)
		}

		sessionIDs = append(sessionIDs, sessRow.SessionID)

		// Insert events (INSERT OR IGNORE for idempotency).
		for _, ev := range g.events {
			// Only set blob hashes if the blob was actually copied to
			// this repo's store. Otherwise leave NULL to avoid dangling
			// references that break attribution or provenance.
			payloadHash := ev.PayloadHash
			if payloadHash != "" && !copiedBlobs[payloadHash] {
				payloadHash = ""
			}
			provenanceHash := ev.ProvenanceHash
			if provenanceHash != "" && !copiedBlobs[provenanceHash] {
				provenanceHash = ""
			}

			// Relativize absolute file paths in tool_uses against the target repo
			// so attribution can match them against repo-relative diff paths.
			toolUsesJSON := relativizeToolPaths(ev.ToolUsesJSON, repoPath)

			eventSource := ev.EventSource
			if eventSource == "" {
				eventSource = "transcript"
			}

			// Skip replayed step events when the same tool step was already
			// recorded directly by the hook path.
			if eventSource == "transcript" && ev.TurnID != "" && ev.ToolUseID != "" && ev.ToolName != "" {
				exists, err := txq.StepEventExists(ctx, sqldb.StepEventExistsParams{
					TurnID:    sqlstore.NullStr(ev.TurnID),
					ToolUseID: sqlstore.NullStr(ev.ToolUseID),
					ToolName:  sqlstore.NullStr(ev.ToolName),
				})
				if err == nil && exists {
					continue // Already captured directly - skip replay.
				}
			}

			// Dedup: skip the replayed transcript prompt when a direct hook
			// prompt event already exists for the same turn. Keep tool_result
			// user events; only plain user prompts are suppressed here.
			if eventSource == "transcript" && ev.TurnID != "" && ev.Role == "user" && ev.Kind == "user" {
				exists, err := txq.PromptEventExists(ctx, sqlstore.NullStr(ev.TurnID))
				if err == nil && exists {
					continue
				}
			}

			if err := txq.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
				EventID:           ev.EventID,
				SessionID:         sessRow.SessionID,
				RepositoryID:      repo.RepositoryID,
				Ts:                ev.Timestamp,
				Kind:              ev.Kind,
				PayloadHash:       sqlstore.NullStr(payloadHash),
				Role:              sqlstore.NullStr(ev.Role),
				ToolUses:          sqlstore.NullStr(toolUsesJSON),
				TokensIn:          sqlstore.NullInt64(ev.TokensIn),
				TokensOut:         sqlstore.NullInt64(ev.TokensOut),
				TokensCacheRead:   sqlstore.NullInt64(ev.TokensCacheRead),
				TokensCacheCreate: sqlstore.NullInt64(ev.TokensCacheCreate),
				Summary:           sqlstore.NullStr(ev.Summary),
				ProviderEventID:   sqlstore.NullStr(ev.ProviderEventID),
				TurnID:            sqlstore.NullStr(ev.TurnID),
				ToolUseID:         sqlstore.NullStr(ev.ToolUseID),
				ToolName:          sqlstore.NullStr(ev.ToolName),
				EventSource:       eventSource,
				ProvenanceHash:    sqlstore.NullStr(provenanceHash),
			}); err != nil {
				return sessionIDs, fmt.Errorf("insert event: %w", err)
			}
			rowsWritten++
		}
	}

	if err := tx.Commit(); err != nil {
		return sessionIDs, fmt.Errorf("commit tx: %w", err)
	}
	dbDuration := time.Since(dbStart)

	// Emit implementation observations (fail-open, best-effort).
	// One observation per unique (provider, provider_session_id) in this batch.
	// Aggregate across groups that share the same session (different source_keys)
	// to get the latest event_ts and prefer non-empty parent/source fields.
	type obsAgg struct {
		provider          string
		providerSessionID string
		parentSessionID   string
		sourceProjectPath string
		latestEventTs     int64
	}
	obsMap := make(map[string]*obsAgg)
	for _, key := range groupOrder {
		g := groups[key]
		dedup := g.events[0].Provider + "|" + g.providerSessionID
		agg, ok := obsMap[dedup]
		if !ok {
			obsMap[dedup] = &obsAgg{
				provider:          g.events[0].Provider,
				providerSessionID: g.providerSessionID,
				parentSessionID:   g.parentSessionID,
				sourceProjectPath: g.sourceProjectPath,
				latestEventTs:     g.latestEventTs,
			}
			continue
		}
		if g.latestEventTs > agg.latestEventTs {
			agg.latestEventTs = g.latestEventTs
		}
		if agg.parentSessionID == "" && g.parentSessionID != "" {
			agg.parentSessionID = g.parentSessionID
		}
		if agg.sourceProjectPath == "" && g.sourceProjectPath != "" {
			agg.sourceProjectPath = g.sourceProjectPath
		}
	}
	for _, agg := range obsMap {
		EmitObservation(ctx, Observation{
			Provider:          agg.provider,
			ProviderSessionID: agg.providerSessionID,
			ParentSessionID:   agg.parentSessionID,
			SourceProjectPath: agg.sourceProjectPath,
			TargetRepoPath:    repoPath,
			EventTs:           agg.latestEventTs,
		})
	}

	doctor.AddBenchStats(ctx, repoPath, doctor.BenchStats{
		RowsWritten:  rowsWritten,
		BlobsWritten: blobCount,
		BytesWritten: blobBytes,
		DBDuration:   dbDuration,
		BlobDuration: blobDuration,
	})

	return sessionIDs, nil
}

// relativizeToolPaths converts absolute file_path values in tool_uses JSON
// to repo-relative paths. This is needed for Cursor IDE events where
// enrichFromCodeHashes stores absolute paths (correct for routing) but
// the attribution system compares against repo-relative diff paths.
//
// Only processes entries that have absolute paths; relative paths pass through.
func relativizeToolPaths(toolUsesJSON, repoPath string) string {
	if toolUsesJSON == "" || repoPath == "" {
		return toolUsesJSON
	}
	// Fast path: no absolute paths to relativize.
	if !strings.Contains(toolUsesJSON, `"file_path":"\/`) && !strings.Contains(toolUsesJSON, `"file_path":"/`) {
		return toolUsesJSON
	}

	type tool struct {
		Name     string `json:"name"`
		FilePath string `json:"file_path,omitempty"`
		FileOp   string `json:"file_op,omitempty"`
	}
	type payload struct {
		ContentTypes []string `json:"content_types,omitempty"`
		Tools        []tool   `json:"tools,omitempty"`
	}

	var p payload
	if err := json.Unmarshal([]byte(toolUsesJSON), &p); err != nil {
		return toolUsesJSON
	}

	changed := false
	for i, t := range p.Tools {
		if filepath.IsAbs(t.FilePath) {
			if rel, err := filepath.Rel(repoPath, t.FilePath); err == nil && !strings.HasPrefix(rel, "..") {
				p.Tools[i].FilePath = rel
				changed = true
			}
		}
	}

	if !changed {
		return toolUsesJSON
	}

	out, err := json.Marshal(p)
	if err != nil {
		return toolUsesJSON
	}
	return string(out)
}
