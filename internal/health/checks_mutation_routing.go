package health

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
)

// mutationRoutingWindow bounds the "recent" slice of the metric so the effect
// of capture changes on fresh events reads separately from history.
const mutationRoutingWindow = 7 * 24 * time.Hour

// mutationFileOps are the file operations that can change repository contents.
// Read-only operations (file_op "read") never carry mutation ownership.
var mutationFileOps = map[string]bool{
	"write":  true,
	"edit":   true,
	"delete": true,
	"exec":   true,
}

// mutationRoutingCounts buckets mutation-capable events by whether the captured
// tool use carries a structured file path.
//
// Scope of this metric: it counts structured-path presence only. It does NOT
// measure repository-routing outcomes. An event without a structured path is
// not necessarily misrouted or launch-directory attributed — Bash tool windows,
// for example, can capture such an event in the command's target repository via
// observed changes. Actual routing outcomes (path match, tool-window target,
// observed delta, or launch-directory fallback) are a separate, capture-time
// measurement. Note also that stored tool_uses paths are relativized per target
// repo on write (broker.relativizeToolPaths), so the absolute-vs-relative split
// is not recoverable from storage either.
type mutationRoutingCounts struct {
	total       int // mutation-capable events
	withPath    int // carried a structured file_path
	withoutPath int // no structured file_path (e.g. shell exec)
}

// checkMutationRouting reports how many mutation-capable events carry no
// structured file path. It changes no routing behavior and asserts no
// attribution fault; it only surfaces structured-path coverage so later capture
// changes are measurable.
func checkMutationRouting(ctx context.Context, opts Options) []Check {
	if opts.RepoPath == "" {
		return nil
	}
	dbPath := filepath.Join(opts.RepoPath, ".semantica", "lineage.db")
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	h, err := openLineage(ctx, dbPath)
	if err != nil {
		return []Check{{
			Category: "attribution", ID: "mutation_routing", Status: StatusWarn,
			Message: "could not open lineage.db: " + err.Error(),
		}}
	}
	defer func() { _ = sqlstore.Close(h) }()

	repo, err := h.Queries.GetRepositoryByRootPath(ctx, opts.RepoPath)
	if err != nil {
		return nil
	}

	cutoff := time.Now().Add(-mutationRoutingWindow).UnixMilli()
	all, recent, err := scanMutationRouting(ctx, h.DB, repo.RepositoryID, cutoff)
	if err != nil {
		return []Check{{
			Category: "attribution", ID: "mutation_routing", Status: StatusWarn,
			Message: "mutation-routing scan failed: " + err.Error(),
		}}
	}
	return []Check{mutationRoutingResult(all, recent)}
}

// mutationRoutingResult renders the informational diagnostic. It is a pure
// function of the counts so the wording stays regression-tested. The metric is
// structured-path coverage, never a fault or fallback rate.
func mutationRoutingResult(all, recent mutationRoutingCounts) Check {
	if all.total == 0 {
		return Check{
			Category: "attribution", ID: "mutation_routing", Status: StatusOK,
			Message: "no mutation-capable events recorded yet",
		}
	}
	msg := fmt.Sprintf(
		"mutation-capable events without structured paths: %d of %d (%.0f%%), %d with structured paths; last 7d: %d of %d without paths",
		all.withoutPath, all.total, pct(all.withoutPath, all.total), all.withPath,
		recent.withoutPath, recent.total,
	)
	return Check{
		Category: "attribution", ID: "mutation_routing", Status: StatusOK,
		Message: msg,
	}
}

// scanMutationRouting classifies mutation-capable events in the repo, returning
// all-time counts and the counts within the recent window (ts >= cutoff).
func scanMutationRouting(ctx context.Context, db *sql.DB, repoID string, cutoff int64) (all, recent mutationRoutingCounts, err error) {
	const q = `SELECT tool_uses, ts FROM agent_events
WHERE repository_id = ?
  AND tool_uses IS NOT NULL AND tool_uses <> ''
  AND (tool_uses LIKE '%"file_op":"write"%'
    OR tool_uses LIKE '%"file_op":"edit"%'
    OR tool_uses LIKE '%"file_op":"delete"%'
    OR tool_uses LIKE '%"file_op":"exec"%')`
	rows, err := db.QueryContext(ctx, q, repoID)
	if err != nil {
		return all, recent, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var toolUses string
		var ts int64
		if err := rows.Scan(&toolUses, &ts); err != nil {
			return all, recent, err
		}
		mutation, hasPath := classifyMutationRouting(toolUses)
		if !mutation {
			continue // matched the LIKE prefilter but is not mutation-capable after parse
		}
		applyMutationClass(&all, hasPath)
		if ts >= cutoff {
			applyMutationClass(&recent, hasPath)
		}
	}
	return all, recent, rows.Err()
}

// classifyMutationRouting parses one tool_uses blob. It reports whether the
// event is mutation-capable and, if so, whether it carried any structured path.
func classifyMutationRouting(toolUses string) (mutation, hasPath bool) {
	var payload struct {
		Tools []struct {
			FilePath string `json:"file_path"`
			FileOp   string `json:"file_op"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(toolUses), &payload); err != nil {
		return false, false
	}
	for _, t := range payload.Tools {
		if mutationFileOps[t.FileOp] {
			mutation = true
		}
		if t.FilePath != "" {
			hasPath = true
		}
	}
	return mutation, hasPath
}

func applyMutationClass(c *mutationRoutingCounts, hasPath bool) {
	c.total++
	if hasPath {
		c.withPath++
	} else {
		c.withoutPath++
	}
}

func pct(part, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) * 100 / float64(total)
}
