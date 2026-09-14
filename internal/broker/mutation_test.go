package broker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	attrevents "github.com/semanticash/cli/internal/attribution/events"
	"github.com/semanticash/cli/internal/store/blobs"
)

func mutationEvent(id, name, path, cwd string) RawEvent {
	data, _ := json.Marshal(map[string]any{"tools": []map[string]string{{"name": name, "file_path": path}}})
	ev := RawEvent{EventID: id, Provider: "codex", SourceKey: "session", ProviderSessionID: "session", TurnID: "turn", ToolName: name, ToolUsesJSON: string(data), SourceProjectPath: cwd}
	if path != "" {
		ev.FilePaths = []string{path}
	}
	return ev
}

func TestMutationPathsDistinguishesReadOnlyAndMissingToolMetadata(t *testing.T) {
	for _, tc := range []struct {
		metadata   string
		unresolved bool
	}{
		{`{"content_types":["tool_use"],"tools":[{"name":"Read"}]}`, false},
		{`{"content_types":["tool_use"]}`, true},
		{`{"tools":[{}]}`, true},
		{`{broken`, true},
	} {
		mutation, _, missing := MutationPaths(RawEvent{ToolUsesJSON: tc.metadata})
		if mutation != tc.unresolved || missing != tc.unresolved {
			t.Fatalf("%s: mutation=%v missing=%v", tc.metadata, mutation, missing)
		}
	}
}

func TestPlanRoutesKeepsOrchestrationContext(t *testing.T) {
	for _, name := range []string{"Agent", "task", "Subagent", "invoke_agent"} {
		for _, metadata := range []string{"", `{"content_types":["tool_use"]}`, `{"content_types":["tool_use"],"tools":[{"name":"` + name + `","file_op":"exec"}]}`} {
			ev := RawEvent{EventID: "boundary", ToolName: name, ToolUsesJSON: metadata, SourceProjectPath: "/work/a", TurnID: "parent-turn", ParentSessionID: "parent-session"}
			matches, unresolved := PlanRoutes([]RawEvent{ev}, []RegisteredRepo{makeRepo("/work/a")})
			if len(unresolved) != 0 || len(matches) != 1 || len(matches[0].Events) != 1 {
				t.Fatalf("%s %s: matches=%+v unresolved=%+v", name, metadata, matches, unresolved)
			}
			got := matches[0].Events[0]
			if matches[0].Repo.CanonicalPath != "/work/a" || got.TurnID != ev.TurnID || got.ParentSessionID != ev.ParentSessionID || got.ToolUsesJSON != ev.ToolUsesJSON {
				t.Fatalf("lost boundary context: %+v", got)
			}
		}
	}
	for _, metadata := range []string{
		`{"content_types":["provider_file_edit"]}`,
		`{"tools":[{"name":"Agent","file_op":"exec"},{"name":"Bash","file_op":"exec"}]}`,
		`{"tools":[{"name":"FutureOrchestrator","file_op":"exec"}]}`,
	} {
		ev := RawEvent{EventID: "mixed", ToolName: "Agent", ToolUsesJSON: metadata, SourceProjectPath: "/work/a"}
		if matches, unresolved := PlanRoutes([]RawEvent{ev}, []RegisteredRepo{makeRepo("/work/a")}); len(matches) != 0 || len(unresolved) != 1 {
			t.Fatalf("unknown mutation bypassed: %+v %+v", matches, unresolved)
		}
	}
}

func TestPlanRoutesDoesNotAssignMutationsByCWD(t *testing.T) {
	repos := []RegisteredRepo{makeRepo("/work/a"), makeRepo("/work/b"), makeRepo("/work/b/nested")}
	for _, tc := range []struct {
		name        string
		ev          RawEvent
		destination string
		unresolved  bool
	}{
		{"opaque_shell", mutationEvent("shell", "Bash", "", "/work/a"), "", true},
		{"unknown_tool", mutationEvent("unknown", "CustomTool", "", "/work/a"), "", true},
		{"write_b", mutationEvent("write", "Write", "/work/b/code.go", "/work/a"), "/work/b", false},
		{"nested", mutationEvent("nested", "Edit", "/work/b/nested/code.go", "/work/a"), "/work/b/nested", false},
		{"unregistered", mutationEvent("outside", "Write", "/outside/code.go", "/work/a"), "", true},
		{"relative_unknown", mutationEvent("relative", "Write", "code.go", ""), "", true},
		{"read_context", mutationEvent("read", "Read", "", "/work/a"), "/work/a", false},
		{"conversation", RawEvent{EventID: "text", SourceProjectPath: "/work/a"}, "/work/a", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			matches, unresolved := PlanRoutes([]RawEvent{tc.ev}, repos)
			if (len(unresolved) > 0) != tc.unresolved {
				t.Fatalf("unresolved=%+v", unresolved)
			}
			if tc.destination == "" {
				if len(matches) != 0 {
					t.Fatalf("false destination: %+v", matches)
				}
				return
			}
			if len(matches) != 1 || matches[0].Repo.CanonicalPath != tc.destination {
				t.Fatalf("matches=%+v", matches)
			}
		})
	}
	if m := RouteNoPathEvents([]RawEvent{mutationEvent("shell", "Bash", "", "/work/a")}, repos, "/work/a"); m != nil {
		t.Fatal("public fallback accepts mutations")
	}
	repos[1].Active = false
	if m, u := PlanRoutes([]RawEvent{mutationEvent("disabled", "Write", "/work/b/file", "/work/a")}, repos); len(m) != 0 || len(u) != 1 {
		t.Fatal("inactive destination accepted")
	}
}

