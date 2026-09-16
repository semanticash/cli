//go:build turnexperiment

package toolsnap

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const turnWorkloadDir = "semantica_turn_benchmark"

// This opt-in benchmark writes only dedicated fixtures in sample and sample2.
// Detached commits leave their original branches untouched. Cleanup restores
// their original checkouts without resetting unrelated files or index entries.
func TestTurnExperimentChangedWorkload(t *testing.T) {
	if os.Getenv("SEMANTICA_TURN_EXPERIMENT_CHANGED") != "1" {
		t.Skip("set SEMANTICA_TURN_EXPERIMENT_CHANGED=1 to modify the two sample repositories")
	}
	raw, err := os.ReadFile(os.Getenv("SEMANTICA_TURN_EXPERIMENT_REPOS"))
	if err != nil {
		t.Fatal(err)
	}
	var subjects []turnSubject
	if err := json.Unmarshal(raw, &subjects); err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 7 {
		t.Fatal("expected the frozen seven-repository input")
	}
	var targets []string
	for _, name := range []string{"sample", "sample2"} {
		for _, s := range subjects {
			if filepath.Base(s.Path) == name {
				targets = append(targets, s.Path)
			}
		}
	}
	if len(targets) != 2 {
		t.Fatal("sample and sample2 must each appear once")
	}
	for _, path := range targets {
		prepareTurnWorkload(t, path)
	}

	type measurement struct {
		Contended bool          `json:"contended"`
		Iteration int           `json:"iteration"`
		Warmup    bool          `json:"warmup"`
		Start     time.Duration `json:"start_ns"`
		Work      time.Duration `json:"work_ns"`
		End       time.Duration `json:"end_ns"`
		Samples   []turnSample  `json:"samples"`
	}
	var report struct {
		LogicalCPUs  int           `json:"logical_cpus"`
		Measurements []measurement `json:"measurements"`
		Pressure     pressureStats `json:"pressure"`
	}
	report.LogicalCPUs = runtime.NumCPU()
	for _, contended := range []bool{false, true} {
		storage := t.TempDir()
		var pressure *turnPressure
		if contended {
			pressure = startTurnPressure(t, t.TempDir())
			defer pressure.stop()
		}
		for iteration := 0; iteration < 31; iteration++ {
			// Reset only fixture contents, committing the baseline outside timing.
			for _, path := range targets {
				resetTurnWorkload(t, path)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			turn, err := beginObservedTurn(ctx, "changed-benchmark", fmt.Sprint(iteration), storage, subjects, 7)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			workStart := time.Now()
			for _, path := range targets {
				for file := 0; file < 16; file++ {
					writeTurnWorkloadFile(t, path, fmt.Sprintf("file%02d.txt", file), 1)
				}
			}
			writeTurnWorkloadFile(t, targets[0], "untracked.txt", 1)
			workloadGit(t, targets[1], "add", "--", turnWorkloadDir)
			workloadGit(t, targets[1], "commit", "--only", "-qm", "turn experiment change", "--", turnWorkloadDir)
			writeTurnWorkloadFile(t, targets[1], "file16.txt", 2)
			workTime := time.Since(workStart)
			finishObservedTurn(ctx, turn, "changed-benchmark", fmt.Sprint(iteration), true)
			cancel()
			report.Measurements = append(report.Measurements, measurement{contended, iteration, iteration == 0, turn.StartTime, workTime, turn.EndTime, turn.Samples})
			checkChangedTurn(t, turn, targets)
			t.Logf("contended=%v iteration=%d warmup=%v start=%s work=%s end=%s", contended, iteration, iteration == 0, turn.StartTime, workTime, turn.EndTime)
		}
		if pressure != nil {
			report.Pressure = pressure.stop()
			if report.Pressure.Error != "" {
				t.Errorf("contention writer failed: %s", report.Pressure.Error)
			}
		}
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("SEMANTICA_TURN_EXPERIMENT_OUTPUT"); path != "" {
		if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Log(string(data))
	}
}

func workloadGit(t *testing.T, path string, args ...string) string {
	t.Helper()
	out, err := runWorkloadGit(path, args...)
	if err != nil {
		t.Fatalf("workload git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(out)
}

func runWorkloadGit(path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{
		"-c", "core.hooksPath=" + os.DevNull, "-c", "commit.gpgsign=false",
		"-c", "core.fsmonitor=false", "-c", "user.name=Semantica experiment",
		"-c", "user.email=experiment@localhost",
	}, args...)...)
	cmd.Dir = path
	cmd.Env = gitEnv(nil)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func prepareTurnWorkload(t *testing.T, path string) {
	t.Helper()
	if workloadGit(t, path, "status", "--porcelain=v1", "--untracked-files=all") != "" {
		t.Fatalf("repository must be clean: %s", path)
	}
	fixture := filepath.Join(path, turnWorkloadDir)
	if _, err := os.Lstat(fixture); !os.IsNotExist(err) {
		t.Fatalf("fixture path already exists or cannot be inspected: %s", fixture)
	}
	head := workloadGit(t, path, "rev-parse", "HEAD")
	branch, _ := runWorkloadGit(path, "symbolic-ref", "--quiet", "--short", "HEAD")
	branch = strings.TrimSpace(branch)
	workloadGit(t, path, "switch", "--detach", head)
	t.Cleanup(func() {
		if _, err := runWorkloadGit(path, "symbolic-ref", "--quiet", "HEAD"); err == nil {
			t.Errorf("refusing cleanup after an external branch switch in %s", path)
			return
		}
		// Discard only owned fixture edits. The tree/index outside it is untouched.
		if _, err := runWorkloadGit(path, "restore", "--source=HEAD", "--staged", "--worktree", "--", turnWorkloadDir); err != nil {
			t.Logf("fixture restore: %v", err)
		}
		if err := os.Remove(filepath.Join(fixture, "untracked.txt")); err != nil && !os.IsNotExist(err) {
			t.Error(err)
			return
		}
		args := []string{"switch", branch}
		if branch == "" {
			args = []string{"switch", "--detach", head}
		}
		if out, err := runWorkloadGit(path, args...); err != nil {
			t.Errorf("restore checkout %s: %v: %s", path, err, out)
			return
		}
		if err := os.RemoveAll(fixture); err != nil {
			t.Error(err)
		}
		if actual, err := runWorkloadGit(path, "rev-parse", "HEAD"); err != nil || strings.TrimSpace(actual) != head {
			t.Errorf("original HEAD not restored in %s", path)
		}
	})
	if err := os.Mkdir(fixture, 0o700); err != nil {
		t.Fatal(err)
	}
	for file := 0; file < 24; file++ {
		writeTurnWorkloadFile(t, path, fmt.Sprintf("file%02d.txt", file), 0)
	}
	workloadGit(t, path, "add", "--", turnWorkloadDir)
	workloadGit(t, path, "commit", "--only", "-qm", "turn experiment fixtures", "--", turnWorkloadDir)
}

func resetTurnWorkload(t *testing.T, path string) {
	t.Helper()
	if _, err := runWorkloadGit(path, "symbolic-ref", "--quiet", "HEAD"); err == nil {
		t.Fatalf("external branch switch in %s", path)
	}
	if err := os.Remove(filepath.Join(path, turnWorkloadDir, "untracked.txt")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for file := 0; file < 24; file++ {
		writeTurnWorkloadFile(t, path, fmt.Sprintf("file%02d.txt", file), 0)
	}
	workloadGit(t, path, "add", "--", turnWorkloadDir)
	if workloadGit(t, path, "diff", "--cached", "--name-only", "--", turnWorkloadDir) != "" {
		workloadGit(t, path, "commit", "--only", "-qm", "turn experiment baseline", "--", turnWorkloadDir)
	}
}

func writeTurnWorkloadFile(t *testing.T, path, name string, revision int) {
	t.Helper()
	var content strings.Builder
	for line := 0; line < 256; line++ {
		value := 0
		if line%4 == 0 {
			value = revision
		}
		fmt.Fprintf(&content, "record=%04d value=%02d payload=abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstuvwxyz\n", line, value)
	}
	if err := os.WriteFile(filepath.Join(path, turnWorkloadDir, name), []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func checkChangedTurn(t *testing.T, turn *observedTurn, targets []string) {
	t.Helper()
	for _, s := range turn.Samples {
		if s.State == "unknown" {
			continue
		} // Keep failures in the timing report.
		var want []string
		if s.Path == targets[0] || s.Path == targets[1] {
			for file := 0; file < 16; file++ {
				want = append(want, fmt.Sprintf("%s/file%02d.txt", turnWorkloadDir, file))
			}
			last := "untracked.txt"
			if s.Path == targets[1] {
				last = "file16.txt"
			}
			want = append(want, turnWorkloadDir+"/"+last)
			sort.Strings(want)
		}
		wantState := "unchanged"
		if len(want) > 0 {
			wantState = "changed"
		}
		if s.State != wantState || strings.Join(s.Files, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("unexpected result for %s: %s %v", s.Path, s.State, s.Files)
		}
		if s.Path == targets[1] && (len(s.Boundaries) != 2 || s.Boundaries[0].Commit == "" || len(s.Boundaries[0].Files) != 16) {
			t.Errorf("commit evidence lost: %+v", s.Boundaries)
		}
	}
}

type pressureStats struct {
	Duration     time.Duration `json:"duration_ns"`
	HashBytes    int64         `json:"hash_bytes"`
	WrittenBytes int64         `json:"written_bytes"`
	Error        string        `json:"error,omitempty"`
}

type turnPressure struct {
	done    chan struct{}
	wg      sync.WaitGroup
	once    sync.Once
	started time.Time
	hashed  atomic.Int64
	written atomic.Int64
	err     error
	stats   pressureStats
}

// Two CPU workers hash fixed 32 MiB batches with a 10 ms pause. One writer
// cycles eight 1 MiB files, syncing each write and pausing 50 ms afterward.
func startTurnPressure(t *testing.T, dir string) *turnPressure {
	t.Helper()
	p := &turnPressure{done: make(chan struct{}), started: time.Now()}
	ready := make(chan struct{}, 3)
	for worker := 0; worker < 2; worker++ {
		p.wg.Go(func() {
			data := make([]byte, 1<<20)
			data[0] = byte(worker)
			ready <- struct{}{}
			for {
				for range 32 {
					sum := sha256.Sum256(data)
					data[0] ^= sum[0]
				}
				p.hashed.Add(32 << 20)
				select {
				case <-p.done:
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
		})
	}
	p.wg.Go(func() {
		data := make([]byte, 1<<20)
		for i := range data {
			data[i] = byte(i % 251)
		}
		ready <- struct{}{}
		for i := 0; ; i++ {
			f, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("pressure-%d", i%8)), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if err == nil {
				var n int
				n, err = f.Write(data)
				p.written.Add(int64(n))
				if err == nil {
					err = f.Sync()
				}
				closeErr := f.Close()
				if err == nil {
					err = closeErr
				}
			}
			if err != nil {
				p.err = err
				return
			}
			select {
			case <-p.done:
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	})
	for range 3 {
		<-ready
	}
	return p
}

func (p *turnPressure) stop() pressureStats {
	p.once.Do(func() {
		close(p.done)
		p.wg.Wait()
		p.stats = pressureStats{Duration: time.Since(p.started), HashBytes: p.hashed.Load(), WrittenBytes: p.written.Load()}
		if p.err != nil {
			p.stats.Error = p.err.Error()
		}
	})
	return p.stats
}

func TestTurnExperimentWorkloadCleanup(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	path := testRepo(t)
	head := workloadGit(t, path, "rev-parse", "HEAD")
	t.Run("detached_workload", func(t *testing.T) {
		prepareTurnWorkload(t, path)
		writeTurnWorkloadFile(t, path, "file00.txt", 1)
		workloadGit(t, path, "add", "--", turnWorkloadDir)
		workloadGit(t, path, "commit", "--only", "-qm", "test change", "--", turnWorkloadDir)
		writeTurnWorkloadFile(t, path, "file01.txt", 2)
		writeTurnWorkloadFile(t, path, "untracked.txt", 1)
		// Concurrent user work outside the fixture must survive cleanup.
		writeFile(t, path, "sub/b.txt", "user edit\n")
		workloadGit(t, path, "add", "--", "sub/b.txt")
		writeFile(t, path, "user-late.txt", "keep\n")
	})
	if got := workloadGit(t, path, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD: %s, want %s", got, head)
	}
	if got := workloadGit(t, path, "branch", "--show-current"); got != "main" {
		t.Fatalf("branch: %s", got)
	}
	if _, err := os.Stat(filepath.Join(path, turnWorkloadDir)); !os.IsNotExist(err) {
		t.Fatal("fixture not removed")
	}
	if got := workloadGit(t, path, "diff", "--cached", "--name-only"); got != "sub/b.txt" {
		t.Fatalf("user index changed: %q", got)
	}
	if data, err := os.ReadFile(filepath.Join(path, "user-late.txt")); err != nil || string(data) != "keep\n" {
		t.Fatalf("user file changed: %q %v", data, err)
	}
}

func TestTurnExperimentPressureStops(t *testing.T) {
	p := startTurnPressure(t, t.TempDir())
	stats := p.stop()
	if stats.Error != "" || stats.HashBytes == 0 || stats.WrittenBytes == 0 {
		t.Fatalf("pressure did not run: %+v", stats)
	}
	if again := p.stop(); again != stats {
		t.Fatal("stop is not idempotent")
	}
}
