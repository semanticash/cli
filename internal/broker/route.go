package broker

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/platform"
)

// RouteEvents associates absolute paths with the deepest active repository.
// Events spanning repositories are routed to each match once.
func RouteEvents(events []RawEvent, repos []RegisteredRepo) []RepoMatch {
	if len(events) == 0 || len(repos) == 0 {
		return nil
	}

	// Pre-normalize repo paths: ensure trailing slash for prefix matching.
	type repoEntry struct {
		repo   RegisteredRepo
		prefix string // canonical_path + "/"
	}
	var entries []repoEntry
	for _, r := range repos {
		if !r.Active {
			continue
		}
		p := platform.NormalizePathForCompare(r.CanonicalPath)
		if !strings.HasSuffix(p, "/") {
			p += "/"
		}
		entries = append(entries, repoEntry{repo: r, prefix: p})
	}

	// Accumulate events per repo.
	matched := make(map[string][]RawEvent) // canonical_path -> events

	for _, ev := range events {
		if len(ev.FilePaths) == 0 {
			continue
		}

		// For each file path, find the deepest (longest prefix) matching repo.
		// Collect unique repos per event to avoid duplicates.
		repoSet := make(map[string]bool)
		for _, fp := range ev.FilePaths {
			if !platform.LooksAbsolutePath(fp) {
				continue
			}
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

// RouteNoPathEvents associates non-mutation context with its session repository.
// Mutation events require destination evidence and never use this fallback.
func RouteNoPathEvents(events []RawEvent, repos []RegisteredRepo, sourceProjectPath string) *RepoMatch {
	if len(events) == 0 || sourceProjectPath == "" {
		return nil
	}
	var contextEvents []RawEvent
	for _, ev := range events {
		mutation, _, _ := MutationPaths(ev)
		if !mutation && len(ev.FilePaths) == 0 {
			contextEvents = append(contextEvents, ev)
		}
	}
	if len(contextEvents) == 0 {
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
		if !repos[i].Active {
			continue
		}
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
		Events: contextEvents,
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
