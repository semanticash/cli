//go:build turnexperiment

package toolsnap

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func turnRepos(t *testing.T) []turnSubject {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return []turnSubject{{"A", testRepo(t)}, {"B", testRepo(t)}, {"C", testRepo(t)}}
}

func beginTestTurn(t *testing.T, subjects []turnSubject, concurrency int) *observedTurn {
	t.Helper()
	turn, err := beginObservedTurn(context.Background(), "session", "turn", t.TempDir(), subjects, concurrency)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range turn.Samples {
		if s.Reason != "" {
			t.Fatalf("baseline %s: %s", s.RepositoryID, s.Reason)
		}
		if s.before.TreeHash == "" {
			t.Fatalf("baseline %s did not finish", s.RepositoryID)
		}
	}
	return turn
}

func checkTurn(t *testing.T, turn *observedTurn, states []string, files map[int][]string) {
	t.Helper()
	for i, s := range turn.Samples {
		if s.State != states[i] {
			t.Errorf("%s: got %s (%s), want %s", s.RepositoryID, s.State, s.Reason, states[i])
		}
		if !reflect.DeepEqual(s.Files, files[i]) {
			t.Errorf("%s: files %v, want %v", s.RepositoryID, s.Files, files[i])
		}
		if s.State == "unknown" && s.Reason == "" {
			t.Errorf("%s: unknown without reason", s.RepositoryID)
		}
	}
}

func TestTurnExperimentControlled(t *testing.T) {
	for _, concurrency := range []int{1, 3} {
		t.Run(fmt.Sprintf("concurrency_%d", concurrency), func(t *testing.T) {
			t.Run("NetChanges", func(t *testing.T) { testTurnNetChanges(t, concurrency) })
			t.Run("Unknown", func(t *testing.T) { testTurnUnknown(t, concurrency) })
			t.Run("FrozenSubjects", func(t *testing.T) { testTurnFrozenSubjects(t, concurrency) })
			t.Run("OverlappingTurns", func(t *testing.T) { testTurnOverlappingTurns(t, concurrency) })
			t.Run("Background", func(t *testing.T) { testTurnBackground(t, concurrency) })
			t.Run("Commits", func(t *testing.T) { testTurnCommits(t, concurrency) })
		})
	}
}

