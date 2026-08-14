package platform

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWriteFileAtomic_NewFileUsesDefaultMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")

	if err := WriteFileAtomic(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Errorf("content = %q, want %q", data, "hello")
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o644 {
			t.Errorf("mode = %o, want 644", fi.Mode().Perm())
		}
	}
}

func TestWriteFileAtomic_PreservesExistingMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WriteFileAtomic(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600 preserved", fi.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if string(data) != "new" {
		t.Errorf("content = %q, want %q", data, "new")
	}
}

func TestWriteFileAtomic_LeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")

	if err := WriteFileAtomic(path, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "out.json" {
			t.Errorf("leftover file %q in destination directory", e.Name())
		}
	}
}

func TestWriteFileAtomic_InjectedReplaceFailureKeepsExistingFile(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.json")
	if err := os.WriteFile(dest, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dest, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Injecting the failure against a regular file discriminates against
	// delete-destination-first replacement, which would leave it absent.
	orig := atomicReplace
	t.Cleanup(func() { atomicReplace = orig })
	wantErr := errors.New("injected replace failure")
	atomicReplace = func(oldPath, newPath string) error { return wantErr }

	err := WriteFileAtomic(dest, []byte("new"), 0o644)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want injected replace failure", err)
	}

	data, readErr := os.ReadFile(dest)
	if readErr != nil || string(data) != "original" {
		t.Errorf("destination = %q err=%v, want original bytes intact", data, readErr)
	}
	if runtime.GOOS != "windows" {
		fi, statErr := os.Stat(dest)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("destination mode = %o, want 600 intact", fi.Mode().Perm())
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "out.json" {
			t.Errorf("leftover file %q after failed replacement", e.Name())
		}
	}
}

func TestWriteFileAtomic_ReplaceFailureKeepsDestinationAndCleansTemp(t *testing.T) {
	dir := t.TempDir()
	// A non-empty directory at the destination makes the final rename
	// fail after the temp file is fully written.
	dest := filepath.Join(dir, "out.json")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "inner"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := WriteFileAtomic(dest, []byte("new"), 0o644); err == nil {
		t.Fatal("expected replacement failure")
	}

	// The previous destination is intact.
	data, err := os.ReadFile(filepath.Join(dest, "inner"))
	if err != nil || string(data) != "keep" {
		t.Errorf("destination damaged: %q err=%v", data, err)
	}
	// No temp artifact remains beside it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "out.json" {
			t.Errorf("leftover file %q after failed replacement", e.Name())
		}
	}
}

func TestUpdateFileLocked_AbsentFilePassesNil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.json")

	got := []byte("sentinel")
	err := UpdateFileLocked(context.Background(), path, 0o644, func(current []byte) ([]byte, error) {
		got = current
		return []byte("created"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("merge received %q, want nil for absent file", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "created" {
		t.Errorf("content = %q, want %q", data, "created")
	}
}

func TestUpdateFileLocked_NilResultWritesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := UpdateFileLocked(context.Background(), path, 0o644, func(current []byte) ([]byte, error) {
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "original" {
		t.Errorf("content = %q, want untouched original", data)
	}

	// A nil result on an absent file must not create it.
	absent := filepath.Join(dir, "absent.json")
	if err := UpdateFileLocked(context.Background(), absent, 0o644, func([]byte) ([]byte, error) {
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(absent); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("absent file was created: stat err = %v", err)
	}
}

func TestUpdateFileLocked_MergeErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("merge failed")
	err := UpdateFileLocked(context.Background(), path, 0o644, func([]byte) ([]byte, error) {
		return nil, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "original" {
		t.Errorf("content = %q, want untouched original", data)
	}
}

func TestUpdateFileLocked_ConcurrentUpdatesLoseNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "counter.json")
	const workers = 8
	const perWorker = 5

	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				errs[w] = UpdateFileLocked(context.Background(), path, 0o644, func(current []byte) ([]byte, error) {
					n := 0
					if current != nil {
						if err := json.Unmarshal(current, &n); err != nil {
							return nil, err
						}
					}
					return json.Marshal(n + 1)
				})
				if errs[w] != nil {
					return
				}
			}
		}(i)
	}
	wg.Wait()
	for w, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", w, err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	if err := json.Unmarshal(data, &n); err != nil {
		t.Fatal(err)
	}
	if n != workers*perWorker {
		t.Errorf("counter = %d, want %d (lost updates)", n, workers*perWorker)
	}
}

func TestUpdateFileLocked_PreCancelledContext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := UpdateFileLocked(ctx, path, 0o644, func([]byte) ([]byte, error) {
		t.Error("merge ran despite cancelled context")
		return []byte("clobbered"), nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "original" {
		t.Errorf("content = %q, want untouched original", data)
	}
	assertNoTempFiles(t, dir)
}

func TestUpdateFileLocked_ContentionHonorsContext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Hold the sidecar lock so the update has to wait, then time out.
	lockPath := lockPathFor(path)
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	ok, err := TryLockFile(f)
	if err != nil || !ok {
		t.Fatalf("acquire lock: ok=%v err=%v", ok, err)
	}
	defer func() { _ = UnlockFile(f) }()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err = UpdateFileLocked(ctx, path, 0o644, func([]byte) ([]byte, error) {
		t.Error("merge ran while lock was held elsewhere")
		return []byte("clobbered"), nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "original" {
		t.Errorf("content = %q, want untouched original", data)
	}
	assertNoTempFiles(t, dir)
}

// assertNoTempFiles fails if dir contains anything besides cfg.json.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover temp file %q", e.Name())
		}
	}
}
