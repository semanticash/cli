package turncapture

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=" + filepath.Join(dir, "no-hooks"), "-c", "core.fsmonitor=false", "-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s: %v", args, out, err)
	}
}

func write(t *testing.T, repo, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, "value.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T) (Recorder, []Subject) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	r := Recorder{Root: t.TempDir()}
	var subjects []Subject
	for _, id := range []string{"A", "B", "C"} {
		repo := t.TempDir()
		runGit(t, repo, "init")
		write(t, repo, "before\n")
		runGit(t, repo, "add", "value.txt")
		runGit(t, repo, "commit", "-m", "initial")
		subjects = append(subjects, Subject{RepositoryID: id, RegistrationID: id, Path: repo})
	}
	return r, subjects
}

func start(t *testing.T, r Recorder, subjects []Subject, key string) {
	t.Helper()
	if err := r.Begin(context.Background(), "codex", "session", "se-"+key, key, "provider:"+key, subjects); err != nil {
		t.Fatal(err)
	}
}

func send(t *testing.T, r Recorder, key, kind, id string, stop bool) Evidence {
	t.Helper()
	e := Evidence{Kind: kind, ExecutionID: id, ReceivedAt: time.Now().UTC()}
	if err := r.Observe(context.Background(), "codex", "session", key, []Evidence{e}, stop); err != nil {
		t.Fatal(err)
	}
	return e
}

func load(t *testing.T, r Recorder, key string) Record {
	t.Helper()
	var rec Record
	if err := read(filepath.Join(r.dir("codex", "session"), identity("provider:"+key)+".json"), &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestTurnCaptureReloadCommitsAndUnknownCompletion(t *testing.T) {
	for _, commits := range []int{0, 1, 2} {
		t.Run(string(rune('0'+commits)), func(t *testing.T) {
			r, subjects := fixture(t)
			start(t, r, subjects, "turn")
			send(t, r, "turn", "execution_started", "exec", false)
			for n := 0; n < commits; n++ {
				write(t, subjects[1].Path, string(rune('a'+n))+"\n")
				runGit(t, subjects[1].Path, "commit", "-am", "inside turn")
			}
			if commits < 2 {
				write(t, subjects[1].Path, "dirty after commit\n")
			}
			// A new recorder has no in-memory baseline or provider state.
			r = Recorder{Root: r.Root}
			send(t, r, "turn", "stop", "", true)
			rec := load(t, r, "turn")
			if rec.End.TrackedCompletion != "unknown" {
				t.Fatalf("missing terminal: %+v", rec.End)
			}
			for i, want := range []string{"unchanged", "changed", "unchanged"} {
				if got := rec.End.Repositories[i]; got.State != want {
					t.Fatalf("repo %d: %+v", i, got)
				}
			}
			if got := len(rec.End.Repositories[1].Changes); got != commits+1 {
				t.Fatalf("boundaries %d, want %d", got, commits+1)
			}
		})
	}
}

func TestTurnEndImmutableWithLateTerminalAndDuplicateStop(t *testing.T) {
	r, subjects := fixture(t)
	start(t, r, subjects, "turn")
	send(t, r, "turn", "execution_started", "exec", false)
	send(t, r, "turn", "stop", "", true)
	frozen := load(t, r, "turn").End
	write(t, subjects[1].Path, "late change\n")
	send(t, r, "turn", "execution_terminal", "exec", false)
	send(t, r, "turn", "stop", "", true)
	got := load(t, r, "turn")
	if !reflect.DeepEqual(frozen, got.End) {
		t.Fatal("late evidence rewrote the end")
	}
	if len(got.Evidence) <= len(got.End.Evidence) {
		t.Fatal("late evidence not retained separately")
	}
}

func TestDuplicateStartPreservesFrozenSubjectsAndBaseline(t *testing.T) {
	r, subjects := fixture(t)
	start(t, r, subjects[:2], "turn")
	frozen := load(t, r, "turn")
	write(t, subjects[1].Path, "later\n")
	start(t, r, subjects, "turn")
	if got := load(t, r, "turn"); !reflect.DeepEqual(got, frozen) {
		t.Fatal("duplicate start changed baseline")
	}
	send(t, r, "turn", "stop", "", true)
	if got := load(t, r, "turn").End.Repositories[1].State; got != "changed" {
		t.Fatal(got)
	}
}

func TestInterruptedStartAndEndAreNotResnapshotted(t *testing.T) {
	r, subjects := fixture(t)
	start(t, r, subjects, "turn")
	path := filepath.Join(r.dir("codex", "session"), identity("provider:turn")+".json")
	rec := load(t, r, "turn")
	rec.BaselineFinishedAt = time.Time{}
	if err := save(path, rec); err != nil {
		t.Fatal(err)
	}
	start(t, r, subjects, "turn")
	send(t, r, "turn", "stop", "", true)
	got := load(t, r, "turn")
	for _, repo := range got.End.Repositories {
		if repo.State != "unknown" {
			t.Fatal(repo)
		}
	}
	got.End.FinishedAt = time.Time{}
	if err := save(path, got); err != nil {
		t.Fatal(err)
	}
	send(t, r, "turn", "stop", "", true)
	if !reflect.DeepEqual(got.End, load(t, r, "turn").End) {
		t.Fatal("interrupted end was recaptured")
	}
}

func TestTerminalReceivedAfterStopCannotSettleBoundary(t *testing.T) {
	r, subjects := fixture(t)
	start(t, r, subjects, "turn")
	send(t, r, "turn", "execution_started", "exec", false)
	stop := Evidence{Kind: "stop", ReceivedAt: time.Now().UTC()}
	send(t, r, "turn", "execution_terminal", "exec", false)
	if err := r.Observe(context.Background(), "codex", "session", "turn", []Evidence{stop}, true); err != nil {
		t.Fatal(err)
	}
	if got := load(t, r, "turn").End.TrackedCompletion; got != "unknown" {
		t.Fatal(got)
	}
}

func TestOutstandingWorkAcrossTurnsAndLateEvidenceOwnership(t *testing.T) {
	r, subjects := fixture(t)
	start(t, r, subjects, "old")
	send(t, r, "old", "execution_started", "old-exec", false)
	send(t, r, "old", "stop", "", true)
	start(t, r, subjects, "new")
	send(t, r, "new", "execution_started", "new-exec", false)
	send(t, r, "new", "execution_terminal", "new-exec", false)
	send(t, r, "new", "stop", "", true)
	if load(t, r, "new").End.TrackedCompletion != "unknown" {
		t.Fatal("old work ignored")
	}
	send(t, r, "", "execution_terminal", "old-exec", false)
	old := load(t, r, "old")
	if old.Evidence[len(old.Evidence)-1].ExecutionID != "old-exec" {
		t.Fatal("late terminal lost owner")
	}
}

func TestCompletionContract(t *testing.T) {
	for _, tt := range []struct {
		name     string
		evidence []Evidence
		want     string
	}{
		{"stop alone", []Evidence{{Kind: "stop"}}, "unknown"},
		{"foreground", []Evidence{{Kind: "execution_started", ExecutionID: "e"}, {Kind: "execution_terminal", ExecutionID: "e"}}, "settled"},
		{"missing pre", []Evidence{{Kind: "execution_terminal", ExecutionID: "e"}}, "unknown"},
		{"running", []Evidence{{Kind: "inventory_running", TaskID: "t"}}, "unsettled"},
		{"empty list does not finish task", []Evidence{{Kind: "execution_started", ExecutionID: "e"}, {Kind: "managed_task", ExecutionID: "e", TaskID: "t"}, {Kind: "stop", Raw: json.RawMessage(`[]`)}}, "unknown"},
		{"missing inventory", []Evidence{{Kind: "execution_started", ExecutionID: "e"}, {Kind: "execution_terminal", ExecutionID: "e"}, {Kind: "gap"}}, "unknown"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := Completion(tt.evidence)
			if got != tt.want {
				t.Fatalf("got %s want %s", got, tt.want)
			}
		})
	}
}

func TestTurnCaptureUnknownAndDirtyState(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		r, subjects := fixture(t)
		write(t, subjects[0].Path, "already dirty\n")
		write(t, subjects[1].Path, "already dirty\n")
		start(t, r, subjects, "turn")
		write(t, subjects[1].Path, "dirty again\n")
		if unavailable {
			if err := os.Rename(subjects[2].Path, subjects[2].Path+"-moved"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(subjects[2].Path + "-moved") })
		}
		send(t, r, "turn", "stop", "", true)
		end := load(t, r, "turn").End
		if end.Repositories[0].State != "unchanged" || end.Repositories[1].State != "changed" {
			t.Fatalf("dirty comparison: %+v", end.Repositories)
		}
		if unavailable && end.Repositories[2].State != "unknown" {
			t.Fatal("missing repo reported unchanged")
		}
	}
}

