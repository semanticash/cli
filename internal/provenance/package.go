// Package provenance builds per-turn provenance bundles for upload.
package provenance

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/doctor"
	"github.com/semanticash/cli/internal/platform"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// TurnContext holds the metadata needed to package a single turn.
type TurnContext struct {
	TurnID        string
	SessionID     string // provider session ID
	Provider      string
	TranscriptRef string
	StartedAt     int64
	CompletedAt   int64
	CWD           string
	Prompt        PromptCandidate
	TokenUsage    *TurnTokenUsage

	// Empty status means no hook-provided response.
	ResponseCandidate ResponseCandidate
}

// PromptCandidate identifies a prompt object available during packaging.
type PromptCandidate struct {
	EventID string
	Hash    string
}

// PackageTurn builds a turn bundle and persists its manifest. Hook-provided
// objects are copied from sourceBlobs into the repository store.
func PackageTurn(ctx context.Context, repoPath string, tc TurnContext, sourceBlobs *blobs.Store) {
	semDir := filepath.Join(repoPath, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.OpenOptions{
		BusyTimeout: 100 * time.Millisecond,
		Synchronous: "NORMAL",
	})
	if err != nil {
		slog.Debug("provenance: open db failed", "err", err)
		return
	}
	defer func() { _ = sqlstore.Close(h) }()

	repo, err := h.Queries.GetRepositoryByRootPath(ctx, repoPath)
	if err != nil {
		slog.Debug("provenance: resolve repo failed", "err", err)
		return
	}

	// Resolve internal session ID.
	// Try the provider name as-is first (matches most providers), then
	// fall back to underscore normalization for legacy sessions.
	sess, err := resolveProviderSession(ctx, h, repo.RepositoryID, tc.Provider, tc.SessionID)
	if err != nil {
		slog.Debug("provenance: resolve session failed", "err", err)
		return
	}

	bs, err := blobs.NewStore(filepath.Join(semDir, "objects"))
	if err != nil {
		slog.Debug("provenance: open blob store failed", "err", err)
		return
	}

	// Find the prompt event for the bundle.
	promptEvent := findPromptEvent(ctx, h, sess.SessionID, tc.TurnID)
	if promptEvent == nil {
		promptEvent = ensurePromptCandidate(ctx, bs, sourceBlobs, tc.Prompt)
	}

	// Load direct tool events for this turn.
	steps, err := h.Queries.ListStepEventsForTurn(ctx, sqldb.ListStepEventsForTurnParams{
		SessionID: sess.SessionID,
		TurnID:    sqlstore.NullStr(tc.TurnID),
	})
	if err != nil {
		slog.Debug("provenance: list step events failed", "err", err)
		return
	}

	// Add provenance to transcript-only events.
	// Use sess.Provider (the normalized DB name, e.g. "claude_code") rather
	// than tc.Provider (the raw hook name, e.g. "claude-code") so enricher
	// matching works regardless of how the provider name was formatted.
	steps = enrichSteps(ctx, h.Queries, bs, sess.Provider, sess.SessionID, tc.TurnID, steps)

	// Drop Copilot transcript events that duplicate hook events.
	steps = filterCopilotDuplicateSteps(ctx, bs, sess.Provider, steps)

	// Exclude events that only touch ignored files.
	filteredSteps := filterIgnoredSteps(ctx, repoPath, steps, bs)

	// Resolve unambiguous tool-delta references.
	deltaRefs := resolveStepDeltaHashes(ctx, h, filteredSteps)

	// Resolve the final response and ensure its object is available locally.
	response := captureFinalResponse(ctx, h, bs, sess.Provider, sess.SessionID, tc.TurnID, tc.ResponseCandidate)
	response = ensureResponseResolvable(ctx, bs, sourceBlobs, response)
	usage := validateTurnTokenUsage(tc.TokenUsage)
	usageColumns := nullableTurnTokenUsage(usage)

	// Successful responses require a positive completion time for ordering. Use
	// the turn completion time when the provider omitted one; otherwise record missing.
	if (response.Status == responseComplete || response.Status == responseEmpty) && response.CompletedAt <= 0 {
		if tc.CompletedAt > 0 {
			response.CompletedAt = tc.CompletedAt
		} else {
			response = ResponseCandidate{Status: responseMissing, EventID: response.EventID}
		}
	}

	// Build the provenance bundle.
	blobStart := time.Now()
	bundleHash, bundleBytes, bundleErr := buildProvenanceBundleFromFiltered(ctx, bs, tc, sess, promptEvent, filteredSteps, deltaRefs, response)
	blobDuration := time.Since(blobStart)
	blobsWritten := 0
	if bundleHash != "" {
		blobsWritten++
	} else if bundleErr != nil {
		slog.Warn("provenance: build bundle failed", "turn", tc.TurnID, "err", bundleErr)
	}

	// Persist the manifest. A bundle is required.
	status := "packaged"
	if bundleHash == "" {
		status = "failed"
	}
	now := time.Now().UnixMilli()
	dbStart := time.Now()
	if err := h.Queries.UpsertProvenanceManifest(ctx, sqldb.UpsertProvenanceManifestParams{
		ManifestID:           uuid.NewString(),
		RepositoryID:         repo.RepositoryID,
		SessionID:            sess.SessionID,
		TurnID:               tc.TurnID,
		Provider:             tc.Provider,
		Kind:                 "turn_bundle",
		TranscriptRef:        sqlstore.NullStr(tc.TranscriptRef),
		ProvenanceBundleHash: sqlstore.NullStr(bundleHash),
		StartedAt:            tc.StartedAt,
		CompletedAt:          sql.NullInt64{Int64: tc.CompletedAt, Valid: tc.CompletedAt > 0},
		Status:               status,
		ResponseEventID:      sqlstore.NullStr(response.EventID),
		ResponseHash:         sqlstore.NullStr(response.Hash),
		ResponseSummary:      sqlstore.NullStr(response.Summary),
		ResponseStatus:       sqlstore.NullStr(response.Status),
		ResponseCompletedAt:  sql.NullInt64{Int64: response.CompletedAt, Valid: response.CompletedAt > 0},
		TokensIn:             usageColumns.inputUncached,
		TokensOut:            usageColumns.output,
		TokensCacheRead:      usageColumns.cacheRead,
		TokensCacheCreate:    usageColumns.cacheWrite,
		CreatedAt:            now,
		UpdatedAt:            now,
	}); err != nil {
		slog.Debug("provenance: upsert manifest failed", "err", err)
		return
	}
	if usage != nil {
		stored, err := h.Queries.GetProvenanceManifest(ctx, sqldb.GetProvenanceManifestParams{
			RepositoryID: repo.RepositoryID,
			SessionID:    sess.SessionID,
			TurnID:       tc.TurnID,
			Kind:         "turn_bundle",
		})
		if err != nil {
			slog.Debug("provenance: read stored token usage failed", "turn", tc.TurnID, "err", err)
		} else if tokenUsageDiffers(usage, stored) {
			slog.Warn("provenance: preserving existing token usage", "turn", tc.TurnID)
		}
	}
	dbDuration := time.Since(dbStart)

	doctor.AddBenchStats(ctx, repoPath, doctor.BenchStats{
		RowsWritten:  1,
		BlobsWritten: blobsWritten,
		BytesWritten: bundleBytes,
		DBDuration:   dbDuration,
		BlobDuration: blobDuration,
	})

	slog.Debug("provenance: turn packaged",
		"turn", tc.TurnID,
		"steps", len(filteredSteps),
		"bundle", bundleHash != "",
	)
}

