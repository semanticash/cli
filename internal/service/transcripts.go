package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/platform"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"
)

type TranscriptService struct{}

func NewTranscriptService() *TranscriptService { return &TranscriptService{} }

type TranscriptsForCheckpointInput struct {
	RepoPath     string
	CheckpointID string
	Raw          bool   // if true, load payload JSON from blob store
	Verbose      bool   // reserved for CLI formatting; included for symmetry
	Cumulative   bool   // if true, show all events up to checkpoint; default is delta (since previous checkpoint)
	BySession    bool   // if true, group events by session
	SessionID    string // if set, filter to a specific session
	Commit       bool   // if true, filter to sessions that touched files in the commit diff
}

type TranscriptMeta struct {
	CheckpointID string `json:"checkpoint_id"`
	CommitHash   string `json:"commit_hash,omitempty"`
	SessionCount int64  `json:"session_count"`
}

type TranscriptEvent struct {
	EventID           string `json:"event_id"`
	SessionID         string `json:"session_id"`
	Provider          string `json:"provider,omitempty"`
	Ts                int64  `json:"ts"`
	TsISO             string `json:"ts_iso"`
	Kind              string `json:"kind"`
	Role              string `json:"role,omitempty"`
	RoleUpper         string `json:"role_upper,omitempty"`
	ToolName          string `json:"tool_name,omitempty"`
	FilePath          string `json:"file_path,omitempty"`
	FileOp            string `json:"file_op,omitempty"`
	HasThinking       bool   `json:"has_thinking,omitempty"`
	ToolUsesJSON      string `json:"tool_uses,omitempty"`
	TokensIn          int64  `json:"tokens_in,omitempty"`
	TokensOut         int64  `json:"tokens_out,omitempty"`
	TokensCacheRead   int64  `json:"tokens_cache_read,omitempty"`
	TokensCacheCreate int64  `json:"tokens_cache_create,omitempty"`
	Summary           string `json:"summary,omitempty"`
	ProviderEventID   string `json:"provider_event_id,omitempty"`
	PayloadHash       string `json:"payload_hash,omitempty"`
	Payload           string `json:"payload,omitempty"` // only when Raw=true
}

type SessionTranscript struct {
	SessionID         string            `json:"session_id"`
	ProviderSessionID string            `json:"provider_session_id"`
	Provider          string            `json:"provider"`
	Events            []TranscriptEvent `json:"events"`
}

type TranscriptsForCheckpointResult struct {
	Meta     TranscriptMeta      `json:"meta"`
	Events   []TranscriptEvent   `json:"events"`
	Sessions []SessionTranscript `json:"sessions,omitempty"`
}

// TranscriptsInput is the polymorphic entry point. Ref is resolved as a
// checkpoint or session ID (prefix matching). ForceCheckpoint / ForceSession
// bypass auto-resolution.
type TranscriptsInput struct {
	RepoPath        string
	Ref             string
	ForceCheckpoint bool
	ForceSession    bool
	// Checkpoint-mode flags (ignored/invalid in session mode).
	Raw             bool
	Verbose         bool
	Cumulative      bool
	BySession       bool
	FilterSessionID string
	Commit          bool
}

// TranscriptsResult wraps the output from either resolution path.
type TranscriptsResult struct {
	ResolvedAs string                         `json:"resolved_as"` // "checkpoint" or "session"
	Checkpoint *TranscriptsForCheckpointResult `json:"checkpoint,omitempty"`
	Session    *SessionTranscript              `json:"session,omitempty"`
}

