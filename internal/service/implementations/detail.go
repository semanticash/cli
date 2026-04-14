package implementations

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/platform"
	"github.com/semanticash/cli/internal/store/impldb"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// ImplementationDetail is the full view of an implementation with timeline.
type ImplementationDetail struct {
	ImplementationID  string            `json:"implementation_id"`
	Title             string            `json:"title,omitempty"`
	Summary           string            `json:"summary,omitempty"`
	State             string            `json:"state"`
	CreatedAt         int64             `json:"created_at"`
	LastActivityAt    int64             `json:"last_activity_at"`
	Repos             []RepoDetail      `json:"repos"`
	RepoAttribution   []RepoAttribution `json:"repo_attribution,omitempty"`
	Sessions          []SessionDetail   `json:"sessions"`
	Commits           []CommitDetail    `json:"commits"`
	Timeline          []TimelineEntry   `json:"timeline"`
	TotalTokensIn     int64             `json:"total_tokens_in"`
	TotalTokensOut    int64             `json:"total_tokens_out"`
	TotalTokensCached int64             `json:"total_tokens_cached"`
}

// RepoDetail extends RepoSummary with more info for the detail view.
type RepoDetail struct {
	CanonicalPath string `json:"canonical_path"`
	DisplayName   string `json:"display_name"`
	Role          string `json:"role"`
	FirstSeenAt   int64  `json:"first_seen_at"`
	SessionCount  int    `json:"session_count"`
}

// RepoAttribution is the averaged cached AI attribution for a repo's commits
// within one implementation.
type RepoAttribution struct {
	CanonicalPath string  `json:"canonical_path"`
	DisplayName   string  `json:"display_name"`
	AIPercentage  float64 `json:"ai_percentage"`
	CommitCount   int     `json:"commit_count"`
}

// SessionDetail is a session reference in the detail view.
type SessionDetail struct {
	Provider          string `json:"provider"`
	ProviderSessionID string `json:"provider_session_id"`
	SourceProjectPath string `json:"source_project_path,omitempty"`
	AttachRule        string `json:"attach_rule"`
	AttachedAt        int64  `json:"attached_at"`
}

// CommitDetail is a commit reference in the detail view.
type CommitDetail struct {
	CanonicalPath string `json:"canonical_path"`
	DisplayName   string `json:"display_name"`
	CommitHash    string `json:"commit_hash"`
	Subject       string `json:"subject,omitempty"`
	AttachedAt    int64  `json:"attached_at"`
	AttachRule    string `json:"attach_rule"`
}

