package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/turncapture"
)

func observationRecords(t *testing.T, home string) []turncapture.Record {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(home, "turn-observations", "*", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var records []turncapture.Record
	for _, path := range paths {
		if filepath.Base(path) == "session.json" {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var rec turncapture.Record
		if err := json.Unmarshal(b, &rec); err != nil {
			t.Fatal(err)
		}
		records = append(records, rec)
	}
	return records
}

func TestDispatchTurnCaptureCrossRepoByDefault(t *testing.T) {
	for _, provider := range []string{"codex", "claude-code", "gemini-cli", "copilot", "cursor", "kiro-cli"} {
		t.Run(provider, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("SEMANTICA_HOME", home)
			t.Setenv("SEMANTICA_TURN_CAPTURE", "")
			t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			a := newToolWindowWorld(t, home, "A")
			defer func() { _ = broker.Close(a.bh) }()
			b := newToolWindowWorld(t, home, "B")
			defer func() { _ = broker.Close(b.bh) }()
			prov := &fakeProvider{name: provider}
			prompt := &Event{Type: PromptSubmitted, SessionID: "session", ProviderSessionID: "native-session", Prompt: "test", Timestamp: time.Now().UnixMilli(), CWD: a.repoPath}
			if provider == "codex" || provider == "cursor" {
				prompt.ProviderTurnID = "provider-turn"
			}
			if err := Dispatch(context.Background(), prov, prompt, b.bh, nil); err != nil {
				t.Fatal(err)
			}
			recs := observationRecords(t, home)
			if len(recs) != 1 || recs[0].BaselineFinishedAt.IsZero() || len(recs[0].Repositories) != 2 {
				t.Fatalf("baseline not finished before Dispatch returned: %+v", recs)
			}
			for _, v := range recs[0].Repositories {
				if v.Gap != "" {
					t.Fatal(v.Gap)
				}
			}
			// SEMANTICA_TURN_CAPTURE does not control turn capture.
			t.Setenv("SEMANTICA_TURN_CAPTURE", "0")
			if err := os.WriteFile(filepath.Join(b.repoPath, "a.txt"), []byte("cross repo\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitIn(t, b.repoPath, "-c", "core.hooksPath="+home, "commit", "-am", "in turn")
			if err := os.WriteFile(filepath.Join(b.repoPath, "a.txt"), []byte("after commit\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			stop := &Event{Type: AgentCompleted, SessionID: "session", ProviderSessionID: prompt.ProviderSessionID, ProviderTurnID: prompt.ProviderTurnID, CWD: a.repoPath, Timestamp: time.Now().UnixMilli(), BackgroundTasks: json.RawMessage(`[]`)}
			if err := Dispatch(context.Background(), prov, stop, b.bh, nil); err != nil {
				t.Fatal(err)
			}
			rec := observationRecords(t, home)[0]
			if rec.End == nil || rec.End.FinishedAt.IsZero() || rec.End.TrackedCompletion != "unknown" {
				t.Fatalf("Stop is not proof: %+v", rec.End)
			}
			for i, subject := range rec.Repositories {
				want := "unchanged"
				if subject.Subject.Path == b.repoPath {
					want = "changed"
					if len(rec.End.Repositories[i].Changes) != 2 {
						t.Fatal("commit missing")
					}
				}
				if rec.End.Repositories[i].State != want {
					t.Fatalf("%s: %+v", subject.Subject.Path, rec.End.Repositories[i])
				}
			}
			// Duplicate Stop preserves the frozen End after CaptureState cleanup.
			if err := Dispatch(context.Background(), prov, stop, b.bh, nil); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(rec.End, observationRecords(t, home)[0].End) {
				t.Fatal("duplicate Stop changed end")
			}
			if len(windowsIn(t, b.semDir)) != 0 {
				t.Fatal("turn capture created tool windows")
			}
		})
	}
}

func TestTurnCaptureUnsupportedProviderDoesNotCreateStorage(t *testing.T) {
	for _, provider := range []string{"kiro-ide"} {
		t.Run(provider, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("SEMANTICA_HOME", home)
			p := &fakeProvider{name: provider}
			if err := Dispatch(context.Background(), p, &Event{Type: PromptSubmitted, SessionID: "s", Prompt: "test"}, nil, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(home, "turn-observations")); !os.IsNotExist(err) {
				t.Fatal("unsupported provider created storage")
			}
		})
	}
}

func TestCursorTurnCaptureRequiresPromptGeneration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	world := newToolWindowWorld(t, home, "repo")
	defer func() { _ = broker.Close(world.bh) }()
	prov := &fakeProvider{name: "cursor"}
	for _, event := range []*Event{
		{Type: PromptSubmitted, SessionID: "s", Prompt: "missing generation", CWD: world.repoPath},
		{Type: ToolStepStarted, SessionID: "s", ProviderTurnID: "unstarted", ToolUseID: "shell", ToolName: "Bash"},
		{Type: ToolStepCompleted, SessionID: "s", ProviderTurnID: "unstarted", ToolUseID: "shell", ToolName: "Bash"},
		{Type: AgentCompleted, SessionID: "s", ProviderTurnID: "unstarted"},
	} {
		if err := Dispatch(context.Background(), prov, event, world.bh, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "turn-observations")); !os.IsNotExist(err) {
		t.Fatal("Cursor created turn storage without a prompt generation")
	}
}

func TestKiroTurnCaptureSeparatesSessionsInSameWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	world := newToolWindowWorld(t, home, "repo")
	defer func() { _ = broker.Close(world.bh) }()
	ctx := context.Background()
	missing := &Event{SessionID: "workspace", TurnID: "turn", Prompt: "same prompt"}
	beginTurnCapture(ctx, "kiro-cli", missing, world.bh, 0)
	if len(observationRecords(t, home)) != 0 {
		t.Fatal("missing native session created an observation")
	}
	for _, session := range []string{"native-A", "native-B"} {
		event := *missing
		event.ProviderSessionID = session
		beginTurnCapture(ctx, "kiro-cli", &event, world.bh, 0)
	}
	if len(observationRecords(t, home)) != 2 {
		t.Fatal("native sessions shared a turn observation")
	}
	observeTurnCapture(ctx, "kiro-cli", &Event{Type: AgentCompleted, SessionID: "workspace"})
	for _, rec := range observationRecords(t, home) {
		if rec.End != nil {
			t.Fatal("missing native session closed another session")
		}
	}
	observeTurnCapture(ctx, "kiro-cli", &Event{Type: AgentCompleted, SessionID: "workspace", ProviderSessionID: "native-A"})
	for _, rec := range observationRecords(t, home) {
		if (rec.End != nil) != (rec.SessionID == "native-A") {
			t.Fatalf("completion crossed sessions: %+v", rec)
		}
	}
	observeTurnCapture(ctx, "kiro-cli", &Event{Type: AgentCompleted, SessionID: "workspace", ProviderSessionID: "native-B"})
	for _, rec := range observationRecords(t, home) {
		if rec.End == nil || rec.End.TrackedCompletion != "unknown" {
			t.Fatalf("completion missing or overstated: %+v", rec)
		}
	}
}

func TestCursorTurnEvidencePairsShellExecution(t *testing.T) {
	start, _ := turnEvidence("cursor", &Event{Type: ToolStepStarted, ToolName: "Bash", ToolUseID: "shell"})
	post, _ := turnEvidence("cursor", &Event{Type: ToolStepCompleted, ToolName: "Bash", ToolUseID: "shell"})
	stop, _ := turnEvidence("cursor", &Event{Type: AgentCompleted})
	if state, _ := turncapture.Completion(append(append(start, post...), stop...)); state != "settled" {
		t.Fatalf("paired shell completion: %s", state)
	}
	if state, _ := turncapture.Completion(append(start, stop...)); state != "unknown" {
		t.Fatalf("missing terminal evidence: %s", state)
	}
}

func TestDefaultTurnCaptureSnapshotFailureDoesNotFailDispatch(t *testing.T) {
	for _, provider := range []string{"codex", "claude-code", "gemini-cli", "copilot", "cursor", "kiro-cli"} {
		t.Run(provider, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("SEMANTICA_HOME", home)
			t.Setenv("SEMANTICA_TURN_CAPTURE", "")
			t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			world := newToolWindowWorld(t, home, "repo")
			defer func() { _ = broker.Close(world.bh) }()
			gitIn(t, world.repoPath, "update-index", "--assume-unchanged", "a.txt")
			p := &fakeProvider{name: provider}
			for _, kind := range []EventType{PromptSubmitted, AgentCompleted} {
				if err := Dispatch(context.Background(), p, &Event{Type: kind, SessionID: "s", ProviderSessionID: "native-s", ProviderTurnID: "turn", CWD: world.repoPath}, world.bh, nil); err != nil {
					t.Fatal(err)
				}
			}
			recs := observationRecords(t, home)
			if len(recs) != 1 || recs[0].End == nil || len(recs[0].End.Repositories) != 1 {
				t.Fatal("observation missing")
			}
			if got := recs[0].End.Repositories[0]; got.State != "unknown" || got.Reason != "unsupported_index_state" {
				t.Fatalf("snapshot failure: %+v", got)
			}
		})
	}
}

func TestUnpairedShellCompletionRemainsUnknown(t *testing.T) {
	for _, provider := range []string{"gemini-cli", "copilot", "cursor", "kiro-cli"} {
		t.Run(provider, func(t *testing.T) {
			evidence, stop := turnEvidence(provider, &Event{Type: ToolStepCompleted, ToolName: "Bash", ToolUseID: "step-1"})
			if stop || len(evidence) != 1 || evidence[0].Kind != "execution_terminal" {
				t.Fatalf("unexpected shell evidence: %+v, stop=%v", evidence, stop)
			}
			end, stop := turnEvidence(provider, &Event{Type: AgentCompleted})
			if !stop {
				t.Fatal("agent completion did not trigger end capture")
			}
			if state, _ := turncapture.Completion(append(evidence, end...)); state != "unknown" {
				t.Fatalf("unpaired terminal evidence produced %q", state)
			}
			if state, _ := turncapture.Completion(end); state != "unknown" {
				t.Fatalf("agent completion alone produced %q", state)
			}
		})
	}
}

func TestClaudeTurnEvidenceUsesDeliveredTaskSignals(t *testing.T) {
	start, _ := turnEvidence("claude-code", &Event{Type: ToolStepStarted, ToolUseID: "e", ToolName: "Bash"})
	post, _ := turnEvidence("claude-code", &Event{Type: ToolStepCompleted, ToolUseID: "e", ToolName: "Bash", ToolResponse: json.RawMessage(`{"backgroundTaskId":"task-1"}`)})
	stop, _ := turnEvidence("claude-code", &Event{Type: AgentCompleted, BackgroundTasks: json.RawMessage(`[{"id":"task-1","status":"running"}]`)})
	all := append(append(start, post...), stop...)
	if got, _ := turncapture.Completion(all); got != "unsettled" {
		t.Fatal(got)
	}
	empty, _ := turnEvidence("claude-code", &Event{Type: AgentCompleted, BackgroundTasks: json.RawMessage(`[]`)})
	if got, _ := turncapture.Completion(append(append(start, post...), empty...)); got != "unknown" {
		t.Fatal("empty inventory invented completion")
	}
	if len(post) != 2 || post[0].Kind != "execution_terminal" || post[1].TaskID != "task-1" {
		t.Fatal("managed identity lost")
	}
}

func TestClaudeBashTerminalSurvivesUnusableTaskMetadata(t *testing.T) {
	for _, response := range []struct {
		name string
		raw  json.RawMessage
	}{
		{"missing", nil},
		{"null", json.RawMessage(`null`)},
		{"scalar", json.RawMessage(`"output"`)},
		{"malformed", json.RawMessage(`{"stdout":`)},
		{"array", json.RawMessage(`[]`)},
		{"invalid_task_id", json.RawMessage(`{"backgroundTaskId":42}`)},
	} {
		for _, late := range []bool{false, true} {
			name := response.name + "/in_turn"
			if late {
				name = response.name + "/late"
			}
			t.Run(name, func(t *testing.T) {
				home := t.TempDir()
				r := turncapture.Recorder{Root: filepath.Join(home, "turn-observations")}
				begin := func(id string) {
					if err := r.Begin(context.Background(), "claude-code", "s", id, "", id, nil); err != nil {
						t.Fatal(err)
					}
				}
				observe := func(event *Event) []turncapture.Evidence {
					evidence, stop := turnEvidence("claude-code", event)
					if err := r.Observe(context.Background(), "claude-code", "s", "", evidence, stop); err != nil {
						t.Fatal(err)
					}
					return evidence
				}
				stop := func() { observe(&Event{Type: AgentCompleted, BackgroundTasks: json.RawMessage(`[]`)}) }
				loadTurn := func(id string) turncapture.Record {
					for _, rec := range observationRecords(t, home) {
						if rec.TurnID == id {
							return rec
						}
					}
					t.Fatalf("turn %s missing", id)
					return turncapture.Record{}
				}
				post := func() {
					evidence := observe(&Event{Type: ToolStepCompleted, ToolUseID: "old", ToolName: "Bash", ToolResponse: response.raw})
					if len(evidence) != 2 || evidence[0].Kind != "execution_terminal" || evidence[1].Kind != "gap" {
						t.Fatalf("terminal/gap: %+v", evidence)
					}
					if evidence[0].ExecutionID != "old" || evidence[1].ExecutionID != "old" || evidence[0].ReceivedAt != evidence[1].ReceivedAt {
						t.Fatal("metadata separated from invocation")
					}
				}
				begin("1")
				observe(&Event{Type: ToolStepStarted, ToolUseID: "old", ToolName: "Bash"})
				if !late {
					post()
				}
				stop()
				first := loadTurn("1")
				if first.End.TrackedCompletion != "unknown" {
					t.Fatal("uncertain task metadata settled Turn 1")
				}
				begin("2")
				observe(&Event{Type: ToolStepStarted, ToolUseID: "new", ToolName: "Bash"})
				if late {
					post()
				}
				observe(&Event{Type: ToolStepCompleted, ToolUseID: "new", ToolName: "Bash", ToolResponse: json.RawMessage(`{}`)})
				stop()
				second := loadTurn("2")
				if second.End.TrackedCompletion != "settled" {
					t.Fatalf("old metadata poisoned next turn: %+v", second.End)
				}
				for _, e := range second.Evidence {
					if e.ExecutionID == "old" {
						t.Fatal("late evidence attached to new turn")
					}
				}
				if !reflect.DeepEqual(first.End, loadTurn("1").End) {
					t.Fatal("late metadata rewrote frozen End")
				}
				paths, err := filepath.Glob(filepath.Join(r.Root, "*", "session.json"))
				if err != nil || len(paths) != 1 {
					t.Fatalf("session paths: %v %v", paths, err)
				}
				data, err := os.ReadFile(paths[0])
				if err != nil {
					t.Fatal(err)
				}
				var s struct {
					TerminalAt map[string]time.Time `json:"terminal_at"`
				}
				if err := json.Unmarshal(data, &s); err != nil {
					t.Fatal(err)
				}
				if s.TerminalAt["old"].IsZero() {
					t.Fatal("terminal timestamp missing")
				}
			})
		}
	}
}

func TestClaudeManagedTaskOutlivesTerminalBashScope(t *testing.T) {
	home := t.TempDir()
	r := turncapture.Recorder{Root: filepath.Join(home, "turn-observations")}
	for _, id := range []string{"1", "2"} {
		if err := r.Begin(context.Background(), "claude-code", "s", id, "", id, nil); err != nil {
			t.Fatal(err)
		}
		response := json.RawMessage(`{}`)
		if id == "1" {
			response = json.RawMessage(`{"backgroundTaskId":"task"}`)
		}
		for _, event := range []*Event{
			{Type: ToolStepStarted, ToolUseID: id, ToolName: "Bash"},
			{Type: ToolStepCompleted, ToolUseID: id, ToolName: "Bash", ToolResponse: response},
			{Type: AgentCompleted, BackgroundTasks: json.RawMessage(`[]`)},
		} {
			evidence, stop := turnEvidence("claude-code", event)
			if err := r.Observe(context.Background(), "claude-code", "s", "", evidence, stop); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, rec := range observationRecords(t, home) {
		if rec.End.TrackedCompletion != "unknown" || rec.End.CompletionReason != "managed_task_terminal_source_unavailable" {
			t.Fatalf("managed task lost after Bash terminal: %+v", rec.End)
		}
	}
}

func assertNoTurnSecret(t *testing.T, root, secret string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(secret)) {
			t.Errorf("Bash secret persisted in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTurnEvidenceNeverPersistsBashOutput(t *testing.T) {
	for _, provider := range []string{"codex", "claude-code", "gemini-cli", "copilot", "cursor", "kiro-cli"} {
		t.Run(provider, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("SEMANTICA_HOME", home)
			t.Setenv("SEMANTICA_TURN_CAPTURE", "")
			t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			world := newToolWindowWorld(t, home, "repo")
			defer func() { _ = broker.Close(world.bh) }()
			beginTurnCapture(context.Background(), provider, &Event{SessionID: "s", ProviderSessionID: "native-s", TurnID: "t", ProviderTurnID: "t", Prompt: "test"}, world.bh, 0)
			root := filepath.Join(home, "turn-observations")
			secret := "FAKE_BASH_SECRET_MUST_NOT_PERSIST"
			responses := []json.RawMessage{
				json.RawMessage(`{"stdout":"` + secret + `","stderr":"` + secret + `","backgroundTaskId":"task"}`),
				json.RawMessage(`"` + secret + `"`),
			}
			for i, response := range responses {
				id := string(rune('a' + i))
				observeTurnCapture(context.Background(), provider, &Event{Type: ToolStepStarted, SessionID: "s", ProviderSessionID: "native-s", ToolUseID: id, ToolName: "Bash"})
				observeTurnCapture(context.Background(), provider, &Event{Type: ToolStepCompleted, SessionID: "s", ProviderSessionID: "native-s", ToolUseID: id, ToolName: "Bash", ToolResponse: response})
				assertNoTurnSecret(t, root, secret)
			}
			observeTurnCapture(context.Background(), provider, &Event{Type: AgentCompleted, SessionID: "s", ProviderSessionID: "native-s", BackgroundTasks: json.RawMessage(`[{"id":"task","status":"running","command":"` + secret + `"}]`)})
			assertNoTurnSecret(t, root, secret)
			recs := observationRecords(t, home)
			if len(recs) != 1 || recs[0].End == nil || len(recs[0].Evidence) < 5 {
				t.Fatal("completion evidence missing")
			}
			if provider == "claude-code" && recs[0].Evidence[2].TaskID != "task" {
				t.Fatal("backgroundTaskId lost")
			}
		})
	}
}

func TestTurnCaptureOwnershipIgnoresRetiredGate(t *testing.T) {
	for _, provider := range []string{"codex", "claude-code", "gemini-cli", "copilot", "cursor", "kiro-cli"} {
		for _, secondEnabled := range []string{"0", "1"} {
			t.Run(provider+"/second_enabled_"+secondEnabled, func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("SEMANTICA_HOME", home)
				t.Setenv("SEMANTICA_TURN_CAPTURE", "")
				t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
				t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
				world := newToolWindowWorld(t, home, "repo")
				defer func() { _ = broker.Close(world.bh) }()
				prov := &fakeProvider{name: provider}
				runTurn := func(n int) {
					prompt := &Event{Type: PromptSubmitted, SessionID: "s", ProviderSessionID: "native-s", Prompt: string(rune('0' + n)), CWD: world.repoPath}
					if provider == "codex" || provider == "cursor" {
						prompt.ProviderTurnID = prompt.Prompt
					}
					if err := Dispatch(context.Background(), prov, prompt, world.bh, nil); err != nil {
						t.Fatal(err)
					}
					observeTurnCapture(context.Background(), provider, &Event{Type: ToolStepStarted, SessionID: "s", ProviderSessionID: "native-s", ProviderTurnID: prompt.ProviderTurnID, ToolUseID: prompt.Prompt, ToolName: "Bash"})
					if err := Dispatch(context.Background(), prov, &Event{Type: AgentCompleted, SessionID: "s", ProviderSessionID: "native-s", ProviderTurnID: prompt.ProviderTurnID, BackgroundTasks: json.RawMessage(`[]`)}, world.bh, nil); err != nil {
						t.Fatal(err)
					}
				}
				runTurn(1)
				first := observationRecords(t, home)[0]
				t.Setenv("SEMANTICA_TURN_CAPTURE", secondEnabled)
				runTurn(2)
				recs := observationRecords(t, home)
				want := 2
				if len(recs) != want {
					t.Fatalf("records %d want %d", len(recs), want)
				}
				for _, rec := range recs {
					if rec.TurnID == first.TurnID && !reflect.DeepEqual(rec, first) {
						t.Fatal("Turn 2 modified Turn 1")
					}
					if rec.TurnID != first.TurnID && (rec.End == nil || rec.Evidence[0].ExecutionID != "2") {
						t.Fatal("Turn 2 evidence missing")
					}
				}
				// Late evidence stays with its execution's original turn.
				observeTurnCapture(context.Background(), provider, &Event{Type: ToolStepCompleted, SessionID: "s", ProviderSessionID: "native-s", ToolUseID: "1", ToolName: "Bash", ToolResponse: json.RawMessage(`{}`)})
				for _, rec := range observationRecords(t, home) {
					if rec.TurnID == first.TurnID && (len(rec.Evidence) != len(first.Evidence)+1 || !reflect.DeepEqual(rec.End, first.End)) {
						t.Fatal("late evidence changed frozen end or lost owner")
					}
				}
			})
		}
	}
}
