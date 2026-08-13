package toolsnap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestLinkedWorktreeUsesDistinctIdentityAndSharedObjects verifies
// that a linked worktree resolves its own worktree identity and store
// while reading committed objects through the shared common object
// database.
func TestLinkedWorktreeUsesDistinctIdentityAndSharedObjects(t *testing.T) {
	root := testRepo(t)
	ctx := context.Background()

	linked := filepath.Join(t.TempDir(), "linked-wt")
	run(t, root, "git", "worktree", "add", "-q", linked)
	if err := os.MkdirAll(filepath.Join(linked, ".semantica"), 0o755); err != nil {
		t.Fatal(err)
	}

	mainCtx, err := ResolveRepoContext(ctx, root)
	if err != nil {
		t.Fatalf("resolve main: %v", err)
	}
	linkedCtx, err := ResolveRepoContext(ctx, linked)
	if err != nil {
		t.Fatalf("resolve linked: %v", err)
	}
	if linkedCtx.WorktreeID == mainCtx.WorktreeID {
		t.Errorf("linked worktree shares identity %q with main", linkedCtx.WorktreeID)
	}
	if linkedCtx.CommonDir != mainCtx.CommonDir {
		t.Errorf("common dir differs: %q vs %q", linkedCtx.CommonDir, mainCtx.CommonDir)
	}

	s, err := OpenStore(ctx, linkedCtx, filepath.Join(linked, ".semantica"))
	if err != nil {
		t.Fatalf("open store in linked worktree: %v", err)
	}
	writeFile(t, linked, "a.txt", "linked worktree change\n")
	snap, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	// The tree must mix a store-written blob (the dirty file) with
	// committed blobs resolved through the common-object alternate.
	dirty := run(t, linked, "git", "--git-dir", s.Dir, "cat-file", "blob", snap.TreeHash+":a.txt")
	if dirty != "linked worktree change\n" {
		t.Errorf("dirty blob = %q", dirty)
	}
	committed := run(t, linked, "git", "--git-dir", s.Dir, "cat-file", "blob", snap.TreeHash+":sub/b.txt")
	if committed != "beta\n" {
		t.Errorf("committed blob through alternate = %q", committed)
	}

	// Ref namespaces must not collide between worktrees.
	mainRef := SnapshotRef(mainCtx.WorktreeID, "g", "t")
	linkedRef := SnapshotRef(linkedCtx.WorktreeID, "g", "t")
	if mainRef == linkedRef {
		t.Errorf("worktree refs collide: %q", mainRef)
	}
}

// TestSha256RepositoryGetsMatchingStore verifies object-format
// propagation and end-to-end capture in a SHA-256 repository.
func TestSha256RepositoryGetsMatchingStore(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	run(t, root, "git", "init", "-q", "-b", "main", "--object-format=sha256")
	run(t, root, "git", "config", "user.email", "t@example.com")
	run(t, root, "git", "config", "user.name", "t")
	writeFile(t, root, "a.txt", "alpha\n")
	run(t, root, "git", "add", ".")
	run(t, root, "git", "commit", "-q", "-m", "init")
	if err := os.MkdirAll(filepath.Join(root, ".semantica"), 0o755); err != nil {
		t.Fatal(err)
	}

	rc, err := ResolveRepoContext(ctx, root)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if rc.ObjectFormat != "sha256" {
		t.Fatalf("object format = %q", rc.ObjectFormat)
	}
	s, err := OpenStore(ctx, rc, filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	pre, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("pre capture: %v", err)
	}
	if len(pre.TreeHash) != 64 {
		t.Errorf("sha256 tree hash length = %d", len(pre.TreeHash))
	}
	writeFile(t, root, "a.txt", "alpha rewritten\n")
	post, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("post capture: %v", err)
	}
	changes, err := s.DiffTrees(ctx, pre.TreeHash, post.TreeHash)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "a.txt" || len(changes[0].AfterHash) != 64 {
		t.Errorf("changes = %+v", changes)
	}
}