func (s *TranscriptService) TranscriptsForCheckpoint(ctx context.Context, in TranscriptsForCheckpointInput) (*TranscriptsForCheckpointResult, error) {
	if strings.TrimSpace(in.CheckpointID) == "" {
		return nil, fmt.Errorf("checkpoint_id is required")
	}

	repoPath := in.RepoPath
	if strings.TrimSpace(repoPath) == "" {
		repoPath = "." // matches your other services default pattern
	}

	repo, err := git.OpenRepo(repoPath)
	if err != nil {
		return nil, err
	}
	repoRoot := repo.Root()

	semDir := filepath.Join(repoRoot, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")
	objectsDir := filepath.Join(semDir, "objects")

	if !util.IsEnabled(semDir) {
		return nil, fmt.Errorf("semantica is disabled. run `semantica enable` to re-enable")
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return nil, err
	}
	defer func() { _ = sqlstore.Close(h) }()

	// Resolve prefix / short ID to full checkpoint UUID.
	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, repoRoot)
	if err != nil {
		return nil, fmt.Errorf("repository not found for path %s", repoRoot)
	}
	resolvedID, err := sqlstore.ResolveCheckpointID(ctx, h.Queries, repoRow.RepositoryID, in.CheckpointID)
	if err != nil {
		return nil, err
	}
	in.CheckpointID = resolvedID

	// Ensure checkpoint exists
	cp, err := h.Queries.GetCheckpointByID(ctx, in.CheckpointID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("checkpoint not found: %s", in.CheckpointID)
		}
		return nil, err
	}

	// Get commit hash if linked.
	var commitHash string
	if links, err := h.Queries.GetCommitLinksByCheckpoint(ctx, in.CheckpointID); err == nil && len(links) > 0 {
		commitHash = links[0].CommitHash
	}

	if in.Commit && commitHash == "" {
		return nil, fmt.Errorf("--commit requires a commit-linked checkpoint")
	}

	// When --commit is set, compute the changed file set for diff-based filtering.
	// Paths are normalized to forward-slash repo-relative form.
	var changedFiles map[string]struct{}
	if in.Commit {
		files, err := repo.ChangedFilesForCommit(ctx, commitHash)
		if err != nil {
			return nil, fmt.Errorf("get changed files: %w", err)
		}
		changedFiles = make(map[string]struct{}, len(files))
		for _, f := range files {
			changedFiles[f] = struct{}{}
		}
	}

	// Query events directly by repository + time window (no session_checkpoints dependency).
	// Anchor the delta window to the previous commit-linked checkpoint,
	// ignoring manual/baseline checkpoints - matches attribution behaviour.
	var afterTs int64
	if !in.Cumulative {
		prev, err := h.Queries.GetPreviousCommitLinkedCheckpoint(ctx, sqldb.GetPreviousCommitLinkedCheckpointParams{
			RepositoryID: cp.RepositoryID,
			CreatedAt:    cp.CreatedAt,
		})
		if err == nil {
			afterTs = prev.CreatedAt
		}
		// If no previous commit-linked checkpoint (sql.ErrNoRows), afterTs stays 0 -
		// which means "everything up to this checkpoint" (same as cumulative for the first checkpoint).
	}

	events, err := h.Queries.ListTranscriptEvents(ctx, sqldb.ListTranscriptEventsParams{
		RepositoryID: cp.RepositoryID,
		AfterTs:      afterTs,
		UntilTs:      cp.CreatedAt,
	})
	if err != nil {
		return nil, err
	}

	// Derive session count from the result set.
	sessionSeen := make(map[string]struct{})
	for _, e := range events {
		sessionSeen[e.SessionID] = struct{}{}
	}
	sessionCount := int64(len(sessionSeen))

	var bs *blobs.Store
	if in.Raw {
		bs, err = blobs.NewStore(objectsDir)
		if err != nil {
			return nil, fmt.Errorf("init blob store: %w", err)
		}
	}

	out := &TranscriptsForCheckpointResult{
		Meta: TranscriptMeta{
			CheckpointID: cp.CheckpointID,
			CommitHash:   commitHash,
			SessionCount: sessionCount,
		},
		Events: make([]TranscriptEvent, 0, len(events)),
	}

	// Request-scoped payload cache. Claude Code's tool-use pattern
	// often repeats the same PayloadHash across events in a session,
	// so deduping cuts blob-store round-trips. payloadSeen records
	// every hash we've already attempted (including failures and
	// empty payloads) so we don't retry in the same request.
	var (
		payloadCache map[string]string
		payloadSeen  map[string]bool
	)
	if in.Raw && bs != nil {
		payloadCache = make(map[string]string)
		payloadSeen = make(map[string]bool)
	}

	for _, e := range events {
		ev := TranscriptEvent{
			EventID:           e.EventID,
			SessionID:         e.SessionID,
			Provider:          e.Provider,
			Ts:                e.Ts,
			TsISO:             time.UnixMilli(e.Ts).UTC().Format(time.RFC3339),
			Kind:              e.Kind,
			Role:              nullStr(e.Role),
			ToolUsesJSON:      nullStr(e.ToolUses),
			TokensIn:          e.TokensIn.Int64,
			TokensOut:         e.TokensOut.Int64,
			TokensCacheRead:   e.TokensCacheRead.Int64,
			TokensCacheCreate: e.TokensCacheCreate.Int64,
			Summary:           nullStr(e.Summary),
			ProviderEventID:   nullStr(e.ProviderEventID),
			PayloadHash:       nullStr(e.PayloadHash),
		}
		if ev.Role != "" {
			ev.RoleUpper = strings.ToUpper(ev.Role)
		}
		enrichFromToolUses(&ev)

		if in.Raw && bs != nil && ev.PayloadHash != "" {
			if cached, ok := payloadCache[ev.PayloadHash]; ok {
				ev.Payload = cached
			} else if !payloadSeen[ev.PayloadHash] {
				raw, err := bs.Get(ctx, ev.PayloadHash)
				payloadSeen[ev.PayloadHash] = true
				if err != nil {
					slog.Warn("transcripts: payload load failed",
						"event_id", ev.EventID,
						"payload_hash", ev.PayloadHash,
						"err", err)
				} else if len(raw) > 0 {
					ev.Payload = string(raw)
					payloadCache[ev.PayloadHash] = ev.Payload
				}
			}
		}

		out.Events = append(out.Events, ev)
	}

	// Commit-scope filtering: keep only events from sessions that touched
	// files changed in the commit diff. Once a session has at least one matching event,
	// all its events in the window are included (coherent narrative).
	if in.Commit && changedFiles != nil {
		// Pass 1: find sessions with at least one event matching a changed file.
		matchedSessions := make(map[string]struct{})
		for i := range out.Events {
			if _, ok := matchedSessions[out.Events[i].SessionID]; ok {
				continue // already matched
			}
			for _, fp := range extractToolFilePaths(out.Events[i].ToolUsesJSON) {
				rel := normalizeToolPath(fp, repoRoot)
				if _, ok := changedFiles[rel]; ok {
					matchedSessions[out.Events[i].SessionID] = struct{}{}
					break
				}
			}
		}

		// Pass 2: filter events to matched sessions.
		filtered := make([]TranscriptEvent, 0, len(out.Events))
		for _, ev := range out.Events {
			if _, ok := matchedSessions[ev.SessionID]; ok {
				filtered = append(filtered, ev)
			}
		}
		out.Events = filtered

		// Recompute session count.
		out.Meta.SessionCount = int64(len(matchedSessions))
	}

	// Session grouping: when BySession is set, group events by session
	// and attach session metadata.
	if in.BySession || in.SessionID != "" {
		// Build session groups from events.
		sessionOrder := make([]string, 0)
		sessionMap := make(map[string]*SessionTranscript)

		for i := range out.Events {
			ev := &out.Events[i]
			if in.SessionID != "" && ev.SessionID != in.SessionID {
				continue
			}
			st, ok := sessionMap[ev.SessionID]
			if !ok {
				st = &SessionTranscript{
					SessionID: ev.SessionID,
					Provider:  ev.Provider,
				}
				sessionMap[ev.SessionID] = st
				sessionOrder = append(sessionOrder, ev.SessionID)
			}
			st.Events = append(st.Events, *ev)
		}

		// Look up session metadata by ID (no session_checkpoints dependency).
		sessLookup := make(map[string]sqldb.AgentSession)
		for _, sid := range sessionOrder {
			if sess, err := h.Queries.GetAgentSessionByID(ctx, sid); err == nil {
				sessLookup[sid] = sess
			}
		}

		for _, sid := range sessionOrder {
			st := sessionMap[sid]
			if sess, ok := sessLookup[sid]; ok {
				st.ProviderSessionID = sess.ProviderSessionID
				st.Provider = sess.Provider
			}
			out.Sessions = append(out.Sessions, *st)
		}
	}

	return out, nil
}