// filteredStep carries a step row alongside its pre-filtered file_paths.
// filterIgnoredSteps produces these; buildProvenanceBundleFromFiltered consumes them.
type filteredStep struct {
	Row       sqldb.ListStepEventsForTurnRow
	FilePaths []string // repo-relative, gitignored entries removed
}

// filterIgnoredSteps removes steps whose files are all gitignored, and
// filters file_paths on steps with mixed visibility. Steps without file
// paths (Bash, Agent) pass through unchanged.
func filterIgnoredSteps(ctx context.Context, repoPath string, steps []sqldb.ListStepEventsForTurnRow, bs *blobs.Store) []filteredStep {
	// Collect all repo-relative file paths across all steps, including
	// primary files from provenance blobs (which may not appear in tool_uses).
	allPaths := make(map[string]bool)
	stepPaths := make([][]string, len(steps))
	stepPrimary := make([]string, len(steps))

	for i, s := range steps {
		if s.ToolUses.Valid && s.ToolUses.String != "" {
			paths := extractRepoRelativeFilePaths(s.ToolUses.String, repoPath)
			stepPaths[i] = paths
			for _, p := range paths {
				allPaths[p] = true
			}
		}
		// Extract primary file from provenance blob. Missing or
		// unreadable blobs weaken gitignore filtering and step-to-file
		// association; surface the failure instead of dropping through
		// silently.
		if s.ProvenanceHash.Valid && s.ProvenanceHash.String != "" {
			blob, err := bs.Get(ctx, s.ProvenanceHash.String)
			if err != nil {
				slog.Warn("provenance: primary-file blob load failed",
					"repo_path", repoPath,
					"event_id", s.EventID,
					"provenance_hash", s.ProvenanceHash.String,
					"err", err)
				continue
			}
			raw := extractPrimaryFile(blob)
			if raw != "" {
				pf := toRepoRelative(raw, repoPath)
				if pf != "" {
					stepPrimary[i] = pf
					allPaths[pf] = true
				}
			}
		}
	}

	// Batch-check all paths against Git ignore rules.
	var pathList []string
	for p := range allPaths {
		pathList = append(pathList, p)
	}
	ignored := checkGitIgnored(ctx, repoPath, pathList)

	var result []filteredStep
	for i, s := range steps {
		paths := stepPaths[i]
		primaryFile := stepPrimary[i]

		// No file paths AND no primary file: keep unchanged (Bash, Agent, etc.).
		if len(paths) == 0 && primaryFile == "" {
			result = append(result, filteredStep{Row: s})
			continue
		}

		// If we have a primary file but no tool_uses paths, check the primary
		// file directly. This covers steps where tool_uses is empty but the
		// provenance blob has tool_input.file_path.
		if len(paths) == 0 && primaryFile != "" {
			if ignored[primaryFile] {
				continue // Primary file gitignored: drop step.
			}
			result = append(result, filteredStep{Row: s})
			continue
		}

		if primaryFile != "" {
			// Primary file is gitignored: drop the entire step.
			if ignored[primaryFile] {
				continue
			}
			// Primary file is visible: keep step, filter file_paths.
			visible := filterVisiblePaths(paths, ignored)
			result = append(result, filteredStep{Row: s, FilePaths: visible})
		} else {
			// No primary file determinable.
			visible := filterVisiblePaths(paths, ignored)
			if len(visible) == 0 {
				// All file_paths gitignored: drop step.
				continue
			}
			// Mixed visible/ignored: keep step with filtered paths, clear provenance.
			if len(visible) < len(paths) {
				row := s
				row.ProvenanceHash.Valid = false
				row.ProvenanceHash.String = ""
				result = append(result, filteredStep{Row: row, FilePaths: visible})
			} else {
				// All visible: keep unchanged.
				result = append(result, filteredStep{Row: s, FilePaths: visible})
			}
		}
	}
	return result
}

