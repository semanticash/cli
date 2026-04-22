package service

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/launcher"
)

// These tests exercise drain behavior through the MarkerRunner seam.

// setupDrainEnv registers the given repos in an isolated broker home.
func setupDrainEnv(t *testing.T, repoRoots ...string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("SEMANTICA_HOME", base)

	registryPath, err := broker.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("DefaultRegistryPath: %v", err)
	}

	ctx := context.Background()
	bh, err := broker.Open(ctx, registryPath)
	if err != nil {
		t.Fatalf("broker.Open: %v", err)
	}
	defer func() { _ = broker.Close(bh) }()

	for _, root := range repoRoots {
		if err := os.MkdirAll(filepath.Join(root, ".semantica"), 0o755); err != nil {
			t.Fatalf("setup .semantica for %s: %v", root, err)
		}
		if err := broker.Register(ctx, bh, root, root); err != nil {
			t.Fatalf("broker.Register %s: %v", root, err)
		}
	}
}

// writeMarker writes a minimal valid marker and returns its path.
func writeMarker(t *testing.T, repo, checkpointID string) string {
	t.Helper()
	m := launcher.Marker{
		CheckpointID: checkpointID,
		CommitHash:   "commit-" + checkpointID,
		RepoRoot:     repo,
		WrittenAt:    time.Now().UnixMilli(),
	}
	if err := launcher.Write(m); err != nil {
		t.Fatalf("launcher.Write: %v", err)
	}
	return launcher.MarkerPath(repo, checkpointID)
}

// These tests lock the per-repo wlog routing contract.

func TestWlog_WritesToCurrentWlogWriter(t *testing.T) {
	var buf bytes.Buffer
	prev := wlogWriter
	wlogWriter = &buf
	defer func() { wlogWriter = prev }()

	wlog("hello %s\n", "world")

	if !bytes.Contains(buf.Bytes(), []byte("hello world")) {
		t.Errorf("expected 'hello world' in writer, got %q", buf.String())
	}
}

func TestDrainOne_RedirectsPerJobWlogToRepoLog(t *testing.T) {
	repo := t.TempDir()
	setupDrainEnv(t, repo)
	writeMarker(t, repo, "ckpt-routing")

	// Per-job output must not land in the launcher log.
	var launcherBuf bytes.Buffer
	prev := wlogWriter
	wlogWriter = &launcherBuf
	defer func() { wlogWriter = prev }()

	runner := recordingRunner{
		OnCall: func(in WorkerInput) error {
			wlog("job-wlog: processing %s\n", in.CheckpointID)
			return nil
		},
	}
	if _, err := DrainOnce(context.Background(), runner.Run); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}

	// The runner's line must not reach the launcher writer.
	if bytes.Contains(launcherBuf.Bytes(), []byte("job-wlog:")) {
		t.Errorf("per-job wlog leaked to the launcher writer:\n%s", launcherBuf.String())
	}

	// The repo log should contain the line instead.
	repoLogPath := filepath.Join(repo, ".semantica", "worker.log")
	data, err := os.ReadFile(repoLogPath)
	if err != nil {
		t.Fatalf("read repo worker.log: %v", err)
	}
	if !bytes.Contains(data, []byte("job-wlog: processing ckpt-routing")) {
		t.Errorf("expected per-job wlog line in repo worker.log, got:\n%s", data)
	}

	// The original writer should be restored after the pass.
	if !writerIs(&launcherBuf) {
		t.Errorf("wlogWriter not restored after DrainOnce")
	}
}

// Unix-only repo-log open-failure coverage lives in
// worker_drain_unix_test.go.

// recordingRunner records calls and can inject custom behavior.
type recordingRunner struct {
	mu     sync.Mutex
	Inputs []WorkerInput
	OnCall func(WorkerInput) error // optional override
}

func (r *recordingRunner) Run(_ context.Context, in WorkerInput) error {
	r.mu.Lock()
	r.Inputs = append(r.Inputs, in)
	hook := r.OnCall
	r.mu.Unlock()
	if hook != nil {
		return hook(in)
	}
	return nil
}

func (r *recordingRunner) calls() []WorkerInput {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]WorkerInput, len(r.Inputs))
	copy(out, r.Inputs)
	return out
}

