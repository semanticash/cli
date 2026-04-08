package implementations

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/semanticash/cli/internal/store/impldb"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// ImplementationDetail is the full view of an implementation with timeline.
type ImplementationDetail struct {
	ImplementationID string          `json:"implementation_id"`
	Title            string          `json:"title,omitempty"`
	State            string          `json:"state"`
	CreatedAt        int64           `json:"created_at"`
	LastActivityAt   int64           `json:"last_activity_at"`
	Repos            []RepoDetail    `json:"repos"`
	Sessions         []SessionDetail `json:"sessions"`
	Commits          []CommitDetail  `json:"commits"`
	Timeline         []TimelineEntry `json:"timeline"`
	TotalTokensIn    int64           `json:"total_tokens_in"`
	TotalTokensOut   int64           `json:"total_tokens_out"`
}

// RepoDetail extends RepoSummary with more info for the detail view.
type RepoDetail struct {
	CanonicalPath string `json:"canonical_path"`
	DisplayName   string `json:"display_name"`
	Role          string `json:"role"`
	FirstSeenAt   int64  `json:"first_seen_at"`
	SessionCount  int    `json:"session_count"`
}

// SessionDetail is a session reference in the detail view.
type SessionDetail struct {
	Provider          string `json:"provider"`
	ProviderSessionID string `json:"provider_session_id"`
	AttachRule        string `json:"attach_rule"`
}

// CommitDetail is a commit reference in the detail view.
type CommitDetail struct {
	CanonicalPath string `json:"canonical_path"`
	DisplayName   string `json:"display_name"`
	CommitHash    string `json:"commit_hash"`
	AttachRule    string `json:"attach_rule"`
}

// TimelineEntry is one event in the cross-repo timeline.
type TimelineEntry struct {
	Timestamp int64  `json:"timestamp"`
	RepoName  string `json:"repo_name"`
	Kind      string `json:"kind"`    // "session_start", "edit", "tool", "commit", "event"
	Summary   string `json:"summary"`
	CrossRepo bool   `json:"cross_repo"` // true when repo changed from previous entry
}

