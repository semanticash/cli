package blobs

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestBuildManifest_BasicFiles(t *testing.T) {
	blobDir := filepath.Join(t.TempDir(), "blobs")
	repoDir := t.TempDir()

	bs, err := NewStore(blobDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Create test files.
	writeTestFile(t, repoDir, "a.txt", "aaa\n", 0o644)
	writeTestFile(t, repoDir, "sub/b.txt", "bbb\n", 0o644)

	paths := []string{"a.txt", "sub/b.txt"}
	readFile := func(rel string) ([]byte, error) {
		return os.ReadFile(filepath.Join(repoDir, rel))
	}

	res, err := BuildManifest(ctx, bs, repoDir, paths, readFile, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Manifest.Files) != 2 {
		t.Fatalf("files count = %d, want 2", len(res.Manifest.Files))
	}
	if res.ManifestHash == "" {
		t.Error("manifest hash is empty")
	}
	if res.TotalBytes != 8 { // "aaa\n" + "bbb\n"
		t.Errorf("total bytes = %d, want 8", res.TotalBytes)
	}

	// Verify blobs are retrievable.
	for _, mf := range res.Manifest.Files {
		data, err := bs.Get(ctx, mf.Blob)
		if err != nil {
			t.Errorf("Get blob for %s: %v", mf.Path, err)
			continue
		}
		if int64(len(data)) != mf.Size {
			t.Errorf("%s: blob size = %d, manifest says %d", mf.Path, len(data), mf.Size)
		}
	}
}

func TestBuildManifest_FileModes(t *testing.T) {
	blobDir := filepath.Join(t.TempDir(), "blobs")
	repoDir := t.TempDir()

	bs, err := NewStore(blobDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	writeTestFile(t, repoDir, "regular.txt", "content\n", 0o644)
	writeTestFile(t, repoDir, "script.sh", "#!/bin/sh\n", 0o755)

	paths := []string{"regular.txt", "script.sh"}
	readFile := func(rel string) ([]byte, error) {
		return os.ReadFile(filepath.Join(repoDir, rel))
	}

	res, err := BuildManifest(ctx, bs, repoDir, paths, readFile, nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, mf := range res.Manifest.Files {
		switch mf.Path {
		case "regular.txt":
			if mf.Mode&0o111 != 0 {
				t.Errorf("regular.txt should not be executable: mode = %o", mf.Mode)
			}
		case "script.sh":
			if runtime.GOOS != "windows" && mf.Mode&0o111 == 0 {
				t.Errorf("script.sh should be executable: mode = %o", mf.Mode)
			}
		}
	}
}

func TestBuildManifest_Symlink(t *testing.T) {
	blobDir := filepath.Join(t.TempDir(), "blobs")
	repoDir := t.TempDir()

	bs, err := NewStore(blobDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	writeTestFile(t, repoDir, "target.txt", "target\n", 0o644)
	if err := os.Symlink("target.txt", filepath.Join(repoDir, "link.txt")); err != nil {
		t.Fatal(err)
	}

	paths := []string{"target.txt", "link.txt"}
	readFile := func(rel string) ([]byte, error) {
		return os.ReadFile(filepath.Join(repoDir, rel))
	}

	res, err := BuildManifest(ctx, bs, repoDir, paths, readFile, nil)
	if err != nil {
		t.Fatal(err)
	}

	var linkFile *ManifestFile
	for i := range res.Manifest.Files {
		if res.Manifest.Files[i].Path == "link.txt" {
			linkFile = &res.Manifest.Files[i]
		}
	}
	if linkFile == nil {
		t.Fatal("link.txt not found in manifest")
	}
	if !linkFile.IsSymlink {
		t.Error("link.txt should be marked as symlink")
	}
	if linkFile.LinkTarget != "target.txt" {
		t.Errorf("link target = %q, want %q", linkFile.LinkTarget, "target.txt")
	}
}

func TestBuildManifest_EmptyPaths(t *testing.T) {
	blobDir := filepath.Join(t.TempDir(), "blobs")
	repoDir := t.TempDir()

	bs, err := NewStore(blobDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	res, err := BuildManifest(ctx, bs, repoDir, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Manifest.Files) != 0 {
		t.Errorf("files count = %d, want 0", len(res.Manifest.Files))
	}
	if res.TotalBytes != 0 {
		t.Errorf("total bytes = %d, want 0", res.TotalBytes)
	}
	if res.ManifestHash == "" {
		t.Error("manifest hash should still be set for empty manifest")
	}
}

func TestBuildManifest_ManifestHashStable(t *testing.T) {
	blobDir := filepath.Join(t.TempDir(), "blobs")
	repoDir := t.TempDir()

	bs, err := NewStore(blobDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	writeTestFile(t, repoDir, "f.txt", "content\n", 0o644)

	paths := []string{"f.txt"}
	readFile := func(rel string) ([]byte, error) {
		return os.ReadFile(filepath.Join(repoDir, rel))
	}

	// The manifest includes a timestamp, so hashes differ across calls.
	// This checks that a single result is stored and retrievable.
	res, err := BuildManifest(ctx, bs, repoDir, paths, readFile, nil)
	if err != nil {
		t.Fatal(err)
	}

	mData, err := bs.Get(ctx, res.ManifestHash)
	if err != nil {
		t.Fatalf("manifest blob not retrievable: %v", err)
	}
	if len(mData) == 0 {
		t.Error("manifest blob is empty")
	}
}

func TestBuildManifest_IncrementalSkipsUnchanged(t *testing.T) {
	blobDir := filepath.Join(t.TempDir(), "blobs")
	repoDir := t.TempDir()

	bs, err := NewStore(blobDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	writeTestFile(t, repoDir, "a.txt", "aaa\n", 0o644)
	writeTestFile(t, repoDir, "b.txt", "bbb\n", 0o644)

	paths := []string{"a.txt", "b.txt"}
	readCount := 0
	readFile := func(rel string) ([]byte, error) {
		readCount++
		return os.ReadFile(filepath.Join(repoDir, rel))
	}

	// First build: no previous manifest, should read all files.
	res1, err := BuildManifest(ctx, bs, repoDir, paths, readFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	if readCount != 2 {
		t.Fatalf("first build: readFile called %d times, want 2", readCount)
	}

	// Second build: pass previous files, files unchanged -> should skip reads.
	readCount = 0
	res2, err := BuildManifest(ctx, bs, repoDir, paths, readFile, res1.Manifest.Files)
	if err != nil {
		t.Fatal(err)
	}
	if readCount != 0 {
		t.Errorf("incremental build: readFile called %d times, want 0", readCount)
	}

	// Blob hashes should be identical.
	for i := range res1.Manifest.Files {
		if res1.Manifest.Files[i].Blob != res2.Manifest.Files[i].Blob {
			t.Errorf("file %s: blob hash changed across incremental build", res1.Manifest.Files[i].Path)
		}
	}

	// TotalBytes must still be populated.
	if res2.TotalBytes != res1.TotalBytes {
		t.Errorf("incremental TotalBytes = %d, want %d", res2.TotalBytes, res1.TotalBytes)
	}
}

func TestBuildManifest_IncrementalDetectsModified(t *testing.T) {
	blobDir := filepath.Join(t.TempDir(), "blobs")
	repoDir := t.TempDir()

	bs, err := NewStore(blobDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	writeTestFile(t, repoDir, "a.txt", "original\n", 0o644)

	paths := []string{"a.txt"}
	readFile := func(rel string) ([]byte, error) {
		return os.ReadFile(filepath.Join(repoDir, rel))
	}

	res1, err := BuildManifest(ctx, bs, repoDir, paths, readFile, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Modify file content (also changes mtime).
	writeTestFile(t, repoDir, "a.txt", "modified\n", 0o644)

	readCount := 0
	countingRead := func(rel string) ([]byte, error) {
		readCount++
		return os.ReadFile(filepath.Join(repoDir, rel))
	}

	res2, err := BuildManifest(ctx, bs, repoDir, paths, countingRead, res1.Manifest.Files)
	if err != nil {
		t.Fatal(err)
	}
	if readCount != 1 {
		t.Errorf("modified file: readFile called %d times, want 1", readCount)
	}
	if res1.Manifest.Files[0].Blob == res2.Manifest.Files[0].Blob {
		t.Error("blob hash should differ after modification")
	}
}

func TestBuildManifest_IncrementalNewFile(t *testing.T) {
	blobDir := filepath.Join(t.TempDir(), "blobs")
	repoDir := t.TempDir()

	bs, err := NewStore(blobDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	writeTestFile(t, repoDir, "a.txt", "aaa\n", 0o644)
	readFile := func(rel string) ([]byte, error) {
		return os.ReadFile(filepath.Join(repoDir, rel))
	}

	res1, err := BuildManifest(ctx, bs, repoDir, []string{"a.txt"}, readFile, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Add a new file.
	writeTestFile(t, repoDir, "b.txt", "bbb\n", 0o644)

	readCount := 0
	countingRead := func(rel string) ([]byte, error) {
		readCount++
		return os.ReadFile(filepath.Join(repoDir, rel))
	}

	res2, err := BuildManifest(ctx, bs, repoDir, []string{"a.txt", "b.txt"}, countingRead, res1.Manifest.Files)
	if err != nil {
		t.Fatal(err)
	}
	// Only the new file should be read.
	if readCount != 1 {
		t.Errorf("new file: readFile called %d times, want 1", readCount)
	}
	if len(res2.Manifest.Files) != 2 {
		t.Errorf("file count = %d, want 2", len(res2.Manifest.Files))
	}
}

func TestBuildManifest_IncrementalOldManifestNoModTimeNs(t *testing.T) {
	blobDir := filepath.Join(t.TempDir(), "blobs")
	repoDir := t.TempDir()

	bs, err := NewStore(blobDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	writeTestFile(t, repoDir, "a.txt", "aaa\n", 0o644)

	readFile := func(rel string) ([]byte, error) {
		return os.ReadFile(filepath.Join(repoDir, rel))
	}

	res1, err := BuildManifest(ctx, bs, repoDir, []string{"a.txt"}, readFile, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate old manifest without ModTimeNs.
	oldFiles := make([]ManifestFile, len(res1.Manifest.Files))
	copy(oldFiles, res1.Manifest.Files)
	oldFiles[0].ModTimeNs = 0

	readCount := 0
	countingRead := func(rel string) ([]byte, error) {
		readCount++
		return os.ReadFile(filepath.Join(repoDir, rel))
	}

	_, err = BuildManifest(ctx, bs, repoDir, []string{"a.txt"}, countingRead, oldFiles)
	if err != nil {
		t.Fatal(err)
	}
	// ModTimeNs == 0 should force a rehash.
	if readCount != 1 {
		t.Errorf("old manifest (no mtime): readFile called %d times, want 1", readCount)
	}
}

func writeTestFile(t *testing.T, dir, rel, content string, mode os.FileMode) {
	t.Helper()
	abs := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// TestBuildManifest_DeletedFileSkipped verifies that missing tracked files are
// skipped instead of failing the manifest build.
func TestBuildManifest_DeletedFileSkipped(t *testing.T) {
	blobDir := filepath.Join(t.TempDir(), "blobs")
	repoDir := t.TempDir()

	bs, err := NewStore(blobDir)
	if err != nil {
		t.Fatal(err)
	}

	readFile := func(rel string) ([]byte, error) {
		return os.ReadFile(filepath.Join(repoDir, rel))
	}

	// Keep one existing path and one missing path in the manifest input.
	writeTestFile(t, repoDir, "exists.go", "package main\n", 0o644)
	paths := []string{"exists.go", "deleted.go"}

	res, err := BuildManifest(context.Background(), bs, repoDir, paths, readFile, nil)
	if err != nil {
		t.Fatalf("BuildManifest should not fail for deleted files: %v", err)
	}
	if len(res.Manifest.Files) != 1 {
		t.Fatalf("expected 1 file in manifest, got %d", len(res.Manifest.Files))
	}
	if res.Manifest.Files[0].Path != "exists.go" {
		t.Errorf("expected exists.go, got %s", res.Manifest.Files[0].Path)
	}
}

// TestBuildManifest_FileDisappearsBetweenStatAndRead verifies that a file that
// vanishes after stat is skipped instead of failing the manifest build.
func TestBuildManifest_FileDisappearsBetweenStatAndRead(t *testing.T) {
	blobDir := filepath.Join(t.TempDir(), "blobs")
	repoDir := t.TempDir()

	bs, err := NewStore(blobDir)
	if err != nil {
		t.Fatal(err)
	}

	// Let Lstat succeed, then simulate the file disappearing before readFile.
	writeTestFile(t, repoDir, "vanishing.go", "package main\n", 0o644)
	paths := []string{"vanishing.go"}

	readFile := func(rel string) ([]byte, error) {
		return nil, os.ErrNotExist
	}

	res, err := BuildManifest(context.Background(), bs, repoDir, paths, readFile, nil)
	if err != nil {
		t.Fatalf("BuildManifest should tolerate file disappearing between stat and read: %v", err)
	}
	if len(res.Manifest.Files) != 0 {
		t.Errorf("expected 0 files in manifest, got %d", len(res.Manifest.Files))
	}
}
