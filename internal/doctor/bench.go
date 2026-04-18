package doctor

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const benchEnvVar = "SEMANTICA_DOCTOR_BENCH"

type benchScopeKey struct{}

// BenchStats captures lightweight write activity for one repo during a hook or turn.
type BenchStats struct {
	RowsWritten  int           `json:"-"`
	BlobsWritten int           `json:"-"`
	BytesWritten int64         `json:"-"`
	DBDuration   time.Duration `json:"-"`
	BlobDuration time.Duration `json:"-"`
}

// BenchRecord is the JSONL payload written by the doctor bench recorder.
type BenchRecord struct {
	TS           string `json:"ts"`
	Kind         string `json:"kind"`
	Event        string `json:"event,omitempty"`
	Tool         string `json:"tool,omitempty"`
	TurnID       string `json:"turn_id,omitempty"`
	DurationMS   int64  `json:"duration_ms,omitempty"`
	DBMS         int64  `json:"db_ms,omitempty"`
	BlobMS       int64  `json:"blob_ms,omitempty"`
	RSSMB        int64  `json:"rss_mb,omitempty"`
	CaptureMS    int64  `json:"capture_ms,omitempty"`
	PackageMS    int64  `json:"package_ms,omitempty"`
	RowsWritten  int    `json:"rows_written,omitempty"`
	BlobsWritten int    `json:"blobs_written,omitempty"`
	BytesWritten int64  `json:"bytes_written,omitempty"`
}

// BenchScope aggregates per-repo stats while one hook or turn is being handled.
type BenchScope struct {
	mu      sync.Mutex
	perRepo map[string]BenchStats
}

// WithBenchScope attaches a new aggregation scope to the context.
func WithBenchScope(ctx context.Context) (context.Context, *BenchScope) {
	scope := &BenchScope{}
	return context.WithValue(ctx, benchScopeKey{}, scope), scope
}

// AddBenchStats merges repo-local stats into the current scope, if present.
func AddBenchStats(ctx context.Context, repoPath string, stats BenchStats) {
	if repoPath == "" {
		return
	}
	scope, ok := ctx.Value(benchScopeKey{}).(*BenchScope)
	if !ok || scope == nil {
		return
	}

	scope.mu.Lock()
	defer scope.mu.Unlock()

	if scope.perRepo == nil {
		scope.perRepo = make(map[string]BenchStats)
	}
	current := scope.perRepo[repoPath]
	current.RowsWritten += stats.RowsWritten
	current.BlobsWritten += stats.BlobsWritten
	current.BytesWritten += stats.BytesWritten
	current.DBDuration += stats.DBDuration
	current.BlobDuration += stats.BlobDuration
	scope.perRepo[repoPath] = current
}

// Snapshot returns a copy of the current per-repo stats.
func (s *BenchScope) Snapshot() map[string]BenchStats {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make(map[string]BenchStats, len(s.perRepo))
	for repoPath, stats := range s.perRepo {
		out[repoPath] = stats
	}
	return out
}

// Diff subtracts one snapshot from another.
func Diff(after, before map[string]BenchStats) map[string]BenchStats {
	if len(after) == 0 {
		return nil
	}
	out := make(map[string]BenchStats, len(after))
	for repoPath, stats := range after {
		prev := before[repoPath]
		diff := BenchStats{
			RowsWritten:  stats.RowsWritten - prev.RowsWritten,
			BlobsWritten: stats.BlobsWritten - prev.BlobsWritten,
			BytesWritten: stats.BytesWritten - prev.BytesWritten,
			DBDuration:   stats.DBDuration - prev.DBDuration,
			BlobDuration: stats.BlobDuration - prev.BlobDuration,
		}
		if diff.RowsWritten == 0 && diff.BlobsWritten == 0 && diff.BytesWritten == 0 && diff.DBDuration == 0 && diff.BlobDuration == 0 {
			continue
		}
		out[repoPath] = diff
	}
	return out
}

// EmitBenchRecord appends one JSONL record for the given repo when doctor bench is enabled.
func EmitBenchRecord(repoPath string, record BenchRecord) {
	if !BenchEnabled(repoPath) {
		return
	}
	if record.TS == "" {
		record.TS = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if record.RSSMB == 0 {
		record.RSSMB = currentRSSMB()
	}

	data, err := json.Marshal(record)
	if err != nil {
		slog.Warn("doctor bench: marshal record failed", "repo", repoPath, "err", err)
		return
	}

	logPath := BenchLogPath(repoPath)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		slog.Warn("doctor bench: create log directory failed", "path", logPath, "err", err)
		return
	}

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		slog.Warn("doctor bench: open log failed", "path", logPath, "err", err)
		return
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			slog.Warn("doctor bench: close log failed", "path", logPath, "err", cerr)
		}
	}()

	if _, err := f.Write(append(data, '\n')); err != nil {
		slog.Warn("doctor bench: write log failed", "path", logPath, "err", err)
	}
}

// BenchEnabled returns true when doctor bench recording is enabled for a repo.
func BenchEnabled(repoPath string) bool {
	if enabled := strings.TrimSpace(os.Getenv(benchEnvVar)); enabled != "" {
		switch strings.ToLower(enabled) {
		case "1", "true", "yes", "on":
			return true
		}
	}
	_, err := os.Stat(filepath.Join(repoPath, ".semantica", "doctor", "bench.enabled"))
	return err == nil
}

// BenchLogPath returns the per-repo JSONL output path for doctor bench.
func BenchLogPath(repoPath string) string {
	return filepath.Join(repoPath, ".semantica", "doctor", "bench.jsonl")
}

// Milliseconds converts a duration into integer milliseconds.
func Milliseconds(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return d.Milliseconds()
}