// TranscriptsForSessionInput holds parameters for session-first transcript access.
type TranscriptsForSessionInput struct {
	RepoPath  string
	SessionID string
	Raw       bool
}

// TranscriptsForSession loads the transcript for a specific session by ID.
func (s *TranscriptService) TranscriptsForSession(ctx context.Context, in TranscriptsForSessionInput) (*SessionTranscript, error) {
	if strings.TrimSpace(in.SessionID) == "" {
		return nil, fmt.Errorf("session_id is required")
	}

	repoPath := in.RepoPath
	if strings.TrimSpace(repoPath) == "" {
		repoPath = "."
	}

	repo, err := git.OpenRepo(repoPath)
	if err != nil {
		return nil, err
	}
	repoRoot := repo.Root()

	semDir := filepath.Join(repoRoot, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")
	objectsDir := filepath.Join(semDir, "objects")

	if !util.IsEnabled(semDir) {
		return nil, fmt.Errorf("semantica is disabled. run `semantica enable` to re-enable")
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return nil, err
	}
	defer func() { _ = sqlstore.Close(h) }()

	// Resolve prefix to full session ID.
	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, repoRoot)
	if err != nil {
		return nil, fmt.Errorf("repository not found: %w", err)
	}
	resolvedID, err := sqlstore.ResolveSessionID(ctx, h.Queries, repoRow.RepositoryID, in.SessionID)
	if err != nil {
		return nil, err
	}
	in.SessionID = resolvedID

	sess, err := h.Queries.GetAgentSessionByID(ctx, in.SessionID)
	if err != nil {
		return nil, fmt.Errorf("session not found: %w", err)
	}

	rows, err := h.Queries.ListEventsBySessionASC(ctx, in.SessionID)
	if err != nil {
		return nil, err
	}

	var bs *blobs.Store
	if in.Raw {
		bs, err = blobs.NewStore(objectsDir)
		if err != nil {
			return nil, fmt.Errorf("init blob store: %w", err)
		}
	}

	result := &SessionTranscript{
		SessionID:         sess.SessionID,
		ProviderSessionID: sess.ProviderSessionID,
		Provider:          sess.Provider,
		Events:            make([]TranscriptEvent, 0, len(rows)),
	}

	for _, r := range rows {
		ev := TranscriptEvent{
			EventID:           r.EventID,
			SessionID:         r.SessionID,
			Provider:          r.Provider,
			Ts:                r.Ts,
			TsISO:             time.UnixMilli(r.Ts).UTC().Format(time.RFC3339),
			Kind:              r.Kind,
			Role:              nullStr(r.Role),
			ToolUsesJSON:      nullStr(r.ToolUses),
			TokensIn:          r.TokensIn.Int64,
			TokensOut:         r.TokensOut.Int64,
			TokensCacheRead:   r.TokensCacheRead.Int64,
			TokensCacheCreate: r.TokensCacheCreate.Int64,
			Summary:           nullStr(r.Summary),
			ProviderEventID:   nullStr(r.ProviderEventID),
			PayloadHash:       nullStr(r.PayloadHash),
		}
		if ev.Role != "" {
			ev.RoleUpper = strings.ToUpper(ev.Role)
		}
		enrichFromToolUses(&ev)

		if in.Raw && bs != nil && ev.PayloadHash != "" {
			if raw, err := bs.Get(ctx, ev.PayloadHash); err == nil && len(raw) > 0 {
				ev.Payload = string(raw)
			}
		}

		result.Events = append(result.Events, ev)
	}

	return result, nil
}

