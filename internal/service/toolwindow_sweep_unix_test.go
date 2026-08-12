//go:build !windows

package service

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/semanticash/cli/internal/doctor"
	"github.com/semanticash/cli/internal/toolsnap"
)

// Sweep telemetry counts recovery and terminal errors together.
func TestSweepErrorRecordSumsCompoundErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
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

	reg, err := toolsnap.OpenRegistry(filepath.Join(dir, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	mk := func(tool string, at int64) toolsnap.PendingToolSnapshot {
		return toolsnap.PendingToolSnapshot{
			Key: toolsnap.ToolKey{
				RepositoryID: "r1", Provider: "claude_code",
				SessionID: "s1", TurnID: "t1", ToolUseID: tool,
			},
			ToolName: "Bash", SnapshotRef: "refs/x", TreeHash: "th",
			HeadHash: "hh", ObjectFormat: "sha1", StartedAt: at,
		}
	}
	if _, err := reg.Begin(ctx, mk("tu-a", 1000)); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Begin(ctx, mk("tu-b", 1001)); err != nil {
		t.Fatal(err)
	}

	// Fail tombstone writes during reclamation.
	tombstones := filepath.Join(dir, ".semantica", "tool-windows", "tombstones")
	if err := os.Chmod(tombstones, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(tombstones, 0o755) }()
	// Fail the later store initialization.
	storePath := filepath.Join(dir, ".semantica", "tool-snapshots.git")
	if err := os.RemoveAll(storePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(storePath, []byte("not a git dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	SweepToolWindows(ctx)
	_ = os.Chmod(tombstones, 0o755)

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
		if r.Kind == "toolwindow_sweep" && r.Outcome == "error" {
			found = true
			if r.SweepErrors != 2 {
				t.Fatalf("error record = %+v, want SweepErrors=2 (reclamation error + store failure)", r)
			}
		}
	}
	if !found {
		t.Fatal("no compound error record emitted")
	}
}