func filterVisiblePaths(paths []string, ignored map[string]bool) []string {
	if len(ignored) == 0 {
		return paths
	}
	var visible []string
	for _, p := range paths {
		if !ignored[p] {
			visible = append(visible, p)
		}
	}
	return visible
}

// buildProvenanceBundleFromFiltered builds the bundle using pre-filtered steps
// and file_paths from filterIgnoredSteps.
func buildProvenanceBundleFromFiltered(
	ctx context.Context,
	bs *blobs.Store,
	tc TurnContext,
	sess sqldb.AgentSession,
	prompt *promptInfo,
	steps []filteredStep,
	deltaRefs map[string]string,
	response ResponseCandidate,
) (string, int64, error) {
	bundle := provenanceBundle{
		Version:           1,
		Provider:          tc.Provider,
		SessionID:         sess.SessionID,
		ProviderSessionID: sess.ProviderSessionID,
		TurnID:            tc.TurnID,
		CWD:               tc.CWD,
		StartedAt:         tc.StartedAt,
		CompletedAt:       tc.CompletedAt,
		Steps:             make([]bundleStep, 0, len(steps)),
	}

	// Response metadata upgrades the bundle format. The redacted body remains a
	// separate content-addressed object.
	if response.Status != "" {
		bundle.Version = 2
		bundle.Response = &bundleResponse{
			EventID:     response.EventID,
			Hash:        response.Hash,
			Summary:     response.Summary,
			Status:      response.Status,
			CompletedAt: response.CompletedAt,
		}
	}

	if sess.ParentSessionID.Valid {
		bundle.ParentSessionID = &sess.ParentSessionID.String
	}

	if prompt != nil {
		bundle.Prompt = &bundlePrompt{
			EventID:  prompt.EventID,
			BlobHash: prompt.PayloadHash,
		}
	}

	for _, fs := range steps {
		s := fs.Row
		step := bundleStep{
			EventID: s.EventID,
			Ts:      s.Ts,
		}
		if s.ToolName.Valid {
			step.ToolName = s.ToolName.String
		}
		if s.ToolUseID.Valid {
			step.ToolUseID = s.ToolUseID.String
		}
		if s.ProvenanceHash.Valid {
			step.ProvenanceHash = s.ProvenanceHash.String
		}
		if s.PayloadHash.Valid {
			step.PayloadHash = s.PayloadHash.String
		}
		if h, ok := deltaRefs[s.EventID]; ok {
			step.DeltaHash = h
		}
		if s.Summary.Valid {
			step.Summary = &s.Summary.String
		}
		// Use pre-filtered file_paths from filterIgnoredSteps.
		step.FilePaths = fs.FilePaths
		bundle.Steps = append(bundle.Steps, step)
	}

	data, err := json.Marshal(bundle)
	if err != nil {
		return "", 0, err
	}
	hash, size, err := bs.Put(ctx, data)
	if err != nil {
		return "", 0, err
	}
	return hash, size, nil
}