// Transcripts is the polymorphic entry point: resolves Ref as either a
// checkpoint or session ID, then delegates to the appropriate method.
func (s *TranscriptService) Transcripts(ctx context.Context, in TranscriptsInput) (*TranscriptsResult, error) {
	if strings.TrimSpace(in.Ref) == "" {
		return nil, fmt.Errorf("ref is required")
	}
	if in.ForceCheckpoint && in.ForceSession {
		return nil, fmt.Errorf("--checkpoint and --session are mutually exclusive")
	}

	repoPath := in.RepoPath
	if strings.TrimSpace(repoPath) == "" {
		repoPath = "."
	}

	repo, err := git.OpenRepo(repoPath)
	if err != nil {
		return nil, err
	}
	repoRoot := repo.Root()

	semDir := filepath.Join(repoRoot, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")

	if !util.IsEnabled(semDir) {
		return nil, fmt.Errorf("semantica is disabled. run `semantica enable` to re-enable")
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return nil, err
	}
	defer func() { _ = sqlstore.Close(h) }()

	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, repoRoot)
	if err != nil {
		return nil, fmt.Errorf("repository not found for path %s", repoRoot)
	}
	repoID := repoRow.RepositoryID

	// Resolution: try checkpoint and/or session based on flags.
	var resolvedCheckpoint, resolvedSession string

	if in.ForceCheckpoint {
		resolvedCheckpoint, err = sqlstore.ResolveCheckpointID(ctx, h.Queries, repoID, in.Ref)
		if err != nil {
			return nil, err
		}
	} else if in.ForceSession {
		resolvedSession, err = sqlstore.ResolveSessionID(ctx, h.Queries, repoID, in.Ref)
		if err != nil {
			return nil, err
		}
	} else {
		// Auto-resolve: try both.
		cpID, cpErr := sqlstore.ResolveCheckpointID(ctx, h.Queries, repoID, in.Ref)
		sessID, sessErr := sqlstore.ResolveSessionID(ctx, h.Queries, repoID, in.Ref)

		cpFound := cpErr == nil
		sessFound := sessErr == nil

		switch {
		case cpFound && sessFound:
			return nil, fmt.Errorf("ref %q matches both a checkpoint and a session; use --checkpoint or --session to disambiguate", in.Ref)
		case cpFound:
			resolvedCheckpoint = cpID
		case sessFound:
			resolvedSession = sessID
		default:
			return nil, fmt.Errorf("ref not found: %s", in.Ref)
		}
	}

	// Validate checkpoint-only flags when resolved as session.
	if resolvedSession != "" {
		if in.Cumulative || in.BySession || in.Commit || in.FilterSessionID != "" {
			return nil, fmt.Errorf("flags --cumulative, --by-session, --commit, and --filter-session are only valid for checkpoint refs")
		}
	}

	// Delegate.
	if resolvedCheckpoint != "" {
		res, err := s.TranscriptsForCheckpoint(ctx, TranscriptsForCheckpointInput{
			RepoPath:     in.RepoPath,
			CheckpointID: resolvedCheckpoint,
			Raw:          in.Raw,
			Verbose:      in.Verbose,
			Cumulative:   in.Cumulative,
			BySession:    in.BySession,
			SessionID:    in.FilterSessionID,
			Commit:       in.Commit,
		})
		if err != nil {
			return nil, err
		}
		return &TranscriptsResult{
			ResolvedAs: "checkpoint",
			Checkpoint: res,
		}, nil
	}

	res, err := s.TranscriptsForSession(ctx, TranscriptsForSessionInput{
		RepoPath:  in.RepoPath,
		SessionID: resolvedSession,
		Raw:       in.Raw,
	})
	if err != nil {
		return nil, err
	}
	return &TranscriptsResult{
		ResolvedAs: "session",
		Session:    res,
	}, nil
}

