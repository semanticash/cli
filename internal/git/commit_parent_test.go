package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeAt(t *testing.T, dir, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fileURL returns a cross-platform URL so Git honors --depth for local clones.
func fileURL(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + p
}

func TestCommitParent_RootNormalMerge(t *testing.T) {
	ctx := context.Background()
	repo, dir, runGit := newTreeRepo(t)

	writeAt(t, dir, "a.txt", "a\n")
	runGit("add", "-A")
	runGit("commit", "-m", "root")
	root := runGit("rev-parse", "HEAD")
	if p, isRoot, err := repo.CommitParent(ctx, root); err != nil || !isRoot || p != "" {
		t.Errorf("root = parent %q isRoot %v err %v, want empty root", p, isRoot, err)
	}

	writeAt(t, dir, "b.txt", "b\n")
	runGit("add", "-A")
	runGit("commit", "-m", "child")
	child := runGit("rev-parse", "HEAD")
	if p, isRoot, err := repo.CommitParent(ctx, child); err != nil || isRoot || p != root {
		t.Errorf("child = parent %q isRoot %v err %v, want parent %s", p, isRoot, err, root)
	}

	// Merge commits use the main-line parent.
	mainBranch := runGit("rev-parse", "--abbrev-ref", "HEAD")
	runGit("checkout", "-b", "feature")
	writeAt(t, dir, "f.txt", "f\n")
	runGit("add", "-A")
	runGit("commit", "-m", "feature")
	runGit("checkout", mainBranch)
	writeAt(t, dir, "m.txt", "m\n")
	runGit("add", "-A")
	runGit("commit", "-m", "main2")
	mainTip := runGit("rev-parse", "HEAD")
	runGit("merge", "--no-ff", "feature", "-m", "merge")
	merge := runGit("rev-parse", "HEAD")
	if p, isRoot, err := repo.CommitParent(ctx, merge); err != nil || isRoot || p != mainTip {
		t.Errorf("merge = parent %q isRoot %v err %v, want first parent %s", p, isRoot, err, mainTip)
	}
}

func gitStdin(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	base := []string{"-C", dir, "-c", "user.email=t@t.t", "-c", "user.name=t"}
	c := exec.Command("git", append(base, args...)...)
	c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	c.Stdin = strings.NewReader(stdin)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Parent headers must name commit objects.
func TestCommitParent_RejectsNonCommitParent(t *testing.T) {
	ctx := context.Background()
	repo, dir, runGit := newTreeRepo(t)
	writeAt(t, dir, "a.txt", "a\n")
	runGit("add", "-A")
	runGit("commit", "-m", "root")

	tree := runGit("rev-parse", "HEAD^{tree}")
	blob := gitStdin(t, dir, "hello\n", "hash-object", "-w", "--stdin")
	body := "tree " + tree + "\nparent " + blob +
		"\nauthor t <t@t.t> 0 +0000\ncommitter t <t@t.t> 0 +0000\n\ninvalid parent type\n"
	mal := gitStdin(t, dir, body, "hash-object", "-t", "commit", "-w", "--stdin")

	if p, isRoot, err := repo.CommitParent(ctx, mal); err == nil {
		t.Errorf("CommitParent = parent %q isRoot %v nil err, want rejection of a blob parent", p, isRoot)
	}
}

// A shallow boundary is not a root commit.
func TestCommitParent_ShallowBoundaryFailsClosed(t *testing.T) {
	ctx := context.Background()
	_, srcDir, runSrc := newTreeRepo(t)
	writeAt(t, srcDir, "a.txt", "a\n")
	runSrc("add", "-A")
	runSrc("commit", "-m", "c1")
	writeAt(t, srcDir, "b.txt", "b\n")
	runSrc("add", "-A")
	runSrc("commit", "-m", "c2")

	dst := filepath.Join(t.TempDir(), "shallow")
	clone := exec.Command("git", "clone", "--depth", "1", fileURL(srcDir), dst)
	clone.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := clone.CombinedOutput(); err != nil {
		t.Fatalf("shallow clone failed: %v\n%s", err, out)
	}
	head := func() string {
		c := exec.Command("git", "-C", dst, "rev-parse", "HEAD")
		c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := c.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}()

	repo, err := OpenRepo(dst)
	if err != nil {
		t.Fatal(err)
	}
	p, isRoot, err := repo.CommitParent(ctx, head)
	if err == nil {
		t.Errorf("CommitParent = parent %q isRoot %v nil err, want a missing-history error", p, isRoot)
	} else if !errors.Is(err, ErrMissingHistory) {
		t.Errorf("CommitParent err = %v, want ErrMissingHistory", err)
	}
	if _, err := repo.DiffForCommit(ctx, head); err == nil {
		t.Error("DiffForCommit = nil error at a shallow boundary")
	}
}

func TestEmptyTreeID_ObjectFormatAware(t *testing.T) {
	ctx := context.Background()
	repo, _, _ := newTreeRepo(t)
	id, err := repo.EmptyTreeID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if id != "4b825dc642cb6eb9a060e54bf8d69288fbee4904" {
		t.Errorf("sha1 empty tree = %q, want the well-known sha1 empty tree", id)
	}

	dir := t.TempDir()
	init := exec.Command("git", "init", "--object-format=sha256", dir)
	init.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := init.CombinedOutput(); err != nil {
		t.Skipf("git does not support --object-format=sha256: %v\n%s", err, out)
	}
	repo256, err := OpenRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	id256, err := repo256.EmptyTreeID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if id256 != "6ef19b41225c5369f1c104d45d8d85efa9b057b53b14b4b9b939dd74decc5321" {
		t.Errorf("sha256 empty tree = %q, want the sha256 empty tree, not the sha1 constant", id256)
	}
}

// Root diffs use the repository's object format.
func TestDiffForCommit_SHA256RootCommit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	init := exec.Command("git", "init", "--object-format=sha256", dir)
	init.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := init.CombinedOutput(); err != nil {
		t.Skipf("git does not support --object-format=sha256: %v\n%s", err, out)
	}
	runGit := func(args ...string) string {
		base := []string{"-C", dir, "-c", "user.email=t@t.t", "-c", "user.name=t"}
		c := exec.Command("git", append(base, args...)...)
		c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	writeAt(t, dir, "a.txt", "hello\n")
	runGit("add", "-A")
	runGit("commit", "-m", "root")
	head := runGit("rev-parse", "HEAD")

	repo, err := OpenRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := repo.DiffForCommit(ctx, head)
	if err != nil {
		t.Fatalf("DiffForCommit on a sha256 root commit: %v", err)
	}
	if !strings.Contains(string(diff), "a.txt") || !strings.Contains(string(diff), "+hello") {
		t.Errorf("diff = %q, want the added a.txt content", diff)
	}
}
