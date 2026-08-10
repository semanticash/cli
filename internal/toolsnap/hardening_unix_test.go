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
	"time"
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

// Failed publication must leave a finalized group open for retry.
func TestClosedFalseWhenPublicationFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	r, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := r.Begin(ctx, entry("tu1", 100)); err != nil {
		t.Fatal(err)
	}

	finalized := false
	closed, err := r.Complete(ctx, key("tu1"), 200, func([]PendingToolSnapshot, *GroupFinal) (FinalizeResult, error) {
		finalized = true
		// Deny the state publication that follows finalization.
		if err := os.Chmod(r.dir, 0o500); err != nil {
			t.Fatal(err)
		}
		return FinalizeResult{Done: true}, nil
	})
	defer func() { _ = os.Chmod(r.dir, 0o755) }()
	if !finalized {
		t.Fatal("finalize did not run")
	}
	if closed || err == nil {
		t.Fatalf("closed=%v err=%v, want closed=false with publication error", closed, err)
	}
	if err := os.Chmod(r.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale, err := r.Stale(ctx, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 {
		t.Fatalf("group lost after failed publication: %+v", stale)
	}
}

// A failed registry publication leaves one ref for bounded cleanup.
func TestCaptureAndBeginPublicationFailureLeavesBoundedOrphan(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	root := testRepo(t)
	s := openTestStore(t, root)
	semDir := filepath.Join(root, ".semantica")
	reg, err := OpenRegistry(semDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Keep locking available while preventing registry publication.
	if f, err := os.OpenFile(reg.lockPath(), os.O_RDWR|os.O_CREATE, 0o644); err != nil {
		t.Fatal(err)
	} else {
		_ = f.Close()
	}
	if err := os.Chmod(reg.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(reg.dir, 0o755) }()

	writeFile(t, root, "a.txt", "captured then orphaned\n")
	_, err = reg.CaptureAndBegin(ctx, s, key("tu-orphan"), "Bash", 100)
	if err == nil {
		t.Fatal("publication failure not reported")
	}
	if err := os.Chmod(reg.dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Registry state remains unchanged.
	stale, err := reg.Stale(ctx, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("registry gained windows despite failed publication: %+v", stale)
	}
	// Maintenance retains the fresh orphan.
	refs, err := s.ListRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs = %v, want the single orphan", refs)
	}
	report, err := s.Maintain(ctx, reg, 0)
	if err != nil || report.Deferred || report.RefsKept != 1 || report.RefsDeleted != 0 {
		t.Fatalf("fresh orphan handling: report=%+v err=%v", report, err)
	}
	// Maintenance removes the stale orphan.
	for ref := range refs {
		old := time.Now().Add(-DefaultStaleWindowAge - time.Hour)
		if err := os.Chtimes(filepath.Join(s.Dir, filepath.FromSlash(ref)), old, old); err != nil {
			t.Fatal(err)
		}
	}
	report, err = s.Maintain(ctx, reg, 0)
	if err != nil || report.RefsDeleted != 1 {
		t.Fatalf("aged orphan handling: report=%+v err=%v", report, err)
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