func nullStr(ns sql.NullString) string {
	if !ns.Valid {
		return ""
	}
	return ns.String
}

// enrichFromToolUses parses the tool_uses JSON column to populate ToolName,
// FilePath, FileOp, and HasThinking at runtime. Handles both the new format
// ({"content_types":[...],"tools":[...]}) and the legacy format ([{...},...]).
func enrichFromToolUses(ev *TranscriptEvent) {
	if ev.ToolUsesJSON == "" {
		return
	}

	s := ev.ToolUsesJSON

	// Try new format first: {"content_types":[...],"tools":[...]}
	var newFmt struct {
		ContentTypes []string `json:"content_types"`
		Tools        []struct {
			Name     string `json:"name"`
			FilePath string `json:"file_path"`
			FileOp   string `json:"file_op"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(s), &newFmt); err == nil && (len(newFmt.ContentTypes) > 0 || len(newFmt.Tools) > 0) {
		for _, ct := range newFmt.ContentTypes {
			if ct == "thinking" {
				ev.HasThinking = true
				break
			}
		}
		if len(newFmt.Tools) > 0 {
			ev.ToolName = newFmt.Tools[0].Name
			ev.FilePath = newFmt.Tools[0].FilePath
			ev.FileOp = newFmt.Tools[0].FileOp
		}
		return
	}

	// Legacy format: [{"name":"Edit","file_path":"/foo.go","file_op":"edit"}]
	var legacy []struct {
		Name     string `json:"name"`
		FilePath string `json:"file_path"`
		FileOp   string `json:"file_op"`
	}
	if err := json.Unmarshal([]byte(s), &legacy); err == nil && len(legacy) > 0 {
		ev.ToolName = legacy[0].Name
		ev.FilePath = legacy[0].FilePath
		ev.FileOp = legacy[0].FileOp
	}
}

// extractToolFilePaths returns all file paths from a tool_uses JSON column.
// Handles both the new format ({"tools":[...]}) and legacy format ([{...}]).
func extractToolFilePaths(toolUsesJSON string) []string {
	if toolUsesJSON == "" {
		return nil
	}

	// Try new format: {"tools":[{"file_path":"..."},...]}}
	var newFmt struct {
		Tools []struct {
			FilePath string `json:"file_path"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(toolUsesJSON), &newFmt); err == nil && len(newFmt.Tools) > 0 {
		var paths []string
		for _, t := range newFmt.Tools {
			if t.FilePath != "" {
				paths = append(paths, t.FilePath)
			}
		}
		return paths
	}

	// Legacy format: [{"file_path":"..."},...]
	var legacy []struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal([]byte(toolUsesJSON), &legacy); err == nil {
		var paths []string
		for _, t := range legacy {
			if t.FilePath != "" {
				paths = append(paths, t.FilePath)
			}
		}
		return paths
	}

	return nil
}

// normalizeToolPath converts a file path from tool_uses (which may be absolute,
// OS-specific, or prefixed with file://) into a repo-relative forward-slash
// path that matches git output.
func normalizeToolPath(fp, repoRoot string) string {
	fp = strings.TrimSpace(fp)
	if fp == "" {
		return ""
	}

	// Handle file:// URIs robustly.
	if strings.HasPrefix(fp, "file:") {
		if u, err := url.Parse(fp); err == nil {
			// For file:// URIs, Path is slash form. On Windows, may have /C:/...
			p := u.Path
			if runtime.GOOS == "windows" && len(p) >= 3 && p[0] == '/' && p[2] == ':' {
				p = p[1:] // "/C:/x" -> "C:/x"
			}
			if p != "" {
				fp = p
			}
		} else {
			// Fallback: only strip the scheme prefix, not the whole "file://"
			fp = strings.TrimPrefix(fp, "file://")
		}
	}

	// If absolute, make relative to repoRoot.
	if platform.LooksAbsolutePath(fp) {
		rel, err := filepath.Rel(repoRoot, fp)
		if err != nil {
			return ""
		}
		// Outside repo? ignore to prevent false positives.
		relSlash := filepath.ToSlash(rel)
		if relSlash == ".." || strings.HasPrefix(relSlash, "../") {
			return ""
		}
		fp = rel
	}

	// Normalize to forward slashes (matches git output) and strip leading ./.
	fp = filepath.ToSlash(fp)
	fp = strings.TrimPrefix(fp, "./")
	return fp
}
