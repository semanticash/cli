package annotations

import "testing"

func TestObservationContextCannotImplyDeletion(t *testing.T) {
	ev := Event{EventID: "event", Role: "assistant", Payload: []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"rm victim.txt"}}]}}`)}
	in := DetectInput{RepoRoot: "/repo", Events: []Event{ev}}
	if got := projectEvents(in); len(got) != 1 || len(got[0].removed) != 1 {
		t.Fatalf("control did not recognize deletion: %+v", got)
	}
	in.Events[0].ToolUses = `{"mutation_routing":"context_only"}`
	if got := projectEvents(in); len(got) != 0 {
		t.Fatalf("context inferred file activity: %+v", got)
	}
}
