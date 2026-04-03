package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/semanticash/cli/internal/git"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"
)

type ExplainService struct{}

func NewExplainService() *ExplainService { return &ExplainService{} }

type ExplainInput struct {
	RepoPath string
	Ref      string // commit hash or prefix
}

type ExplainResult struct {
	CommitHash    string `json:"commit_hash"`
	CheckpointID  string `json:"checkpoint_id"`
	CommitSubject string `json:"commit_subject"`
	// Git facts
	FilesChanged int         `json:"files_changed"`
	LinesAdded   int         `json:"lines_added"`
	LinesDeleted int         `json:"lines_deleted"`
	TopFiles     []FileDelta `json:"top_files"`
	// Attribution facts
	AIPercentage   float64 `json:"ai_percentage"`
	AILines        int     `json:"ai_lines"`
	HumanLines     int     `json:"human_lines"`
	FilesWithAI    int     `json:"files_with_ai"`
	FilesHumanOnly int     `json:"files_human_only"`
	// Session facts
	SessionCount int              `json:"session_count"`
	RootSessions int              `json:"root_sessions"`
	Subagents    int              `json:"subagents"`
	Sessions     []SessionSummary `json:"sessions,omitempty"`
	// Transcript (for --generate)
	Transcript []TranscriptEventSummary `json:"transcript,omitempty"`
	// Persisted summary (from --generate)
	Summary *NarrativeResultJSON `json:"summary,omitempty"`
}

// TranscriptEventSummary is a lightweight event for the condensed transcript.
type TranscriptEventSummary struct {
	Role     string `json:"role"`
	Summary  string `json:"summary,omitempty"`
	ToolName string `json:"tool_name,omitempty"`
	FilePath string `json:"file_path,omitempty"`
}

type FileDelta struct {
	Path       string  `json:"path"`
	Added      int     `json:"added"`          // added non-blank lines (same basis as attribution)
	Deleted    int     `json:"deleted"`         // deleted non-blank lines
	TotalLines int     `json:"total_lines"`     // equals Added (added non-blank lines)
	AILines    int     `json:"ai_lines"`
	HumanLines int     `json:"human_lines"`
	AIPercent  float64 `json:"ai_percentage"`
}

type SessionSummary struct {
	SessionID     string `json:"session_id"`
	Provider      string `json:"provider"`
	IsSubagent    bool   `json:"is_subagent,omitempty"`
	StepCount     int64  `json:"step_count"`
	ToolCallCount int64  `json:"tool_call_count"`
	TokensIn      int64  `json:"tokens_in"`
	TokensOut     int64  `json:"tokens_out"`
	TokensCached  int64  `json:"tokens_cached,omitempty"`
}

// NarrativeResultJSON is the persisted form of an LLM-generated playbook.
type NarrativeResultJSON struct {
	Title     string   `json:"title"`
	Intent    string   `json:"intent"`
	Outcome   string   `json:"outcome"`
	Learnings []string `json:"learnings"`
	Friction  []string `json:"friction"`
	OpenItems []string `json:"open_items"`
	Keywords  []string `json:"keywords"`
}

// SaveSummaryInput contains the data needed to persist a generated summary.
type SaveSummaryInput struct {
	RepoPath     string
	CheckpointID string
	Summary      *NarrativeResultJSON
	Model        string
}

// SaveSummary persists a generated summary to the database.
func (s *ExplainService) SaveSummary(ctx context.Context, in SaveSummaryInput) error {
	repoPath := in.RepoPath
	if strings.TrimSpace(repoPath) == "" {
		repoPath = "."
	}

	repo, err := git.OpenRepo(repoPath)
	if err != nil {
		return err
	}
	repoRoot := repo.Root()

	semDir := filepath.Join(repoRoot, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return err
	}
	defer func() { _ = sqlstore.Close(h) }()

	summaryJSON, err := json.Marshal(in.Summary)
	if err != nil {
		return fmt.Errorf("marshal summary: %w", err)
	}

	if err := h.Queries.SaveCheckpointSummary(ctx, sqldb.SaveCheckpointSummaryParams{
		CheckpointID: in.CheckpointID,
		SummaryJson:  sql.NullString{String: string(summaryJSON), Valid: true},
		SummaryModel: sql.NullString{String: in.Model, Valid: in.Model != ""},
	}); err != nil {
		return err
	}

	return nil
}

