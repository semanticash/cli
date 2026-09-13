package broker

import (
	"encoding/json"
	"sort"
)

// RoutingSignal identifies how a repository was selected, not whether the
// selection was correct.
type RoutingSignal string

const (
	// SignalStructuredPath indicates a match by absolute file path.
	SignalStructuredPath RoutingSignal = "structured_path"
	// SignalLaunchFallback indicates a pathless event routed by source project path.
	SignalLaunchFallback RoutingSignal = "launch_fallback"
	// SignalUnresolved indicates that no repository was selected.
	SignalUnresolved RoutingSignal = "unresolved"
)

// mutationToolOps lists operations that may change repository contents.
var mutationToolOps = map[string]bool{
	"write":  true,
	"edit":   true,
	"delete": true,
	"exec":   true,
}

// IsMutationToolUse reports whether tool_uses JSON contains an operation that
// may mutate files. Empty or invalid JSON returns false.
func IsMutationToolUse(toolUsesJSON string) bool {
	if toolUsesJSON == "" {
		return false
	}
	var payload struct {
		Tools []struct {
			FileOp string `json:"file_op"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(toolUsesJSON), &payload); err != nil {
		return false
	}
	for _, t := range payload.Tools {
		if mutationToolOps[t.FileOp] {
			return true
		}
	}
	return false
}

// RoutingDecision records repository selection for a mutation-capable event.
// Selected repositories may legitimately differ from SessionRepo.
type RoutingDecision struct {
	EventID     string
	Provider    string
	Signal      RoutingSignal
	Selected    []string // canonical repo paths selected for the event
	SessionRepo string   // canonical repo path implied by the source project path
}

// RouteWithDecisions combines path routing with the shared source-project
// fallback and returns decisions for mutation-capable events. It performs no I/O.
//
// Tool-window target routing is handled separately and is not included.
func RouteWithDecisions(events []RawEvent, repos []RegisteredRepo) ([]RepoMatch, []RoutingDecision) {
	structural := RouteEvents(events, repos)

	// Index path-based destinations by event ID.
	structuralByEvent := make(map[string][]string)
	for _, m := range structural {
		for _, ev := range m.Events {
			structuralByEvent[ev.EventID] = append(structuralByEvent[ev.EventID], m.Repo.CanonicalPath)
		}
	}

	// Preserve the shared fallback: the first nonempty source project path
	// among pathless events applies to the entire pathless group.
	var noPathEvents []RawEvent
	var sourceProjectPath string
	for _, ev := range events {
		if len(ev.FilePaths) == 0 {
			noPathEvents = append(noPathEvents, ev)
			if sourceProjectPath == "" {
				sourceProjectPath = ev.SourceProjectPath
			}
		}
	}
	matches := structural
	fallbackRepo := ""
	fallbackEvents := make(map[string]bool)
	if len(noPathEvents) > 0 {
		if m := RouteNoPathEvents(noPathEvents, repos, sourceProjectPath); m != nil {
			matches = append(matches, *m)
			fallbackRepo = m.Repo.CanonicalPath
			for _, ev := range m.Events {
				fallbackEvents[ev.EventID] = true
			}
		}
	}

	var decisions []RoutingDecision
	for i := range events {
		ev := &events[i]
		if !IsMutationToolUse(ev.ToolUsesJSON) {
			continue
		}
		d := RoutingDecision{
			EventID:     ev.EventID,
			Provider:    ev.Provider,
			SessionRepo: deepestRepoForPath(ev.SourceProjectPath, repos),
		}
		switch {
		case len(structuralByEvent[ev.EventID]) > 0:
			d.Signal = SignalStructuredPath
			d.Selected = uniqueSortedStrings(structuralByEvent[ev.EventID])
		case fallbackEvents[ev.EventID]:
			d.Signal = SignalLaunchFallback
			d.Selected = []string{fallbackRepo}
		default:
			d.Signal = SignalUnresolved
		}
		decisions = append(decisions, d)
	}
	return matches, decisions
}

// deepestRepoForPath returns the canonical path of the deepest registered repo
// containing path, or "" when none contains it.
func deepestRepoForPath(path string, repos []RegisteredRepo) string {
	if path == "" {
		return ""
	}
	best := ""
	bestLen := 0
	for i := range repos {
		if PathBelongsToRepo(path, repos[i].CanonicalPath) {
			if len(repos[i].CanonicalPath) > bestLen {
				best = repos[i].CanonicalPath
				bestLen = len(repos[i].CanonicalPath)
			}
		}
	}
	return best
}

func uniqueSortedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
