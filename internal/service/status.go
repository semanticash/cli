package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/auth"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/git"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"
)

type StatusService struct{}

func NewStatusService() *StatusService { return &StatusService{} }

type StatusInput struct {
	RepoPath string
}

type StatusResult struct {
	Enabled            bool                `json:"enabled"`
	RepoRoot           string              `json:"repo_root"`
	Connected          bool                `json:"connected"`
	HasRemote          bool                `json:"has_remote"`
	Endpoint           string              `json:"endpoint"`
	RepoProvider       string              `json:"repo_provider"`
	WorkspaceTierTitle string              `json:"workspace_tier_title,omitempty"`
	UpdateAvailable    bool                `json:"update_available,omitempty"`
	LatestVersion      string              `json:"latest_version,omitempty"`
	UpdateDownloadURL  string              `json:"update_download_url,omitempty"`
	AutoPlaybook       bool                `json:"auto_playbook"`
	AutoImplSummary    bool                `json:"auto_implementation_summary"`
	GitTrailers        bool                `json:"git_trailers"`
	LastCheckpoint     *LastCheckpointInfo `json:"last_checkpoint,omitempty"`
	RecentSessions     []RecentSessionInfo `json:"recent_sessions,omitempty"`
	AITrend            []AITrendPoint      `json:"ai_trend,omitempty"`
	PlaybookCount      int64               `json:"playbook_count"`
	Providers          []string            `json:"providers"`
	Broker             *BrokerStatusInfo   `json:"broker,omitempty"`
}

type BrokerStatusInfo struct {
	ActiveRepos   int               `json:"active_repos"`
	InactiveRepos int               `json:"inactive_repos"`
	Repos         []broker.RepoInfo `json:"repos"`
}

type LastCheckpointInfo struct {
	ID        string `json:"id"`
	CreatedAt int64  `json:"created_at"`
	Kind      string `json:"kind"`
	Message   string `json:"message,omitempty"`
	Commit    string `json:"commit,omitempty"`
}

type RecentSessionInfo struct {
	SessionID    string `json:"session_id"`
	Provider     string `json:"provider"`
	StartedAt    int64  `json:"started_at"`
	LastEventAt  int64  `json:"last_event_at"`
	StepCount    int64  `json:"step_count"`
	TokensIn     int64  `json:"tokens_in"`
	TokensOut    int64  `json:"tokens_out"`
	TokensCached int64  `json:"tokens_cached,omitempty"`
}

type AITrendPoint struct {
	CommitHash   string  `json:"commit_hash"`
	AIPercentage float64 `json:"ai_percentage"`
	CreatedAt    int64   `json:"created_at"`
}

