//go:build !windows

package toolsnap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A FIFO cannot be represented as a Git tree entry.
func TestFifoReplacingTrackedFileFailsPartial(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	abs := filepath.Join(root, "a.txt")
	if err := os.Remove(abs); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(abs, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := s.CaptureBefore(context.Background())
	var pe *PartialError
	if !errors.As(err, &pe) || pe.Reason != ReasonUnsupportedPath {
		t.Fatalf("err = %v, want PartialError %s", err, ReasonUnsupportedPath)
	}
}

// Newline-named files use the same byte budget as other files.
func TestNewlinePathBytesCountTowardBudget(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	s.MaxBytesRead = 8
	writeFile(t, root, "line1\nline2.txt", strings.Repeat("overflow\n", 4))
	_, err := s.CaptureBefore(context.Background())
	var pe *PartialError
	if !errors.As(err, &pe) || pe.Reason != ReasonByteLimit {
		t.Fatalf("err = %v, want PartialError %s", err, ReasonByteLimit)
	}
}

// Newline-named files must enter exact blob-size accounting.
func TestNewlinePathBlobEntersExactAccounting(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	content := "newline path content\n"
	writeFile(t, root, "line1\nline2.txt", content)

	entries, written, err := s.hashWorktreePaths(context.Background(), []string{"line1\nline2.txt"})
	if err != nil {
		t.Fatalf("hash paths: %v", err)
	}
	if len(entries) != 1 || len(written) != 1 {
		t.Fatalf("entries = %v, written = %v, want one of each", entries, written)
	}
	size, err := s.blobSizeSum(context.Background(), written)
	if err != nil {
		t.Fatalf("blob size sum: %v", err)
	}
	if size != int64(len(content)) {
		t.Errorf("accounted bytes = %d, want %d", size, len(content))
	}
}

// Symlink targets count toward the byte budget before objects are written.
func TestSymlinkTargetBytesCountTowardBudget(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	s.MaxBytesRead = 8
	if err := os.Symlink("a-target-path-longer-than-the-budget", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	_, err := s.CaptureBefore(context.Background())
	var pe *PartialError
	if !errors.As(err, &pe) || pe.Reason != ReasonByteLimit {
		t.Fatalf("err = %v, want PartialError %s", err, ReasonByteLimit)
	}
	// Nothing may have been written: the store must hold zero loose
	// objects after the rejected capture.
	count := run(t, root, "git", "--git-dir", s.Dir, "count-objects")
	if !strings.HasPrefix(count, "0 objects") {
		t.Errorf("store objects after rejected capture: %q, want none", count)
	}
}
