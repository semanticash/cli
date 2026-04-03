package service

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/git"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"
)

type SessionService struct{}

func NewSessionService() *SessionService { return &SessionService{} }

type SessionListInput struct {
	RepoPath string
	Limit    int64
	All      bool // include sessions with 0 events
}

type SessionInfo struct {
	SessionID         string `json:"session_id"`
	ProviderSessionID string `json:"provider_session_id"`
	Provider          string `json:"provider"`
	ParentSessionID   string `json:"parent_session_id,omitempty"`
	StartedAt         string `json:"started_at"`
	LastEventAt       string `json:"last_event_at"`
	LastEventAtMs     int64  `json:"-"` // unix millis, for relative time formatting
	StepCount         int64  `json:"step_count"`
	ToolCallCount     int64  `json:"tool_call_count"`
	TokensIn          int64  `json:"tokens_in"`
	TokensOut         int64  `json:"tokens_out"`
	TokensCached      int64  `json:"tokens_cached,omitempty"`
	Children          []*SessionInfo `json:"children,omitempty"`
}

// SessionTreeResult holds the tree-structured session list.
type SessionTreeResult struct {
	Roots []*SessionInfo `json:"sessions"`
	Total int            `json:"total"`
}

// sessionStatsRow is the common shape returned by ListSessionsWithStats,
// ListSessionsWithStatsAll, and GetSessionWithStats.
type sessionStatsRow struct {
	SessionID         string
	ProviderSessionID string
	ParentSessionID   sql.NullString
	Provider          string
	StartedAt         int64
	LastEventTs       int64
	StepCount         int64
	ToolCallCount     int64
	TokensIn          int64
	TokensOut         int64
	TokensCached      int64
}

func sessionInfoFromRow(r sessionStatsRow) *SessionInfo {
	si := &SessionInfo{
		SessionID:         r.SessionID,
		ProviderSessionID: r.ProviderSessionID,
		Provider:          r.Provider,
		StartedAt:         time.UnixMilli(r.StartedAt).UTC().Format(time.RFC3339),
		LastEventAt:       time.UnixMilli(r.LastEventTs).UTC().Format(time.RFC3339),
		LastEventAtMs:     r.LastEventTs,
		StepCount:         r.StepCount,
		ToolCallCount:     r.ToolCallCount,
		TokensIn:          r.TokensIn,
		TokensOut:         r.TokensOut,
		TokensCached:      r.TokensCached,
	}
	if r.ParentSessionID.Valid {
		si.ParentSessionID = r.ParentSessionID.String
	}
	return si
}

func (s *SessionService) ListSessions(ctx context.Context, in SessionListInput) (*SessionTreeResult, error) {
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
		return nil, fmt.Errorf("repository not found: %w", err)
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}

	var unified []sessionStatsRow
	if in.All {
		rows, err := h.Queries.ListSessionsWithStatsAll(ctx, sqldb.ListSessionsWithStatsAllParams{
			RepositoryID: repoRow.RepositoryID,
			Limit:        limit,
		})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			unified = append(unified, sessionStatsRow{
				SessionID: r.SessionID, ProviderSessionID: r.ProviderSessionID,
				ParentSessionID: r.ParentSessionID, Provider: r.Provider,
				StartedAt: r.StartedAt, LastEventTs: r.LastEventTs,
				StepCount: r.StepCount, ToolCallCount: r.ToolCallCount,
				TokensIn: r.TokensIn, TokensOut: r.TokensOut, TokensCached: r.TokensCached,
			})
		}
	} else {
		rows, err := h.Queries.ListSessionsWithStats(ctx, sqldb.ListSessionsWithStatsParams{
			RepositoryID: repoRow.RepositoryID,
			Limit:        limit,
		})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			unified = append(unified, sessionStatsRow{
				SessionID: r.SessionID, ProviderSessionID: r.ProviderSessionID,
				ParentSessionID: r.ParentSessionID, Provider: r.Provider,
				StartedAt: r.StartedAt, LastEventTs: r.LastEventTs,
				StepCount: r.StepCount, ToolCallCount: r.ToolCallCount,
				TokensIn: r.TokensIn, TokensOut: r.TokensOut, TokensCached: r.TokensCached,
			})
		}
	}

	byID := make(map[string]*SessionInfo, len(unified))
	allIDs := make(map[string]bool, len(unified))
	var all []*SessionInfo

	for _, r := range unified {
		si := sessionInfoFromRow(r)
		byID[si.SessionID] = si
		allIDs[si.SessionID] = true
		all = append(all, si)
	}

	// Hydrate missing ancestors so limited queries still produce a valid tree.
	for {
		var missing []string
		for _, si := range byID {
			if si.ParentSessionID != "" {
				if _, ok := byID[si.ParentSessionID]; !ok {
					missing = append(missing, si.ParentSessionID)
				}
			}
		}
		if len(missing) == 0 {
			break
		}
		for _, pid := range missing {
			if _, ok := byID[pid]; ok {
				continue
			}
			parentRow, err := h.Queries.GetSessionWithStats(ctx, pid)
			if err != nil {
				// Insert a sentinel so inaccessible parents do not keep reappearing.
				byID[pid] = &SessionInfo{SessionID: pid}
				continue
			}
			byID[parentRow.SessionID] = sessionInfoFromRow(sessionStatsRow{
				SessionID: parentRow.SessionID, ProviderSessionID: parentRow.ProviderSessionID,
				ParentSessionID: parentRow.ParentSessionID, Provider: parentRow.Provider,
				StartedAt: parentRow.StartedAt, LastEventTs: parentRow.LastEventTs,
				StepCount: parentRow.StepCount, ToolCallCount: parentRow.ToolCallCount,
				TokensIn: parentRow.TokensIn, TokensOut: parentRow.TokensOut, TokensCached: parentRow.TokensCached,
			})
		}
	}

	// Attach queried sessions first, then wire in any hydrated ancestors.
	var roots []*SessionInfo
	for _, si := range all {
		if si.ParentSessionID != "" {
			if parent, ok := byID[si.ParentSessionID]; ok {
				parent.Children = append(parent.Children, si)
				continue
			}
		}
		roots = append(roots, si)
	}
	// Wire fetched ancestors into the tree.
	for _, si := range byID {
		if allIDs[si.SessionID] {
			continue // already processed above
		}
		if si.ParentSessionID == "" {
			roots = append(roots, si)
		} else if parent, ok := byID[si.ParentSessionID]; ok {
			parent.Children = append(parent.Children, si)
		} else {
			roots = append(roots, si)
		}
	}

	// Filter empty sessions unless --all is set.
	if !in.All {
		roots = pruneEmpty(roots)
	}

	// Count total (including children) after filtering.
	total := countNodes(roots)

	return &SessionTreeResult{
		Roots: roots,
		Total: total,
	}, nil
}

