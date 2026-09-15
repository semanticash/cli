package hooks

import (
	"context"
	"encoding/json"
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
	if post[0].TaskID != "task-1" || len(post[0].Raw) == 0 {
		t.Fatal("managed identity/evidence lost")
	}
}
