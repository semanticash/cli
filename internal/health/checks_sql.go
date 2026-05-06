package health

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/hooks"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// recentEventWindow is the lookback for the per-provider event check.
const recentEventWindow = 24 * time.Hour

// manifestWindow is the lookback for failed-manifest grouping.
const manifestWindow = 7 * 24 * time.Hour

// failedManifestReasonsToShow is how many top reason groups doctor
// surfaces in text/JSON.
const failedManifestReasonsToShow = 3

// checkRecentEvents reports per-provider event activity in the last
// 24 hours. It also flags a "hooks installed but no events landed"
// regression - the silent capture-drop signal - when capture state
// shows the provider was active in the window.
func checkRecentEvents(ctx context.Context, opts Options) []Check {
	if opts.RepoPath == "" {
		return []Check{{
			Category: "events",
			ID:       "scope",
			Status:   StatusOK,
			Message:  "no repo path supplied; recent-events check skipped",
		}}
	}

	semDir := filepath.Join(opts.RepoPath, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return []Check{{
			Category: "events",
			ID:       "lineage_db",
			Status:   StatusOK,
			Message:  "lineage.db not present (Semantica not enabled in this repo)",
		}}
	}

	h, err := openLineage(ctx, dbPath)
	if err != nil {
		return []Check{{
			Category:    "events",
			ID:          "lineage_db",
			Status:      StatusWarn,
			Message:     "could not open lineage.db: " + err.Error(),
			Remediation: "check filesystem permissions on " + dbPath,
		}}
	}
	defer func() { _ = sqlstore.Close(h) }()

	repo, err := h.Queries.GetRepositoryByRootPath(ctx, opts.RepoPath)
	if err != nil {
		return []Check{{
			Category: "events",
			ID:       "repository",
			Status:   StatusOK,
			Message:  "repo not registered in lineage.db (no captures yet)",
		}}
	}

	since := time.Now().Add(-recentEventWindow).UnixMilli()
	rows, err := h.Queries.ListEventsByProviderInWindow(ctx, sqldb.ListEventsByProviderInWindowParams{
		RepositoryID: repo.RepositoryID,
		Ts:           since,
	})
	if err != nil {
		return []Check{{
			Category:    "events",
			ID:          "query",
			Status:      StatusWarn,
			Message:     "events-by-provider query failed: " + err.Error(),
			Remediation: "may indicate lineage.db schema drift; try `semantica enable`",
		}}
	}

	eventsByProvider := map[string]int64{}
	mostRecentByProvider := map[string]int64{}
	for _, r := range rows {
		eventsByProvider[r.Provider] = r.EventCount
		if ts, ok := r.MostRecentTs.(int64); ok {
			mostRecentByProvider[r.Provider] = ts
		}
	}

	stateActiveByProvider := activeProvidersForRepo(opts.RepoPath, since)

	var checks []Check
	checks = append(checks, summariseEvents(eventsByProvider, mostRecentByProvider))
	checks = append(checks, silentDropChecks(opts, eventsByProvider, stateActiveByProvider)...)

	return checks
}

// activeProvidersForRepo loads the global capture-state directory and
// returns the set of providers with an active session pinned to the
// given repo and updated within the window. Filtering on both axes is
// what keeps the silent-drop check from firing on stale state files
// or on sessions that belong to a different repo.
func activeProvidersForRepo(repoPath string, sinceUnixMilli int64) map[string]bool {
	states, err := hooks.LoadActiveCaptureStates()
	if err != nil || len(states) == 0 {
		return nil
	}
	canonicalRepo, err := filepath.EvalSymlinks(repoPath)
	if err != nil {
		canonicalRepo = filepath.Clean(repoPath)
	}

	result := map[string]bool{}
	for _, s := range states {
			// State files without a reliable in-window timestamp are
			// ignored so stale capture state does not look active.
		if s.Timestamp <= 0 || s.Timestamp < sinceUnixMilli {
			continue
		}
		if s.CWD == "" {
			// Older state writers may have missed CWD. Skip rather
			// than misattribute it to this repo.
			continue
		}
		stateCanonical, err := filepath.EvalSymlinks(s.CWD)
		if err != nil {
			stateCanonical = filepath.Clean(s.CWD)
		}
		if !sameRepo(canonicalRepo, stateCanonical) {
			continue
		}
		result[s.Provider] = true
	}
	return result
}

// sameRepo reports whether candidate is the repo root or a path below
// it. Subdirectory captures are part of the same repo.
func sameRepo(repoRoot, candidate string) bool {
	if repoRoot == candidate {
		return true
	}
	rel, err := filepath.Rel(repoRoot, candidate)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..") && rel != ""
}