// SessionDetailInput holds parameters for a single-session lookup.
type SessionDetailInput struct {
	RepoPath  string
	SessionID string // full UUID or prefix
}

// GetSession resolves a session ID (prefix or full) and returns its stats.
func (s *SessionService) GetSession(ctx context.Context, in SessionDetailInput) (*SessionInfo, error) {
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
		return nil, fmt.Errorf("repository not found: %w", err)
	}

	// Accept full session IDs and unambiguous prefixes.
	resolvedID, err := sqlstore.ResolveSessionID(ctx, h.Queries, repoRow.RepositoryID, in.SessionID)
	if err != nil {
		return nil, err
	}

	r, err := h.Queries.GetSessionWithStats(ctx, resolvedID)
	if err != nil {
		return nil, fmt.Errorf("session not found: %w", err)
	}

	return sessionInfoFromRow(sessionStatsRow{
		SessionID: r.SessionID, ProviderSessionID: r.ProviderSessionID,
		ParentSessionID: r.ParentSessionID, Provider: r.Provider,
		StartedAt: r.StartedAt, LastEventTs: r.LastEventTs,
		StepCount: r.StepCount, ToolCallCount: r.ToolCallCount,
		TokensIn: r.TokensIn, TokensOut: r.TokensOut, TokensCached: r.TokensCached,
	}), nil
}

// pruneEmpty removes tree nodes that have 0 steps and no non-empty children.
func pruneEmpty(nodes []*SessionInfo) []*SessionInfo {
	var out []*SessionInfo
	for _, n := range nodes {
		n.Children = pruneEmpty(n.Children)
		if n.StepCount > 0 || len(n.Children) > 0 {
			out = append(out, n)
		}
	}
	return out
}

func countNodes(nodes []*SessionInfo) int {
	n := len(nodes)
	for _, s := range nodes {
		n += countNodes(s.Children)
	}
	return n
}

// RelativeTime formats a unix-milli timestamp as a human-friendly relative
// duration like "2m", "1h", "3d".
func RelativeTime(ms int64) string {
	d := time.Since(time.UnixMilli(ms))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// CompactTokens formats a token count as a compact string like "1.5k", "12k", "4.4M".
func CompactTokens(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	k := float64(n) / 1000
	if k < 1000 {
		if k < 10 {
			return fmt.Sprintf("%.1fk", k)
		}
		return fmt.Sprintf("%.0fk", k)
	}
	m := k / 1000
	if m < 10 {
		return fmt.Sprintf("%.1fM", m)
	}
	return fmt.Sprintf("%.0fM", m)
}