func testTurnNetChanges(t *testing.T, concurrency int) {
	cases := []struct {
		name   string
		before func(*testing.T, []turnSubject)
		work   func(*testing.T, []turnSubject)
		files  map[int][]string
	}{
		{"clean_no_change", nil, nil, nil},
		{"A_only", nil, func(t *testing.T, r []turnSubject) { writeFile(t, r[0].Path, "a.txt", "new\n") }, map[int][]string{0: {"a.txt"}}},
		{"B_only", nil, func(t *testing.T, r []turnSubject) { writeFile(t, r[1].Path, "a.txt", "new\n") }, map[int][]string{1: {"a.txt"}}},
		{"A_and_B", nil, func(t *testing.T, r []turnSubject) {
			writeFile(t, r[0].Path, "a.txt", "new\n")
			writeFile(t, r[1].Path, "a.txt", "new\n")
		}, map[int][]string{0: {"a.txt"}, 1: {"a.txt"}}},
		{"dirty_further_changed", func(t *testing.T, r []turnSubject) { writeFile(t, r[1].Path, "a.txt", "dirty\n") }, func(t *testing.T, r []turnSubject) {
			writeFile(t, r[1].Path, "a.txt", "agent\n")
		}, map[int][]string{1: {"a.txt"}}},
		{"dirty_unchanged", func(t *testing.T, r []turnSubject) { writeFile(t, r[1].Path, "a.txt", "dirty\n") }, nil, nil},
		{"restored_to_baseline", nil, func(t *testing.T, r []turnSubject) {
			writeFile(t, r[1].Path, "a.txt", "intermediate\n")
			writeFile(t, r[1].Path, "a.txt", "alpha\n")
		}, nil},
		{"independent_human_write_C", nil, func(t *testing.T, r []turnSubject) { writeFile(t, r[2].Path, "a.txt", "human\n") }, map[int][]string{2: {"a.txt"}}},
		{"create_delete_binary", nil, func(t *testing.T, r []turnSubject) {
			writeFile(t, r[1].Path, "new.bin", "\x00binary")
			if err := os.Remove(filepath.Join(r[1].Path, "a.txt")); err != nil {
				t.Fatal(err)
			}
		}, map[int][]string{1: {"a.txt", "new.bin"}}},
		{"opaque_shell_A_to_B", nil, func(t *testing.T, r []turnSubject) {
			if runtime.GOOS == "windows" {
				t.Skip("POSIX shell fixture")
			}
			script := filepath.Join(t.TempDir(), "generate.sh")
			if err := os.WriteFile(script, []byte("printf 'generated\\n' > \"$1/a.txt\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", script, r[1].Path)
			cmd.Dir = r[0].Path
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("script: %v: %s", err, out)
			}
		}, map[int][]string{1: {"a.txt"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := turnRepos(t)
			if c.before != nil {
				c.before(t, r)
			}
			turn := beginTestTurn(t, r, concurrency)
			if c.work != nil {
				c.work(t, r)
			}
			finishObservedTurn(context.Background(), turn, "session", "turn", true)
			states := []string{"unchanged", "unchanged", "unchanged"}
			for i := range c.files {
				states[i] = "changed"
			}
			checkTurn(t, turn, states, c.files)
		})
	}
}

func testTurnUnknown(t *testing.T, concurrency int) {
	for _, name := range []string{"missing_worktree", "replaced_worktree", "unsupported_index", "missing_completion", "end_snapshot_failure", "head_rewritten"} {
		t.Run(name, func(t *testing.T) {
			r := turnRepos(t)
			turn := beginTestTurn(t, r, concurrency)
			ctx := context.Background()
			session := "session"
			states := []string{"unchanged", "unknown", "unchanged"}
			switch name {
			case "missing_worktree", "replaced_worktree":
				if err := os.Rename(r[1].Path, r[1].Path+"-old"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(r[1].Path + "-old") })
				if name == "replaced_worktree" {
					replacement := testRepo(t)
					if err := os.Rename(replacement, r[1].Path); err != nil {
						t.Fatal(err)
					}
				}
			case "unsupported_index":
				run(t, r[1].Path, "git", "update-index", "--assume-unchanged", "a.txt")
				writeFile(t, r[1].Path, "a.txt", "hidden\n")
			case "missing_completion":
				session = ""
				states = []string{"unknown", "unknown", "unknown"}
			case "end_snapshot_failure":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				states = []string{"unknown", "unknown", "unknown"}
			case "head_rewritten":
				run(t, r[1].Path, "git", "commit", "--amend", "-qm", "rewrite")
			}
			finishObservedTurn(ctx, turn, session, "turn", true)
			checkTurn(t, turn, states, nil)
		})
	}
}

func testTurnFrozenSubjects(t *testing.T, concurrency int) {
	r := turnRepos(t)
	run(t, r[1].Path, "git", "update-index", "--skip-worktree", "a.txt")
	turn, err := beginObservedTurn(context.Background(), "session", "turn", t.TempDir(), r, concurrency)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Samples[1].Reason != "unsupported_index_state" {
		t.Fatal("failed baseline not recorded")
	}
	r[0] = r[2]
	run(t, r[1].Path, "git", "update-index", "--no-skip-worktree", "a.txt")
	finishObservedTurn(context.Background(), turn, "session", "turn", true)
	checkTurn(t, turn, []string{"unchanged", "unknown", "unchanged"}, nil)
	if turn.Samples[0].RepositoryID != "A" {
		t.Fatal("candidate set was not frozen")
	}
}

func testTurnOverlappingTurns(t *testing.T, concurrency int) {
	r := turnRepos(t)
	a := beginTestTurn(t, r, concurrency)
	b, err := beginObservedTurn(context.Background(), "other-session", "other-turn", t.TempDir(), r, concurrency)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, r[1].Path, "a.txt", "first\n")
	writeFile(t, r[1].Path, "a.txt", "second\n")
	finishObservedTurn(context.Background(), a, "session", "turn", true)
	finishObservedTurn(context.Background(), b, "other-session", "other-turn", true)
	for _, turn := range []*observedTurn{a, b} {
		checkTurn(t, turn, []string{"unchanged", "changed", "unchanged"}, map[int][]string{1: {"a.txt"}})
	}
}