func TestDrainOnce_SingleMarker_ProcessesAndDeletes(t *testing.T) {
	repo := t.TempDir()
	setupDrainEnv(t, repo)
	path := writeMarker(t, repo, "ckpt-a")

	var runner recordingRunner
	stats, err := DrainOnce(context.Background(), runner.Run)
	if err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if stats.Processed != 1 || stats.Progress() != 1 {
		t.Errorf("stats = %+v, want Processed=1 Progress=1", stats)
	}
	calls := runner.calls()
	if len(calls) != 1 {
		t.Fatalf("runner called %d times, want 1", len(calls))
	}
	if calls[0].CheckpointID != "ckpt-a" || calls[0].RepoRoot != repo {
		t.Errorf("runner got %+v, want checkpoint ckpt-a repo %s", calls[0], repo)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("marker should be deleted after success, stat=%v", err)
	}
}

func TestDrainOnce_MultipleMarkersInOneRepo_ProcessesAll(t *testing.T) {
	repo := t.TempDir()
	setupDrainEnv(t, repo)
	for _, id := range []string{"a", "b", "c"} {
		writeMarker(t, repo, id)
	}

	var runner recordingRunner
	stats, err := DrainOnce(context.Background(), runner.Run)
	if err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if stats.Processed != 3 {
		t.Errorf("stats.Processed = %d, want 3 (stats=%+v)", stats.Processed, stats)
	}
	if got := len(runner.calls()); got != 3 {
		t.Errorf("runner called %d times, want 3", got)
	}
	remaining, _ := launcher.List(repo)
	if len(remaining) != 0 {
		t.Errorf("expected empty pending dir, got %v", remaining)
	}
}

func TestDrainOnce_MultipleRepos_EachRepoDrainedIndependently(t *testing.T) {
	repoA := t.TempDir()
	repoB := t.TempDir()
	setupDrainEnv(t, repoA, repoB)

	writeMarker(t, repoA, "a1")
	writeMarker(t, repoA, "a2")
	writeMarker(t, repoB, "b1")

	var runner recordingRunner
	stats, err := DrainOnce(context.Background(), runner.Run)
	if err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if stats.Processed != 3 {
		t.Errorf("stats.Processed = %d, want 3 (stats=%+v)", stats.Processed, stats)
	}

	seenByRepo := map[string]int{}
	for _, in := range runner.calls() {
		seenByRepo[in.RepoRoot]++
	}
	if seenByRepo[repoA] != 2 {
		t.Errorf("repoA calls = %d, want 2", seenByRepo[repoA])
	}
	if seenByRepo[repoB] != 1 {
		t.Errorf("repoB calls = %d, want 1", seenByRepo[repoB])
	}
}

func TestDrainOnce_RunnerError_LeavesMarkerInPlace(t *testing.T) {
	repo := t.TempDir()
	setupDrainEnv(t, repo)
	path := writeMarker(t, repo, "ckpt-fail")

	runner := recordingRunner{
		OnCall: func(WorkerInput) error { return errors.New("simulated worker failure") },
	}
	stats, err := DrainOnce(context.Background(), runner.Run)
	if err != nil {
		t.Fatalf("DrainOnce surfaced a broker error for a per-marker failure: %v", err)
	}
	if stats.Processed != 0 || stats.RunErrors != 1 {
		t.Errorf("stats = %+v, want Processed=0 RunErrors=1", stats)
	}
	if stats.Progress() != 0 {
		t.Errorf("stats.Progress() = %d, want 0 when the only marker run-errored", stats.Progress())
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("marker should still exist after runner failure, stat=%v", err)
	}
}

func TestDrainOnce_UnregisteredRepoIsIgnored(t *testing.T) {
	repoRegistered := t.TempDir()
	repoOrphan := t.TempDir()
	setupDrainEnv(t, repoRegistered)
	// Leave repoOrphan unregistered.
	writeMarker(t, repoOrphan, "orphan")

	var runner recordingRunner
	stats, err := DrainOnce(context.Background(), runner.Run)
	if err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if stats.Progress() != 0 || stats.Processed != 0 {
		t.Errorf("stats = %+v, want all zeroes when only repo is unregistered", stats)
	}
	if got := len(runner.calls()); got != 0 {
		t.Errorf("runner should not have been called; got %d calls", got)
	}
	// The orphan marker stays on disk because the repo is not registered.
	if _, err := os.Stat(launcher.MarkerPath(repoOrphan, "orphan")); err != nil {
		t.Errorf("orphan marker removed unexpectedly, stat=%v", err)
	}
}

