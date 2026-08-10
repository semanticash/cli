package toolsnap

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/platform"
)

// retainGroup creates a completed group that remains retryable.
func retainGroup(t *testing.T, reg *Registry, tool, ref string) {
	t.Helper()
	ctx := context.Background()
	e := entry(tool, 100)
	e.SnapshotRef = ref
	if _, err := reg.Begin(ctx, e); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("retained for retry")
	if _, err := reg.Complete(ctx, key(tool), 200, func(_ []PendingToolSnapshot, _ *GroupFinal) (FinalizeResult, error) {
		return FinalizeResult{Final: GroupFinal{PartialReason: ReasonLockTimeout}}, boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
}

// backdateObjects ages loose objects past the prune grace period.
func backdateObjects(t *testing.T, s *Store, age time.Duration) {
	t.Helper()
	old := time.Now().Add(-age)
	err := filepath.WalkDir(filepath.Join(s.Dir, "objects"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		return os.Chtimes(path, old, old)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceDefersWhenWindowActive(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Begin(context.Background(), entry("tu-active", 100)); err != nil {
		t.Fatal(err)
	}
	report, err := s.Maintain(context.Background(), reg, 0)
	if err != nil {
		t.Fatalf("maintain: %v", err)
	}
	if !report.Deferred || report.ActiveWindows != 1 || report.RefsDeleted != 0 || report.PruneRan {
		t.Fatalf("report = %+v, want deferred no-op", report)
	}
}

func TestMaintenanceDefersOnLockContention(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(reg.lockPath(), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	if err := platform.LockFile(holder); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	report, err := s.Maintain(context.Background(), reg, 0)
	if err != nil {
		t.Fatalf("maintain under contention: %v", err)
	}
	if !report.Deferred {
		t.Fatalf("report = %+v, want deferred", report)
	}
	// The short lock attempt must not queue behind capture.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("maintenance waited %v", elapsed)
	}
}

func TestMaintenanceDeletesUnreferencedRefsKeepsReferenced(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	writeFile(t, root, "a.txt", "referenced window content\n")
	snap, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	kept := SnapshotRef("main", "g-kept", "tu-kept")
	if err := s.CreateRef(ctx, kept, snap.TreeHash); err != nil {
		t.Fatal(err)
	}
	retainGroup(t, reg, "tu-kept", kept)

	// Capture a distinct tree for unreferenced refs.
	writeFile(t, root, "b.txt", "orphan only\n")
	orphanSnap, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	agedOrphan := SnapshotRef("main", "g-orphan", "tu-orphan")
	if err := s.CreateRef(ctx, agedOrphan, orphanSnap.TreeHash); err != nil {
		t.Fatal(err)
	}
	freshOrphan := SnapshotRef("main", "g-orphan-fresh", "tu-orphan-fresh")
	if err := s.CreateRef(ctx, freshOrphan, orphanSnap.TreeHash); err != nil {
		t.Fatal(err)
	}
	// Only the aged orphan crosses the stale window.
	old := time.Now().Add(-DefaultStaleWindowAge - time.Hour)
	if err := os.Chtimes(filepath.Join(s.Dir, filepath.FromSlash(agedOrphan)), old, old); err != nil {
		t.Fatal(err)
	}

	report, err := s.Maintain(ctx, reg, 0)
	if err != nil {
		t.Fatalf("maintain: %v", err)
	}
	if report.Deferred || report.RefsDeleted != 1 || report.RefsKept != 2 || !report.PruneRan {
		t.Fatalf("report = %+v, want aged orphan deleted, fresh orphan and referenced kept", report)
	}
	refs, err := s.ListRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[kept] != snap.TreeHash || refs[freshOrphan] != orphanSnap.TreeHash {
		t.Fatalf("refs after maintenance = %v", refs)
	}
}

// Mid-pass expiry preserves completed counters and defers remaining work.
func TestMaintenanceMidPassExpiryDefersPreservingProgress(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	writeFile(t, root, "a.txt", "orphan content\n")
	snap, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	orphan := SnapshotRef("main", "g-orphan", "tu-orphan")
	if err := s.CreateRef(ctx, orphan, snap.TreeHash); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-DefaultStaleWindowAge - time.Hour)
	if err := os.Chtimes(filepath.Join(s.Dir, filepath.FromSlash(orphan)), old, old); err != nil {
		t.Fatal(err)
	}

	prevHold := maintenanceMaxHold
	maintenanceMaxHold = 500 * time.Millisecond
	maintenanceBeforePrune = func() { time.Sleep(700 * time.Millisecond) }
	defer func() {
		maintenanceMaxHold = prevHold
		maintenanceBeforePrune = nil
	}()

	report, err := s.Maintain(ctx, reg, 0)
	if err != nil {
		t.Fatalf("maintain: %v", err)
	}
	if !report.Deferred || report.PruneRan {
		t.Fatalf("report = %+v, want deferred before prune", report)
	}
	if report.RefsDeleted != 1 {
		t.Fatalf("report = %+v, want completed ref deletion preserved", report)
	}
}

func TestMaintenancePrunesOnlyPastGraceAndNeverUserRepo(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	writeFile(t, root, "a.txt", "orphaned snapshot content\n")
	snap, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ref := SnapshotRef("main", "g1", "tu1")
	if err := s.CreateRef(ctx, ref, snap.TreeHash); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRef(ctx, ref, snap.TreeHash); err != nil {
		t.Fatal(err)
	}
	countStore := func() string {
		return run(t, root, "git", "--git-dir", s.Dir, "count-objects")
	}
	if strings.HasPrefix(countStore(), "0 objects") {
		t.Fatal("store holds no objects to prune")
	}
	userObjectsBefore := run(t, root, "git", "count-objects", "-v")

	// Within the grace period: unreachable objects survive.
	report, err := s.Maintain(ctx, reg, 0)
	if err != nil {
		t.Fatalf("maintain: %v", err)
	}
	if !report.PruneRan || strings.HasPrefix(countStore(), "0 objects") {
		t.Fatalf("young unreachable objects pruned: %+v", report)
	}

	// Past the grace period: pruned.
	backdateObjects(t, s, DefaultPruneGrace+time.Hour)
	report, err = s.Maintain(ctx, reg, 0)
	if err != nil {
		t.Fatalf("maintain: %v", err)
	}
	if !strings.HasPrefix(countStore(), "0 objects") {
		t.Fatalf("aged unreachable objects survive: %s", countStore())
	}
	if report.StoreBytes != 0 {
		t.Errorf("store bytes after prune = %d", report.StoreBytes)
	}

	// The user repository is untouched throughout.
	if got := run(t, root, "git", "count-objects", "-v"); got != userObjectsBefore {
		t.Errorf("user repository objects changed:\nbefore: %s\nafter: %s", userObjectsBefore, got)
	}
}

// Maintenance preserves refs that keep a retained final tree reachable.
func TestMaintenancePreservesCapturedFinalPostRef(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	writeFile(t, root, "a.txt", "final state awaiting persistence\n")
	post, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	preRef := SnapshotRef("main", "g-cap", "tu-cap")
	if err := s.CreateRef(ctx, preRef, post.TreeHash); err != nil {
		t.Fatal(err)
	}
	postRef := "refs/semantica/tool-windows/main-x/g-cap-x/tu-cap-x/post"
	if err := s.CreateRef(ctx, postRef, post.TreeHash); err != nil {
		t.Fatal(err)
	}

	e := entry("tu-cap", 100)
	e.SnapshotRef = preRef
	if _, err := reg.Begin(ctx, e); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("persist failed")
	if _, err := reg.Complete(ctx, key("tu-cap"), 200, func(_ []PendingToolSnapshot, _ *GroupFinal) (FinalizeResult, error) {
		return FinalizeResult{Final: GroupFinal{PostTreeHash: post.TreeHash, CapturedAt: 200}}, boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}

	backdateObjects(t, s, DefaultPruneGrace+time.Hour)
	report, err := s.Maintain(ctx, reg, 0)
	if err != nil {
		t.Fatalf("maintain: %v", err)
	}
	if report.Deferred || report.RefsDeleted != 0 || report.RefsKept != 2 {
		t.Fatalf("report = %+v, want both refs kept", report)
	}
	// The retained tree remains readable for retry.
	content := run(t, root, "git", "--git-dir", s.Dir, "cat-file", "blob", post.TreeHash+":a.txt")
	if content != "final state awaiting persistence\n" {
		t.Fatalf("captured final tree lost: %q", content)
	}
}

// The pass deadline includes work performed while holding the lock.
func TestMaintenancePassBoundDefers(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	prev := maintenanceMaxHold
	maintenanceMaxHold = time.Nanosecond
	defer func() { maintenanceMaxHold = prev }()

	report, err := s.Maintain(context.Background(), reg, 0)
	if err != nil {
		t.Fatalf("maintain: %v", err)
	}
	if !report.Deferred || report.PruneRan {
		t.Fatalf("report = %+v, want deferred pass", report)
	}
}

// Caller cancellation is returned as an error.
func TestMaintenanceCallerCancellationIsError(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.Maintain(ctx, reg, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want identity-preserving context.Canceled", err)
	}
}

func TestMaintenancePreservesReferencedObjectsRegardlessOfAge(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	writeFile(t, root, "a.txt", "long pending window content\n")
	snap, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ref := SnapshotRef("main", "g-pending", "tu-pending")
	if err := s.CreateRef(ctx, ref, snap.TreeHash); err != nil {
		t.Fatal(err)
	}
	retainGroup(t, reg, "tu-pending", ref)

	backdateObjects(t, s, DefaultPruneGrace+time.Hour)
	if _, err := s.Maintain(ctx, reg, 0); err != nil {
		t.Fatalf("maintain: %v", err)
	}
	// The referenced tree must still be fully readable.
	content := run(t, root, "git", "--git-dir", s.Dir, "cat-file", "blob", snap.TreeHash+":a.txt")
	if content != "long pending window content\n" {
		t.Fatalf("referenced snapshot lost: %q", content)
	}
}
