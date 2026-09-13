package broker

import (
	"reflect"
	"testing"
)

const (
	mutWrite = `{"tools":[{"name":"Write","file_op":"write","file_path":"/x"}]}`
	mutExec  = `{"tools":[{"name":"Bash","file_op":"exec"}]}`
	readOnly = `{"tools":[{"name":"Read","file_op":"read","file_path":"/x"}]}`
)

func TestIsMutationToolUse(t *testing.T) {
	cases := map[string]bool{
		mutWrite: true,
		mutExec:  true,
		readOnly: false,
		"":       false,
		`{bad`:   false,
	}
	for in, want := range cases {
		if got := IsMutationToolUse(in); got != want {
			t.Errorf("IsMutationToolUse(%q) = %v, want %v", in, got, want)
		}
	}
}

func repo(id, path string) RegisteredRepo {
	return RegisteredRepo{RepoID: id, Path: path, CanonicalPath: path, Active: true}
}

func TestRouteWithDecisions_Signals(t *testing.T) {
	repos := []RegisteredRepo{repo("api", "/repos/api"), repo("cli", "/repos/cli")}

	events := []RawEvent{
		// The file path selects api despite the session starting in cli.
		{EventID: "e1", Provider: "codex", ToolUsesJSON: mutWrite,
			FilePaths: []string{"/repos/api/foo.go"}, SourceProjectPath: "/repos/cli"},
		// Pathless commands use the source-project fallback.
		{EventID: "e2", Provider: "codex", ToolUsesJSON: mutExec, SourceProjectPath: "/repos/cli"},
		// Read-only events produce no mutation decision.
		{EventID: "e4", Provider: "codex", ToolUsesJSON: readOnly,
			FilePaths: []string{"/repos/api/foo.go"}, SourceProjectPath: "/repos/cli"},
	}

	_, decisions := RouteWithDecisions(events, repos)

	byID := make(map[string]RoutingDecision, len(decisions))
	for _, d := range decisions {
		byID[d.EventID] = d
	}
	if _, ok := byID["e4"]; ok {
		t.Errorf("read-only event must not produce a routing decision")
	}
	if len(decisions) != 2 {
		t.Fatalf("decisions = %d, want 2 (mutation-capable only)", len(decisions))
	}

	// Preserve the distinction between destination and session repository.
	if d := byID["e1"]; d.Signal != SignalStructuredPath ||
		!reflect.DeepEqual(d.Selected, []string{"/repos/api"}) || d.SessionRepo != "/repos/cli" {
		t.Errorf("e1 = %+v, want structured_path selected=[/repos/api] session=/repos/cli", d)
	}
	if d := byID["e2"]; d.Signal != SignalLaunchFallback ||
		!reflect.DeepEqual(d.Selected, []string{"/repos/cli"}) {
		t.Errorf("e2 = %+v, want launch_fallback selected=[/repos/cli]", d)
	}
}

// A pathless event remains unresolved when its source project is unregistered.
func TestRouteWithDecisions_NoPathUnresolvedWhenSessionUnregistered(t *testing.T) {
	repos := []RegisteredRepo{repo("api", "/repos/api")}
	events := []RawEvent{
		{EventID: "e1", Provider: "codex", ToolUsesJSON: mutExec, SourceProjectPath: "/elsewhere"},
	}
	_, decisions := RouteWithDecisions(events, repos)
	if len(decisions) != 1 || decisions[0].Signal != SignalUnresolved || len(decisions[0].Selected) != 0 {
		t.Fatalf("decisions = %+v, want one unresolved with no selection", decisions)
	}
}

func TestRouteWithDecisions_PathBearingButUnmatchedIsUnresolved(t *testing.T) {
	repos := []RegisteredRepo{repo("api", "/repos/api")}
	// An unmatched file path does not qualify for the pathless fallback.
	events := []RawEvent{
		{EventID: "e1", Provider: "codex", ToolUsesJSON: mutWrite,
			FilePaths: []string{"/somewhere/else/foo.go"}, SourceProjectPath: "/somewhere"},
	}
	_, decisions := RouteWithDecisions(events, repos)
	if len(decisions) != 1 || decisions[0].Signal != SignalUnresolved {
		t.Fatalf("decisions = %+v, want one unresolved", decisions)
	}
}
