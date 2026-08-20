//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
)

// These benchmarks run real CLI pre- and post-Bash hooks in an isolated
// repository. Each batch verifies workspace state, evidence links, pending
// windows, and snapshot refs. Results use nearest-rank p50, p95, and max.
//
// Environment knobs:
//
//	SEMANTICA_BENCH_SOURCE_REPO  clone this repository as the fixture
//	                             (default: synthesize files instead)
//	SEMANTICA_BENCH_FILES        synthesized committed files (default 500)
//	SEMANTICA_BENCH_DIRTY        pre-existing dirty files (dirty variants, default 500)
//
// Run, for example:
//
//	make build
//	go test -tags e2e -bench ToolWindow -benchtime 100x -count 3 -run xxx ./e2e
type benchWorld struct {
	b          testing.TB
	repo       string
	transcript string
	env        []string
	editFile   string
	cycles     int
}

func newBenchWorld(b testing.TB) *benchWorld {
	b.Helper()
	// Use best-effort cleanup because child processes may outlive a failed run.
	scratch, err := os.MkdirTemp("", "semantica-toolwindow-bench-")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = os.RemoveAll(scratch) })
	home := filepath.Join(scratch, "home")
	repo := filepath.Join(scratch, "repo")
	for _, d := range []string{home, filepath.Join(scratch, "xdg"), filepath.Join(scratch, "hookless")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			b.Fatal(err)
		}
	}
	w := &benchWorld{b: b, repo: repo}
	// Keep fixture behavior independent of user and system Git configuration.
	w.env = append(os.Environ(),
		"HOME="+home,
		"SEMANTICA_HOME="+filepath.Join(home, ".semantica-global"),
		"XDG_CONFIG_HOME="+filepath.Join(scratch, "xdg"),
		// Isolate transcript ownership checks from the host configuration.
		"CLAUDE_CONFIG_DIR="+filepath.Join(home, ".claude"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull)

	if src := os.Getenv("SEMANTICA_BENCH_SOURCE_REPO"); src != "" {
		clone := exec.Command("git", "clone", "--local", "-q", src, repo)
		clone.Dir = scratch
		clone.Env = w.env
		if out, err := clone.CombinedOutput(); err != nil {
			b.Fatalf("clone %s: %v\n%s", src, err, out)
		}
	} else {
		if err := os.MkdirAll(repo, 0o755); err != nil {
			b.Fatal(err)
		}
		w.gitIn("init", "-q", "-b", "main")
	}
	w.gitIn("config", "user.email", "bench@test")
	w.gitIn("config", "user.name", "bench")
	if os.Getenv("SEMANTICA_BENCH_SOURCE_REPO") == "" {
		files := benchEnvInt(b, "SEMANTICA_BENCH_FILES", 500)
		for i := 0; i < files; i++ {
			w.write(fmt.Sprintf("file%05d.txt", i), fmt.Sprintf("content of file %d\n", i))
		}
		w.gitIn("add", ".")
		w.gitIn("commit", "-q", "-m", "seed")
	}

	// Suppress hooks while committing setup files to avoid worker contention.
	w.run(semBinary, "enable", "--providers", "claude-code")
	w.editFile = "bench-edit-target.txt"
	w.write(w.editFile, "initial\n")
	w.gitIn("-c", "core.hooksPath="+filepath.Join(scratch, "hookless"), "add", "-A")
	w.gitIn("-c", "core.hooksPath="+filepath.Join(scratch, "hookless"), "commit", "-q", "-m", "enable artifacts and edit target")
	if n := w.dirtyCount(); n != 0 {
		b.Fatalf("fixture not clean after setup: %d dirty files", n)
	}

	// Store the fixture under Claude Code's configured projects directory.
	transcriptDir := filepath.Join(home, ".claude", "projects", "bench")
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		b.Fatal(err)
	}
	w.transcript = filepath.Join(transcriptDir, "transcript.jsonl")
	if err := os.WriteFile(w.transcript, nil, 0o644); err != nil {
		b.Fatal(err)
	}
	w.hookTimed("session-start", map[string]any{
		"session_id": "bench-sess", "transcript_path": w.transcript, "cwd": repo})
	w.hookTimed("user-prompt-submit", map[string]any{
		"session_id": "bench-sess", "transcript_path": w.transcript, "cwd": repo,
		"prompt": "bench"})
	return w
}