func summariseEvents(eventsByProvider, mostRecentByProvider map[string]int64) Check {
	if len(eventsByProvider) == 0 {
		return Check{
			Category: "events",
			ID:       "summary",
			Status:   StatusOK,
			Message:  "no events in the last 24h (idle is normal)",
		}
	}

	providers := make([]string, 0, len(eventsByProvider))
	for p := range eventsByProvider {
		providers = append(providers, p)
	}
	sort.Strings(providers)

	var parts []string
	now := time.Now()
	for _, p := range providers {
		recent := ""
		if ts, ok := mostRecentByProvider[p]; ok && ts > 0 {
			recent = ", most recent " + relativeAge(now, time.UnixMilli(ts)) + " ago"
		}
		parts = append(parts, fmt.Sprintf("%s: %d%s", p, eventsByProvider[p], recent))
	}
	return Check{
		Category: "events",
		ID:       "summary",
		Status:   StatusOK,
		Message:  "last 24h - " + strings.Join(parts, "; "),
	}
}

// silentDropChecks returns a warning for each provider that has hooks
// installed and shows capture-state activity in the window but
// produced zero events. That combination is the regression signal.
func silentDropChecks(opts Options, eventsByProvider map[string]int64, stateActiveByProvider map[string]bool) []Check {
	if opts.RepoPath == "" {
		return nil
	}

	var checks []Check
	for _, p := range hooks.ListProviders() {
		name := p.Name()
		if !p.AreHooksInstalled(nil, opts.RepoPath) {
			continue
		}
		if !stateActiveByProvider[name] {
			// Provider hooks installed but no capture activity in
			// the window. Could be idle; not a fault.
			continue
		}
		if hasEventsForHookProvider(name, eventsByProvider) {
			continue
		}
		checks = append(checks, Check{
			Category:    "events",
			ID:          "silent_drop:" + name,
			Status:      StatusWarn,
			Message:     p.DisplayName() + ": capture state active but 0 events in the last 24h",
			Remediation: "check the worker log (`semantica launcher status`) for hook or capture errors",
		})
	}
	return checks
}

// hasEventsForHookProvider checks the events map under each storage
// name a hook provider could be persisted as. Hook registry names
// use dashes ("claude-code", "kiro-ide") while
// `agent_sessions.provider` historically uses underscores or `_cli`
// suffixes ("claude_code", "gemini_cli"), so the health check accepts
// both forms.
func hasEventsForHookProvider(hookName string, eventsByProvider map[string]int64) bool {
	for _, key := range storageProviderCandidates(hookName) {
		if eventsByProvider[key] > 0 {
			return true
		}
	}
	return false
}

func storageProviderCandidates(hookName string) []string {
	switch hookName {
	case "claude-code":
		return []string{"claude_code", hookName}
	case "gemini":
		return []string{"gemini_cli", hookName}
	default:
		dashed := hookName
		under := strings.ReplaceAll(hookName, "-", "_")
		if dashed == under {
			return []string{dashed}
		}
		return []string{dashed, under}
	}
}

// checkManifests reports counts of provenance manifests by status in
// the last 7 days and groups failed reasons. The stable
// "redaction failed: <kind>: <err>" prefix produced by
// `redactionFailedReason` in `internal/provenance/sync.go` is preserved
// in the grouping.
func checkManifests(ctx context.Context, opts Options) []Check {
	if opts.RepoPath == "" {
		return []Check{{
			Category: "manifests",
			ID:       "scope",
			Status:   StatusOK,
			Message:  "no repo path supplied; manifest check skipped",
		}}
	}

	semDir := filepath.Join(opts.RepoPath, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return []Check{{
			Category: "manifests",
			ID:       "lineage_db",
			Status:   StatusOK,
			Message:  "lineage.db not present (Semantica not enabled in this repo)",
		}}
	}

	h, err := openLineage(ctx, dbPath)
	if err != nil {
		return []Check{{
			Category:    "manifests",
			ID:          "lineage_db",
			Status:      StatusWarn,
			Message:     "could not open lineage.db: " + err.Error(),
			Remediation: "check filesystem permissions on " + dbPath,
		}}
	}
	defer func() { _ = sqlstore.Close(h) }()

	repo, err := h.Queries.GetRepositoryByRootPath(ctx, opts.RepoPath)
	if err != nil {
		return []Check{{
			Category: "manifests",
			ID:       "repository",
			Status:   StatusOK,
			Message:  "repo not registered in lineage.db (no manifests yet)",
		}}
	}

	since := time.Now().Add(-manifestWindow).UnixMilli()
	statusRows, err := h.Queries.CountManifestsByStatus(ctx, sqldb.CountManifestsByStatusParams{
		RepositoryID: repo.RepositoryID,
		CreatedAt:    since,
	})
	if err != nil {
		return []Check{{
			Category:    "manifests",
			ID:          "query",
			Status:      StatusWarn,
			Message:     "manifests-by-status query failed: " + err.Error(),
			Remediation: "may indicate lineage.db schema drift; try `semantica enable`",
		}}
	}

	statusByName := map[string]int64{}
	for _, r := range statusRows {
		statusByName[r.Status] = r.Count
	}

	failedRows, err := h.Queries.ListFailedManifestReasons(ctx, sqldb.ListFailedManifestReasonsParams{
		RepositoryID: repo.RepositoryID,
		CreatedAt:    since,
	})
	if err != nil {
		return []Check{{
			Category:    "manifests",
			ID:          "query",
			Status:      StatusWarn,
			Message:     "failed-reasons query failed: " + err.Error(),
			Remediation: "may indicate lineage.db schema drift; try `semantica enable`",
		}}
	}

	return assembleManifestChecks(statusByName, failedRows)
}

