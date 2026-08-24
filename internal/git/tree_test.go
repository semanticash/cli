package git

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newTreeRepo(t *testing.T) (*Repo, string, func(args ...string) string) {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init", dir)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	runGit := func(args ...string) string {
		base := []string{"-C", dir, "-c", "user.email=t@t.t", "-c", "user.name=t", "-c", "core.autocrlf=false"}
		c := exec.Command("git", append(base, args...)...)
		c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	repo, err := OpenRepo(dir)
	if err != nil {
		t.Fatalf("OpenRepo: %v", err)
	}
	return repo, dir, runGit
}

func TestObjectFormat(t *testing.T) {
	repo, _, _ := newTreeRepo(t)
	f, err := repo.ObjectFormat(context.Background())
	if err != nil {
		t.Fatalf("ObjectFormat: %v", err)
	}
	if f != "sha1" && f != "sha256" {
		t.Errorf("ObjectFormat = %q, want sha1 or sha256", f)
	}
}

func TestLsTreeEntriesAndCatFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable and symlink modes are POSIX-specific")
	}
	ctx := context.Background()
	repo, dir, runGit := newTreeRepo(t)

	mustWrite := func(rel string, b []byte, mode os.FileMode) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, mode); err != nil {
			t.Fatal(err)
		}
	}

	mustWrite("regular.txt", []byte("hello\nworld\n"), 0o644)
	mustWrite("script.sh", []byte("#!/bin/sh\necho hi\n"), 0o755)
	mustWrite("dir/space name.txt", []byte("spaced"), 0o644) // path with a space
	mustWrite("dir/文件.txt", []byte("u"), 0o644)              // non-ASCII path
	mustWrite("bin.dat", []byte("a\x00b"), 0o644)            // content with an embedded NUL, no trailing newline
	if err := os.Symlink("regular.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	runGit("add", "-A")
	runGit("commit", "-m", "tree")
	commit := runGit("rev-parse", "HEAD")

	entries, err := repo.LsTreeEntries(ctx, commit)
	if err != nil {
		t.Fatalf("LsTreeEntries: %v", err)
	}
	byPath := make(map[string]TreeEntry, len(entries))
	for _, e := range entries {
		byPath[e.Path] = e
	}

	if e := byPath["regular.txt"]; e.Mode != "100644" || e.Type != "blob" {
		t.Errorf("regular.txt = %+v, want mode 100644 blob", e)
	}
	if e := byPath["script.sh"]; e.Mode != "100755" {
		t.Errorf("script.sh mode = %q, want 100755", e.Mode)
	}
	if e := byPath["link"]; e.Mode != "120000" || e.Type != "blob" {
		t.Errorf("link = %+v, want mode 120000 blob", e)
	}
	// Git paths keep forward slashes and preserve spaces and non-ASCII bytes.
	if _, ok := byPath["dir/space name.txt"]; !ok {
		t.Errorf("spaced path missing; got paths %v", pathsOf(entries))
	}
	if _, ok := byPath["dir/文件.txt"]; !ok {
		t.Errorf("non-ASCII path missing; got paths %v", pathsOf(entries))
	}

	// CatFileBatch streams exact bytes in request order.
	oids := []string{byPath["regular.txt"].ObjectID, byPath["bin.dat"].ObjectID, byPath["link"].ObjectID}
	blobs := collectBatch(t, repo, ctx, oids)
	if len(blobs) != 3 {
		t.Fatalf("CatFileBatch yielded %d blobs, want 3", len(blobs))
	}
	if string(blobs[0]) != "hello\nworld\n" {
		t.Errorf("regular content = %q", blobs[0])
	}
	if !bytes.Equal(blobs[1], []byte("a\x00b")) {
		t.Errorf("bin content = %q, want a<NUL>b", blobs[1])
	}
	// A symlink blob's content is the link target.
	if string(blobs[2]) != "regular.txt" {
		t.Errorf("symlink blob = %q, want regular.txt", blobs[2])
	}
}

