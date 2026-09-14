package hooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	attrevents "github.com/semanticash/cli/internal/attribution/events"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
)

func TestCaptureUnresolvedRetentionPrecedesOffset(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()
	bs, err := blobs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	event := broker.RawEvent{EventID: "opaque", Provider: "test", SourceKey: "session", ProviderSessionID: "session", SourceProjectPath: w.repoPath,
		Kind: "assistant", Role: "assistant", ToolName: "Bash", ToolUsesJSON: `{"tools":[{"name":"Bash"}]}`, PayloadHash: strings.Repeat("a", 64)}
	state := &CaptureState{SessionID: "session", Provider: "test", TurnID: "turn", CWD: w.repoPath, TranscriptRef: "transcript", TranscriptOffset: 7, Timestamp: 1}
	if err := SaveCaptureState(state); err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{name: "test", transcriptOffset: 99, events: []broker.RawEvent{event}}
	hook := &Event{SessionID: "session", TranscriptRef: "transcript"}
	if _, err := CaptureAndRouteForRepo(ctx, p, hook, w.bh, bs, w.repoPath); err == nil {
		t.Fatal("missing blob should block offset advancement")
	}
	state, err = LoadCaptureState("session")
	if err != nil || state.TranscriptOffset != 7 {
		t.Fatalf("offset advanced on retention failure: %+v %v", state, err)
	}
	hash, _, err := bs.Put(ctx, []byte("captured payload"))
	if err != nil {
		t.Fatal(err)
	}
	p.events[0].PayloadHash = hash
	ok, err := CaptureAndRouteForRepo(ctx, p, hook, w.bh, bs, w.repoPath)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	state, err = LoadCaptureState("session")
	if err != nil || state.TranscriptOffset != 99 {
		t.Fatal(state, err)
	}
	h, err := sqlstore.Open(ctx, filepath.Join(w.semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	var n int
	if err := h.DB.QueryRow("select count(*) from agent_events where event_id='opaque'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("opaque mutation routed by cwd: %d %v", n, err)
	}
	root, _ := broker.UnresolvedMutationDir()
	paths, _ := filepath.Glob(filepath.Join(root, "*.json"))
	if len(paths) != 1 {
		t.Fatal(paths)
	}
}

func TestDispatchCrossRepoShellKeepsObservationContext(t *testing.T) {
	for _, changePrimary := range []bool{false, true} {
		name := "only_secondary"
		if changePrimary {
			name = "primary_delta"
		}
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("SEMANTICA_HOME", home)
			a := newToolWindowWorld(t, home, "a")
			b := newToolWindowWorld(t, home, "b")
			ctx := context.Background()
			bs, err := blobs.NewStore(filepath.Join(home, "objects"))
			if err != nil {
				t.Fatal(err)
			}
			payload := []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"rm victim.txt"}}]}}`)
			hash, _, err := bs.Put(ctx, payload)
			if err != nil {
				t.Fatal(err)
			}
			raw := broker.RawEvent{EventID: "shell", Provider: "codex", SourceKey: "session", ProviderSessionID: "session", SourceProjectPath: a.repoPath, Kind: "assistant", Role: "assistant", ToolName: "Bash", ToolUseID: "shell", TurnID: "turn", EventSource: "hook", Timestamp: time.Now().UnixMilli(), ToolUsesJSON: `{"tools":[{"name":"Bash"}]}`, PayloadHash: hash}
			p := &fakeDirectProvider{fakeProvider: fakeProvider{name: "codex"}, directEvents: []broker.RawEvent{raw}}
			if err := SaveCaptureState(&CaptureState{SessionID: "session", Provider: "codex", TurnID: "turn", CWD: a.repoPath, Timestamp: 1}); err != nil {
				t.Fatal(err)
			}
			pre := startedEvent("session", "shell", a.repoPath)
			pre.ToolInput = []byte(`{"command":"generate"}`)
			if err := Dispatch(ctx, p, pre, a.bh, bs); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(b.repoPath, "changed.txt"), []byte("secondary change\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if changePrimary {
				if err := os.WriteFile(filepath.Join(a.repoPath, "changed.txt"), []byte("observed primary change\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			post := *pre
			post.Type = ToolStepCompleted
			post.Timestamp = time.Now().UnixMilli()
			post.ToolResponse = []byte(`{"exit_code":0}`)
			if err := Dispatch(ctx, p, &post, a.bh, bs); err != nil {
				t.Fatal(err)
			}
			h, err := sqlstore.Open(ctx, filepath.Join(a.semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sqlstore.Close(h) }()
			var tools string
			if err := h.DB.QueryRow("select tool_uses from agent_events where event_id='shell'").Scan(&tools); err != nil {
				t.Fatal(err)
			}
			if !attrevents.ContextOnly(tools) {
				t.Fatalf("cwd copy claims mutation ownership: %s", tools)
			}
			c, _ := attrevents.BuildCandidatesFromRows([]attrevents.EventRow{{EventID: "shell", Provider: "codex", Role: "assistant", ToolUses: tools, PayloadHash: hash, Payload: payload}}, a.repoPath, nil)
			if len(c.InferredDeletions) != 0 || len(c.ProviderTouchedFiles) != 0 {
				t.Fatalf("cwd generated attribution: %+v", c)
			}
			var deltaHash string
			if err := h.DB.QueryRow("select evidence_hash from agent_event_evidence_links where event_id='shell'").Scan(&deltaHash); err != nil {
				t.Fatal(err)
			}
			repoBlobs, err := blobs.NewStore(filepath.Join(a.semDir, "objects"))
			if err != nil {
				t.Fatal(err)
			}
			data, err := repoBlobs.Get(ctx, deltaHash)
			if err != nil {
				t.Fatal(err)
			}
			var delta struct {
				Files []json.RawMessage `json:"files"`
			}
			if err := json.Unmarshal(data, &delta); err != nil {
				t.Fatal(err)
			}
			if (len(delta.Files) > 0) != changePrimary {
				t.Fatalf("unexpected observed delta: %s", data)
			}
			if len(windowsIn(t, b.semDir)) != 0 {
				t.Fatal("secondary repository was observed")
			}
			root, _ := broker.UnresolvedMutationDir()
			paths, _ := filepath.Glob(filepath.Join(root, "*.json"))
			if len(paths) != 1 {
				t.Fatal(paths)
			}
		})
	}
}