func TestConcurrentEvidenceDoesNotLoseExecutionScopes(t *testing.T) {
	r, subjects := fixture(t)
	start(t, r, subjects, "turn")
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b", "c"} {
		wg.Go(func() {
			for _, kind := range []string{"execution_started", "execution_terminal"} {
				if err := r.Observe(context.Background(), "codex", "session", "turn", []Evidence{{Kind: kind, ExecutionID: id, ReceivedAt: time.Now().UTC()}}, false); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	send(t, r, "turn", "stop", "", true)
	end := load(t, r, "turn").End
	if end.TrackedCompletion != "settled" || len(end.Evidence) != 7 {
		t.Fatalf("lost delivery: %+v", end)
	}
}

func TestInvalidRecordNotOverwrittenAndMissingCursorRecovered(t *testing.T) {
	r, subjects := fixture(t)
	start(t, r, subjects, "turn")
	dir := r.dir("codex", "session")
	if err := os.Remove(filepath.Join(dir, "session.json")); err != nil {
		t.Fatal(err)
	}
	start(t, r, subjects, "turn")
	send(t, r, "turn", "stop", "", true)
	if load(t, r, "turn").End == nil {
		t.Fatal("lost cursor not recovered")
	}
	path := filepath.Join(dir, identity("provider:turn")+".json")
	if err := os.WriteFile(path, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Begin(context.Background(), "codex", "session", "t", "turn", "provider:turn", subjects); err == nil {
		t.Fatal("invalid existing record accepted")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "broken" {
		t.Fatal("invalid record overwritten")
	}
}

func TestMissingEarlierEvidenceDoesNotPreventEndSnapshot(t *testing.T) {
	r, subjects := fixture(t)
	start(t, r, subjects, "old")
	send(t, r, "old", "execution_started", "old-exec", false)
	start(t, r, subjects, "new")
	if err := os.Remove(filepath.Join(r.dir("codex", "session"), identity("provider:old")+".json")); err != nil {
		t.Fatal(err)
	}
	write(t, subjects[1].Path, "new state\n")
	send(t, r, "new", "stop", "", true)
	end := load(t, r, "new").End
	if end == nil || end.TrackedCompletion != "unknown" || end.Repositories[1].State != "changed" {
		t.Fatalf("completion gap blocked observation: %+v", end)
	}
}