func TestLsTreeEntries_Gitlink(t *testing.T) {
	ctx := context.Background()
	repo, dir, runGit := newTreeRepo(t)

	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-m", "root")
	head := runGit("rev-parse", "HEAD")

	// A gitlink entry points at a commit object without a worktree directory.
	runGit("update-index", "--add", "--cacheinfo", "160000,"+head+",submod")
	runGit("commit", "-m", "add gitlink")
	commit := runGit("rev-parse", "HEAD")

	entries, err := repo.LsTreeEntries(ctx, commit)
	if err != nil {
		t.Fatalf("LsTreeEntries: %v", err)
	}
	var gl *TreeEntry
	for i := range entries {
		if entries[i].Path == "submod" {
			gl = &entries[i]
		}
	}
	if gl == nil {
		t.Fatalf("gitlink entry missing; got %v", pathsOf(entries))
	}
	if gl.Mode != "160000" || gl.Type != "commit" {
		t.Errorf("gitlink = %+v, want mode 160000 commit", *gl)
	}
	if gl.ObjectID != head {
		t.Errorf("gitlink object = %q, want %q", gl.ObjectID, head)
	}
}

func TestCatFileBatch_MissingObject(t *testing.T) {
	ctx := context.Background()
	repo, dir, runGit := newTreeRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-m", "c")

	err := repo.CatFileBatch(ctx, []string{strings.Repeat("0", 40)}, func(string, []byte) error { return nil })
	if err == nil {
		t.Error("CatFileBatch = nil error for a missing object")
	}
}

// collectBatch copies callback content for assertions.
func collectBatch(t *testing.T, repo *Repo, ctx context.Context, oids []string) [][]byte {
	t.Helper()
	var out [][]byte
	err := repo.CatFileBatch(ctx, oids, func(_ string, content []byte) error {
		out = append(out, append([]byte(nil), content...))
		return nil
	})
	if err != nil {
		t.Fatalf("CatFileBatch: %v", err)
	}
	return out
}

func TestLsTreeEntries_RejectsShortHash(t *testing.T) {
	repo, _, _ := newTreeRepo(t)
	if _, err := repo.LsTreeEntries(context.Background(), "abc123"); err == nil {
		t.Error("LsTreeEntries = nil error for a short hash")
	}
}

func pathsOf(entries []TreeEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Path
	}
	return out
}

// Special path bytes remain intact in NUL-terminated output.
func TestParseLsTreeOutput_SpecialPaths(t *testing.T) {
	rec := func(mode, typ, oid, path string) string {
		return mode + " " + typ + " " + oid + "\t" + path
	}
	oid := strings.Repeat("a", 40)
	special := []string{
		"a b.txt",          // space
		"has\ttab.txt",     // tab inside the path
		"has\nnewline.txt", // newline inside the path
		"dir/\"quote\".go", // quote
		"back\\slash.go",   // backslash
		"unicode/文件.txt",   // non-ASCII
	}
	var buf []byte
	for _, p := range special {
		buf = append(buf, []byte(rec("100644", "blob", oid, p))...)
		buf = append(buf, 0)
	}
	entries, err := parseLsTreeOutput(buf)
	if err != nil {
		t.Fatalf("parseLsTreeOutput: %v", err)
	}
	if len(entries) != len(special) {
		t.Fatalf("got %d entries, want %d", len(entries), len(special))
	}
	for i, p := range special {
		if entries[i].Path != p {
			t.Errorf("entry %d path = %q, want %q", i, entries[i].Path, p)
		}
	}
}

func TestParseLsTreeOutput_Rejects(t *testing.T) {
	oid := strings.Repeat("a", 40)
	valid := "100644 blob " + oid + "\ta.txt"
	tests := map[string][]byte{
		"no tab":             []byte("100644 blob " + oid + "\x00"),
		"short meta":         []byte("100644 blob\tpath\x00"),
		"non-utf8 path":      append([]byte("100644 blob "+oid+"\t"), append([]byte{0xff, 0xfe}, 0)...),
		"truncated no NUL":   []byte(valid), // valid record but missing its terminator
		"interior empty":     []byte(valid + "\x00\x00" + valid + "\x00"),
		"trailing empty run": []byte(valid + "\x00\x00"),
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseLsTreeOutput(in); err == nil {
				t.Errorf("parseLsTreeOutput(%q) = nil error", name)
			}
		})
	}
}

func TestParseLsTreeOutput_EmptyIsOK(t *testing.T) {
	entries, err := parseLsTreeOutput(nil)
	if err != nil || len(entries) != 0 {
		t.Errorf("parseLsTreeOutput(nil) = %v, %v; want no entries, no error", entries, err)
	}
	entries, err = parseLsTreeOutput([]byte{})
	if err != nil || len(entries) != 0 {
		t.Errorf("parseLsTreeOutput([]) = %v, %v; want no entries, no error", entries, err)
	}
}

