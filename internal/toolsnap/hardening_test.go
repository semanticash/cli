package toolsnap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Inherited Git routing variables must not redirect snapshot writes.
func TestInheritedObjectDirectoryCannotBypassStore(t *testing.T) {
	root := testRepo(t)
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(root, ".git", "objects"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(root, ".git", "index"))

	objectsBefore := run(t, root, "git", "count-objects", "-v")
	indexBefore, err := os.ReadFile(filepath.Join(root, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}

	s := openTestStore(t, root)
	writeFile(t, root, "a.txt", "must land in the isolated store\n")
	snap, err := s.CaptureBefore(context.Background())
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if len(snap.DirtyPath) != 1 {
		t.Fatalf("dirty paths = %v", snap.DirtyPath)
	}

	// Run verification outside the inherited override.
	if err := os.Unsetenv("GIT_OBJECT_DIRECTORY"); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("GIT_INDEX_FILE"); err != nil {
		t.Fatal(err)
	}

	if got := run(t, root, "git", "count-objects", "-v"); got != objectsBefore {
		t.Errorf("user repository objects changed under inherited GIT_OBJECT_DIRECTORY:\nbefore: %s\nafter: %s", objectsBefore, got)
	}
	indexAfter, err := os.ReadFile(filepath.Join(root, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if string(indexBefore) != string(indexAfter) {
		t.Error("user repository index changed during capture")
	}
	// The snapshot blob must be readable from the isolated store.
	content := run(t, root, "git", "--git-dir", s.Dir, "cat-file", "blob", snap.TreeHash+":a.txt")
	if content != "must land in the isolated store\n" {
		t.Errorf("snapshot blob = %q", content)
	}
}

func TestCandidatePathLimitFailsPartial(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	s.MaxCandidatePaths = 3
	for i := 0; i < 5; i++ {
		writeFile(t, root, filepath.Join("many", string(rune('a'+i))+".txt"), "x\n")
	}
	_, err := s.CaptureBefore(context.Background())
	var pe *PartialError
	if !errors.As(err, &pe) || pe.Reason != ReasonFileLimit {
		t.Fatalf("err = %v, want PartialError %s", err, ReasonFileLimit)
	}
}

func TestByteLimitFailsPartial(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	s.MaxBytesRead = 16
	writeFile(t, root, "big.txt", strings.Repeat("payload\n", 8))
	_, err := s.CaptureBefore(context.Background())
	var pe *PartialError
	if !errors.As(err, &pe) || pe.Reason != ReasonByteLimit {
		t.Fatalf("err = %v, want PartialError %s", err, ReasonByteLimit)
	}
}

func TestMalformedStatusRecordFailsPartial(t *testing.T) {
	_, err := parseStatusRecords("Z bogus record\x00")
	var pe *PartialError
	if !errors.As(err, &pe) || pe.Reason != ReasonMalformedStatus {
		t.Fatalf("err = %v, want PartialError %s", err, ReasonMalformedStatus)
	}
}

func TestRefComponentsDoNotCollide(t *testing.T) {
	pairs := [][2]string{
		{"a/b", "a?b"},
		// Encoded output must not collide with the same literal input.
		{"a/b", sanitizeRefComponent("a/b")},
		{"a_b", "a/b"},
		{strings.Repeat("x", 200), strings.Repeat("x", 200) + "y"},
	}
	for _, p := range pairs {
		if sanitizeRefComponent(p[0]) == sanitizeRefComponent(p[1]) {
			t.Errorf("identities %q and %q collide", p[0], p[1])
		}
	}
	if got := sanitizeRefComponent("tool-use_01"); got != sanitizeRefComponent("tool-use_01") {
		t.Errorf("sanitization not deterministic: %q", got)
	}
	if got := sanitizeRefComponent(strings.Repeat("x", 200)); len(got) > 64 {
		t.Errorf("oversized identity not shortened: %d chars", len(got))
	}
}

// HEAD movement between context resolution and capture must degrade
// to partial instead of mixing two repository states.
func TestHeadMovementBetweenResolveAndCaptureFailsPartial(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	writeFile(t, root, "a.txt", "moves head\n")
	run(t, root, "git", "add", "a.txt")
	run(t, root, "git", "commit", "-q", "-m", "move head after resolve")

	// s.repo still carries the HEAD resolved before the commit.
	_, err := s.CaptureBefore(ctx)
	var pe *PartialError
	if !errors.As(err, &pe) || pe.Reason != ReasonHeadChanged {
		t.Fatalf("err = %v, want PartialError %s", err, ReasonHeadChanged)
	}
}

// A leftover temp file from a crashed publication must neither block
// retry nor appear as a ref; the published ref must be fully written.
func TestCrashedRefPublicationDoesNotBlockRetry(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	snap, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	ref := SnapshotRef("main", "g-crash", "t1")
	path := filepath.Join(s.Dir, filepath.FromSlash(ref))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".tmp-99999", []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.CreateRef(ctx, ref, snap.TreeHash); err != nil {
		t.Fatalf("create ref with stale temp present: %v", err)
	}
	refs, err := s.ListRefs(ctx)
	if err != nil {
		t.Fatalf("list refs: %v", err)
	}
	if refs[ref] != snap.TreeHash {
		t.Errorf("published ref = %q, want %q", refs[ref], snap.TreeHash)
	}
	if len(refs) != 1 {
		t.Errorf("refs = %v, want exactly the published ref", refs)
	}
}

func TestCreateRefRejectsTraversal(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	outside := filepath.Join(filepath.Dir(s.Dir), "escaped")
	for _, evil := range []string{
		refPrefix + "/../../../escaped",
		// Backslashes are separators on Windows via filepath.FromSlash.
		refPrefix + `/..\..\..\escaped`,
		refPrefix + `/a\b/c`,
		refPrefix + "/a./b",
		refPrefix + "//b",
	} {
		if err := s.CreateRef(ctx, evil, strings.Repeat("ab", 20)); err == nil {
			t.Errorf("traversal ref accepted: %q", evil)
		}
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Errorf("traversal wrote outside the refs namespace: %v", err)
	}
}

// TestCreateRefRejectsInvalidTargets restores the target validation
// git update-ref provided: only a full lowercase hex object name of
// the repository's format may be published.
func TestCreateRefRejectsInvalidTargets(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()
	ref := SnapshotRef("main", "g-target", "t1")

	valid := strings.Repeat("ab", 20)
	for _, target := range []string{
		"",
		"abc",                         // wrong length
		valid + "ab",                  // too long
		strings.Repeat("zz", 20),      // non-hex
		strings.ToUpper(valid),        // uppercase never produced
		valid[:39] + "\n",             // newline injection
		valid + "\nref: refs/heads/m", // symref injection
	} {
		if err := s.CreateRef(ctx, ref, target); err == nil {
			t.Errorf("invalid target accepted: %q", target)
		}
	}
	refs, err := s.ListRefs(ctx)
	if err != nil {
		t.Fatalf("list refs: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("refs after rejected targets = %v, want none", refs)
	}
	if err := s.CreateRef(ctx, ref, valid); err != nil {
		t.Errorf("valid target rejected: %v", err)
	}
}

// An unreadable packed-refs file blocks publication instead of
// weakening create-if-absent.
func TestPackedRefsReadFailureFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-based unreadability not enforceable on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	packed := filepath.Join(s.Dir, "packed-refs")
	if err := os.WriteFile(packed, []byte("# pack-refs with: peeled fully-peeled sorted\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	ref := SnapshotRef("main", "g-packed", "t1")
	if err := s.CreateRef(ctx, ref, strings.Repeat("ab", 20)); err == nil {
		t.Fatal("publication succeeded despite unreadable packed-refs")
	}
}

// A sha1 store whose config mentions sha256 only in a comment must
// not fast-pass as sha256.
func TestFormatCommentCannotGrantFastPath(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	run(t, root, "git", "init", "-q", "-b", "main", "--object-format=sha256")
	if err := os.MkdirAll(filepath.Join(root, ".semantica"), 0o755); err != nil {
		t.Fatal(err)
	}
	storeDir := filepath.Join(root, ".semantica", storeDirName)
	run(t, root, "git", "init", "-q", "--bare", "--object-format=sha1", storeDir)
	cfg := filepath.Join(storeDir, "config")
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, append(raw, []byte("# objectformat = sha256\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	rc, err := ResolveRepoContext(ctx, root)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := OpenStore(ctx, rc, filepath.Join(root, ".semantica")); !errors.Is(err, ErrStoreIncompatible) {
		t.Fatalf("err = %v, want ErrStoreIncompatible", err)
	}
}

func TestParseStoreObjectFormat(t *testing.T) {
	minSha1 := "[core]\n\trepositoryformatversion = 0\n\tbare = true\n[gc]\n\tauto = 0\n"
	cases := []struct {
		raw    string
		format string
		ok     bool
	}{
		{minSha1, "sha1", true},
		{"[core]\n\trepositoryformatversion = 1\n\tbare = true\n[extensions]\n\tobjectformat = sha256\n[gc]\n\tauto = 0\n", "sha256", true},
		{"[core] # objectformat = sha256\n\trepositoryformatversion = 0\n\tbare = true\n[gc]\n\tauto = 0\n", "sha1", true},
		// Below the generated minimum shape.
		{"[core]\n\tbare = true\n", "", false},
		{"[core]\n\trepositoryformatversion = 0\n\tbare = true\n", "", false},
		{minSha1 + "[gc]\n\tauto = 1\n", "", false},
		// Extensions without repository format version 1.
		{"[extensions]\n\tobjectformat = sha256\n", "", false},
		{"[extensions]\n\tobjectformat = tiger192\n", "", false},
		// Unknown sections, keys, and extension entries.
		{minSha1 + "[fetch]\n\tprune = true\n", "", false},
		{"[core]\n\trepositoryformatversion = 0\n\tbare = true\n\tfsmonitor = /bin/evil\n[gc]\n\tauto = 0\n", "", false},
		{minSha1 + "[extensions]\n\tworktreeconfig = true\n", "", false},
		{minSha1 + "[gc]\n\tpruneexpire = now\n", "", false},
		// Subsections and malformed content.
		{"[remote \"origin\"]\n\turl = x\n", "", false},
		{"[core]\n\tbare = false\n", "", false},
		{"[broken\nline\n", "", false},
		{"[core]\nnot a key value pair\n", "", false},
	}
	for _, c := range cases {
		format, ok := parseStoreObjectFormat(c.raw)
		if format != c.format || ok != c.ok {
			t.Errorf("parse(%q) = (%q, %v), want (%q, %v)", c.raw, format, ok, c.format, c.ok)
		}
	}
}

func TestGitEnvScrubIsCaseInsensitive(t *testing.T) {
	t.Setenv("git_object_directory", "/tmp/redirected-objects")
	t.Setenv("Git_Index_File", "/tmp/redirected-index")
	for _, kv := range gitEnv(nil) {
		key, _, _ := strings.Cut(kv, "=")
		if strings.EqualFold(key, "GIT_OBJECT_DIRECTORY") || strings.EqualFold(key, "GIT_INDEX_FILE") {
			t.Errorf("case-variant routing variable survived scrub: %q", kv)
		}
	}
}

func TestNewlinePathUsesPerFileFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("newline filenames unsupported on windows")
	}
	root := testRepo(t)
	s := openTestStore(t, root)
	weird := "line1\nline2.txt"
	writeFile(t, root, weird, "newline path content\n")

	snap, err := s.CaptureBefore(context.Background())
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
		t.Fatalf("newline path missing from dirty set %v", snap.DirtyPath)
	}
	out := run(t, root, "git", "--git-dir", s.Dir, "ls-tree", "-r", "-z", snap.TreeHash)
	if !strings.Contains(out, "line1\nline2.txt") {
		t.Errorf("newline path missing from tree: %q", out)
	}
}
