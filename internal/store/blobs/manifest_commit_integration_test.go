package blobs_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/store/blobs"
)

// TestBuildCommitManifest_RealRepo builds a manifest from a real Git repository.
func TestBuildCommitManifest_RealRepo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable and symlink modes are POSIX-specific")
	}
	ctx := context.Background()
	dir := t.TempDir()

	initCmd := exec.Command("git", "init", dir)
	initCmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := initCmd.CombinedOutput(); err != nil {
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

	if err := os.WriteFile(filepath.Join(dir, "regular.txt"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "script.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("regular.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-m", "root")
	commit := runGit("rev-parse", "HEAD")

	repo, err := git.OpenRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	objectFormat, err := repo.ObjectFormat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	treeID, err := repo.CommitTree(ctx, commit)
	if err != nil {
		t.Fatal(err)
	}
	treeEntries, err := repo.LsTreeEntries(ctx, commit)
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]blobs.CommitTreeEntry, len(treeEntries))
	for i, e := range treeEntries {
		entries[i] = blobs.CommitTreeEntry{Path: e.Path, GitMode: e.Mode, GitObjectID: e.ObjectID, GitType: e.Type}
	}

	bs, err := blobs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res, err := blobs.BuildCommitManifest(ctx, bs, blobs.CommitManifestInput{
		ObjectFormat: objectFormat, CommitHash: commit, TreeID: treeID,
		Entries: entries, ReadObjects: repo.CatFileBatch,
	})
	if err != nil {
		t.Fatalf("BuildCommitManifest: %v", err)
	}

	if !res.Manifest.IsCommitScoped() {
		t.Fatal("manifest is not commit-scoped")
	}
	if res.Manifest.CommitHash != commit || res.Manifest.TreeID != treeID || res.Manifest.ObjectFormat != objectFormat {
		t.Errorf("identity = %s/%s/%s, want %s/%s/%s",
			res.Manifest.CommitHash, res.Manifest.TreeID, res.Manifest.ObjectFormat, commit, treeID, objectFormat)
	}

	byPath := make(map[string]blobs.ManifestFile, len(res.Manifest.Files))
	for _, f := range res.Manifest.Files {
		byPath[f.Path] = f
	}

	// CAS content matches the committed bytes.
	assertBlob := func(path string, want []byte, wantMode, wantType string) {
		t.Helper()
		f, ok := byPath[path]
		if !ok {
			t.Fatalf("%s missing from manifest", path)
		}
		if f.GitMode != wantMode || f.EntryType != wantType {
			t.Errorf("%s = mode %q type %q, want %q %q", path, f.GitMode, f.EntryType, wantMode, wantType)
		}
		got, err := bs.Get(ctx, f.Blob)
		if err != nil {
			t.Fatalf("get %s blob: %v", path, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s content = %q, want %q", path, got, want)
		}
	}
	assertBlob("regular.txt", []byte("hello\nworld\n"), "100644", blobs.EntryRegular)
	assertBlob("script.sh", []byte("#!/bin/sh\necho hi\n"), "100755", blobs.EntryRegular)
	assertBlob("link", []byte("regular.txt"), "120000", blobs.EntrySymlink) // symlink blob is the target
}
