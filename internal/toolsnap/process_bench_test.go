package toolsnap

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"testing"
	"time"
)

// TestMain lets the test binary re-exec itself as a hook-shaped child
// process: resolve repository context, open the store, capture, and
// persist a ref, then exit. The pre-hook latency thresholds cover
// that full lifecycle. The test binary approximates Semantica startup;
// the installed hook entry point must also meet these thresholds before
// capture is enabled.
func TestMain(m *testing.M) {
	if dir := os.Getenv("TOOLSNAP_BENCH_CHILD_DIR"); dir != "" {
		if err := benchChild(dir, os.Getenv("TOOLSNAP_BENCH_CHILD_REF")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func benchChild(dir, toolUseID string) error {
	ctx := context.Background()
	rc, err := ResolveRepoContext(ctx, dir)
	if err != nil {
		return err
	}
	s, err := OpenStore(ctx, rc, dir+"/.semantica")
	if err != nil {
		return err
	}
	snap, err := s.CaptureBefore(ctx)
	if err != nil {
		return err
	}
	return s.CreateRef(ctx, SnapshotRef(rc.WorktreeID, "bench", toolUseID), snap.TreeHash)
}

// runHookProcesses spawns n child processes against root and reports
// wall-clock percentiles per full process. Ref identities include the
// benchmark iteration so repeated -benchtime samples never collide.
func runHookProcesses(b *testing.B, root string, iter, n int) (p50, p95, max time.Duration) {
	b.Helper()
	self, err := os.Executable()
	if err != nil {
		b.Fatal(err)
	}
	durations := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		cmd := exec.Command(self)
		cmd.Env = append(os.Environ(),
			"TOOLSNAP_BENCH_CHILD_DIR="+root,
			fmt.Sprintf("TOOLSNAP_BENCH_CHILD_REF=tu-%d-%d", iter, i),
		)
		start := time.Now()
		out, err := cmd.CombinedOutput()
		elapsed := time.Since(start)
		if err != nil {
			b.Fatalf("hook process %d: %v\n%s", i, err, out)
		}
		durations = append(durations, elapsed)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	return durations[n/2], durations[n*95/100], durations[n-1]
}

func benchHookProcess(b *testing.B, files, dirty, procs int) {
	s := benchRepo(b, files, dirty)
	root := s.repo.WorktreeRoot
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p50, p95, max := runHookProcesses(b, root, i, procs)
		b.ReportMetric(float64(p50.Milliseconds()), "p50-ms")
		b.ReportMetric(float64(p95.Milliseconds()), "p95-ms")
		b.ReportMetric(float64(max.Milliseconds()), "max-ms")
	}
}

// Performance thresholds: clean p50 < 50ms and p95 < 150ms;
// large dirty worktrees p95 < 500ms.
func BenchmarkHookProcessClean500(b *testing.B)       { benchHookProcess(b, 500, 0, 100) }
func BenchmarkHookProcessClean5000(b *testing.B)      { benchHookProcess(b, 5000, 0, 100) }
func BenchmarkHookProcessDirty10of500(b *testing.B)   { benchHookProcess(b, 500, 10, 50) }
func BenchmarkHookProcessDirty500of5000(b *testing.B) { benchHookProcess(b, 5000, 500, 30) }
