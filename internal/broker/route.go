package broker

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/platform"
)

// RouteEvents matches raw events to registered repos by file path containment.
//
// Correctness rule: an event belongs to a repo if it touched one or more file
// paths under that repo's canonical root.
//
// No-path fallback: events without file paths are not routed by this function.
// The caller must handle the fallback (e.g., route to the source repo when the
// provider session path matches a registered repo).
//
// Deepest-match rule: each file path routes to the deepest (longest canonical
// path) matching repo. This prevents nested repos from leaking events to
// parent repos. An event that touches files in multiple repos (at different
// nesting levels) routes to each deepest match once.
//
// Disabled repo rule: only active repos in the input slice are considered.
// The caller should pass ListActiveRepos output.
func RouteEvents(events []RawEvent, repos []RegisteredRepo) []RepoMatch {
	if len(events) == 0 || len(repos) == 0 {
		return nil
	}

	// Pre-normalize repo paths: ensure trailing slash for prefix matching.
	type repoEntry struct {
		repo   RegisteredRepo
		prefix string // canonical_path + "/"
	}
	entries := make([]repoEntry, len(repos))
	for i, r := range repos {
		p := platform.NormalizePathForCompare(r.CanonicalPath)
		if !strings.HasSuffix(p, "/") {
			p += "/"
		}
		entries[i] = repoEntry{repo: r, prefix: p}
	}

	// Accumulate events per repo.
	matched := make(map[string][]RawEvent) // canonical_path -> events

	for _, ev := range events {
		if len(ev.FilePaths) == 0 {
			continue // no file paths - caller handles fallback
		}

		// For each file path, find the deepest (longest prefix) matching repo.
		// Collect unique repos per event to avoid duplicates.
		repoSet := make(map[string]bool)
		for _, fp := range ev.FilePaths {
			cleaned := platform.NormalizePathForCompare(fp)
			var bestCP string
			var bestLen int
			for _, entry := range entries {
				root := strings.TrimSuffix(entry.prefix, "/")
				if (cleaned == root || strings.HasPrefix(cleaned, entry.prefix)) && len(entry.prefix) > bestLen {
					bestCP = entry.repo.CanonicalPath
					bestLen = len(entry.prefix)
				}
			}
			if bestCP != "" {
				repoSet[bestCP] = true
			}
		}

		for cp := range repoSet {
			matched[cp] = append(matched[cp], ev)
		}
	}

	// Build result slice preserving repo order.
	var result []RepoMatch
	for _, entry := range entries {
		if evts, ok := matched[entry.repo.CanonicalPath]; ok {
			result = append(result, RepoMatch{
				Repo:   entry.repo,
				Events: evts,
			})
		}
	}

	return result
}

// RouteNoPathEvents handles events that have no file paths by matching them
// to repos via the source's project path. This is a fallback heuristic for
// non-file events (e.g., pure text conversations), not a strong cross-repo
// routing rule.
//
// sourceProjectPath should be the provider-specific project path that the
// session was launched from (e.g., the decoded Claude project directory).
func RouteNoPathEvents(events []RawEvent, repos []RegisteredRepo, sourceProjectPath string) *RepoMatch {
	if len(events) == 0 || sourceProjectPath == "" {
		return nil
	}

	// Match the deepest (most specific) repo whose root contains the source
	// project path. Handles sessions launched from a subdir inside an enabled
	// repo (e.g., /repo/subdir -> registered /repo). Longest match wins to
	// avoid ambiguity when nested repos are registered (e.g., /repo and
	// /repo/subrepo).
	var bestRepo *RegisteredRepo
	bestLen := 0
	for i := range repos {
		if PathBelongsToRepo(sourceProjectPath, repos[i].CanonicalPath) {
			if len(repos[i].CanonicalPath) > bestLen {
				bestRepo = &repos[i]
				bestLen = len(repos[i].CanonicalPath)
			}
		}
	}

	if bestRepo == nil {
		return nil
	}
	return &RepoMatch{
		Repo:   *bestRepo,
		Events: events,
	}
}

// ExtractFilePaths parses the tool_uses JSON and returns all unique absolute
// file paths found. Recognizes both POSIX and Windows absolute path formats
// since agent payloads may use either regardless of host OS.
func ExtractFilePaths(toolUsesJSON string) []string {
	if toolUsesJSON == "" {
		return nil
	}

	// Fast path: if no "file_path" key, skip JSON parsing.
	if !strings.Contains(toolUsesJSON, "file_path") {
		return nil
	}

	type tool struct {
		FilePath string `json:"file_path"`
	}
	type payload struct {
		Tools []tool `json:"tools"`
	}

	var p payload
	if err := json.Unmarshal([]byte(toolUsesJSON), &p); err != nil {
		return nil
	}

	seen := make(map[string]bool)
	var paths []string
	for _, t := range p.Tools {
		if t.FilePath == "" {
			continue
		}
		if !platform.LooksAbsolutePath(t.FilePath) {
			continue
		}
		cleaned := filepath.ToSlash(filepath.Clean(t.FilePath))
		if !seen[cleaned] {
			seen[cleaned] = true
			paths = append(paths, cleaned)
		}
	}

	return paths
}
