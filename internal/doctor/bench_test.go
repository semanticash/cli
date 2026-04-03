package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBenchScopeAggregatesByRepo(t *testing.T) {
	t.Parallel()

	ctx, scope := WithBenchScope(t.Context())

	AddBenchStats(ctx, "/repo-a", BenchStats{RowsWritten: 2, BlobsWritten: 1, BytesWritten: 8, DBDuration: 3 * time.Millisecond})
	AddBenchStats(ctx, "/repo-a", BenchStats{RowsWritten: 1, BlobDuration: 7 * time.Millisecond})
	AddBenchStats(ctx, "/repo-b", BenchStats{RowsWritten: 4, BytesWritten: 16})

	got := scope.Snapshot()
	if got["/repo-a"].RowsWritten != 3 {
		t.Fatalf("repo-a rows = %d, want 3", got["/repo-a"].RowsWritten)
	}
	if got["/repo-a"].BlobsWritten != 1 {
		t.Fatalf("repo-a blobs = %d, want 1", got["/repo-a"].BlobsWritten)
	}
	if got["/repo-a"].BytesWritten != 8 {
		t.Fatalf("repo-a bytes = %d, want 8", got["/repo-a"].BytesWritten)
	}
	if got["/repo-a"].DBDuration != 3*time.Millisecond {
		t.Fatalf("repo-a db duration = %s, want 3ms", got["/repo-a"].DBDuration)
	}
	if got["/repo-a"].BlobDuration != 7*time.Millisecond {
		t.Fatalf("repo-a blob duration = %s, want 7ms", got["/repo-a"].BlobDuration)
	}
	if got["/repo-b"].RowsWritten != 4 {
		t.Fatalf("repo-b rows = %d, want 4", got["/repo-b"].RowsWritten)
	}
}

func TestEmitBenchRecord_WritesJSONLWhenEnabled(t *testing.T) {
	t.Setenv(benchEnvVar, "1")
	repoPath := t.TempDir()

	EmitBenchRecord(repoPath, BenchRecord{
		Kind:       "hook",
		Event:      "ToolStepCompleted",
		Tool:       "Edit",
		DurationMS: 42,
		DBMS:       8,
		BlobMS:     11,
	})

	data, err := os.ReadFile(BenchLogPath(repoPath))
	if err != nil {
		t.Fatalf("read bench log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(lines))
	}

	var record BenchRecord
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("unmarshal bench record: %v", err)
	}
	if record.Kind != "hook" {
		t.Fatalf("kind = %q, want hook", record.Kind)
	}
	if record.Tool != "Edit" {
		t.Fatalf("tool = %q, want Edit", record.Tool)
	}
	if record.TS == "" {
		t.Fatal("ts should be populated")
	}
}

func TestBenchEnabled_UsesEnableFile(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	if BenchEnabled(repoPath) {
		t.Fatal("BenchEnabled returned true before enable file exists")
	}

	enablePath := filepath.Join(repoPath, ".semantica", "doctor", "bench.enabled")
	if err := os.MkdirAll(filepath.Dir(enablePath), 0o755); err != nil {
		t.Fatalf("mkdir enable dir: %v", err)
	}
	if err := os.WriteFile(enablePath, nil, 0o644); err != nil {
		t.Fatalf("write enable file: %v", err)
	}

	if !BenchEnabled(repoPath) {
		t.Fatal("BenchEnabled returned false with enable file present")
	}
}