func benchEnvInt(b testing.TB, name string, def int) int {
	b.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		b.Fatalf("%s: %v", name, err)
	}
	return n
}

func (w *benchWorld) run(name string, args ...string) {
	w.b.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = w.repo
	cmd.Env = w.env
	if out, err := cmd.CombinedOutput(); err != nil {
		w.b.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func (w *benchWorld) gitIn(args ...string) { w.run("git", args...) }

func (w *benchWorld) write(name, content string) {
	w.b.Helper()
	if err := os.WriteFile(filepath.Join(w.repo, name), []byte(content), 0o644); err != nil {
		w.b.Fatal(err)
	}
}

func (w *benchWorld) hookTimed(phase string, payload map[string]any) float64 {
	w.b.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		w.b.Fatal(err)
	}
	cmd := exec.Command(semBinary, "capture", "claude-code", phase)
	cmd.Dir = w.repo
	cmd.Env = w.env
	cmd.Stdin = bytes.NewReader(raw)
	start := time.Now()
	if err := cmd.Run(); err != nil {
		w.b.Fatalf("hook %s: %v", phase, err)
	}
	return float64(time.Since(start).Microseconds()) / 1000
}

func (w *benchWorld) dirtyCount() int {
	w.b.Helper()
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = w.repo
	cmd.Env = w.env
	out, err := cmd.Output()
	if err != nil {
		w.b.Fatal(err)
	}
	return bytes.Count(out, []byte("\n"))
}

// cycle runs one captured Bash pre/post pair and returns both timings.
func (w *benchWorld) cycle(i int) (pre, post float64) {
	w.b.Helper()
	tu := fmt.Sprintf("toolu_bench_%d_%d", os.Getpid(), i)
	base := map[string]any{
		"session_id":      "bench-sess",
		"transcript_path": w.transcript,
		"cwd":             w.repo, "tool_name": "Bash", "tool_use_id": tu,
		"tool_input": map[string]any{"command": "true"},
	}
	pre = w.hookTimed("pre-bash", base)
	w.write(w.editFile, fmt.Sprintf("edited by cycle %d\n", i))
	postPayload := map[string]any{"tool_response": map[string]any{"stdout": ""}}
	for k, v := range base {
		postPayload[k] = v
	}
	post = w.hookTimed("post-bash", postPayload)
	w.gitIn("checkout", "--", w.editFile)
	w.cycles++
	return pre, post
}

// assertFixtures verifies evidence counts and the absence of pending state.
func (w *benchWorld) assertFixtures() {
	w.b.Helper()
	if w.cycles == 0 {
		w.b.Fatal("no cycles ran")
	}
	semDir := filepath.Join(w.repo, ".semantica")
	raw, err := os.ReadFile(filepath.Join(semDir, "tool-windows", "registry.json"))
	if err != nil {
		w.b.Fatalf("registry state unreadable after %d cycles: %v", w.cycles, err)
	}
	var state struct {
		Windows []json.RawMessage `json:"windows"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		w.b.Fatalf("registry state malformed: %v\n%s", err, raw)
	}
	if len(state.Windows) != 0 {
		w.b.Fatalf("open windows after run: %s", raw)
	}

	refs := exec.Command("git", "--git-dir", filepath.Join(semDir, "tool-snapshots.git"), "for-each-ref")
	refs.Env = w.env
	out, err := refs.Output()
	if err != nil {
		w.b.Fatalf("list snapshot refs: %v", err)
	}
	if len(out) != 0 {
		w.b.Fatalf("leftover snapshot refs:\n%s", out)
	}

	ctx := context.Background()
	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		w.b.Fatalf("open lineage db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	var links int
	if err := h.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM agent_event_evidence_links").Scan(&links); err != nil {
		w.b.Fatalf("count evidence links: %v", err)
	}
	if links != w.cycles {
		w.b.Fatalf("evidence links = %d, want %d", links, w.cycles)
	}
}

// percentile returns the nearest-rank percentile of sorted samples.
func percentile(sorted []float64, p float64) float64 {
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	return sorted[idx]
}

func report(b *testing.B, name string, xs []float64) {
	sort.Float64s(xs)
	b.ReportMetric(percentile(xs, 0.50), name+"-p50-ms")
	b.ReportMetric(percentile(xs, 0.95), name+"-p95-ms")
	b.ReportMetric(xs[len(xs)-1], name+"-max-ms")
}

// TestPercentileNearestRank pins the nearest-rank convention.
func TestPercentileNearestRank(t *testing.T) {
	seq := func(n int) []float64 {
		xs := make([]float64, n)
		for i := range xs {
			xs[i] = float64(i + 1)
		}
		return xs
	}
	cases := []struct {
		n        int
		p        float64
		expected float64
	}{
		{2, 0.50, 1}, {2, 0.95, 2},
		{50, 0.50, 25}, {50, 0.95, 48},
		{100, 0.50, 50}, {100, 0.95, 95},
	}
	for _, c := range cases {
		if got := percentile(seq(c.n), c.p); got != c.expected {
			t.Errorf("percentile(n=%d, p=%v) = %v, want %v", c.n, c.p, got, c.expected)
		}
	}
}

func runCycles(b *testing.B, w *benchWorld, wantDirty int) {
	b.Helper()
	var pres, posts []float64
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if n := w.dirtyCount(); n != wantDirty {
			b.Fatalf("dirty files = %d before pre hook, want %d", n, wantDirty)
		}
		pre, post := w.cycle(i)
		pres = append(pres, pre)
		posts = append(posts, post)
	}
	b.StopTimer()
	w.assertFixtures()
	report(b, "pre", pres)
	report(b, "post", posts)
}

// TestHookRecordMatchesExternalDuration compares recorded and external timing.
//
// The 75ms allowance covers process launch, initialization before the timing
// origin, the final JSONL append, and process teardown.
//
// Delaying JSON completion verifies that recorded timing includes the stdin read.
func TestHookRecordMatchesExternalDuration(t *testing.T) {
	w := newBenchWorld(t)
	if err := os.MkdirAll(filepath.Join(w.repo, ".semantica", "doctor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.repo, ".semantica", "doctor", "bench.enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	const cycles = 8
	external := map[string]float64{}
	for i := 0; i < cycles; i++ {
		tu := fmt.Sprintf("toolu_bench_%d_%d", os.Getpid(), i)
		pre, _ := w.cycle(i)
		external[tu] = pre
	}

	// The recorded duration must include this pre-dispatch wait.
	slowTU := "toolu_slow_stdin"
	payload, err := json.Marshal(map[string]any{
		"session_id":      "bench-sess",
		"transcript_path": w.transcript,
		"cwd":             w.repo, "tool_name": "Bash", "tool_use_id": slowTU,
		"tool_input": map[string]any{"command": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	slow := exec.Command(semBinary, "capture", "claude-code", "pre-bash")
	slow.Dir = w.repo
	slow.Env = w.env
	stdin, err := slow.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := slow.Start(); err != nil {
		t.Fatal(err)
	}
	// Pause while the decoder waits for the final byte. The recorded duration
	// must include this wait.
	if _, err := stdin.Write(payload[:len(payload)-1]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if _, err := stdin.Write(payload[len(payload)-1:]); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	if err := slow.Wait(); err != nil {
		t.Fatalf("slow-stdin hook: %v", err)
	}

	records := readBenchRecords(t, w.repo)
	seen := map[string]bool{}
	slowSeen := false
	var maxGap float64
	for _, r := range records {
		if r.Kind != "hook" || r.Event != "ToolStepStarted" {
			continue
		}
		if seen[r.ToolUseID] {
			t.Fatalf("duplicate pre-hook record for %s", r.ToolUseID)
		}
		seen[r.ToolUseID] = true
		if r.ToolUseID == slowTU {
			slowSeen = true
			if r.DurationMS < 140 {
				t.Fatalf("slow-stdin recorded %dms; timing does not start at process origin", r.DurationMS)
			}
			continue
		}
		ext, ok := external[r.ToolUseID]
		if !ok {
			t.Fatalf("unexpected pre-hook record for %s", r.ToolUseID)
		}
		gap := ext - float64(r.DurationMS)
		if gap < 0 {
			t.Fatalf("recorded %dms exceeds external %.1fms for %s", r.DurationMS, ext, r.ToolUseID)
		}
		if gap > 75 {
			t.Fatalf("gap %.1fms for %s exceeds the documented 75ms bound (external %.1f, recorded %d)",
				gap, r.ToolUseID, ext, r.DurationMS)
		}
		if gap > maxGap {
			maxGap = gap
		}
	}
	if !slowSeen {
		t.Fatal("slow-stdin record missing; the origin proof never ran")
	}
	for tu := range external {
		if !seen[tu] {
			t.Fatalf("no pre-hook record for %s", tu)
		}
	}
	t.Logf("unrecorded remainder across %d hooks: max %.1fms", len(external), maxGap)
}

func readBenchRecords(t *testing.T, repo string) []benchJSONRecord {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repo, ".semantica", "doctor", "bench.jsonl"))
	if err != nil {
		t.Fatalf("bench log: %v", err)
	}
	var out []benchJSONRecord
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r benchJSONRecord
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatalf("bench record malformed: %v: %s", err, line)
		}
		out = append(out, r)
	}
	return out
}

type benchJSONRecord struct {
	Kind       string `json:"kind"`
	Event      string `json:"event"`
	ToolUseID  string `json:"tool_use_id"`
	DurationMS int64  `json:"duration_ms"`
}

// BenchmarkToolWindowCycleClean measures full cycles on a clean tree.
func BenchmarkToolWindowCycleClean(b *testing.B) {
	w := newBenchWorld(b)
	runCycles(b, w, 0)
}

// BenchmarkToolWindowCycleDirtyUntracked measures full cycles with
// pre-existing untracked files.
func BenchmarkToolWindowCycleDirtyUntracked(b *testing.B) {
	w := newBenchWorld(b)
	dirty := benchEnvInt(b, "SEMANTICA_BENCH_DIRTY", 500)
	for i := 0; i < dirty; i++ {
		w.write(fmt.Sprintf("untracked%05d.txt", i), fmt.Sprintf("untracked %d\n", i))
	}
	runCycles(b, w, dirty)
}

// BenchmarkToolWindowCycleDirtyTracked measures full cycles with
// pre-existing modifications to tracked files, the more common
// mid-turn state. Requires the synthetic fixture.
func BenchmarkToolWindowCycleDirtyTracked(b *testing.B) {
	if os.Getenv("SEMANTICA_BENCH_SOURCE_REPO") != "" {
		b.Skip("tracked-dirty variant requires the synthetic fixture")
	}
	w := newBenchWorld(b)
	dirty := benchEnvInt(b, "SEMANTICA_BENCH_DIRTY", 500)
	files := benchEnvInt(b, "SEMANTICA_BENCH_FILES", 500)
	if dirty > files {
		b.Fatalf("SEMANTICA_BENCH_DIRTY %d exceeds committed files %d", dirty, files)
	}
	for i := 0; i < dirty; i++ {
		w.write(fmt.Sprintf("file%05d.txt", i), fmt.Sprintf("modified content of file %d\n", i))
	}
	runCycles(b, w, dirty)
}
