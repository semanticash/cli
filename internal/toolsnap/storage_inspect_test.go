package toolsnap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ensureCoordinationLock creates the lock normally established by capture or
// maintenance.
func ensureCoordinationLock(t *testing.T, reg *Registry) {
	t.Helper()
	if err := os.WriteFile(reg.lockPath(), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// backdateRef ages a loose ref file so it falls before the stale cutoff.
func backdateRef(t *testing.T, s *Store, ref string, age time.Duration) {
	t.Helper()
	old := time.Now().Add(-age)
	path := filepath.Join(s.Dir, filepath.FromSlash(ref))
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

// captureRef captures the current workspace and points a fresh ref at it.
func captureRef(t *testing.T, s *Store, root, file, content, ref string) {
	t.Helper()
	ctx := context.Background()
	writeFile(t, root, file, content)
	snap, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRef(ctx, ref, snap.TreeHash); err != nil {
		t.Fatal(err)
	}
}

func TestInspectStoragePinsCutoffsFromNow(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	ensureCoordinationLock(t, reg)
	now := time.Now()
	insp, err := s.InspectStorage(context.Background(), reg, 0, now)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !insp.RefCutoff.Equal(now.Add(-DefaultStaleWindowAge)) {
		t.Errorf("ref cutoff = %v, want %v", insp.RefCutoff, now.Add(-DefaultStaleWindowAge))
	}
	// grace 0 clamps to DefaultPruneGrace.
	if !insp.ObjectExpire.Equal(now.Add(-DefaultPruneGrace)) {
		t.Errorf("object expire = %v, want %v", insp.ObjectExpire, now.Add(-DefaultPruneGrace))
	}
}

// Inspection and maintenance must classify the same protected, fresh, and
// stale refs.
func TestInspectStorageRefClassificationMatchesMaintain(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	semDir := filepath.Join(root, ".semantica")
	reg, err := OpenRegistry(semDir)
	if err != nil {
		t.Fatal(err)
	}
	ensureCoordinationLock(t, reg)

	// refA: referenced by a retained (complete) window -> protected.
	refA := SnapshotRef("main", "g-a", "tu-a")
	captureRef(t, s, root, "a1.txt", "a-one\n", refA)
	retainGroup(t, reg, "tu-a", refA)

	// refB: unreferenced and aged past the cutoff -> stale-eligible.
	refB := SnapshotRef("main", "g-b", "tu-b")
	captureRef(t, s, root, "b1.txt", "b-one\n", refB)
	backdateRef(t, s, refB, DefaultStaleWindowAge+time.Hour)

	// refC: unreferenced but fresh -> kept.
	refC := SnapshotRef("main", "g-c", "tu-c")
	captureRef(t, s, root, "c1.txt", "c-one\n", refC)

	insp, err := s.InspectStorage(context.Background(), reg, 0, time.Now())
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if insp.Deferred {
		t.Fatalf("inspection deferred: %s", insp.DeferReason)
	}
	if insp.RefsTotal != 3 {
		t.Fatalf("refs total = %d, want 3", insp.RefsTotal)
	}
	if insp.RefsReferenced != 1 {
		t.Errorf("refs referenced = %d, want 1 (refA)", insp.RefsReferenced)
	}
	if insp.RefsFresh != 1 {
		t.Errorf("refs fresh = %d, want 1 (refC)", insp.RefsFresh)
	}
	if insp.RefsStaleEligible != 1 {
		t.Errorf("refs stale-eligible = %d, want 1 (refB)", insp.RefsStaleEligible)
	}
	if insp.RefsKept() != 2 {
		t.Errorf("refs kept = %d, want 2", insp.RefsKept())
	}

	report, err := s.Maintain(context.Background(), reg, 0)
	if err != nil {
		t.Fatalf("maintain: %v", err)
	}
	if report.Deferred {
		t.Fatal("maintain deferred unexpectedly")
	}
	if report.RefsDeleted != insp.RefsStaleEligible {
		t.Errorf("maintain deleted %d refs, inspection said %d eligible", report.RefsDeleted, insp.RefsStaleEligible)
	}
	if report.RefsKept != insp.RefsKept() {
		t.Errorf("maintain kept %d refs, inspection said %d kept", report.RefsKept, insp.RefsKept())
	}

	// The eligible ref is gone; the protected and fresh refs remain.
	refs, err := s.ListRefs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := refs[refB]; ok {
		t.Error("stale-eligible refB survived maintenance")
	}
	if _, ok := refs[refA]; !ok {
		t.Error("protected refA was deleted")
	}
	if _, ok := refs[refC]; !ok {
		t.Error("fresh refC was deleted")
	}
}

// Aged unreachable objects are reported with their disk usage, and pruning
// removes them.
func TestInspectStorageReportsEligibleObjects(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	semDir := filepath.Join(root, ".semantica")
	reg, err := OpenRegistry(semDir)
	if err != nil {
		t.Fatal(err)
	}
	ensureCoordinationLock(t, reg)

	// Capture new content without a ref: the tree and blob are dangling.
	writeFile(t, root, "dangling.txt", "dangling content\n")
	if _, err := s.CaptureBefore(context.Background()); err != nil {
		t.Fatal(err)
	}
	backdateObjects(t, s, DefaultPruneGrace+time.Hour)

	insp, err := s.InspectStorage(context.Background(), reg, 0, time.Now())
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if insp.Deferred {
		t.Fatalf("inspection deferred: %s", insp.DeferReason)
	}
	if insp.EligibleObjects == 0 {
		t.Fatal("expected dangling objects to be eligible")
	}
	if insp.EligibleObjectBytes == 0 {
		t.Error("eligible objects reported zero bytes")
	}

	if _, err := s.Maintain(context.Background(), reg, 0); err != nil {
		t.Fatalf("maintain: %v", err)
	}
	after, err := s.InspectStorage(context.Background(), reg, 0, time.Now())
	if err != nil {
		t.Fatalf("inspect after prune: %v", err)
	}
	if after.EligibleObjects != 0 {
		t.Errorf("eligible objects after prune = %d, want 0", after.EligibleObjects)
	}
}

// An active window defers inspection without measuring eligibility.
func TestInspectStorageDefersWhenWindowActive(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	ensureCoordinationLock(t, reg)
	if _, err := reg.Begin(context.Background(), entry("tu-active", 100)); err != nil {
		t.Fatal(err)
	}
	insp, err := s.InspectStorage(context.Background(), reg, 0, time.Now())
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !insp.Deferred || insp.DeferReason != "active_windows" {
		t.Errorf("deferred=%v reason=%q, want active_windows", insp.Deferred, insp.DeferReason)
	}
	if insp.RefsStaleEligible != 0 || insp.EligibleObjects != 0 {
		t.Error("eligibility measured despite deferral")
	}
}

// While the coordination lock is held, the receipt lock is also held, so
// receipt publication cannot race an inspection's receipt overlay.
func TestCoordinationLockHoldsReceiptLock(t *testing.T) {
	root := testRepo(t)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	ensureCoordinationLock(t, reg)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = reg.WithCoordinationLock(context.Background(), func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	f, err := reg.acquireReceiptLock(ctx)
	close(release)
	<-done
	if err == nil {
		_ = f.Close()
		t.Fatal("receipt lock was acquirable while coordination lock held it")
	}
}

// A second coordination-lock holder (for example post-closure ref release)
// cannot proceed while an inspection owns the lock.
func TestCoordinationLockIsExclusive(t *testing.T) {
	root := testRepo(t)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	ensureCoordinationLock(t, reg)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = reg.WithCoordinationLock(context.Background(), func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	ran := false
	err = reg.WithCoordinationLock(ctx, func() error { ran = true; return nil })
	close(release)
	<-done
	if ran || err == nil {
		t.Fatalf("second holder ran=%v err=%v, want blocked", ran, err)
	}
}

// A lock-free transient mutation during measurement produces a transient_change
// deferral, not a stale reading.
func TestInspectStorageDefersOnTransientMutation(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	ensureCoordinationLock(t, reg)
	tombstones := filepath.Join(root, ".semantica", "tool-windows", "tombstones")
	prev := storageInspectMidSeam
	storageInspectMidSeam = func() {
		_ = os.WriteFile(filepath.Join(tombstones, "deadbeefdeadbeef"), []byte("{}"), 0o644)
	}
	defer func() { storageInspectMidSeam = prev }()

	insp, err := s.InspectStorage(context.Background(), reg, 0, time.Now())
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !insp.Deferred || insp.DeferReason != "transient_change" {
		t.Errorf("deferred=%v reason=%q, want transient_change", insp.Deferred, insp.DeferReason)
	}
}

// A registry read error whose transient dirs moved during the read becomes a
// transient_change deferral.
func TestInspectStorageReadErrorWithTransientChangeDefers(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	ensureCoordinationLock(t, reg)
	tombstones := filepath.Join(root, ".semantica", "tool-windows", "tombstones")
	prev := registrySnapshot
	registrySnapshot = func(string) (RegistrySnapshot, error) {
		// Simulate a lock-free publication racing the read.
		_ = os.WriteFile(filepath.Join(tombstones, "cafebabecafebabe"), []byte("{}"), 0o644)
		return RegistrySnapshot{}, errors.New("simulated transient read race")
	}
	defer func() { registrySnapshot = prev }()

	insp, err := s.InspectStorage(context.Background(), reg, 0, time.Now())
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !insp.Deferred || insp.DeferReason != "transient_change" {
		t.Errorf("deferred=%v reason=%q, want transient_change", insp.Deferred, insp.DeferReason)
	}
}

// A registry read error with unchanged transient dirs remains a hard error.
func TestInspectStorageReadErrorWithoutTransientChangeErrors(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	ensureCoordinationLock(t, reg)
	sentinel := errors.New("simulated corrupt registry state")
	prev := registrySnapshot
	registrySnapshot = func(string) (RegistrySnapshot, error) {
		return RegistrySnapshot{}, sentinel
	}
	defer func() { registrySnapshot = prev }()

	insp, err := s.InspectStorage(context.Background(), reg, 0, time.Now())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if insp.Deferred {
		t.Error("corrupt-state error reported as deferral")
	}
}

// Caller cancellation is a hard error, not a deferral.
func TestInspectStorageCancellationIsError(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	ensureCoordinationLock(t, reg)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	insp, err := s.InspectStorage(ctx, reg, 0, time.Now())
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if insp.Deferred {
		t.Error("cancellation reported as deferral")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want identity-preserving context.Canceled", err)
	}
}

// A registry from another repository must be rejected, not silently locked.
func TestInspectStorageRejectsForeignRegistry(t *testing.T) {
	rootA := testRepo(t)
	s := openTestStore(t, rootA)
	rootB := testRepo(t)
	foreign, err := OpenRegistry(filepath.Join(rootB, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InspectStorage(context.Background(), foreign, 0, time.Now()); err == nil {
		t.Fatal("expected error for mismatched registry")
	}
}

func TestParsePruneDryRun(t *testing.T) {
	ids, err := parsePruneDryRun("abc123 blob\ndef456 tree\n")
	if err != nil {
		t.Fatalf("valid parse: %v", err)
	}
	if len(ids) != 2 || ids[0] != "abc123" || ids[1] != "def456" {
		t.Fatalf("ids = %v", ids)
	}
	if got, err := parsePruneDryRun("  \n"); err != nil || len(got) != 0 {
		t.Errorf("blank output: ids=%v err=%v", got, err)
	}
	for _, bad := range []string{"abc123\n", "abc123 blob extra\n", "   \tblob\n"} {
		if _, err := parsePruneDryRun(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestParseObjectSizes(t *testing.T) {
	total, err := parseObjectSizes("a 10\nb 5\n", []string{"a", "b"})
	if err != nil || total != 15 {
		t.Fatalf("valid: total=%d err=%v", total, err)
	}
	if got, err := parseObjectSizes("", nil); err != nil || got != 0 {
		t.Errorf("empty ids: %d %v", got, err)
	}
	cases := map[string]struct {
		out string
		ids []string
	}{
		"missing":       {"a missing\n", []string{"a"}},
		"malformed":     {"noSpaceLine\n", []string{"a"}},
		"negative":      {"a -3\n", []string{"a"}},
		"nonnumeric":    {"a xyz\n", []string{"a"}},
		"duplicate":     {"a 3\na 4\n", []string{"a"}},
		"unmatched":     {"b 5\n", []string{"a"}},
		"tooFew":        {"a 5\n", []string{"a", "b"}},
		"missingForOne": {"a 5\nb missing\n", []string{"a", "b"}},
	}
	for name, c := range cases {
		if _, err := parseObjectSizes(c.out, c.ids); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestParseCountObjects(t *testing.T) {
	good := "count: 3\nsize: 4\nin-pack: 2\npacks: 1\nsize-pack: 8\nprune-packable: 0\ngarbage: 1\nsize-garbage: 2\n"
	oc, err := parseCountObjects(good)
	if err != nil {
		t.Fatalf("valid: %v", err)
	}
	if oc.TotalObjects != 5 || oc.TotalBytes != (4+8)*1024 || oc.GarbageCount != 1 || oc.GarbageBytes != 2*1024 {
		t.Fatalf("parsed = %+v", oc)
	}
	base := map[string]string{
		"count": "3", "size": "4", "in-pack": "2",
		"size-pack": "8", "garbage": "1", "size-garbage": "2",
	}
	build := func(mut func(map[string]string) string) string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		return mut(m)
	}
	render := func(m map[string]string) string {
		var b strings.Builder
		for k, v := range m {
			b.WriteString(k + ": " + v + "\n")
		}
		return b.String()
	}
	// missing field
	if _, err := parseCountObjects(build(func(m map[string]string) string { delete(m, "size"); return render(m) })); err == nil {
		t.Error("missing field: expected error")
	}
	// malformed value
	if _, err := parseCountObjects(build(func(m map[string]string) string { m["count"] = "x"; return render(m) })); err == nil {
		t.Error("malformed value: expected error")
	}
	// negative value
	if _, err := parseCountObjects(build(func(m map[string]string) string { m["garbage"] = "-1"; return render(m) })); err == nil {
		t.Error("negative value: expected error")
	}
	// duplicate field
	if _, err := parseCountObjects(render(base) + "count: 9\n"); err == nil {
		t.Error("duplicate field: expected error")
	}
}

// While the registry lock is held (by capture or maintenance), the inspection
// cannot acquire it and defers rather than reading inconsistent state.
func TestInspectStorageDefersUnderConcurrentLock(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	ensureCoordinationLock(t, reg)

	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = reg.withLock(context.Background(), func(*registryState) (bool, error) {
			close(entered)
			<-release
			return false, nil
		})
	}()
	<-entered

	insp, err := s.InspectStorage(context.Background(), reg, 0, time.Now())
	close(release)
	<-done
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !insp.Deferred || insp.DeferReason != "lock_timeout" {
		t.Errorf("deferred=%v reason=%q, want lock_timeout", insp.Deferred, insp.DeferReason)
	}
}

func TestRefProtectionSetsIncludesFinals(t *testing.T) {
	windows := []PendingToolSnapshot{{SnapshotRef: "refs/w", TreeHash: "tree-w"}}
	finals := map[string]GroupFinal{"g1": {PostTreeHash: "tree-f"}}
	referenced, protectedTrees := refProtectionSets(windows, finals)
	if !referenced["refs/w"] {
		t.Error("window ref not marked referenced")
	}
	if !protectedTrees["tree-w"] {
		t.Error("window tree not protected")
	}
	if !protectedTrees["tree-f"] {
		t.Error("group-final post tree not protected")
	}
}

// refStaleEligible is the shared rule; cover each branch, including cutoff
// equality and unreadable/packed refs.
func TestRefStaleEligibleBranches(t *testing.T) {
	dir := t.TempDir()
	refName := "refs/semantica/tool-windows/x"
	path := filepath.Join(dir, filepath.FromSlash(refName))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("deadbeef\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	referenced := map[string]bool{"refs/referenced": true}
	protected := map[string]bool{"protected-tree": true}

	// Referenced ref: never eligible.
	if refStaleEligible(dir, "refs/referenced", "any", referenced, protected, time.Now()) {
		t.Error("referenced ref reported eligible")
	}
	// Target-protected ref: never eligible (this is the Finals-protected path).
	if refStaleEligible(dir, refName, "protected-tree", referenced, protected, time.Now()) {
		t.Error("target-protected ref reported eligible")
	}
	// Unreadable/packed ref: no loose file, never eligible.
	if refStaleEligible(dir, "refs/missing", "t", referenced, protected, time.Now()) {
		t.Error("unreadable ref reported eligible")
	}
	if !refIsUnreadable(dir, "refs/missing") {
		t.Error("missing ref not reported unreadable")
	}
	// Fresh unprotected ref: newer than cutoff, not eligible.
	if refStaleEligible(dir, refName, "t", referenced, protected, time.Now().Add(-time.Hour)) {
		t.Error("fresh ref reported eligible")
	}
	// Cutoff equality: mtime exactly equal to cutoff is eligible (not After).
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !refStaleEligible(dir, refName, "t", referenced, protected, fi.ModTime()) {
		t.Error("ref at exactly the cutoff should be eligible")
	}
	// Older than cutoff: eligible.
	if !refStaleEligible(dir, refName, "t", referenced, protected, fi.ModTime().Add(time.Second)) {
		t.Error("aged unprotected ref should be eligible")
	}
}