func assembleManifestChecks(statusByName map[string]int64, failedRows []sqldb.ListFailedManifestReasonsRow) []Check {
	failed := statusByName["failed"]
	uploaded := statusByName["uploaded"]
	pending := statusByName["pending"] + statusByName["packaged"] + statusByName["uploading"]

	var checks []Check
	summary := fmt.Sprintf("last 7d - %d uploaded, %d failed, %d pending", uploaded, failed, pending)
	if failed == 0 {
		checks = append(checks, Check{
			Category: "manifests",
			ID:       "summary",
			Status:   StatusOK,
			Message:  summary,
		})
		return checks
	}

	checks = append(checks, Check{
		Category: "manifests",
		ID:       "summary",
		Status:   StatusWarn,
		Message:  summary,
	})

	groups := groupFailedReasons(failedRows)
	for i, g := range groups {
		if i >= failedManifestReasonsToShow {
			break
		}
		checks = append(checks, Check{
			Category:    "manifests",
			ID:          fmt.Sprintf("failed_reason:%d", i+1),
			Status:      StatusWarn,
			Message:     fmt.Sprintf("\"%s\" (%d)", g.group, g.count),
			Remediation: failedReasonRemediation(g.group),
		})
	}
	if len(groups) > failedManifestReasonsToShow {
		more := 0
		for _, g := range groups[failedManifestReasonsToShow:] {
			more += int(g.count)
		}
		checks = append(checks, Check{
			Category: "manifests",
			ID:       "failed_reason:other",
			Status:   StatusWarn,
			Message:  fmt.Sprintf("(%d more across %d additional reasons)", more, len(groups)-failedManifestReasonsToShow),
		})
	}
	return checks
}

type reasonGroup struct {
	group string
	count int64
}

// groupFailedReasons folds raw last_error strings into stable group
// keys, summing counts per group, sorted by count desc.
func groupFailedReasons(rows []sqldb.ListFailedManifestReasonsRow) []reasonGroup {
	counts := map[string]int64{}
	for _, r := range rows {
		key := reasonGroupKey(r.LastError)
		counts[key] += r.Count
	}

	groups := make([]reasonGroup, 0, len(counts))
	for k, v := range counts {
		groups = append(groups, reasonGroup{group: k, count: v})
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].count != groups[j].count {
			return groups[i].count > groups[j].count
		}
		return groups[i].group < groups[j].group
	})
	return groups
}

// reasonGroupKey extracts a stable category label from a manifest
// last_error string. The redaction prefix is preserved at
// "redaction failed: <kind>" granularity so redaction failures remain
// distinguishable from missing-blob errors.
func reasonGroupKey(reason string) string {
	const redactionPrefix = "redaction failed: "
	if strings.HasPrefix(reason, redactionPrefix) {
		rest := reason[len(redactionPrefix):]
		if idx := strings.Index(rest, ":"); idx > 0 {
			return redactionPrefix + rest[:idx]
		}
		return strings.TrimSuffix(redactionPrefix, ": ")
	}
	if strings.Contains(reason, "referenced by bundle but missing locally") {
		return "missing local blob"
	}
	if idx := strings.Index(reason, ":"); idx > 0 {
		return strings.TrimSpace(reason[:idx])
	}
	return reason
}

func failedReasonRemediation(group string) string {
	switch {
	case strings.HasPrefix(group, "redaction failed:"):
		return "redaction is fail-closed by design; check the worker log for the underlying redactor error"
	case group == "missing local blob":
		return "the referenced blob was garbage-collected before upload; usually self-heals on next turn"
	}
	return ""
}

// openLineage opens the per-repo lineage.db with the same options
// used by the sync path.
func openLineage(ctx context.Context, dbPath string) (*sqlstore.Handle, error) {
	return sqlstore.Open(ctx, dbPath, sqlstore.OpenOptions{
		BusyTimeout: 100 * time.Millisecond,
		Synchronous: "NORMAL",
	})
}

// relativeAge formats a short relative duration suitable for
// inline diagnostic output ("2m", "3h", "1d").
func relativeAge(now, then time.Time) string {
	d := now.Sub(then)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}
