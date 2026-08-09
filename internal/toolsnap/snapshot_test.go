package toolsnap

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testRepo creates a committed repository with a .semantica directory
// and returns its root.
func testRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	run(t, root, "git", "init", "-q", "-b", "main")
	run(t, root, "git", "config", "user.email", "t@example.com")
	run(t, root, "git", "config", "user.name", "t")
	writeFile(t, root, "a.txt", "alpha\n")
	writeFile(t, root, "sub/b.txt", "beta\n")
	writeFile(t, root, ".gitignore", "ignored.txt\n.semantica/\n")
	run(t, root, "git", "add", ".")
	run(t, root, "git", "commit", "-q", "-m", "init")
	if err := os.MkdirAll(filepath.Join(root, ".semantica"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func openTestStore(t *testing.T, root string) *Store {
	t.Helper()
	ctx := context.Background()
	rc, err := ResolveRepoContext(ctx, root)
	if err != nil {
		t.Fatalf("resolve context: %v", err)
	}
	s, err := OpenStore(ctx, rc, filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

func run(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCleanRepoSnapshotMatchesHead(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	snap, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	headTree := strings.TrimSpace(run(t, root, "git", "rev-parse", "HEAD^{tree}"))
	if snap.TreeHash != headTree {
		t.Errorf("clean snapshot tree = %s, want HEAD tree %s", snap.TreeHash, headTree)
	}
	if len(snap.DirtyPath) != 0 {
		t.Errorf("clean repo dirty paths = %v, want none", snap.DirtyPath)
	}
}

func TestDirtySnapshotCapturesWorktreeBytes(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	writeFile(t, root, "a.txt", "alpha modified\n")
	writeFile(t, root, "new.txt", "created\n")
	writeFile(t, root, "ignored.txt", "never captured\n")
	if err := os.Remove(filepath.Join(root, "sub", "b.txt")); err != nil {
		t.Fatal(err)
	}

	snap, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	got := strings.Join(snap.DirtyPath, ",")
	if got != "a.txt,new.txt,sub/b.txt" {
		t.Errorf("dirty paths = %q", got)
	}

	// The tree must contain worktree bytes for dirty paths, drop the
	// deletion, and exclude ignored files.
	ls := run(t, root, "git", "--git-dir", s.Dir, "ls-tree", "-r", "--name-only", snap.TreeHash)
	names := strings.Fields(ls)
	want := map[string]bool{".gitignore": true, "a.txt": true, "new.txt": true}
	if len(names) != len(want) {
		t.Fatalf("tree entries = %v, want %v", names, want)
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected tree entry %q", n)
		}
	}
	content := run(t, root, "git", "--git-dir", s.Dir, "cat-file", "blob", snap.TreeHash+":a.txt")
	if content != "alpha modified\n" {
		t.Errorf("a.txt blob = %q", content)
	}
}

func TestSnapshotWritesNothingToUserRepository(t *testing.T) {
	root := testRepo(t)

	countObjects := func() string {
		return run(t, root, "git", "count-objects", "-v")
	}
	refsBefore := run(t, root, "git", "for-each-ref")
	objectsBefore := countObjects()

	s := openTestStore(t, root)
	writeFile(t, root, "a.txt", "dirty content that must land in the isolated store only\n")
	if _, err := s.CaptureBefore(context.Background()); err != nil {
		t.Fatalf("capture: %v", err)
	}

	if got := run(t, root, "git", "for-each-ref"); got != refsBefore {
		t.Errorf("user repository refs changed:\nbefore: %s\nafter: %s", refsBefore, got)
	}
	if got := countObjects(); got != objectsBefore {
		t.Errorf("user repository objects changed:\nbefore: %s\nafter: %s", objectsBefore, got)
	}
}

func TestEmptyDeltaOnUnchangedTrees(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	writeFile(t, root, "a.txt", "pre-existing dirt\n")
	pre, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("pre capture: %v", err)
	}
	// Tool ran but changed nothing: post equals pre by tree hash.
	post, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("post capture: %v", err)
	}
	if pre.TreeHash != post.TreeHash {
		t.Fatalf("unchanged worktree produced different trees: %s vs %s", pre.TreeHash, post.TreeHash)
	}
	changes, err := s.DiffTrees(ctx, pre.TreeHash, post.TreeHash)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if changes != nil {
		t.Errorf("equal trees diffed to %v, want nil short-circuit", changes)
	}
}

func TestDiffTreesReportsByteChanges(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	pre, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("pre capture: %v", err)
	}
	writeFile(t, root, "a.txt", "alpha rewritten\n")
	writeFile(t, root, "gen/out.txt", "generated\n")
	if err := os.Remove(filepath.Join(root, "sub", "b.txt")); err != nil {
		t.Fatal(err)
	}
	post, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("post capture: %v", err)
	}

	changes, err := s.DiffTrees(ctx, pre.TreeHash, post.TreeHash)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	byPath := map[string]FileChange{}
	for _, c := range changes {
		byPath[c.Path] = c
	}
	if len(changes) != 3 {
		t.Fatalf("changes = %v, want 3", changes)
	}
	if byPath["a.txt"].Op != 'M' {
		t.Errorf("a.txt op = %c, want M", byPath["a.txt"].Op)
	}
	if byPath["gen/out.txt"].Op != 'A' {
		t.Errorf("gen/out.txt op = %c, want A", byPath["gen/out.txt"].Op)
	}
	if byPath["sub/b.txt"].Op != 'D' {
		t.Errorf("sub/b.txt op = %c, want D", byPath["sub/b.txt"].Op)
	}

	before, err := s.ReadBlob(ctx, byPath["a.txt"].BeforeHash)
	if err != nil {
		t.Fatalf("read before blob: %v", err)
	}
	after, err := s.ReadBlob(ctx, byPath["a.txt"].AfterHash)
	if err != nil {
		t.Fatalf("read after blob: %v", err)
	}
	if string(before) != "alpha\n" || string(after) != "alpha rewritten\n" {
		t.Errorf("blob contents = %q -> %q", before, after)
	}
}

