package util

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/semanticash/cli/internal/platform"
)

// RoutingDecisionLogBasename names the log in AppConfigDir. Global storage
// includes unresolved events that have no destination repository.
const RoutingDecisionLogBasename = "routing-decisions.log"

// routingDecisionLogMaxSize triggers retention of the newest half of the log
// at a record boundary. Tests may override it.
var routingDecisionLogMaxSize int64 = 5 * 1024 * 1024

// Each lock has a bounded retry count; exhausted retries skip the batch.
// Tests may override these limits.
var (
	routingLockAttempts   = 20
	routingLockRetryDelay = 5 * time.Millisecond
)

// RoutingDecisionEntry records one event/destination pair as JSONL.
// Use (EventID, Repo) to deduplicate retries. Persisted reports write success
// for that destination; unresolved entries have an empty Repo and Persisted=false.
//
// SignalDiff reports a difference from the session repository, not an
// attribution error.
type RoutingDecisionEntry struct {
	Timestamp   string `json:"ts"`
	EventID     string `json:"event_id"`
	Provider    string `json:"provider,omitempty"`
	Signal      string `json:"signal"`
	Repo        string `json:"repo,omitempty"`
	SessionRepo string `json:"session_repo,omitempty"`
	SignalDiff  bool   `json:"signal_diff,omitempty"`
	Persisted   bool   `json:"persisted"`
}

var routingDecisionMu sync.Mutex

// AppendRoutingDecisions appends entries to the global routing log.
// Rotation and append share a cross-process lock. Lock retries are bounded,
// and diagnostic failures are not returned to capture callers.
func AppendRoutingDecisions(entries []RoutingDecisionEntry) {
	if len(entries) == 0 {
		return
	}
	dir, err := AppConfigDir()
	if err != nil {
		return
	}

	// Limit mutex retries as well as file-lock retries.
	if !acquireRoutingMutex() {
		return
	}
	defer routingDecisionMu.Unlock()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, RoutingDecisionLogBasename)

	// A separate lock file preserves lock identity across log rotation.
	lockFile, ok := acquireRoutingLock(dir)
	if !ok {
		return
	}
	defer func() {
		_ = platform.UnlockFile(lockFile)
		_ = lockFile.Close()
	}()

	if err := truncateRoutingLogIfTooLarge(path); err != nil {
		_ = err // best-effort; continue to append
	}

	// Open after rotation to avoid appending to a replaced file.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	for _, entry := range entries {
		if entry.Timestamp == "" {
			entry.Timestamp = time.Now().UTC().Format(time.RFC3339)
		}
		line, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		_, _ = f.Write(append(line, '\n'))
	}
}

// acquireRoutingMutex returns false when bounded lock retries are exhausted.
func acquireRoutingMutex() bool {
	for attempt := 0; attempt < routingLockAttempts; attempt++ {
		if routingDecisionMu.TryLock() {
			return true
		}
		time.Sleep(routingLockRetryDelay)
	}
	return false
}

// acquireRoutingLock acquires the sidecar lock with bounded retries.
// The caller must unlock and close the returned file. Failure returns (nil, false).
func acquireRoutingLock(dir string) (*os.File, bool) {
	lockPath := filepath.Join(dir, RoutingDecisionLogBasename+".lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false
	}
	for attempt := 0; attempt < routingLockAttempts; attempt++ {
		locked, err := platform.TryLockFile(lockFile)
		if err != nil {
			_ = lockFile.Close()
			return nil, false
		}
		if locked {
			return lockFile, true
		}
		time.Sleep(routingLockRetryDelay)
	}
	_ = lockFile.Close()
	return nil, false
}

func truncateRoutingLogIfTooLarge(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() < routingDecisionLogMaxSize {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	keep := info.Size() / 2
	if _, err := f.Seek(info.Size()-keep, io.SeekStart); err != nil {
		return err
	}
	tail, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	if i := indexOfByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, tail, 0o644); err != nil {
		return err
	}
	return platform.SafeRename(tmp, path)
}

// ReadRoutingDecisionTail parses the last maxLines lines, skipping invalid JSON.
// A missing log returns no entries and no error.
func ReadRoutingDecisionTail(maxLines int) ([]RoutingDecisionEntry, error) {
	dir, err := AppConfigDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, RoutingDecisionLogBasename)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	body, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	lines := splitLines(body)
	start := 0
	if len(lines) > maxLines {
		start = len(lines) - maxLines
	}
	entries := make([]RoutingDecisionEntry, 0, len(lines)-start)
	for _, line := range lines[start:] {
		if len(line) == 0 {
			continue
		}
		var e RoutingDecisionEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// RoutingDecisionLogPath returns the global routing log path.
func RoutingDecisionLogPath() (string, error) {
	dir, err := AppConfigDir()
	if err != nil {
		return "", fmt.Errorf("routing-decisions log path: %w", err)
	}
	return filepath.Join(dir, RoutingDecisionLogBasename), nil
}