// TimelineEntry is one event in the cross-repo timeline.
type TimelineEntry struct {
	Timestamp int64  `json:"timestamp"`
	RepoName  string `json:"repo_name"`
	Kind      string `json:"kind"` // "session_start", "edit", "tool", "commit", "event"
	Summary   string `json:"summary"`
	FilePath  string `json:"file_path,omitempty"`
	FileOp    string `json:"file_op,omitempty"`
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
	summary := implementationSummaryFromMetadata(impl.MetadataJson)

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

	// Load provider sessions.
	provSessRows, _ := h.Queries.ListProviderSessionsForImplementation(ctx, fullID)
	sessions := make([]SessionDetail, 0, len(provSessRows))
	for _, ps := range provSessRows {
		sourceProjectPath := ""
		if ps.SourceProjectPath.Valid {
			sourceProjectPath = ps.SourceProjectPath.String
		}
		sessions = append(sessions, SessionDetail{
			Provider:          ps.Provider,
			ProviderSessionID: ps.ProviderSessionID,
			SourceProjectPath: sourceProjectPath,
			AttachRule:        ps.AttachRule,
			AttachedAt:        ps.AttachedAt,
		})
	}
	ensureOriginRepo(repos, sessions)
	sort.Slice(repos, func(i, j int) bool {
		if repos[i].Role == "origin" && repos[j].Role != "origin" {
			return true
		}
		if repos[i].Role != "origin" && repos[j].Role == "origin" {
			return false
		}
		return repos[i].FirstSeenAt < repos[j].FirstSeenAt
	})

	// Load commits using the repo display name from implementation_repos.
	commitRows, _ := h.Queries.ListImplementationCommits(ctx, fullID)
	commits := make([]CommitDetail, 0, len(commitRows))
	for _, c := range commitRows {
		dn := displayNameByPath[c.CanonicalPath]
		if dn == "" {
			dn = filepath.Base(c.CanonicalPath) // fallback
		}
		subject := lookupCommitSubject(ctx, c.CanonicalPath, c.CommitHash)
		commits = append(commits, CommitDetail{
			CanonicalPath: c.CanonicalPath,
			DisplayName:   dn,
			CommitHash:    c.CommitHash,
			Subject:       subject,
			AttachedAt:    c.AttachedAt,
			AttachRule:    c.AttachRule,
		})
	}

	// Build timeline and compute tokens via deduplicated session stats.
	var timeline []TimelineEntry
	var totalIn, totalOut, totalCached int64

	for _, repo := range repos {
		entries := loadRepoTimeline(ctx, h, fullID, repo.CanonicalPath, repo.DisplayName)
		timeline = append(timeline, entries...)

		// Token totals: use GetSessionWithStats (deduplicated) per session.
		tIn, tOut, tCached := loadRepoTokens(ctx, fullID, h, repo.CanonicalPath)
		totalIn += tIn
		totalOut += tOut
		totalCached += tCached
	}

	repoAttribution := loadRepoAttribution(ctx, commits)

	// Add commit entries using the consistent display name.
	for _, c := range commits {
		timeline = append(timeline, TimelineEntry{
			Timestamp: c.AttachedAt,
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
		ImplementationID:  fullID,
		Title:             title,
		Summary:           summary,
		State:             impl.State,
		CreatedAt:         impl.CreatedAt,
		LastActivityAt:    impl.LastActivityAt,
		Repos:             repos,
		RepoAttribution:   repoAttribution,
		Sessions:          sessions,
		Commits:           commits,
		Timeline:          timeline,
		TotalTokensIn:     totalIn,
		TotalTokensOut:    totalOut,
		TotalTokensCached: totalCached,
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
				filePath, fileOp := extractTimelineToolInfo(ev.ToolUses, canonicalPath)
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
					FilePath:  filePath,
					FileOp:    fileOp,
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
) (int64, int64, int64) {
	allRepoSessions, _ := h.Queries.ListRepoSessionsForImplementation(ctx, implID)

	var sessionIDs []string
	for _, rs := range allRepoSessions {
		if rs.CanonicalPath == canonicalPath {
			sessionIDs = append(sessionIDs, rs.SessionID)
		}
	}
	if len(sessionIDs) == 0 {
		return 0, 0, 0
	}

	dbPath := filepath.Join(canonicalPath, ".semantica", "lineage.db")
	repoH, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return 0, 0, 0
	}
	defer func() { _ = sqlstore.Close(repoH) }()

	var totalIn, totalOut, totalCached int64
	for _, sessID := range sessionIDs {
		stats, err := repoH.Queries.GetSessionWithStats(ctx, sessID)
		if err != nil {
			continue
		}
		totalIn += stats.TokensIn
		totalOut += stats.TokensOut
		totalCached += stats.TokensCached
	}
	return totalIn, totalOut, totalCached
}

func loadRepoAttribution(ctx context.Context, commits []CommitDetail) []RepoAttribution {
	type acc struct {
		sum   float64
		count int
	}

	byRepo := make(map[string][]CommitDetail)
	for _, commit := range commits {
		byRepo[commit.CanonicalPath] = append(byRepo[commit.CanonicalPath], commit)
	}

	results := make([]RepoAttribution, 0, len(byRepo))
	for canonicalPath, repoCommits := range byRepo {
		dbPath := filepath.Join(canonicalPath, ".semantica", "lineage.db")
		repoH, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
		if err != nil {
			continue
		}

		var stats acc
		for _, commit := range repoCommits {
			link, err := repoH.Queries.GetCommitLinkByCommitHash(ctx, commit.CommitHash)
			if err != nil {
				continue
			}
			cpStats, err := repoH.Queries.GetCheckpointStats(ctx, link.CheckpointID)
			if err != nil || cpStats.AiPercentage < 0 {
				continue
			}
			stats.sum += cpStats.AiPercentage
			stats.count++
		}
		_ = sqlstore.Close(repoH)

		if stats.count == 0 {
			continue
		}

		results = append(results, RepoAttribution{
			CanonicalPath: canonicalPath,
			DisplayName:   repoCommits[0].DisplayName,
			AIPercentage:  stats.sum / float64(stats.count),
			CommitCount:   stats.count,
		})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].DisplayName < results[j].DisplayName
	})

	return results
}