func TestPlanRoutesKeepsEachContextAndPartialMutation(t *testing.T) {
	repos := []RegisteredRepo{makeRepo("/work/a"), makeRepo("/work/b")}
	events := []RawEvent{{EventID: "a", SourceProjectPath: "/work/a"}, {EventID: "b", SourceProjectPath: "/work/b"}}
	m, u := PlanRoutes(events, repos)
	if len(u) != 0 || len(m) != 2 || m[1].Repo.CanonicalPath != "/work/b" {
		t.Fatalf("mixed session contexts: %+v", m)
	}
	ev := mutationEvent("mixed", "Write", "/work/b/file", "/work/a")
	ev.ToolUsesJSON = `{"tools":[{"name":"Write","file_path":"/work/b/file"},{"name":"Bash"}]}`
	m, u = PlanRoutes([]RawEvent{ev}, repos)
	if len(m) != 1 || m[0].Repo.CanonicalPath != "/work/b" || len(u) != 1 || !attrevents.ContextOnly(m[0].Events[0].ToolUsesJSON) {
		t.Fatalf("partial mutation lost uncertainty: %+v %+v", m, u)
	}
	ev.ToolUsesJSON = `{"tools":[{"name":"Read","file_path":"/work/b/file"},{"name":"Bash"}]}`
	if m, u = PlanRoutes([]RawEvent{ev}, repos); len(m) != 0 || len(u) != 1 {
		t.Fatalf("read path became mutation destination: %+v", m)
	}
}

func TestObservationContextSurvivesPathNormalization(t *testing.T) {
	ev := ObservationContext(mutationEvent("mixed", "Write", "/work/a/file", "/work/a"))
	if !attrevents.ContextOnly(relativizeToolPaths(ev.ToolUsesJSON, "/work/a")) {
		t.Fatal("path normalization erased context-only marker")
	}
}

func TestUnresolvedRetentionPreservesObjectsAndIdentity(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	ctx := context.Background()
	src, err := blobs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hash, _, err := src.Put(ctx, []byte("original captured evidence"))
	if err != nil {
		t.Fatal(err)
	}
	ev := mutationEvent("shell", "Bash", "", "/work/a")
	ev.PayloadHash, ev.ProvenanceHash = hash, hash
	ev.TokenUsageValid = true
	if err := RetainUnresolvedMutations(ctx, []RawEvent{ev}, src); err != nil {
		t.Fatal(err)
	}
	root, _ := UnresolvedMutationDir()
	paths, err := filepath.Glob(filepath.Join(root, "*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatal(paths, err)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var record UnresolvedMutation
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if !record.TokenUsageValid || string(record.Objects[hash]) != "original captured evidence" || record.Event.SourceProjectPath != "/work/a" {
		t.Fatalf("incomplete retained event: %+v", record)
	}
	// Redelivery needs no source blobs and preserves the first receipt time.
	ev.Timestamp, ev.SessionStartedAt = 999, 999
	if err := RetainUnresolvedMutations(ctx, []RawEvent{ev}, nil); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(paths[0])
	if string(again) != string(raw) {
		t.Fatal("retry rewrote original record")
	}
	ev.ToolUsesJSON = `{"tools":[{"name":"Write"}]}`
	if err := RetainUnresolvedMutations(ctx, []RawEvent{ev}, src); err == nil {
		t.Fatal("event ID/content collision accepted")
	}
	after, _ := os.ReadFile(paths[0])
	if string(after) != string(raw) {
		t.Fatal("collision replaced retained evidence")
	}
}

func TestUnresolvedRetentionRejectsMissingObjects(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	src, err := blobs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ev := mutationEvent("missing", "Bash", "", "/work/a")
	ev.PayloadHash = strings.Repeat("a", 64)
	if err := RetainUnresolvedMutations(context.Background(), []RawEvent{ev}, src); err == nil {
		t.Fatal("missing evidence accepted")
	}
	root, _ := UnresolvedMutationDir()
	paths, _ := filepath.Glob(filepath.Join(root, "*.json"))
	if len(paths) != 0 {
		t.Fatal("incomplete event acknowledged")
	}
}

func TestUnresolvedRetentionConcurrentReplay(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	ev := mutationEvent("concurrent", "Bash", "", "/work/a")
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- RetainUnresolvedMutations(context.Background(), []RawEvent{ev}, nil) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	root, _ := UnresolvedMutationDir()
	paths, _ := filepath.Glob(filepath.Join(root, "*.json"))
	if len(paths) != 1 {
		t.Fatal(paths)
	}
}
