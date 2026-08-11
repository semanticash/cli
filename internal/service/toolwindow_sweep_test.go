package service

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/doctor"
	"github.com/semanticash/cli/internal/toolsnap"
)

// A sweep records recovery progress and snapshot-store size.
func TestSweepEmitsBenchRecord(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()
	enableSemantica(t, ctx, dir)
	benchDir := filepath.Join(dir, ".semantica", "doctor")
	if err := os.MkdirAll(benchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(benchDir, "bench.enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// One recoverable partial: record plus durable event, link missing.
	reg, err := toolsnap.OpenRegistry(filepath.Join(dir, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	evtID := strings.Repeat("ab", 32)
	if _, err := reg.LoadOrRecordPendingPartial(toolsnap.PendingPartialRecord{
		Key: toolsnap.ToolKey{
			RepositoryID: "r1", Provider: "claude_code",
			SessionID: "s1", TurnID: "t1", ToolUseID: "tu-sw",
		},
		EventID: evtID, Reason: "pre_snapshot_missing",
		ToolName: "Bash", CommandSummary: "cmd", Timestamp: 5000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := broker.WriteEventsToRepo(ctx, dir, []broker.RawEvent{{
		EventID: evtID, SourceKey: "/data/s1.jsonl", Provider: "claude_code",
		Timestamp: 5000, Kind: "assistant", Role: "assistant",
		TurnID: "t1", ToolUseID: "tu-sw", ToolName: "Bash", EventSource: "hook",
		ProviderSessionID: "s1", SessionStartedAt: 1500,
		SessionMetaJSON: `{"source_key":"x"}`,
	}}, nil); err != nil {
		t.Fatal(err)
	}

	SweepToolWindows(ctx)

	f, err := os.Open(doctor.BenchLogPath(dir))
	if err != nil {
		t.Fatalf("bench log: %v", err)
	}
	defer func() { _ = f.Close() }()
	found := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r doctor.BenchRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("bench record malformed: %v", err)
		}
		if r.Kind == "toolwindow_sweep" {
			found = true
			if r.PartialsReplayed != 1 || r.SweepErrors != 0 {
				t.Fatalf("sweep record = %+v, want one replayed partial", r)
			}
		}
	}
	if !found {
		t.Fatal("no toolwindow_sweep record emitted")
	}
}

// A failed sweep emits an error record.
func TestSweepFailureEmitsErrorRecord(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()
	enableSemantica(t, ctx, dir)
	benchDir := filepath.Join(dir, ".semantica", "doctor")
	if err := os.MkdirAll(benchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(benchDir, "bench.enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Break the repository so the sweep cannot resolve it.
	if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}

	SweepToolWindows(ctx)

	f, err := os.Open(doctor.BenchLogPath(dir))
	if err != nil {
		t.Fatalf("bench log: %v", err)
	}
	defer func() { _ = f.Close() }()
	found := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r doctor.BenchRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("bench record malformed: %v", err)
		}
		if r.Kind == "toolwindow_sweep" && r.Outcome == "error" && r.SweepErrors == 1 {
			found = true
		}
	}
	if !found {
		t.Fatal("failed sweep left no telemetry record")
	}
}
