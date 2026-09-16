package toolsnap

import (
	"context"
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

	foreign := t.TempDir()
	t.Setenv("GIT_COMMON_DIR", foreign)
	ctx := context.Background()
	store := &Store{Dir: dir, repo: RepoContext{ObjectFormat: "sha1"}}
	if err := store.initialize(ctx); err != nil {
		t.Fatal(err)
	}
	common, err := store.git(ctx, "rev-parse", "--git-common-dir")
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Clean(strings.TrimSpace(common)); !strings.EqualFold(got, filepath.Clean(gitDir)) {
		t.Fatalf("Git expanded or redirected its common directory: %q, want %q", got, gitDir)
	}
	if got, err := store.git(ctx, "config", "--local", "--get", "gc.auto"); err != nil || strings.TrimSpace(got) != "0" {
		t.Fatalf("store config unavailable: %q, %v", got, err)
	}
	if got, err := store.git(ctx, "rev-parse", "--is-bare-repository"); err != nil || strings.TrimSpace(got) != "true" {
		t.Fatalf("store is no longer bare: %q, %v", got, err)
	}
	if entries, err := os.ReadDir(foreign); err != nil || len(entries) != 0 {
		t.Fatalf("inherited common directory was modified: %v, %v", entries, err)
	}
}
