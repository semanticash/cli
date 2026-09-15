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

func TestDispatchTurnCaptureCrossRepoAndGateFlip(t *testing.T) {
	for _, provider := range []string{"codex", "claude-code"} {
		t.Run(provider, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("SEMANTICA_HOME", home)
			t.Setenv("SEMANTICA_TURN_CAPTURE", "1")
			t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			a := newToolWindowWorld(t, home, "A")
			defer func() { _ = broker.Close(a.bh) }()
			b := newToolWindowWorld(t, home, "B")
			defer func() { _ = broker.Close(b.bh) }()
			prov := &fakeProvider{name: provider}
			prompt := &Event{Type: PromptSubmitted, SessionID: "session", Prompt: "test", Timestamp: time.Now().UnixMilli(), CWD: a.repoPath}
			if provider == "codex" {
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
			// Disabling capture must still allow this turn to finish.
			t.Setenv("SEMANTICA_TURN_CAPTURE", "0")
			if err := os.WriteFile(filepath.Join(b.repoPath, "a.txt"), []byte("cross repo\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitIn(t, b.repoPath, "-c", "core.hooksPath="+home, "commit", "-am", "in turn")
			if err := os.WriteFile(filepath.Join(b.repoPath, "a.txt"), []byte("after commit\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			stop := &Event{Type: AgentCompleted, SessionID: "session", ProviderTurnID: prompt.ProviderTurnID, CWD: a.repoPath, Timestamp: time.Now().UnixMilli(), BackgroundTasks: json.RawMessage(`[]`)}
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
			// Duplicate Stop must preserve the end after CaptureState is deleted.
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

func TestTurnCaptureDisabledDoesNotCreateStorage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	t.Setenv("SEMANTICA_TURN_CAPTURE", "0")
	p := &fakeProvider{name: "codex"}
	if err := Dispatch(context.Background(), p, &Event{Type: PromptSubmitted, SessionID: "s", Prompt: "test"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "turn-observations")); !os.IsNotExist(err) {
		t.Fatal("disabled capture created storage")
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
	if post[0].TaskID != "task-1" {
		t.Fatal("managed identity lost")
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
	for _, provider := range []string{"codex", "claude-code"} {
		t.Run(provider, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("SEMANTICA_HOME", home)
			t.Setenv("SEMANTICA_TURN_CAPTURE", "1")
			t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			world := newToolWindowWorld(t, home, "repo")
			defer func() { _ = broker.Close(world.bh) }()
			beginTurnCapture(context.Background(), provider, &Event{SessionID: "s", TurnID: "t", Prompt: "test"}, world.bh, 0)
			root := filepath.Join(home, "turn-observations")
			secret := "FAKE_BASH_SECRET_MUST_NOT_PERSIST"
			responses := []json.RawMessage{
				json.RawMessage(`{"stdout":"` + secret + `","stderr":"` + secret + `","backgroundTaskId":"task"}`),
				json.RawMessage(`"` + secret + `"`),
			}
			for i, response := range responses {
				id := string(rune('a' + i))
				observeTurnCapture(context.Background(), provider, &Event{Type: ToolStepStarted, SessionID: "s", ToolUseID: id, ToolName: "Bash"})
				observeTurnCapture(context.Background(), provider, &Event{Type: ToolStepCompleted, SessionID: "s", ToolUseID: id, ToolName: "Bash", ToolResponse: response})
				assertNoTurnSecret(t, root, secret)
			}
			observeTurnCapture(context.Background(), provider, &Event{Type: AgentCompleted, SessionID: "s", BackgroundTasks: json.RawMessage(`[{"id":"task","status":"running","command":"` + secret + `"}]`)})
			assertNoTurnSecret(t, root, secret)
			recs := observationRecords(t, home)
			if len(recs) != 1 || recs[0].End == nil || len(recs[0].Evidence) < 5 {
				t.Fatal("completion evidence missing")
			}
			if provider == "claude-code" && recs[0].Evidence[1].TaskID != "task" {
				t.Fatal("backgroundTaskId lost")
			}
		})
	}
}

func TestTurnCaptureOwnershipAcrossConfigurationChanges(t *testing.T) {
	for _, provider := range []string{"codex", "claude-code"} {
		for _, secondEnabled := range []string{"0", "1"} {
			t.Run(provider+"/second_enabled_"+secondEnabled, func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("SEMANTICA_HOME", home)
				t.Setenv("SEMANTICA_TURN_CAPTURE", "1")
				t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
				t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
				world := newToolWindowWorld(t, home, "repo")
				defer func() { _ = broker.Close(world.bh) }()
				prov := &fakeProvider{name: provider}
				runTurn := func(n int) {
					prompt := &Event{Type: PromptSubmitted, SessionID: "s", Prompt: string(rune('0' + n)), CWD: world.repoPath}
					if provider == "codex" {
						prompt.ProviderTurnID = prompt.Prompt
					}
					if err := Dispatch(context.Background(), prov, prompt, world.bh, nil); err != nil {
						t.Fatal(err)
					}
					observeTurnCapture(context.Background(), provider, &Event{Type: ToolStepStarted, SessionID: "s", ProviderTurnID: prompt.ProviderTurnID, ToolUseID: prompt.Prompt, ToolName: "Bash"})
					if err := Dispatch(context.Background(), prov, &Event{Type: AgentCompleted, SessionID: "s", ProviderTurnID: prompt.ProviderTurnID, BackgroundTasks: json.RawMessage(`[]`)}, world.bh, nil); err != nil {
						t.Fatal(err)
					}
				}
				runTurn(1)
				first := observationRecords(t, home)[0]
				t.Setenv("SEMANTICA_TURN_CAPTURE", secondEnabled)
				runTurn(2)
				recs := observationRecords(t, home)
				want := 1
				if secondEnabled == "1" {
					want = 2
				}
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
				// Execution ownership must preserve late evidence for Turn 1.
				observeTurnCapture(context.Background(), provider, &Event{Type: ToolStepCompleted, SessionID: "s", ToolUseID: "1", ToolName: "Bash", ToolResponse: json.RawMessage(`{}`)})
				for _, rec := range observationRecords(t, home) {
					if rec.TurnID == first.TurnID && (len(rec.Evidence) != len(first.Evidence)+1 || !reflect.DeepEqual(rec.End, first.End)) {
						t.Fatal("late evidence changed frozen end or lost owner")
					}
				}
			})
		}
	}
}