func TestDrainOnce_CorruptMarkerIsDeletedAndSkipped(t *testing.T) {
	repo := t.TempDir()
	setupDrainEnv(t, repo)

	// Seed a corrupt marker file directly.
	dir := launcher.PendingDir(repo)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir pending: %v", err)
	}
	corruptPath := filepath.Join(dir, "corrupt.job")
	if err := os.WriteFile(corruptPath, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("seed corrupt: %v", err)
	}

	var runner recordingRunner
	stats, err := DrainOnce(context.Background(), runner.Run)
	if err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if stats.Processed != 0 {
		t.Errorf("corrupt marker must not count as Processed, stats=%+v", stats)
	}
	if stats.Rejected != 1 {
		t.Errorf("corrupt marker should have been Rejected, stats=%+v", stats)
	}
	if stats.Progress() != 1 {
		t.Errorf("stats.Progress() = %d, want 1 (rejection removes the marker)", stats.Progress())
	}
	if got := len(runner.calls()); got != 0 {
		t.Errorf("runner must not be called for corrupt marker, got %d", got)
	}
	if _, err := os.Stat(corruptPath); !os.IsNotExist(err) {
		t.Errorf("corrupt marker should have been deleted, stat=%v", err)
	}
}

func TestDrainOnce_ContextCancellationStopsEarly(t *testing.T) {
	repo := t.TempDir()
	setupDrainEnv(t, repo)
	writeMarker(t, repo, "first")
	writeMarker(t, repo, "second")
	writeMarker(t, repo, "third")

	ctx, cancel := context.WithCancel(context.Background())
	var called int32
	runner := recordingRunner{
		OnCall: func(WorkerInput) error {
			// Cancel after the first call so later iterations see ctx.Err().
			if atomic.AddInt32(&called, 1) == 1 {
				cancel()
			}
			return nil
		},
	}
	stats, err := DrainOnce(ctx, runner.Run)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got stats=%+v err=%v", stats, err)
	}
	if stats.Processed >= 3 {
		t.Errorf("cancellation should have stopped the loop, stats=%+v", stats)
	}
}

func TestDrainUntilStable_EmptyQueueExitsImmediately(t *testing.T) {
	repo := t.TempDir()
	setupDrainEnv(t, repo)

	var runner recordingRunner
	start := time.Now()
	err := DrainUntilStable(context.Background(), 0, runner.Run)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("DrainUntilStable: %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("zero-linger empty drain took too long: %v", elapsed)
	}
	if got := len(runner.calls()); got != 0 {
		t.Errorf("runner should not have been called, got %d", got)
	}
}

// A marker written mid-drain should be picked up by the same invocation.
func TestDrainUntilStable_AbsorbsMarkerWrittenDuringDrain(t *testing.T) {
	repo := t.TempDir()
	setupDrainEnv(t, repo)
	writeMarker(t, repo, "first")

	var wrote bool
	runner := recordingRunner{
		OnCall: func(in WorkerInput) error {
			// Drop a new marker that the next pass should discover.
			if in.CheckpointID == "first" && !wrote {
				wrote = true
				writeMarker(t, repo, "second")
			}
			return nil
		},
	}
	if err := DrainUntilStable(context.Background(), 0, runner.Run); err != nil {
		t.Fatalf("DrainUntilStable: %v", err)
	}
	if got := len(runner.calls()); got != 2 {
		t.Errorf("runner should have been called twice (first, then second arrival), got %d", got)
	}
	remaining, _ := launcher.List(repo)
	if len(remaining) != 0 {
		t.Errorf("pending should be empty after DrainUntilStable, got %v", remaining)
	}
}

// A marker written during linger should still be processed.
func TestDrainUntilStable_LingerCatchesLateMarker(t *testing.T) {
	repo := t.TempDir()
	setupDrainEnv(t, repo)

	var runner recordingRunner
	// Write a marker during the linger window.
	go func() {
		time.Sleep(30 * time.Millisecond)
		writeMarker(t, repo, "late-arrival")
	}()

	if err := DrainUntilStable(context.Background(), 100*time.Millisecond, runner.Run); err != nil {
		t.Fatalf("DrainUntilStable: %v", err)
	}
	calls := runner.calls()
	if len(calls) != 1 || calls[0].CheckpointID != "late-arrival" {
		t.Errorf("expected exactly one call for late-arrival marker, got %+v", calls)
	}
}