func TestReadCatFileBatch_Rejects(t *testing.T) {
	oid := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)
	tests := map[string]string{
		"missing object":   oid + " missing\n",
		"oid mismatch":     other + " blob 3\nabc\n",
		"unknown type":     oid + " bogus 3\nabc\n",
		"negative size":    oid + " blob -1\n",
		"nonnumeric size":  oid + " blob NaN\n",
		"bad separator":    oid + " blob 3\nabcX", // 'X' instead of newline
		"truncated body":   oid + " blob 5\nab",   // fewer than 5 content bytes
		"malformed header": "garbage\n",
	}
	noop := func(string, []byte) error { return nil }
	for name, resp := range tests {
		t.Run(name, func(t *testing.T) {
			br := bufio.NewReader(strings.NewReader(resp))
			if err := readCatFileBatch(br, []string{oid}, noop); err == nil {
				t.Errorf("readCatFileBatch(%q) = nil error", name)
			}
		})
	}
}

func TestReadCatFileBatch_OK(t *testing.T) {
	oid := strings.Repeat("a", 40)
	// The content contains a newline within its declared size.
	resp := oid + " blob 6\nhe\nllo\n"
	var got [][]byte
	err := readCatFileBatch(bufio.NewReader(strings.NewReader(resp)), []string{oid}, func(_ string, content []byte) error {
		got = append(got, append([]byte(nil), content...))
		return nil
	})
	if err != nil {
		t.Fatalf("readCatFileBatch: %v", err)
	}
	if len(got) != 1 || string(got[0]) != "he\nllo" {
		t.Errorf("content = %q, want %q", got, "he\nllo")
	}
}

func TestReadCatFileBatch_CallbackErrorAborts(t *testing.T) {
	oid := strings.Repeat("a", 40)
	resp := oid + " blob 3\nabc\n"
	sentinel := errTest("boom")
	err := readCatFileBatch(bufio.NewReader(strings.NewReader(resp)), []string{oid}, func(string, []byte) error {
		return sentinel
	})
	if err != sentinel {
		t.Errorf("readCatFileBatch err = %v, want the callback error", err)
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }

func TestCatFileBatch_RejectsInjection(t *testing.T) {
	repo, _, _ := newTreeRepo(t)
	// Reject a newline before writing the request to Git.
	err := repo.CatFileBatch(context.Background(), []string{"deadbeef\n0000"}, func(string, []byte) error { return nil })
	if err == nil {
		t.Error("CatFileBatch = nil error for an injected request")
	}
}

func TestTreeReader_Cancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	repo, dir, runGit := newTreeRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-m", "c")
	commit := runGit("rev-parse", "HEAD")
	cancel() // cancel before the reads run

	if _, err := repo.LsTreeEntries(ctx, commit); err == nil {
		t.Error("LsTreeEntries = nil error under a cancelled context")
	}
	entries, _ := repo.LsTreeEntries(context.Background(), commit)
	if len(entries) == 0 {
		t.Fatal("need at least one object id for the cat-file cancellation check")
	}
	err := repo.CatFileBatch(ctx, []string{entries[0].ObjectID}, func(string, []byte) error { return nil })
	if err == nil {
		t.Error("CatFileBatch = nil error under a cancelled context")
	}
}

func TestTreeReader_SHA256Repo(t *testing.T) {
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
	repo, err := OpenRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if f, err := repo.ObjectFormat(ctx); err != nil || f != "sha256" {
		t.Fatalf("ObjectFormat = %q err=%v, want sha256", f, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("sha256 content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-m", "c")
	commit := runGit("rev-parse", "HEAD")

	entries, err := repo.LsTreeEntries(ctx, commit)
	if err != nil {
		t.Fatalf("LsTreeEntries: %v", err)
	}
	if len(entries) != 1 || len(entries[0].ObjectID) != 64 {
		t.Fatalf("entries = %+v, want one 64-hex object id", entries)
	}
	blobs := collectBatch(t, repo, ctx, []string{entries[0].ObjectID})
	if string(blobs[0]) != "sha256 content\n" {
		t.Errorf("content = %q", blobs[0])
	}
}
