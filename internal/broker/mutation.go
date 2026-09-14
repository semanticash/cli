package broker

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/platform"
)

// orchestrationTool recognizes delegation boundaries, not the delegated edits.
func orchestrationTool(name string) bool {
	switch strings.ToLower(name) {
	case "agent", "task", "subagent", "invoke_agent":
		return true
	default:
		return false
	}
}

// mutationTool keeps known context tools separate from possible mutations.
func mutationTool(name, operation string) bool {
	if orchestrationTool(name) {
		return false
	}
	switch strings.ToLower(operation) {
	case "write", "edit", "delete", "rename", "create", "exec":
		return true
	}
	switch strings.ToLower(name) {
	case "", "read", "readfile", "read_file", "glob", "grep", "ls", "list_files", "websearch", "webfetch", "todowrite", "todoread":
		return false
	default:
		return true
	}
}

// MutationPaths returns provider-supplied mutation paths and unresolved coverage.
// Shell command text and session directories never supply mutation paths.
func MutationPaths(ev RawEvent) (mutation bool, paths []string, missing bool) {
	var p struct {
		ContentTypes []string `json:"content_types"`
		Tools        []struct {
			Name     string `json:"name"`
			FilePath string `json:"file_path"`
			FileOp   string `json:"file_op"`
		} `json:"tools"`
	}
	if ev.ToolUsesJSON != "" && json.Unmarshal([]byte(ev.ToolUsesJSON), &p) != nil {
		return true, nil, true
	}
	seen := map[string]bool{}
	add := func(path string) {
		if !platform.LooksAbsolutePath(path) {
			missing = true
			return
		}
		path = filepath.ToSlash(filepath.Clean(path))
		if !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	for _, tool := range p.Tools {
		if tool.Name != "" && !mutationTool(tool.Name, tool.FileOp) {
			continue
		}
		mutation = true
		path := tool.FilePath
		if path != "" && !platform.LooksAbsolutePath(path) && platform.LooksAbsolutePath(ev.SourceProjectPath) {
			// Only accept provider-normalized paths also present in FilePaths.
			candidate := filepath.ToSlash(filepath.Join(ev.SourceProjectPath, path))
			for _, supplied := range ev.FilePaths {
				if platform.NormalizePathForCompare(supplied) == platform.NormalizePathForCompare(candidate) {
					path = supplied
					break
				}
			}
		}
		add(path)
	}
	if len(p.Tools) == 0 && mutationTool(ev.ToolName, "") {
		mutation = true
		for _, path := range ev.FilePaths {
			add(path)
		}
		if len(paths) == 0 {
			missing = true
		}
	}
	if !mutation {
		if len(p.Tools) == 0 {
			for _, kind := range p.ContentTypes {
				if (kind == "tool_use" && !orchestrationTool(ev.ToolName)) || strings.HasSuffix(kind, "file_edit") {
					mutation, missing = true, true
				}
			}
		}
		if mutationTool(ev.ToolName, "") {
			mutation, missing = true, true
		}
	}
	return mutation, paths, missing
}

// PlanRoutes separates destination evidence from session context.
func PlanRoutes(events []RawEvent, repos []RegisteredRepo) (matches []RepoMatch, unresolved []RawEvent) {
	for _, ev := range events {
		mutation, paths, missing := MutationPaths(ev)
		if mutation {
			routed := ev
			routed.FilePaths = paths
			for _, path := range paths {
				if len(RouteEvents([]RawEvent{{FilePaths: []string{path}}}, repos)) == 0 {
					missing = true
				}
			}
			if missing || len(paths) == 0 {
				unresolved = append(unresolved, ev)
			}
			if missing {
				routed = ObservationContext(routed)
			}
			matches = append(matches, RouteEvents([]RawEvent{routed}, repos)...)
			continue
		}
		if len(ev.FilePaths) > 0 {
			matches = append(matches, RouteEvents([]RawEvent{ev}, repos)...)
		} else if m := RouteNoPathEvents([]RawEvent{ev}, repos, ev.SourceProjectPath); m != nil {
			matches = append(matches, *m)
		}
	}
	// Batch writes by repository while preserving event order.
	var grouped []RepoMatch
	indices := map[string]int{}
	for _, m := range matches {
		i, ok := indices[m.Repo.CanonicalPath]
		if !ok {
			i = len(grouped)
			indices[m.Repo.CanonicalPath] = i
			grouped = append(grouped, RepoMatch{Repo: m.Repo})
		}
		grouped[i].Events = append(grouped[i].Events, m.Events...)
	}
	return grouped, unresolved
}

// ObservationContext preserves an event without asserting mutation ownership.
// Verified tool deltas are evaluated independently of this raw event.
func ObservationContext(ev RawEvent) RawEvent {
	var p map[string]json.RawMessage
	if json.Unmarshal([]byte(ev.ToolUsesJSON), &p) != nil || p == nil {
		p = map[string]json.RawMessage{}
	}
	p["mutation_routing"] = json.RawMessage(`"context_only"`)
	data, _ := json.Marshal(p)
	ev.ToolUsesJSON = string(data)
	return ev
}
