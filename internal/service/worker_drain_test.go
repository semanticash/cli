package service

import (
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

// These tests exercise DrainOnce / DrainUntilStable through the
// MarkerRunner seam rather than through the real WorkerService,
// which avoids having to stand up a full lineage DB / git
// repository / checkpoint row per marker. The runner's job is
// small enough that a fake can faithfully represent it.

// setupDrainEnv prepares a temporary SEMANTICA_HOME, registers
// the given repo roots as active repositories in the broker
// registry, and returns the list of repo paths for the caller to
// populate with markers.
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

// writeMarker persists a minimal valid marker in the repo's
// pending directory and returns the on-disk path.
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

// recordingRunner captures every WorkerInput it receives in call
// order. Callers can optionally provide a hook that returns an
// error, writes additional markers, or otherwise exercises
// edge behavior.
type recordingRunner struct {
	mu      sync.Mutex
	Inputs  []WorkerInput
	OnCall  func(WorkerInput) error // optional behavior override
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
	// Intentionally do NOT register repoOrphan.
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
	// Orphan marker still present on disk; the drain loop has no
	// opinion on it because the repo is not registered.
	if _, err := os.Stat(launcher.MarkerPath(repoOrphan, "orphan")); err != nil {
		t.Errorf("orphan marker removed unexpectedly, stat=%v", err)
	}
}

func TestDrainOnce_CorruptMarkerIsDeletedAndSkipped(t *testing.T) {
	repo := t.TempDir()
	setupDrainEnv(t, repo)

	// Seed a corrupt marker file directly rather than through Write.
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
			// Cancel after the first call so subsequent
			// iterations observe ctx.Err().
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

// Repeat-until-stable: if the runner writes a new marker mid-drain,
// the second pass must pick it up in the same DrainUntilStable call.
func TestDrainUntilStable_AbsorbsMarkerWrittenDuringDrain(t *testing.T) {
	repo := t.TempDir()
	setupDrainEnv(t, repo)
	writeMarker(t, repo, "first")

	var wrote bool
	runner := recordingRunner{
		OnCall: func(in WorkerInput) error {
			// While processing "first", drop a new marker
			// that the outer loop should discover on the
			// next pass.
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

// Linger test: a marker dropped during the idle interval between
// the empty first pass and the final scan must be processed
// before DrainUntilStable returns.
func TestDrainUntilStable_LingerCatchesLateMarker(t *testing.T) {
	repo := t.TempDir()
	setupDrainEnv(t, repo)

	var runner recordingRunner
	// Schedule a marker to appear ~30ms after DrainUntilStable
	// starts. With a 100ms linger that is well within the
	// window the final rescan will cover.
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

// A pass in which every marker fails to run must not be treated
// as an empty queue. DrainUntilStable previously used "runner
// successes" as its exit signal, which caused it to exit with
// markers still queued and then miss the retry that should only
// happen on a subsequent kickstart invocation. The new Progress
// signal counts "markers that left the queue," which zero runner
// failures correctly report as zero. DrainUntilStable must still
// exit quickly (not spin) so the retry is deferred rather than
// attempted in a tight inner loop.
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
	// Per-invocation skip set: each failing marker is tried
	// exactly once. More than two calls means the skip set
	// regressed and failures are being retried within the
	// same invocation.
	if got := len(runner.calls()); got != 2 {
		t.Errorf("runner invoked %d times; expected exactly 2 (one per marker)", got)
	}
}

// A pass that mixes one successful marker with one persistently
// failing marker must not retry the failure inside the same
// invocation. Without the per-invocation skip set, the success
// keeps Progress > 0 which drives another pass, the failing
// marker is still on disk, and it runs again (and again). The
// skip set ensures a failing marker is attempted at most once
// per DrainUntilStable call even when unrelated success markers
// keep the outer loop going.
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

// Fresh DrainUntilStable invocations must start with an empty
// skip set so a previously-failing marker gets another attempt.
// Using DrainOnce directly across two separate calls exercises
// the non-retry path (DrainOnce does not maintain skip), but
// what users actually observe is two separate DrainUntilStable
// invocations (two kickstarts). This test asserts that contract
// by running DrainUntilStable twice against the same repo and
// confirming the second invocation re-attempts the bad marker.
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

	// Second invocation. Same marker, same failing runner. A
	// fresh skip set means this invocation retries the marker.
	if err := DrainUntilStable(context.Background(), 0, runner.Run); err != nil {
		t.Fatalf("second DrainUntilStable: %v", err)
	}
	if got := len(runner.calls()) - afterFirst; got != 1 {
		t.Errorf("second invocation made %d calls, want exactly 1 (the retry)", got)
	}
}

// Inverse of the linger test: when nothing arrives during the
// linger, DrainUntilStable must not spin forever. The total time
// should be bounded just above the linger interval.
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