// TestSha1StoreRejectedForSha256Repository verifies the format
// compatibility probe fails closed instead of writing mixed-format
// trees.
func TestSha1StoreRejectedForSha256Repository(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	run(t, root, "git", "init", "-q", "-b", "main", "--object-format=sha256")
	if err := os.MkdirAll(filepath.Join(root, ".semantica"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A pre-existing sha1 store, as if the repository were re-created
	// with a different format under the same .semantica directory.
	storeDir := filepath.Join(root, ".semantica", storeDirName)
	run(t, root, "git", "init", "-q", "--bare", "--object-format=sha1", storeDir)

	rc, err := ResolveRepoContext(ctx, root)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := OpenStore(ctx, rc, filepath.Join(root, ".semantica")); !errors.Is(err, ErrStoreIncompatible) {
		t.Fatalf("err = %v, want ErrStoreIncompatible", err)
	}
}

// transientObjectRace identifies expected failures while repository
// maintenance moves objects between loose and packed storage.
func transientObjectRace(err error) bool {
	var pe *PartialError
	if errors.As(err, &pe) {
		return true
	}
	msg := err.Error()
	for _, marker := range []string{
		"bad object", "unable to read", "not a tree object",
		"bad file", "does not exist", "missing",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// TestCaptureConcurrentWithUserMaintenance verifies capture remains valid or
// fails cleanly while the user repository is repacked.
func TestCaptureConcurrentWithUserMaintenance(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	// Confirm maintenance is available before starting the race.
	if _, err := gitOutput(ctx, root, "repack", "-adq"); err != nil {
		t.Fatalf("maintenance handshake: %v", err)
	}

	var cycles atomic.Int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := gitOutput(ctx, root, "repack", "-adq"); err != nil {
				continue
			}
			if _, err := gitOutput(ctx, root, "prune-packed", "-q"); err != nil {
				continue
			}
			cycles.Add(1)
		}
	}()
	stopMaintenance := sync.OnceFunc(func() {
		close(stop)
		<-done
	})
	defer stopMaintenance()

	captured := 0
	for i := 0; i < 60 && (captured < 5 || cycles.Load() < 1); i++ {
		writeFile(t, root, "churn.txt", fmt.Sprintf("iteration %d\n", i))
		snap, err := s.CaptureBefore(ctx)
		if err != nil {
			if !transientObjectRace(err) {
				t.Errorf("capture %d unexpected error under maintenance: %v", i, err)
			}
			continue
		}
		if _, err := s.git(ctx, "ls-tree", snap.TreeHash); err != nil {
			if !transientObjectRace(err) {
				t.Errorf("tree read %d unexpected error under maintenance: %v", i, err)
			}
			continue
		}
		captured++
	}
	stopMaintenance()

	if got := cycles.Load(); got == 0 {
		t.Error("no maintenance cycle completed during the capture loop")
	}
	if captured == 0 {
		t.Error("no capture succeeded under maintenance")
	}
	if out := run(t, root, "git", "fsck", "--no-progress"); strings.Contains(out, "error") {
		t.Errorf("user repository fsck reported errors:\n%s", out)
	}
	if out := run(t, root, "git", "--git-dir", s.Dir, "fsck", "--no-progress"); strings.Contains(out, "error") {
		t.Errorf("snapshot store fsck reported errors:\n%s", out)
	}
}

// partialClone creates a blob-filtered clone with an unreachable promisor.
// Local file transport requires uploadpack.allowFilter on the source.
func partialClone(t *testing.T, root string) string {
	t.Helper()
	run(t, root, "git", "config", "uploadpack.allowFilter", "true")
	clone := filepath.Join(t.TempDir(), "partial")
	run(t, filepath.Dir(clone), "git", "clone", "-q", "--filter=blob:none", "file://"+root, clone)
	run(t, clone, "git", "config", "user.email", "t@example.com")
	run(t, clone, "git", "config", "user.name", "t")
	run(t, clone, "git", "remote", "set-url", "origin", "file:///nonexistent-remote-path")
	if err := os.MkdirAll(filepath.Join(clone, ".semantica"), 0o755); err != nil {
		t.Fatal(err)
	}
	return clone
}

// TestPartialCloneCaptureDoesNotFetch verifies capture succeeds in a
// blob:none clone with an unreachable remote: snapshot construction
// reads only worktree bytes and never materializes promised blobs.
func TestPartialCloneCaptureDoesNotFetch(t *testing.T) {
	root := testRepo(t)
	clone := partialClone(t, root)
	ctx := context.Background()

	rc, err := ResolveRepoContext(ctx, clone)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	s, err := OpenStore(ctx, rc, filepath.Join(clone, ".semantica"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	pre, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("clean capture in partial clone: %v", err)
	}
	writeFile(t, clone, "a.txt", "partial clone edit\n")
	post, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("dirty capture in partial clone: %v", err)
	}
	changes, err := s.DiffTrees(ctx, pre.TreeHash, post.TreeHash)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "a.txt" {
		t.Fatalf("changes = %+v", changes)
	}
	// The after blob is worktree content written to the store and must
	// be readable without any remote.
	after, err := s.ReadBlob(ctx, changes[0].AfterHash)
	if err != nil {
		t.Fatalf("read after blob: %v", err)
	}
	if string(after) != "partial clone edit\n" {
		t.Errorf("after blob = %q", after)
	}
}

// TestPartialCloneMissingPromisedBlobFailsWithoutFetch verifies that
// reading a promised-but-absent blob errors promptly instead of
// fetching; this is the alternate_object_missing degradation path.
//
// A blob:none clone materializes HEAD blobs at checkout, so the
// genuinely absent object must come from history: a superseded file
// version is promised but never downloaded.
func TestPartialCloneMissingPromisedBlobFailsWithoutFetch(t *testing.T) {
	root := testRepo(t)
	historicalHash := strings.TrimSpace(run(t, root, "git", "rev-parse", "HEAD:a.txt"))
	writeFile(t, root, "a.txt", "superseding version\n")
	run(t, root, "git", "add", "a.txt")
	run(t, root, "git", "commit", "-q", "-m", "supersede")

	clone := partialClone(t, root)
	ctx := context.Background()

	rc, err := ResolveRepoContext(ctx, clone)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	s, err := OpenStore(ctx, rc, filepath.Join(clone, ".semantica"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// The historical version is promised but absent locally. Store
	// isolation must turn the read into a local missing-object error.
	if _, err := s.ReadBlob(ctx, historicalHash); err == nil {
		t.Fatal("promised blob read succeeded, want missing-object failure")
	}
}

// TestUserRepoPruneInvalidatesAlternateObjects verifies fail-closed
// behavior when user-repository history rewriting plus aggressive
// pruning removes objects a pending snapshot referenced through the
// alternate.
func TestUserRepoPruneInvalidatesAlternateObjects(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	pre, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	ref := SnapshotRef("main", "g1", "t1")
	if err := s.CreateRef(ctx, ref, pre.TreeHash); err != nil {
		t.Fatalf("create ref: %v", err)
	}
	// Resolve a captured blob while the alternate still has it. A
	// clean capture's tree and blobs live only in the user repository.
	lsOut := run(t, root, "git", "--git-dir", s.Dir, "ls-tree", pre.TreeHash)
	blobHash := strings.Fields(lsOut)[2]
	if _, err := s.ReadBlob(ctx, blobHash); err != nil {
		t.Fatalf("blob unreadable before prune: %v", err)
	}

	// Rewrite history so the captured tree's objects become
	// unreachable in the user repository, then prune it.
	run(t, root, "git", "checkout", "-q", "--orphan", "replaced")
	run(t, root, "git", "rm", "-rqf", ".")
	writeFile(t, root, "only.txt", "replacement history\n")
	run(t, root, "git", "add", ".")
	run(t, root, "git", "commit", "-q", "-m", "replacement")
	run(t, root, "git", "branch", "-q", "-D", "main")
	run(t, root, "git", "reflog", "expire", "--expire=now", "--all")
	run(t, root, "git", "gc", "-q", "--prune=now")

	// The private ref protects nothing in the user repository: the
	// pre tree and its blobs lived only there and are now pruned.
	// Both tree enumeration and blob reads must fail cleanly rather
	// than fabricate content.
	if _, err := s.git(ctx, "ls-tree", pre.TreeHash); err == nil {
		t.Error("pruned alternate tree still enumerable, want failure")
	}
	if _, err := s.ReadBlob(ctx, blobHash); err == nil {
		t.Error("pruned alternate blob read succeeded, want failure")
	}
}

// TestExternalDiffEnvironmentCannotShapeEvidence verifies inherited
// diff-related environment cannot alter raw tree comparison.
func TestExternalDiffEnvironmentCannotShapeEvidence(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	t.Setenv("GIT_EXTERNAL_DIFF", "false") // command that always fails
	t.Setenv("GIT_DIFF_OPTS", "-u99")

	pre, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	writeFile(t, root, "a.txt", "external diff must not run\n")
	post, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	changes, err := s.DiffTrees(ctx, pre.TreeHash, post.TreeHash)
	if err != nil {
		t.Fatalf("diff under external-diff env: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "a.txt" || changes[0].Op != 'M' {
		t.Errorf("changes = %+v", changes)
	}
	after, err := s.ReadBlob(ctx, changes[0].AfterHash)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if string(after) != "external diff must not run\n" {
		t.Errorf("evidence bytes = %q", after)
	}
}

// TestGlobalConfigCannotShapeEvidence verifies user-level git config
// (diff drivers, autocrlf, external tools) cannot alter capture or
// comparison output.
func TestGlobalConfigCannotShapeEvidence(t *testing.T) {
	globalDir := t.TempDir()
	globalCfg := filepath.Join(globalDir, "gitconfig")
	if err := os.WriteFile(globalCfg, []byte(
		"[core]\n\tautocrlf = true\n[diff]\n\texternal = false\n\talgorithm = patience\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)

	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	pre, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	writeFile(t, root, "a.txt", "crlf test\r\nsecond line\r\n")
	post, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	changes, err := s.DiffTrees(ctx, pre.TreeHash, post.TreeHash)
	if err != nil {
		t.Fatalf("diff under global config: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %+v", changes)
	}
	after, err := s.ReadBlob(ctx, changes[0].AfterHash)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	// autocrlf must not have filtered the captured bytes.
	if string(after) != "crlf test\r\nsecond line\r\n" {
		t.Errorf("evidence bytes = %q, CRLF filtered by global config", after)
	}
}
