package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/semanticash/cli/internal/platform"
)

// errRepoLockTimeout reports bounded-wait exhaustion. It never means
// the requested work was handled elsewhere; callers must re-check.
var errRepoLockTimeout = errors.New("repository worker lock timeout")

// repoLockWait bounds how long a worker waits for the repository
// lock. A var so contention tests can shorten it.
var repoLockWait = 2 * time.Minute

const repoLockPoll = 200 * time.Millisecond

// WorkerLockPath returns the repository worker lock location.
func WorkerLockPath(semDir string) string {
	return filepath.Join(semDir, "worker.lock")
}

// RepoLockInfo is diagnostic metadata; the file lock is authoritative.
type RepoLockInfo struct {
	PID          int    `json:"pid"`
	AcquiredAt   int64  `json:"acquired_at"` // unix ms
	CheckpointID string `json:"checkpoint_id,omitempty"`
}

// ReadRepoLockInfo parses holder metadata, best-effort.
func ReadRepoLockInfo(path string) (RepoLockInfo, error) {
	var info RepoLockInfo
	b, err := os.ReadFile(path)
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(b, &info); err != nil {
		return info, err
	}
	return info, nil
}

type repoLock struct {
	f *os.File
}

// acquireRepoLock waits up to wait for exclusive repository ownership.
func acquireRepoLock(ctx context.Context, semDir, checkpointID string, wait time.Duration) (*repoLock, error) {
	path := WorkerLockPath(semDir)
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		return nil, fmt.Errorf("worker lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open worker lock: %w", err)
	}
	deadline := time.Now().Add(wait)
	for {
		ok, err := platform.TryLockFile(f)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("acquire worker lock: %w", err)
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, errRepoLockTimeout
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(repoLockPoll):
		}
	}

	info := RepoLockInfo{PID: os.Getpid(), AcquiredAt: time.Now().UnixMilli(), CheckpointID: checkpointID}
	if b, err := json.Marshal(info); err == nil {
		_ = f.Truncate(0)
		_, _ = f.WriteAt(b, 0)
		_ = f.Sync()
	}
	return &repoLock{f: f}, nil
}

func (l *repoLock) release() {
	_ = platform.UnlockFile(l.f)
	_ = l.f.Close()
}