func testTurnBackground(t *testing.T, concurrency int) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	r := turnRepos(t)
	turn := beginTestTurn(t, r, concurrency)
	cmd := exec.Command("sh", "-c", "printf 'ready\\n'; read token; printf 'late\\n' > \"$1/a.txt\"", "sh", r[1].Path)
	cmd.Dir = r[0].Path
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close(); _ = cmd.Wait() }()
	ready, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(ready) != "ready" {
		t.Fatalf("background startup: %q %v", ready, err)
	}
	finishObservedTurn(context.Background(), turn, "session", "turn", false)
	checkTurn(t, turn, []string{"unknown", "unknown", "unknown"}, nil)
}

func testTurnCommits(t *testing.T, concurrency int) {
	for _, name := range []string{"commit_clean", "commit_then_dirty", "two_commits", "commit_then_revert", "empty_commit", "commit_preserves_unrelated_dirt"} {
		t.Run(name, func(t *testing.T) {
			r := turnRepos(t)
			if name == "commit_preserves_unrelated_dirt" {
				writeFile(t, r[1].Path, "untouched.txt", "pre-existing untracked\n")
			}
			turn := beginTestTurn(t, r, concurrency)
			b := r[1].Path
			commit := func() string {
				run(t, b, "git", "add", "a.txt", "sub/b.txt")
				run(t, b, "git", "commit", "--allow-empty", "-qm", "during turn")
				return strings.TrimSpace(run(t, b, "git", "rev-parse", "HEAD"))
			}
			if name != "empty_commit" {
				writeFile(t, b, "a.txt", "committed\n")
			}
			commits := []string{commit()}
			files := map[int][]string{1: {"a.txt"}}
			states := []string{"unchanged", "changed", "unchanged"}
			switch name {
			case "commit_then_dirty":
				writeFile(t, b, "sub/b.txt", "uncommitted\n")
				files[1] = []string{"a.txt", "sub/b.txt"}
			case "two_commits":
				writeFile(t, b, "sub/b.txt", "second commit\n")
				commits = append(commits, commit())
				files[1] = []string{"a.txt", "sub/b.txt"}
			case "commit_then_revert":
				writeFile(t, b, "a.txt", "alpha\n")
				commits = append(commits, commit())
			case "empty_commit":
				states[1], files = "unchanged", nil
			}
			finishObservedTurn(context.Background(), turn, "session", "turn", true)
			checkTurn(t, turn, states, files)
			boundaries := turn.Samples[1].Boundaries
			if len(boundaries) != len(commits)+1 {
				t.Fatalf("boundaries = %v, want commits plus worktree", boundaries)
			}
			for i, hash := range commits {
				if boundaries[i].Commit != hash {
					t.Errorf("boundary %d lost commit %s", i, hash)
				}
			}
			if name == "commit_then_dirty" {
				if !reflect.DeepEqual(boundaries[0].Files, []string{"a.txt"}) || !reflect.DeepEqual(boundaries[1].Files, []string{"a.txt", "sub/b.txt"}) {
					t.Fatalf("commit/worktree changes conflated: %v", boundaries)
				}
			}
			if name == "commit_then_revert" {
				if !reflect.DeepEqual(boundaries[0].Files, []string{"a.txt"}) || !reflect.DeepEqual(boundaries[1].Files, []string{"a.txt"}) {
					t.Fatalf("intermediate change lost: %v", boundaries)
				}
			}
		})
	}
}

func TestTurnExperimentParallelWaits(t *testing.T) {
	entered := make(chan int, 3)
	release := make(chan struct{})
	done := make(chan struct{})
	defer close(release)
	go func() {
		observeTurnRepos(3, 3, func(i int) { entered <- i; <-release })
		close(done)
	}()
	seen := make(map[int]bool)
	for range 3 {
		select {
		case i := <-entered:
			if seen[i] {
				t.Fatalf("duplicate repository %d", i)
			}
			seen[i] = true
		case <-time.After(5 * time.Second):
			t.Fatal("repository observations did not run concurrently")
		}
	}
	select {
	case <-done:
		t.Fatal("returned before observations finished")
	default:
	}
	for range 3 {
		release <- struct{}{}
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("completed observations did not settle")
	}
}