func (s *StatusService) Status(ctx context.Context, in StatusInput) (*StatusResult, error) {
	repo, err := git.OpenRepo(in.RepoPath)
	if err != nil {
		return nil, err
	}
	repoRoot := repo.Root()

	semDir := filepath.Join(repoRoot, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")

	// Check if semantica is enabled.
	if _, err := os.Stat(dbPath); err != nil {
		return &StatusResult{Enabled: false, RepoRoot: repoRoot}, nil
	}
	if !util.IsEnabled(semDir) {
		return &StatusResult{Enabled: false, RepoRoot: repoRoot}, nil
	}

	// Detect repo provider and remote presence from git remote URL.
	repoProvider := "unknown"
	hasRemote := false
	if remoteURL, urlErr := repo.RemoteURL(ctx); urlErr == nil && remoteURL != "" {
		hasRemote = true
		repoProvider = git.ProviderFromRemoteURL(remoteURL)
	}

	result := &StatusResult{
		Enabled:         true,
		RepoRoot:        repoRoot,
		Connected:       util.IsConnected(semDir),
		HasRemote:       hasRemote,
		Endpoint:        auth.EffectiveEndpoint(),
		RepoProvider:    repoProvider,
		AutoPlaybook:    util.IsPlaybookEnabled(semDir),
		AutoImplSummary: util.IsImplementationSummaryEnabled(semDir),
		GitTrailers:     util.TrailersEnabled(semDir),
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return nil, err
	}
	defer func() { _ = sqlstore.Close(h) }()

	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, repoRoot)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return result, nil
		}
		return nil, err
	}
	repoID := repoRow.RepositoryID

	// Last checkpoint.
	if cp, err := h.Queries.GetLatestCheckpointForRepo(ctx, repoID); err == nil {
		info := &LastCheckpointInfo{
			ID:        cp.CheckpointID,
			CreatedAt: cp.CreatedAt,
			Kind:      cp.Kind,
		}
		if cp.Message.Valid {
			info.Message = cp.Message.String
		}
		// Try to get commit hash via commit_links.
		if rows, err := h.Queries.ListCheckpointsWithCommit(ctx, sqldb.ListCheckpointsWithCommitParams{
			RepositoryID: repoID,
			Limit:        1,
		}); err == nil && len(rows) > 0 && rows[0].CheckpointID == cp.CheckpointID && rows[0].CommitHash.Valid {
			info.Commit = rows[0].CommitHash.String
		}
		result.LastCheckpoint = info
	}

	// Recent sessions (last 24h).
	sinceTs := time.Now().Add(-24 * time.Hour).UnixMilli()
	if sessions, err := h.Queries.ListRecentSessionsWithStats(ctx, sqldb.ListRecentSessionsWithStatsParams{
		RepositoryID: repoID,
		SinceTs:      sinceTs,
		Limit:        5,
	}); err == nil {
		for _, s := range sessions {
			result.RecentSessions = append(result.RecentSessions, RecentSessionInfo{
				SessionID:    s.SessionID,
				Provider:     s.Provider,
				StartedAt:    s.StartedAt,
				LastEventAt:  s.LastEventTs,
				StepCount:    s.StepCount,
				TokensIn:     s.TokensIn,
				TokensOut:    s.TokensOut,
				TokensCached: s.TokensCached,
			})
		}
	}

	// AI attribution trend (last 5 commits).
	if rows, err := h.Queries.ListRecentAIPercentages(ctx, sqldb.ListRecentAIPercentagesParams{
		RepositoryID: repoID,
		Limit:        5,
	}); err == nil {
		for _, r := range rows {
			result.AITrend = append(result.AITrend, AITrendPoint{
				CommitHash:   r.CommitHash,
				AIPercentage: r.AiPercentage,
				CreatedAt:    r.CreatedAt,
			})
		}
	}

	// Playbook count.
	if count, err := h.Queries.CountCheckpointsWithSummary(ctx, repoID); err == nil {
		result.PlaybookCount = count
	}

	// Providers: merge DB-observed providers with settings (installed hooks).
	// DB gives providers that have produced events; settings gives providers
	// with hooks installed but no events yet. Normalize names (underscores
	// vs hyphens) to avoid duplicates like "claude_code" and "claude-code".
	seen := make(map[string]bool)
	addProvider := func(p string) {
		norm := strings.ReplaceAll(p, "-", "_")
		if !seen[norm] {
			result.Providers = append(result.Providers, p)
			seen[norm] = true
		}
	}
	if providers, err := h.Queries.ListDistinctProviders(ctx, repoID); err == nil {
		for _, p := range providers {
			addProvider(p)
		}
	}
	if settings, err := util.ReadSettings(semDir); err == nil {
		for _, p := range settings.Providers {
			addProvider(p)
		}
	}

	// Broker status (best-effort).
	if bs, err := broker.GetStatus(ctx); err == nil && len(bs.Repos) > 0 {
		result.Broker = &BrokerStatusInfo{
			ActiveRepos:   bs.ActiveCount,
			InactiveRepos: len(bs.Repos) - bs.ActiveCount,
			Repos:         bs.Repos,
		}
	}

	return result, nil
}

// FormatDuration formats a duration in milliseconds between two timestamps
// as a compact human-readable string.
func FormatDuration(startMs, endMs int64) string {
	d := time.Duration(endMs-startMs) * time.Millisecond
	if d < 0 {
		return "0s"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
}