// GetDetail loads the full implementation detail with cross-repo timeline.
func GetDetail(ctx context.Context, implID string) (*ImplementationDetail, error) {
	h, err := openGlobalDB(ctx)
	if err != nil {
		return nil, fmt.Errorf("open implementations db: %w", err)
	}
	defer func() { _ = impldb.Close(h) }()

	fullID, err := resolveImplID(ctx, h, implID)
	if err != nil {
		return nil, err
	}

	impl, err := h.Queries.GetImplementation(ctx, fullID)
	if err != nil {
		return nil, fmt.Errorf("implementation %s not found", implID)
	}

	title := ""
	if impl.Title.Valid {
		title = impl.Title.String
	}

	// Load repos and build canonical_path -> display_name map.
	repoRows, _ := h.Queries.ListImplementationRepos(ctx, fullID)
	repoSessionRows, _ := h.Queries.ListRepoSessionsForImplementation(ctx, fullID)

	displayNameByPath := make(map[string]string)
	sessPerRepo := make(map[string]int)
	for _, rr := range repoRows {
		displayNameByPath[rr.CanonicalPath] = rr.DisplayName
	}
	for _, rs := range repoSessionRows {
		sessPerRepo[rs.CanonicalPath]++
	}

	repos := make([]RepoDetail, 0, len(repoRows))
	for _, rr := range repoRows {
		repos = append(repos, RepoDetail{
			CanonicalPath: rr.CanonicalPath,
			DisplayName:   rr.DisplayName,
			Role:          rr.RepoRole,
			FirstSeenAt:   rr.FirstSeenAt,
			SessionCount:  sessPerRepo[rr.CanonicalPath],
		})
	}
	sort.Slice(repos, func(i, j int) bool {
		if repos[i].Role == "origin" && repos[j].Role != "origin" {
			return true
		}
		if repos[i].Role != "origin" && repos[j].Role == "origin" {
			return false
		}
		return repos[i].FirstSeenAt < repos[j].FirstSeenAt
	})

	// Load provider sessions.
	provSessRows, _ := h.Queries.ListProviderSessionsForImplementation(ctx, fullID)
	sessions := make([]SessionDetail, 0, len(provSessRows))
	for _, ps := range provSessRows {
		sessions = append(sessions, SessionDetail{
			Provider:          ps.Provider,
			ProviderSessionID: ps.ProviderSessionID,
			AttachRule:        ps.AttachRule,
		})
	}

	// Load commits using the repo display name from implementation_repos.
	commitRows, _ := h.Queries.ListImplementationCommits(ctx, fullID)
	commits := make([]CommitDetail, 0, len(commitRows))
	for _, c := range commitRows {
		dn := displayNameByPath[c.CanonicalPath]
		if dn == "" {
			dn = filepath.Base(c.CanonicalPath) // fallback
		}
		commits = append(commits, CommitDetail{
			CanonicalPath: c.CanonicalPath,
			DisplayName:   dn,
			CommitHash:    c.CommitHash,
			AttachRule:     c.AttachRule,
		})
	}

	// Build timeline and compute tokens via deduplicated session stats.
	var timeline []TimelineEntry
	var totalIn, totalOut int64

	for _, repo := range repos {
		entries := loadRepoTimeline(ctx, h, fullID, repo.CanonicalPath, repo.DisplayName)
		timeline = append(timeline, entries...)

		// Token totals: use GetSessionWithStats (deduplicated) per session.
		tIn, tOut := loadRepoTokens(ctx, fullID, h, repo.CanonicalPath)
		totalIn += tIn
		totalOut += tOut
	}

	// Add commit entries using the consistent display name.
	for _, c := range commits {
		ts := lookupCommitTimestamp(ctx, c.CanonicalPath, c.CommitHash)
		timeline = append(timeline, TimelineEntry{
			Timestamp: ts,
			RepoName:  c.DisplayName,
			Kind:      "commit",
			Summary:   fmt.Sprintf("commit %s", c.CommitHash[:minLen(len(c.CommitHash), 7)]),
		})
	}

	// Stable sort: timestamp primary, repo name secondary for deterministic
	// ordering of same-millisecond events across repos.
	sort.SliceStable(timeline, func(i, j int) bool {
		if timeline[i].Timestamp != timeline[j].Timestamp {
			return timeline[i].Timestamp < timeline[j].Timestamp
		}
		return timeline[i].RepoName < timeline[j].RepoName
	})

	// Mark cross-repo transitions.
	prevRepo := ""
	for i := range timeline {
		if prevRepo != "" && timeline[i].RepoName != prevRepo {
			timeline[i].CrossRepo = true
		}
		prevRepo = timeline[i].RepoName
	}

	return &ImplementationDetail{
		ImplementationID: fullID,
		Title:            title,
		State:            impl.State,
		CreatedAt:        impl.CreatedAt,
		LastActivityAt:   impl.LastActivityAt,
		Repos:            repos,
		Sessions:         sessions,
		Commits:          commits,
		Timeline:         timeline,
		TotalTokensIn:    totalIn,
		TotalTokensOut:   totalOut,
	}, nil
}

const timelinePageSize int64 = 500