// stepDeltaEvidenceKind identifies canonical tool-delta evidence links.
const stepDeltaEvidenceKind = "tool_delta"

// resolveStepDeltaHashes returns unambiguous tool-delta references by event ID.
// Events linked to multiple groups are omitted.
func resolveStepDeltaHashes(ctx context.Context, h *sqlstore.Handle, steps []filteredStep) map[string]string {
	refs := make(map[string]string, len(steps))
	for _, fs := range steps {
		eventID := fs.Row.EventID
		if eventID == "" {
			continue
		}
		links, err := h.Queries.ListEvidenceLinksByEvent(ctx, eventID)
		if err != nil {
			slog.Debug("provenance: list evidence links failed", "event_id", eventID, "err", err)
			continue
		}
		if hash, ok := selectStepDeltaHash(links); ok {
			refs[eventID] = hash
		}
	}
	return refs
}

// selectStepDeltaHash returns a hash only when one tool-delta group is linked.
func selectStepDeltaHash(links []sqldb.AgentEventEvidenceLink) (string, bool) {
	hash := ""
	count := 0
	for _, l := range links {
		if l.EvidenceKind != stepDeltaEvidenceKind {
			continue
		}
		count++
		hash = l.EvidenceHash
	}
	if count != 1 || hash == "" {
		return "", false
	}
	return hash, true
}

// provenanceBundle is the JSON shape written for a packaged turn.
type provenanceBundle struct {
	Version           int             `json:"version"`
	Provider          string          `json:"provider"`
	SessionID         string          `json:"session_id"`
	ProviderSessionID string          `json:"provider_session_id"`
	ParentSessionID   *string         `json:"parent_session_id"`
	TurnID            string          `json:"turn_id"`
	CWD               string          `json:"cwd,omitempty"`
	StartedAt         int64           `json:"started_at"`
	CompletedAt       int64           `json:"completed_at,omitempty"`
	Prompt            *bundlePrompt   `json:"prompt,omitempty"`
	Steps             []bundleStep    `json:"steps"`
	Response          *bundleResponse `json:"response,omitempty"`
}