func lookupCommitSubject(ctx context.Context, canonicalPath, commitHash string) string {
	repo, err := git.OpenRepo(canonicalPath)
	if err != nil {
		return ""
	}
	subject, err := repo.CommitSubject(ctx, commitHash)
	if err != nil {
		return ""
	}
	return subject
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

func ensureOriginRepo(repos []RepoDetail, sessions []SessionDetail) {
	for _, repo := range repos {
		if repo.Role == "origin" {
			return
		}
	}

	originPath := deriveOriginCanonicalPath(repos, sessions)
	if originPath == "" {
		return
	}
	for i := range repos {
		if repos[i].CanonicalPath == originPath {
			repos[i].Role = "origin"
			return
		}
	}
}

func deriveOriginCanonicalPath(repos []RepoDetail, sessions []SessionDetail) string {
	for _, sess := range sessions {
		if sess.SourceProjectPath == "" {
			continue
		}
		if canonicalPath := matchRepoForSourceProjectPath(sess.SourceProjectPath, repos); canonicalPath != "" {
			return canonicalPath
		}
	}
	return ""
}

func matchRepoForSourceProjectPath(sourceProjectPath string, repos []RepoDetail) string {
	for _, repo := range repos {
		if broker.PathBelongsToRepo(sourceProjectPath, repo.CanonicalPath) {
			return repo.CanonicalPath
		}
	}

	base := sourceProjectBaseName(sourceProjectPath)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return ""
	}

	var suffixMatch string
	for _, repo := range repos {
		if repo.DisplayName == base {
			return repo.CanonicalPath
		}
		if strings.HasSuffix(repo.DisplayName, "-"+base) {
			if suffixMatch != "" {
				return ""
			}
			suffixMatch = repo.CanonicalPath
		}
	}
	return suffixMatch
}

func sourceProjectBaseName(sourceProjectPath string) string {
	return filepath.Base(filepath.Clean(strings.TrimSpace(sourceProjectPath)))
}

func extractTimelineToolInfo(toolUsesNS sql.NullString, repoRoot string) (string, string) {
	if !toolUsesNS.Valid || toolUsesNS.String == "" {
		return "", ""
	}

	type toolUse struct {
		Name     string `json:"name"`
		FilePath string `json:"file_path"`
		FileOp   string `json:"file_op"`
	}

	var newFmt struct {
		Tools []toolUse `json:"tools"`
	}
	if err := json.Unmarshal([]byte(toolUsesNS.String), &newFmt); err == nil && len(newFmt.Tools) > 0 {
		return normalizeTimelineToolPath(newFmt.Tools[0].FilePath, repoRoot), normalizeTimelineFileOp(newFmt.Tools[0])
	}

	var legacy []toolUse
	if err := json.Unmarshal([]byte(toolUsesNS.String), &legacy); err == nil && len(legacy) > 0 {
		return normalizeTimelineToolPath(legacy[0].FilePath, repoRoot), normalizeTimelineFileOp(legacy[0])
	}

	return "", ""
}

func normalizeTimelineFileOp(tool struct {
	Name     string `json:"name"`
	FilePath string `json:"file_path"`
	FileOp   string `json:"file_op"`
}) string {
	op := strings.ToLower(strings.TrimSpace(tool.FileOp))
	switch op {
	case "create", "new":
		return "new"
	case "delete", "remove", "rm":
		return "deleted"
	case "edit", "write", "replace", "update", "save":
		return "edited"
	}

	name := strings.ToLower(strings.TrimSpace(tool.Name))
	switch name {
	case "write", "createfile", "create_file", "write_file":
		return "new"
	case "delete", "deletefile", "remove":
		return "deleted"
	case "edit", "replace", "editfile", "edit_file", "save_file", "kiro_file_edit":
		return "edited"
	default:
		return ""
	}
}

func normalizeTimelineToolPath(fp, repoRoot string) string {
	fp = strings.TrimSpace(fp)
	if fp == "" {
		return ""
	}

	if strings.HasPrefix(fp, "file:") {
		if u, err := url.Parse(fp); err == nil {
			p := u.Path
			if runtime.GOOS == "windows" && len(p) >= 3 && p[0] == '/' && p[2] == ':' {
				p = p[1:]
			}
			if p != "" {
				fp = p
			}
		} else {
			fp = strings.TrimPrefix(fp, "file://")
		}
	}

	if platform.LooksAbsolutePath(fp) {
		rel, err := filepath.Rel(repoRoot, fp)
		if err != nil {
			return ""
		}
		relSlash := filepath.ToSlash(rel)
		if relSlash == ".." || strings.HasPrefix(relSlash, "../") {
			return ""
		}
		fp = rel
	}

	fp = filepath.ToSlash(fp)
	return strings.TrimPrefix(fp, "./")
}