// All-run-error passes should exit quickly and leave markers for a later invocation.
func TestDrainUntilStable_AllRunErrorsExitsQuicklyWithMarkersStillQueued(t *testing.T) {
	repo := t.TempDir()
	setupDrainEnv(t, repo)
	writeMarker(t, repo, "a")
	writeMarker(t, repo, "b")

	runner := recordingRunner{
		OnCall: func(WorkerInput) error { return errors.New("deterministic failure") },
	}

	done := make(chan error, 1)
	go func() {
		done <- DrainUntilStable(context.Background(), 0, runner.Run)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("DrainUntilStable: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("DrainUntilStable did not exit; likely spinning on run errors")
	}

	remaining, _ := launcher.List(repo)
	if len(remaining) != 2 {
		t.Errorf("markers should remain for a later retry, got %v", remaining)
	}
	// Each failing marker should run once per invocation.
	if got := len(runner.calls()); got != 2 {
		t.Errorf("runner invoked %d times; expected exactly 2 (one per marker)", got)
	}
}

// Mixed success and failure should not retry the failed marker in the same invocation.
func TestDrainUntilStable_MixedSuccessAndFailureDoesNotRetryFailureInInvocation(t *testing.T) {
	repo := t.TempDir()
	setupDrainEnv(t, repo)
	writeMarker(t, repo, "good")
	writeMarker(t, repo, "bad")

	badCalls := 0
	runner := recordingRunner{
		OnCall: func(in WorkerInput) error {
			if in.CheckpointID == "bad" {
				badCalls++
				return errors.New("deterministic failure on bad marker")
			}
			return nil
		},
	}

	if err := DrainUntilStable(context.Background(), 0, runner.Run); err != nil {
		t.Fatalf("DrainUntilStable: %v", err)
	}

	if badCalls != 1 {
		t.Errorf("bad marker was run %d times; per-invocation skip set must bound it to exactly 1",
			badCalls)
	}

	// The good marker still gets processed and its file is gone.
	if _, err := os.Stat(launcher.MarkerPath(repo, "good")); !os.IsNotExist(err) {
		t.Errorf("good marker should be deleted, stat=%v", err)
	}
	// The bad marker stays on disk for the next invocation.
	if _, err := os.Stat(launcher.MarkerPath(repo, "bad")); err != nil {
		t.Errorf("bad marker should remain on disk for next invocation, stat=%v", err)
	}
}

// Each DrainUntilStable call should start with a fresh skip set.
func TestDrainUntilStable_SkipSetIsPerInvocationNotPersistent(t *testing.T) {
	repo := t.TempDir()
	setupDrainEnv(t, repo)
	writeMarker(t, repo, "bad")

	runner := recordingRunner{
		OnCall: func(WorkerInput) error { return errors.New("always fails") },
	}

	if err := DrainUntilStable(context.Background(), 0, runner.Run); err != nil {
		t.Fatalf("first DrainUntilStable: %v", err)
	}
	afterFirst := len(runner.calls())
	if afterFirst != 1 {
		t.Fatalf("first invocation made %d calls, want exactly 1", afterFirst)
	}

	// A fresh invocation should retry the marker once.
	if err := DrainUntilStable(context.Background(), 0, runner.Run); err != nil {
		t.Fatalf("second DrainUntilStable: %v", err)
	}
	if got := len(runner.calls()) - afterFirst; got != 1 {
		t.Errorf("second invocation made %d calls, want exactly 1 (the retry)", got)
	}
}

// With no new markers, linger should delay exit once and then stop.
func TestDrainUntilStable_NoArrivalExitsAfterLinger(t *testing.T) {
	repo := t.TempDir()
	setupDrainEnv(t, repo)

	var runner recordingRunner
	const linger = 50 * time.Millisecond
	start := time.Now()
	if err := DrainUntilStable(context.Background(), linger, runner.Run); err != nil {
		t.Fatalf("DrainUntilStable: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < linger {
		t.Errorf("exited faster than the linger: elapsed=%v linger=%v", elapsed, linger)
	}
	if elapsed > linger+200*time.Millisecond {
		t.Errorf("exited much later than expected: elapsed=%v linger=%v", elapsed, linger)
	}
	if got := len(runner.calls()); got != 0 {
		t.Errorf("runner should not have been called, got %d", got)
	}
}
