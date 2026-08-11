package hooks

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/doctor"
)

func enableBench(t *testing.T, semDir string) {
	t.Helper()
	dir := filepath.Join(semDir, "doctor")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bench.enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func benchRecords(t *testing.T, repoPath string) []doctor.BenchRecord {
	t.Helper()
	f, err := os.Open(doctor.BenchLogPath(repoPath))
	if err != nil {
		t.Fatalf("bench log: %v", err)
	}
	defer func() { _ = f.Close() }()
	var out []doctor.BenchRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r doctor.BenchRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("bench record malformed: %v: %s", err, sc.Text())
		}
		out = append(out, r)
	}
	return out
}

func findRecord(records []doctor.BenchRecord, kind, phase string) *doctor.BenchRecord {
	for i := range records {
		if records[i].Kind == kind && records[i].Phase == phase {
			return &records[i]
		}
	}
	return nil
}

// A captured cycle records both hook outcomes and evidence details.
func TestToolWindowBenchRecords(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	enableBench(t, w.semDir)
	ctx := context.Background()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-br", Provider: "claude-code",
		TurnID: "turn-1", CWD: w.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := handleToolStepStarted(ctx, "claude-code", startedEvent("sess-br", "toolu_br", w.repoPath), w.bh); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.repoPath, "gen.txt"), []byte("generated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	post := postBashEvent("sess-br", "toolu_br", w.repoPath, "cmd")
	if !completeToolWindow(ctx, "claude-code", post, w.bh, nil, []broker.RawEvent{bashRawEvent("evt-br", "toolu_br", "sess-br")}) {
		t.Fatal("completion not handled")
	}

	records := benchRecords(t, w.repoPath)
	pre := findRecord(records, "toolwindow", "pre")
	if pre == nil || pre.Outcome != "registered" || pre.SessionID != "sess-br" ||
		pre.ToolUseID != "toolu_br" || pre.DurationMS < 0 {
		t.Fatalf("pre record = %+v, want registered with identity", pre)
	}
	postRec := findRecord(records, "toolwindow", "post")
	if postRec == nil || postRec.Outcome != "closed_complete" || postRec.PartialReason != "" ||
		postRec.FilesChanged != 1 || postRec.GroupMembers != 1 || postRec.SessionID != "sess-br" {
		t.Fatalf("post record = %+v, want closed_complete with one file", postRec)
	}

	// A post without a pre records the exclusion and its reason.
	post2 := postBashEvent("sess-br", "toolu_mp2", w.repoPath, "cmd")
	completeToolWindow(ctx, "claude-code", post2, w.bh, nil, []broker.RawEvent{bashRawEvent("evt-br2", "toolu_mp2", "sess-br")})
	records = benchRecords(t, w.repoPath)
	var missing *doctor.BenchRecord
	for i := range records {
		if records[i].Kind == "toolwindow" && records[i].Outcome == "missing_pre" {
			missing = &records[i]
		}
	}
	if missing == nil || missing.PartialReason != "pre_snapshot_missing" {
		t.Fatalf("records = %+v, want a missing_pre exclusion record", records)
	}
}

// Dispatch records pre-Bash hooks even when they write no database rows.
func TestPreHookBenchRecordEmitted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	enableBench(t, w.semDir)
	ctx := context.Background()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-hb", Provider: "claude-code",
		TurnID: "turn-1", CWD: w.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// Use an earlier origin to distinguish process timing from dispatch timing.
	dctx := WithHookStart(ctx, time.Now().Add(-100*time.Millisecond))
	if err := Dispatch(dctx, &fakeProvider{name: "claude-code"}, startedEvent("sess-hb", "toolu_hb", w.repoPath), w.bh, nil); err != nil {
		t.Fatal(err)
	}
	if wins := windowsIn(t, w.semDir); len(wins) != 1 {
		t.Fatalf("windows = %+v, want the real wiring to have run", wins)
	}

	records := benchRecords(t, w.repoPath)
	var hook *doctor.BenchRecord
	for i := range records {
		if records[i].Kind == "hook" && records[i].Event == "ToolStepStarted" {
			hook = &records[i]
		}
	}
	if hook == nil || hook.SessionID != "sess-hb" || hook.ToolUseID != "toolu_hb" {
		t.Fatalf("records = %+v, want a ToolStepStarted hook record with identity", records)
	}
	if hook.DurationMS < 100 {
		t.Fatalf("duration = %dms, want the injected process-start origin honored", hook.DurationMS)
	}
}

// Post-hook setup failures produce an error record.
func TestPostOpenFailureRecordsErrorOutcome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	enableBench(t, w.semDir)
	ctx := context.Background()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-er", Provider: "claude-code",
		TurnID: "turn-1", CWD: w.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// A file where the registry directory belongs breaks OpenRegistry.
	if err := os.WriteFile(filepath.Join(w.semDir, "tool-windows"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	post := postBashEvent("sess-er", "toolu_er", w.repoPath, "cmd")
	if completeToolWindow(ctx, "claude-code", post, w.bh, nil, []broker.RawEvent{bashRawEvent("evt-er", "toolu_er", "sess-er")}) {
		t.Fatal("broken registry reported handled")
	}
	records := benchRecords(t, w.repoPath)
	rec := findRecord(records, "toolwindow", "post")
	if rec == nil || rec.Outcome != "error" {
		t.Fatalf("records = %+v, want a classified error record", records)
	}
}