func TestSpecialCharacterPaths(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	weird := `sp ace/uni-é世.txt`
	if runtime.GOOS == "windows" {
		weird = "sp ace/plain.txt"
	}
	writeFile(t, root, weird, "odd path\n")

	snap, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	found := false
	for _, p := range snap.DirtyPath {
		if p == weird {
			found = true
		}
	}
	if !found {
		t.Fatalf("special path %q missing from dirty set %v", weird, snap.DirtyPath)
	}
	content := run(t, root, "git", "--git-dir", s.Dir, "cat-file", "blob", snap.TreeHash+":"+weird)
	if content != "odd path\n" {
		t.Errorf("special path blob = %q", content)
	}
}

func TestSymlinkCapturedAsLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on windows")
	}
	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	if err := os.Symlink("a.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	snap, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	ls := run(t, root, "git", "--git-dir", s.Dir, "ls-tree", snap.TreeHash, "link.txt")
	if !strings.HasPrefix(ls, "120000 blob") {
		t.Errorf("symlink tree entry = %q, want mode 120000", ls)
	}
	content := run(t, root, "git", "--git-dir", s.Dir, "cat-file", "blob", snap.TreeHash+":link.txt")
	if content != "a.txt" {
		t.Errorf("symlink blob = %q, want target path", content)
	}
}

func TestRefLifecycleWithCompareAndSwap(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	snap, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	ref := SnapshotRef("main", "g1", "tu1")
	if err := s.CreateRef(ctx, ref, snap.TreeHash); err != nil {
		t.Fatalf("create ref: %v", err)
	}
	// Second create must fail: the ref already exists.
	if err := s.CreateRef(ctx, ref, snap.TreeHash); err == nil {
		t.Error("duplicate ref creation succeeded, want CAS failure")
	}
	refs, err := s.ListRefs(ctx)
	if err != nil {
		t.Fatalf("list refs: %v", err)
	}
	if refs[ref] != snap.TreeHash {
		t.Errorf("ref target = %q, want %q", refs[ref], snap.TreeHash)
	}
	// Deleting with a wrong expected hash must preserve the ref.
	wrong := strings.Repeat("beef", 10)
	if err := s.DeleteRef(ctx, ref, wrong); err == nil {
		t.Error("delete with wrong expected hash succeeded, want CAS failure")
	}
	if err := s.DeleteRef(ctx, ref, snap.TreeHash); err != nil {
		t.Fatalf("delete ref: %v", err)
	}
	refs, err = s.ListRefs(ctx)
	if err != nil {
		t.Fatalf("list refs after delete: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("refs after delete = %v, want none", refs)
	}
}

func TestUserGitattributesCannotShapeEvidence(t *testing.T) {
	root := testRepo(t)
	// Configure text conversion that would alter patch output.
	writeFile(t, root, ".gitattributes", "*.txt diff=mangle\n")
	run(t, root, "git", "config", "diff.mangle.textconv", "tr a-z A-Z <")
	run(t, root, "git", "add", ".gitattributes")
	run(t, root, "git", "commit", "-q", "-m", "attrs")

	s := openTestStore(t, root)
	ctx := context.Background()

	pre, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("pre capture: %v", err)
	}
	writeFile(t, root, "a.txt", "lowercase stays lowercase\n")
	post, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("post capture: %v", err)
	}
	changes, err := s.DiffTrees(ctx, pre.TreeHash, post.TreeHash)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "a.txt" {
		t.Fatalf("changes = %v", changes)
	}
	after, err := s.ReadBlob(ctx, changes[0].AfterHash)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	// Raw plumbing must return literal bytes, never textconv output.
	if string(after) != "lowercase stays lowercase\n" {
		t.Errorf("evidence bytes = %q, textconv leaked into evidence", after)
	}
}
