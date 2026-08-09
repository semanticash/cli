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

func TestGitEnvScrubIsCaseInsensitive(t *testing.T) {
	t.Setenv("git_object_directory", "/tmp/evil")
	t.Setenv("Git_Index_File", "/tmp/evil-index")
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