// bundleResponse describes the final response referenced by a bundle.
type bundleResponse struct {
	EventID     string `json:"event_id,omitempty"`
	Hash        string `json:"hash,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Status      string `json:"status"`
	CompletedAt int64  `json:"completed_at,omitempty"`
}

type bundlePrompt struct {
	EventID  string `json:"event_id"`
	BlobHash string `json:"blob_hash,omitempty"`
}

type bundleStep struct {
	EventID        string `json:"event_id"`
	Ts             int64  `json:"ts"`
	ToolName       string `json:"tool_name,omitempty"`
	ToolUseID      string `json:"tool_use_id,omitempty"`
	ProvenanceHash string `json:"provenance_hash,omitempty"`
	PayloadHash    string `json:"payload_hash,omitempty"`
	// DeltaHash is the local CAS hash for an unambiguous tool delta. Sync replaces
	// it with the redacted upload hash.
	DeltaHash string   `json:"delta_hash,omitempty"`
	Summary   *string  `json:"summary"`
	FilePaths []string `json:"file_paths,omitempty"`
}

// promptInfo holds the prompt event reference stored in the bundle.
type promptInfo struct {
	EventID     string
	PayloadHash string
}

// findPromptEvent looks up the direct prompt event for a turn.
func findPromptEvent(ctx context.Context, h *sqlstore.Handle, sessionID, turnID string) *promptInfo {
	row, err := h.Queries.GetPromptEventForTurn(ctx, sqldb.GetPromptEventForTurnParams{
		SessionID: sessionID,
		TurnID:    sqlstore.NullStr(turnID),
	})
	if err != nil {
		return nil
	}
	pi := &promptInfo{EventID: row.EventID}
	if row.PayloadHash.Valid {
		pi.PayloadHash = row.PayloadHash.String
	}
	return pi
}

// copilotMutationCanonical maps Copilot tool names (from both hook and
// transcript paths) to a canonical lowercase key for dedup comparison.
//
// Hook path uses:  Bash, Edit, Write
// Transcript uses: bash, edit, create, copilot_file_edit
//
// "create" is the transcript twin of hook "Write" - both represent file
// creation. "copilot_file_edit" is synthetic from tool.execution_complete
// and overlaps with hook Write/Edit.
var copilotMutationCanonical = map[string]string{
	"bash": "bash", "Bash": "bash",
	"edit": "edit", "Edit": "edit",
	"create": "write", "Create": "write",
	"write": "write", "Write": "write",
	"copilot_file_edit": "file_edit",
}

// filterCopilotDuplicateSteps removes transcript-sourced step events that
// duplicate hook-backed steps in Copilot sessions. For non-Copilot providers
// the input is returned unchanged.
//
// Matching is per-step: a transcript step is only suppressed when a specific
// hook step can be identified as its twin via a unique identity key.
//
// Two identity signals are used depending on tool type:
//
//   - File-mutation tools (Edit, Write, create, copilot_file_edit): matched
//     by file_path in tool_uses JSON. Both the hook path (direct_emit.go
//     serializeStepToolUses) and transcript path (extract.go) include
//     file_path; relativizeToolPaths normalizes both to repo-relative.
//
//   - Bash: matched by command string extracted from provenance blobs.
//     After enrichment, both hook and transcript Bash provenance contain
//     tool_input.command (redacted identically via redact.String), so the
//     strings are directly comparable. Steps without a provenance blob
//     (enrichment failed or no companion) are never suppressed.
//
// A match is only made when the identity key is unique on both sides for
// the same canonical tool class. When the same file or command appears
// multiple times in a turn, the correspondence is ambiguous and the
// transcript step is kept.
func filterCopilotDuplicateSteps(ctx context.Context, bs *blobs.Store, provider string, steps []sqldb.ListStepEventsForTurnRow) []sqldb.ListStepEventsForTurnRow {
	if provider != "copilot" {
		return steps
	}

	type dedupKey struct {
		canon string
		key   string
	}

	// Build hook-side identity keys and count frequencies.
	type hookEntry struct {
		canon string
		key   string
	}
	filePathCache := make(map[string][]string)
	pathsFor := func(s sqldb.ListStepEventsForTurnRow) []string {
		if paths, ok := filePathCache[s.EventID]; ok {
			return paths
		}
		paths := extractToolUsesFilePaths(s.ToolUses)
		filePathCache[s.EventID] = paths
		return paths
	}
	commandCache := make(map[string]string)
	commandFor := func(s sqldb.ListStepEventsForTurnRow) string {
		if cmd, ok := commandCache[s.EventID]; ok {
			return cmd
		}
		cmd := extractProvenanceCommand(ctx, bs, s.ProvenanceHash)
		commandCache[s.EventID] = cmd
		return cmd
	}

	var hookEntries []hookEntry
	hookFreq := make(map[dedupKey]int)
	for _, s := range steps {
		if s.EventSource != "hook" || !s.ToolName.Valid {
			continue
		}
		canon, ok := copilotMutationCanonical[s.ToolName.String]
		if !ok {
			continue
		}
		if canon == "bash" {
			if cmd := commandFor(s); cmd != "" {
				hookEntries = append(hookEntries, hookEntry{canon: canon, key: cmd})
				hookFreq[dedupKey{canon, cmd}]++
			}
		} else {
			for _, fp := range pathsFor(s) {
				hookEntries = append(hookEntries, hookEntry{canon: canon, key: fp})
				hookFreq[dedupKey{canon, fp}]++
			}
		}
	}
	if len(hookEntries) == 0 {
		return steps
	}

	// Build transcript-side identity keys and count frequencies.
	txFreq := make(map[dedupKey]int)
	for _, s := range steps {
		if s.EventSource != "transcript" || !s.ToolName.Valid {
			continue
		}
		canon, ok := copilotMutationCanonical[s.ToolName.String]
		if !ok {
			continue
		}
		if canon == "bash" {
			cmd := commandFor(s)
			if cmd != "" {
				txFreq[dedupKey{"bash", cmd}]++
			}
		} else {
			for _, fp := range pathsFor(s) {
				txFreq[dedupKey{canon, fp}]++
			}
		}
	}

	// isUnique returns true when the identity key is unique on both sides:
	// exactly 1 transcript entry with (txCanon, key), and exactly 1 hook
	// entry with any of hookCanons and the same key.
	isUnique := func(hookCanons []string, txCanon, key string) bool {
		if txFreq[dedupKey{txCanon, key}] != 1 {
			return false
		}
		for _, c := range hookCanons {
			if hookFreq[dedupKey{c, key}] == 1 {
				return true
			}
		}
		return false
	}

	// Track which hook entries have been consumed (1:1).
	hookUsed := make([]bool, len(hookEntries))
	consumeHook := func(canons []string, key string) bool {
		for i, e := range hookEntries {
			if hookUsed[i] {
				continue
			}
			for _, c := range canons {
				if e.canon == c && e.key == key {
					hookUsed[i] = true
					return true
				}
			}
		}
		return false
	}

	// txRequestPaths is the set of file paths that have a request-side
	// transcript entry (create/edit/write). copilot_file_edit is the
	// completion half of the same operation and should follow its twin's
	// fate: suppressed only when the twin was matched, kept otherwise.
	txRequestPaths := make(map[string]bool)
	for _, s := range steps {
		if s.EventSource != "transcript" || !s.ToolName.Valid {
			continue
		}
		canon := copilotMutationCanonical[s.ToolName.String]
		if canon != "" && canon != "bash" && canon != "file_edit" {
			for _, fp := range pathsFor(s) {
				txRequestPaths[fp] = true
			}
		}
	}

	// matchedPaths tracks file paths whose transcript create/edit/write
	// entry was successfully matched to a hook twin. A copilot_file_edit
	// entry for the same path is the completion half of that same logical
	// step and should also be suppressed.
	matchedPaths := make(map[string]bool)

	filtered := make([]sqldb.ListStepEventsForTurnRow, 0, len(steps))
	for _, s := range steps {
		if s.EventSource != "transcript" || !s.ToolName.Valid {
			filtered = append(filtered, s)
			continue
		}
		canon, isMutation := copilotMutationCanonical[s.ToolName.String]
		if !isMutation {
			filtered = append(filtered, s)
			continue
		}

		matched := false
		if canon == "bash" {
			cmd := commandFor(s)
			if cmd != "" && isUnique([]string{"bash"}, "bash", cmd) {
				matched = consumeHook([]string{"bash"}, cmd)
			}
		} else if canon == "file_edit" {
			// copilot_file_edit is the tool.execution_complete entry for a
			// file operation whose request side is a create/edit transcript
			// entry. It should follow its twin's fate: suppressed when the
			// twin was matched, kept otherwise.
			paths := pathsFor(s)
			hasTwin := false
			for _, fp := range paths {
				if txRequestPaths[fp] {
					hasTwin = true
					if matchedPaths[fp] {
						matched = true
					}
					break
				}
			}
			if !matched && !hasTwin {
				// No request-side twin at all - copilot_file_edit is the
				// sole transcript entry for this file. Try direct hook matching.
				for _, fp := range paths {
					if isUnique([]string{"write", "edit"}, canon, fp) && consumeHook([]string{"write", "edit"}, fp) {
						matched = true
						break
					}
				}
			}
		} else {
			hookCanons := []string{canon}
			for _, fp := range pathsFor(s) {
				if isUnique(hookCanons, canon, fp) && consumeHook(hookCanons, fp) {
					matched = true
					matchedPaths[fp] = true
					break
				}
			}
		}
		if matched {
			continue // suppress
		}
		filtered = append(filtered, s)
	}
	return filtered
}

// extractProvenanceCommand reads a provenance blob and returns the
// tool_input.command value. Returns "" if the blob is missing or does
// not contain a command.
func extractProvenanceCommand(ctx context.Context, bs *blobs.Store, hash sql.NullString) string {
	if !hash.Valid || hash.String == "" || bs == nil {
		return ""
	}
	blob, err := bs.Get(ctx, hash.String)
	if err != nil || len(blob) == 0 {
		return ""
	}
	var prov struct {
		ToolInput struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}
	if json.Unmarshal(blob, &prov) != nil {
		return ""
	}
	return prov.ToolInput.Command
}

// extractToolUsesFilePaths returns the file_path values from a tool_uses JSON column.
func extractToolUsesFilePaths(toolUses sql.NullString) []string {
	if !toolUses.Valid || toolUses.String == "" {
		return nil
	}
	var payload struct {
		Tools []struct {
			FilePath string `json:"file_path"`
		} `json:"tools"`
	}
	if json.Unmarshal([]byte(toolUses.String), &payload) != nil {
		return nil
	}
	var paths []string
	for _, t := range payload.Tools {
		if t.FilePath != "" {
			paths = append(paths, t.FilePath)
		}
	}
	return paths
}

// extractRepoRelativeFilePaths extracts file paths from a tool_uses JSON
// string and normalizes them to repo-relative paths.
func extractRepoRelativeFilePaths(toolUsesJSON, repoRoot string) []string {
	var payload struct {
		Tools []struct {
			FilePath string `json:"file_path"`
		} `json:"tools"`
	}
	if json.Unmarshal([]byte(toolUsesJSON), &payload) != nil {
		return nil
	}
	var paths []string
	for _, t := range payload.Tools {
		if t.FilePath == "" {
			continue
		}
		rel := toRepoRelative(t.FilePath, repoRoot)
		if rel != "" {
			paths = append(paths, rel)
		}
	}
	return paths
}

// toRepoRelative converts a path to repo-relative.
// Returns empty string for any path (absolute or relative) that
// escapes the repo root, so machine-specific or out-of-repo paths
// never leak into the bundle.
func toRepoRelative(p, repoRoot string) string {
	if repoRoot == "" || p == "" {
		return p
	}
	if platform.LooksAbsolutePath(p) {
		rel, err := filepath.Rel(filepath.Clean(repoRoot), filepath.Clean(p))
		if err != nil {
			return ""
		}
		p = rel
	}
	cleaned := filepath.ToSlash(filepath.Clean(p))
	if strings.HasPrefix(cleaned, "..") {
		return ""
	}
	return cleaned
}
