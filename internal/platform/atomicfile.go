package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// atomicLockPollInterval bounds cancellation latency while waiting for a lock.
const atomicLockPollInterval = 25 * time.Millisecond

// atomicReplace publishes the temp file; a test seam for replacement failures.
var atomicReplace = ReplaceFile

// WriteFileAtomic replaces path through a synced sibling file. Existing
// permissions are preserved; defaultMode applies when path does not exist.
func WriteFileAtomic(path string, data []byte, defaultMode os.FileMode) error {
	mode := defaultMode
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	fail := func(step string, err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("%s for %s: %w", step, path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail("write temp", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("sync temp", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return fail("chmod temp", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp for %s: %w", path, err)
	}
	if err := atomicReplace(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// WithFileLock runs fn under a process-shared lock associated with path.
// Waiting for the lock honors ctx.
func WithFileLock(ctx context.Context, path string, fn func() error) error {
	unlock, err := lockSidecar(ctx, lockPathFor(path))
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

// UpdateFileLocked serializes an atomic read-modify-write. merge receives
// nil for a missing file; returning nil leaves the destination unchanged.
func UpdateFileLocked(ctx context.Context, path string, defaultMode os.FileMode, merge func(current []byte) ([]byte, error)) error {
	return WithFileLock(ctx, path, func() error {
		current, err := os.ReadFile(path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("read %s: %w", path, err)
			}
			current = nil
		}
		next, err := merge(current)
		if err != nil {
			return err
		}
		if next == nil {
			return nil
		}
		return WriteFileAtomic(path, next, defaultMode)
	})
}

// lockPathFor derives a stable lock path outside the target directory.
func lockPathFor(target string) string {
	abs, err := filepath.Abs(target)
	if err != nil {
		abs = target
	}
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(os.TempDir(), "semantica-lock-"+hex.EncodeToString(sum[:8])+".lock")
}

// lockSidecar acquires an exclusive lock while honoring ctx.
func lockSidecar(ctx context.Context, lockPath string) (func(), error) {
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", lockPath, err)
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		ok, err := TryLockFile(f)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", lockPath, err)
		}
		if ok {
			break
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(atomicLockPollInterval):
		}
	}
	if err := ctx.Err(); err != nil {
		_ = UnlockFile(f)
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = UnlockFile(f)
		_ = f.Close()
	}, nil
}
