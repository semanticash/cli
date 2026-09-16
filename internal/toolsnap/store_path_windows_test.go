package toolsnap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreGitLocationPreservesLongDirectoryIdentity(t *testing.T) {
	dir := t.TempDir()
	for len(dir) < 300 {
		dir = filepath.Join(dir, strings.Repeat("snapshot", 8))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cwd, gitDir, err := storeGitLocation(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cwd) >= 220 || len(gitDir) >= 220 {
		t.Fatalf("Git still receives a long path: cwd=%q git-dir=%q", cwd, gitDir)
	}
	original, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	alias, err := os.Stat(gitDir)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(original, alias) {
		t.Fatal("Git directory does not identify the original store")
	}
	if cwd != filepath.VolumeName(gitDir)+string(filepath.Separator) {
		t.Fatalf("process directory is not the volume root: %q", cwd)
	}
}