// loadRepoTimeline opens a repo's lineage.db and fetches all events for
// sessions linked to this implementation via keyset pagination.
func loadRepoTimeline(
	ctx context.Context,
	h *impldb.Handle,
	implID, canonicalPath, displayName string,
) []TimelineEntry {
	allRepoSessions, _ := h.Queries.ListRepoSessionsForImplementation(ctx, implID)

	var sessionIDs []string
	for _, rs := range allRepoSessions {
		if rs.CanonicalPath == canonicalPath {
			sessionIDs = append(sessionIDs, rs.SessionID)
		}
	}
	if len(sessionIDs) == 0 {
		return nil
	}

	dbPath := filepath.Join(canonicalPath, ".semantica", "lineage.db")
	repoH, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return nil
	}
	defer func() { _ = sqlstore.Close(repoH) }()

	var entries []TimelineEntry
	for _, sessID := range sessionIDs {
		var afterTs int64
		var afterEventID string
		for {
			page, err := repoH.Queries.ListAgentEventsBySessionPaged(ctx, sqldb.ListAgentEventsBySessionPagedParams{
				SessionID:    sessID,
				AfterTs:      afterTs,
				AfterEventID: afterEventID,
				PageLimit:    timelinePageSize,
			})
			if err != nil || len(page) == 0 {
				break
			}

			for _, ev := range page {
				summary := ""
				if ev.Summary.Valid {
					summary = ev.Summary.String
				}
				kind := "event"
				if ev.ToolName.Valid && ev.ToolName.String != "" {
					kind = "tool"
					summary = ev.ToolName.String
					if ev.Summary.Valid && ev.Summary.String != "" {
						summary = ev.ToolName.String + " " + ev.Summary.String
					}
				}

				entries = append(entries, TimelineEntry{
					Timestamp: ev.Ts,
					RepoName:  displayName,
					Kind:      kind,
					Summary:   summary,
				})
			}

			last := page[len(page)-1]
			afterTs = last.Ts
			afterEventID = last.EventID

			if int64(len(page)) < timelinePageSize {
				break // last page
			}
		}
	}

	return entries
}

// loadRepoTokens uses GetSessionWithStats (deduplicated token logic) for
// each session linked to this implementation in the given repo.
func loadRepoTokens(
	ctx context.Context,
	implID string,
	h *impldb.Handle,
	canonicalPath string,
) (int64, int64) {
	allRepoSessions, _ := h.Queries.ListRepoSessionsForImplementation(ctx, implID)

	var sessionIDs []string
	for _, rs := range allRepoSessions {
		if rs.CanonicalPath == canonicalPath {
			sessionIDs = append(sessionIDs, rs.SessionID)
		}
	}
	if len(sessionIDs) == 0 {
		return 0, 0
	}

	dbPath := filepath.Join(canonicalPath, ".semantica", "lineage.db")
	repoH, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return 0, 0
	}
	defer func() { _ = sqlstore.Close(repoH) }()

	var totalIn, totalOut int64
	for _, sessID := range sessionIDs {
		stats, err := repoH.Queries.GetSessionWithStats(ctx, sessID)
		if err != nil {
			continue
		}
		totalIn += stats.TokensIn
		totalOut += stats.TokensOut
	}
	return totalIn, totalOut
}

// lookupCommitTimestamp finds the linked_at timestamp for a commit.
func lookupCommitTimestamp(ctx context.Context, canonicalPath, commitHash string) int64 {
	dbPath := filepath.Join(canonicalPath, ".semantica", "lineage.db")
	repoH, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return 0
	}
	defer func() { _ = sqlstore.Close(repoH) }()

	link, err := repoH.Queries.GetCommitLinkByCommitHash(ctx, commitHash)
	if err != nil {
		return 0
	}
	return link.LinkedAt
}

// resolveImplID resolves a short ID prefix to a full implementation ID.
// Uses a SQL LIKE query with no cap on implementation count.
func resolveImplID(ctx context.Context, h *impldb.Handle, id string) (string, error) {
	// Try exact match first.
	_, err := h.Queries.GetImplementation(ctx, id)
	if err == nil {
		return id, nil
	}

	// Prefix match via SQL LIKE (no row limit issue).
	matches, err := h.Queries.ResolveImplementationByPrefix(ctx, impldb.NullStr(id))
	if err != nil {
		return "", fmt.Errorf("resolve implementation ID: %w", err)
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no implementation matching %q", id)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("ambiguous implementation ID %q matches %d implementations", id, len(matches))
	}
}

func minLen(a, b int) int {
	if a < b {
		return a
	}
	return b
}