func (s *ExplainService) Explain(ctx context.Context, in ExplainInput) (*ExplainResult, error) {
	if strings.TrimSpace(in.Ref) == "" {
		return nil, fmt.Errorf("ref is required")
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

	// Resolve git refs (HEAD, HEAD~1, etc.) to full hashes before DB lookup.
	ref := in.Ref
	if resolved, gitErr := repo.ResolveRef(ctx, ref); gitErr == nil {
		ref = resolved
	}

	// Resolve ref as commit hash or checkpoint (exact, then prefix).
	commitHash, err := resolveCommitRef(ctx, h.Queries, repoID, ref)
	if err != nil {
		return nil, err
	}

	// --- Git facts ---
	subject, _ := repo.CommitSubject(ctx, commitHash)

	// --- Attribution facts ---
	attrSvc := NewAttributionService()
	blame, err := attrSvc.Blame(ctx, BlameInput{RepoPath: in.RepoPath, Ref: commitHash})
	if err != nil {
		return nil, fmt.Errorf("attribution: %w", err)
	}

	// Derive header totals from attribution (non-blank lines only).
	var totalAdded, totalDeleted int
	filesWithAI := 0
	for _, f := range blame.Files {
		totalAdded += f.TotalLines
		totalDeleted += f.DeletedNonBlank
		if f.AIExactLines+f.AIFormattedLines+f.AIModifiedLines > 0 {
			filesWithAI++
		}
	}
	filesChanged := len(blame.Files)
	filesHumanOnly := filesChanged - filesWithAI

	// Top files by total attribution-scoped lines (added non-blank), max 5.
	blameFiles := make([]FileAttribution, len(blame.Files))
	copy(blameFiles, blame.Files)
	sort.Slice(blameFiles, func(i, j int) bool {
		return blameFiles[i].TotalLines > blameFiles[j].TotalLines
	})
	topN := 5
	if len(blameFiles) < topN {
		topN = len(blameFiles)
	}
	topFiles := make([]FileDelta, topN)
	for i := 0; i < topN; i++ {
		f := blameFiles[i]
		aiLines := f.AIExactLines + f.AIFormattedLines + f.AIModifiedLines
		topFiles[i] = FileDelta{
			Path:       f.Path,
			Added:      f.TotalLines,
			Deleted:    f.DeletedNonBlank,
			TotalLines: f.TotalLines,
			AILines:    aiLines,
			HumanLines: f.HumanLines,
			AIPercent:  f.AIPercent,
		}
	}

	// --- Session facts + transcript ---
	// Resolve commit -> checkpoint -> time window, then find sessions in scope.
	sessions, transcript, err := s.sessionsForCommit(ctx, h, repo, repoRoot, repoID, commitHash)
	if err != nil {
		// Non-fatal: session data may not exist.
		sessions = nil
		transcript = nil
	}

	rootCount, subCount := 0, 0
	for _, sess := range sessions {
		if sess.IsSubagent {
			subCount++
		} else {
			rootCount++
		}
	}

	// Try to load persisted summary.
	var summary *NarrativeResultJSON
	if row, err := h.Queries.GetCheckpointSummary(ctx, blame.CheckpointID); err == nil && row.SummaryJson.Valid {
		var s NarrativeResultJSON
		if json.Unmarshal([]byte(row.SummaryJson.String), &s) == nil {
			summary = &s
		}
	}

	return &ExplainResult{
		CommitHash:     commitHash,
		CheckpointID:   blame.CheckpointID,
		CommitSubject:  subject,
		FilesChanged:   filesChanged,
		LinesAdded:     totalAdded,
		LinesDeleted:   totalDeleted,
		TopFiles:       topFiles,
		AIPercentage:   blame.AIPercentage,
		AILines:        blame.AILines,
		HumanLines:     blame.HumanLines,
		FilesWithAI:    filesWithAI,
		FilesHumanOnly: filesHumanOnly,
		SessionCount:   len(sessions),
		RootSessions:   rootCount,
		Subagents:      subCount,
		Sessions:       sessions,
		Transcript:     transcript,
		Summary:        summary,
	}, nil
}

// resolveCommitRef resolves a ref as a commit hash or checkpoint ID.
// It tries commit hash first (exact, then prefix), then falls back to
// checkpoint ID resolution (matching the blame command's behaviour).
func resolveCommitRef(ctx context.Context, q *sqldb.Queries, repoID, ref string) (string, error) {
	// Try as commit hash - exact match in commit_links.
	link, err := q.GetCommitLinkByCommitHash(ctx, ref)
	if err == nil {
		return link.CommitHash, nil
	}

	// Try as commit hash - prefix match in commit_links.
	matches, err := q.ResolveCommitLinkByPrefix(ctx, sqldb.ResolveCommitLinkByPrefixParams{
		CommitHash:   ref + "%",
		RepositoryID: repoID,
	})
	if err == nil && len(matches) == 1 {
		return matches[0], nil
	}
	if err == nil && len(matches) > 1 {
		return "", fmt.Errorf("commit prefix %q is ambiguous, provide more characters", ref)
	}

	// Try as checkpoint ID.
	cpID, cpErr := sqlstore.ResolveCheckpointID(ctx, q, repoID, ref)
	if cpErr != nil {
		return "", fmt.Errorf("ref %q is not a known commit or checkpoint", ref)
	}

	// Resolve checkpoint -> linked commit.
	cpLinks, err := q.GetCommitLinksByCheckpoint(ctx, cpID)
	if err != nil || len(cpLinks) == 0 {
		return "", fmt.Errorf("checkpoint %s has no linked commit", util.ShortID(cpID))
	}
	return cpLinks[0].CommitHash, nil
}

// sessionsForCommit finds agent sessions that touched files in the commit diff,
// computes per-session stats, classifies root vs subagent, and returns a
// condensed transcript of matched events.
func (s *ExplainService) sessionsForCommit(
	ctx context.Context,
	h *sqlstore.Handle,
	repo *git.Repo,
	repoRoot, repoID, commitHash string,
) ([]SessionSummary, []TranscriptEventSummary, error) {
	// Get checkpoint linked to this commit.
	link, err := h.Queries.GetCommitLinkByCommitHash(ctx, commitHash)
	if err != nil {
		return nil, nil, fmt.Errorf("no checkpoint linked to commit")
	}

	cp, err := h.Queries.GetCheckpointByID(ctx, link.CheckpointID)
	if err != nil {
		return nil, nil, err
	}

	// Time window: previous commit-linked checkpoint -> this checkpoint.
	var afterTs int64
	prev, err := h.Queries.GetPreviousCommitLinkedCheckpoint(ctx, sqldb.GetPreviousCommitLinkedCheckpointParams{
		RepositoryID: cp.RepositoryID,
		CreatedAt:    cp.CreatedAt,
	})
	if err == nil {
		afterTs = prev.CreatedAt
	}

	// Get changed files for commit-scope filtering.
	changedFiles, err := repo.ChangedFilesForCommit(ctx, commitHash)
	if err != nil {
		return nil, nil, err
	}
	changedSet := make(map[string]struct{}, len(changedFiles))
	for _, f := range changedFiles {
		changedSet[f] = struct{}{}
	}

	// Query events in window.
	events, err := h.Queries.ListTranscriptEvents(ctx, sqldb.ListTranscriptEventsParams{
		RepositoryID: cp.RepositoryID,
		AfterTs:      afterTs,
		UntilTs:      cp.CreatedAt,
	})
	if err != nil {
		return nil, nil, err
	}

	// Pass 1: find sessions with at least one event touching a changed file.
	matchedSessions := make(map[string]struct{})
	for _, e := range events {
		if _, ok := matchedSessions[e.SessionID]; ok {
			continue
		}
		for _, fp := range extractToolFilePaths(nullStr(e.ToolUses)) {
			rel := normalizeToolPath(fp, repoRoot)
			if _, ok := changedSet[rel]; ok {
				matchedSessions[e.SessionID] = struct{}{}
				break
			}
		}
	}

	// Pass 2: compute per-session stats from matched sessions.
	type sessAcc struct {
		provider     string
		steps        int64
		toolCalls    int64
		tokensIn     int64
		tokensOut    int64
		tokensCached int64
	}
	accum := make(map[string]*sessAcc)
	for _, e := range events {
		if _, ok := matchedSessions[e.SessionID]; !ok {
			continue
		}
		a, ok := accum[e.SessionID]
		if !ok {
			a = &sessAcc{provider: e.Provider}
			accum[e.SessionID] = a
		}
		a.steps++
		if e.ToolUses.Valid && e.ToolUses.String != "" {
			a.toolCalls++
		}
		a.tokensIn += e.TokensIn.Int64
		a.tokensOut += e.TokensOut.Int64
		a.tokensCached += e.TokensCacheRead.Int64 + e.TokensCacheCreate.Int64
	}

	// Build result with root/subagent classification.
	var result []SessionSummary
	for sid, a := range accum {
		isSubagent := false
		if sess, err := h.Queries.GetAgentSessionByID(ctx, sid); err == nil {
			isSubagent = sess.ParentSessionID.Valid && sess.ParentSessionID.String != ""
		}
		result = append(result, SessionSummary{
			SessionID:     sid,
			Provider:      a.provider,
			IsSubagent:    isSubagent,
			StepCount:     a.steps,
			ToolCallCount: a.toolCalls,
			TokensIn:      a.tokensIn,
			TokensOut:     a.tokensOut,
			TokensCached:  a.tokensCached,
		})
	}

	// Sort: root sessions first, then subagents, each by steps descending.
	sort.Slice(result, func(i, j int) bool {
		if result[i].IsSubagent != result[j].IsSubagent {
			return !result[i].IsSubagent
		}
		return result[i].StepCount > result[j].StepCount
	})

	// Build condensed transcript from matched sessions' events.
	var transcript []TranscriptEventSummary
	for _, e := range events {
		if _, ok := matchedSessions[e.SessionID]; !ok {
			continue
		}
		role := nullStr(e.Role)
		summary := nullStr(e.Summary)
		toolUses := nullStr(e.ToolUses)

		ev := TranscriptEventSummary{Role: role, Summary: summary}

		// Extract tool name and file path from tool_uses JSON.
		if toolUses != "" {
			var newFmt struct {
				Tools []struct {
					Name     string `json:"name"`
					FilePath string `json:"file_path"`
				} `json:"tools"`
			}
			if err := json.Unmarshal([]byte(toolUses), &newFmt); err == nil && len(newFmt.Tools) > 0 {
				ev.ToolName = newFmt.Tools[0].Name
				ev.FilePath = newFmt.Tools[0].FilePath
			} else {
				var legacy []struct {
					Name     string `json:"name"`
					FilePath string `json:"file_path"`
				}
				if err := json.Unmarshal([]byte(toolUses), &legacy); err == nil && len(legacy) > 0 {
					ev.ToolName = legacy[0].Name
					ev.FilePath = legacy[0].FilePath
				}
			}
		}

		transcript = append(transcript, ev)
	}

	return result, transcript, nil
}
