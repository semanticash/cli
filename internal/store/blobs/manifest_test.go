package blobs

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuildManifest_CancelledContextAborts(t *testing.T) {
	blobDir := filepath.Join(t.TempDir(), "blobs")
	repoDir := t.TempDir()

	bs, err := NewStore(blobDir)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, repoDir, "a.txt", "aaa\n", 0o644)

	reads := 0
	readFile := func(rel string) ([]byte, error) {
		reads++
		return os.ReadFile(filepath.Join(repoDir, rel))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := BuildManifest(ctx, bs, repoDir, []string{"a.txt"}, readFile, nil); err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if reads != 0 {
		t.Errorf("readFile ran %d times under a cancelled context, want 0", reads)
	}
}

func TestBuildManifest_CancelledContextNoEmptyManifest(t *testing.T) {
	bs, err := NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := BuildManifest(ctx, bs, t.TempDir(), nil, nil, nil)
	if err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res != nil {
		t.Errorf("result = %+v under a cancelled context, want nil", res)
	}
}

// Cancellation during serialization must stop the manifest write.
func TestBuildManifest_CancelDuringMarshalAborts(t *testing.T) {
	blobDir := filepath.Join(t.TempDir(), "blobs")
	bs, err := NewStore(blobDir)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	orig := marshalManifest
	t.Cleanup(func() { marshalManifest = orig })
	marshalManifest = func(m Manifest) ([]byte, error) {
		cancel()
		return orig(m)
	}

	// With no paths, only the post-marshal check can observe cancellation.
	res, err := BuildManifest(ctx, bs, t.TempDir(), nil, nil, nil)
	if err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res != nil {
		t.Errorf("result = %+v, want nil", res)
	}
	entries := 0
	_ = filepath.WalkDir(blobDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			entries++
		}
		return nil
	})
	if entries != 0 {
		t.Errorf("blob store holds %d object(s); the manifest must not persist when cancellation hits during marshaling", entries)
	}
}

// A vanished file can bypass the post-read check but not the final check.
func TestBuildManifest_CancelBeforePersistAborts(t *testing.T) {
	blobDir := filepath.Join(t.TempDir(), "blobs")
	repoDir := t.TempDir()

	bs, err := NewStore(blobDir)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, repoDir, "a.txt", "aaa\n", 0o644)

	ctx, cancel := context.WithCancel(context.Background())
	readFile := func(rel string) ([]byte, error) {
		cancel()
		return nil, os.ErrNotExist
	}
	if _, err := BuildManifest(ctx, bs, repoDir, []string{"a.txt"}, readFile, nil); err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	entries := 0
	_ = filepath.WalkDir(blobDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			entries++
		}
		return nil
	})
	if entries != 0 {
		t.Errorf("blob store holds %d object(s); the manifest must not persist after cancellation", entries)
	}
}

// Cancellation during a read stops both file and manifest writes.
func TestBuildManifest_CancelInsideFinalReaderAborts(t *testing.T) {
	blobDir := filepath.Join(t.TempDir(), "blobs")
	repoDir := t.TempDir()

	bs, err := NewStore(blobDir)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, repoDir, "a.txt", "aaa\n", 0o644)

	ctx, cancel := context.WithCancel(context.Background())
	readFile := func(rel string) ([]byte, error) {
		b, err := os.ReadFile(filepath.Join(repoDir, rel))
		cancel()
		return b, err
	}
	if _, err := BuildManifest(ctx, bs, repoDir, []string{"a.txt"}, readFile, nil); err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	entries := 0
	_ = filepath.WalkDir(blobDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			entries++
		}
		return nil
	})
	if entries != 0 {
		t.Errorf("blob store holds %d object(s) after cancellation inside the final read, want 0", entries)
	}
}

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

// BuildManifest produces a valid version-2 workspace manifest.
func TestBuildManifest_ProducesV2Workspace(t *testing.T) {
	bs, err := NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	repoDir := t.TempDir()
	writeTestFile(t, repoDir, "a.txt", "hi\n", 0o644)
	readFile := func(rel string) ([]byte, error) { return os.ReadFile(filepath.Join(repoDir, rel)) }

	ctx := context.Background()
	mr, err := BuildManifest(ctx, bs, repoDir, []string{"a.txt"}, readFile, nil)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if mr.Manifest.Version != 2 || mr.Manifest.Scope != ScopeWorkspace {
		t.Errorf("manifest = v%d scope %q, want version 2 workspace", mr.Manifest.Version, mr.Manifest.Scope)
	}
	if mr.Manifest.IsCommitScoped() {
		t.Error("workspace manifest must not be commit-scoped")
	}
	raw, err := bs.Get(ctx, mr.ManifestHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseManifest(raw); err != nil {
		t.Errorf("stored workspace manifest fails ParseManifest: %v", err)
	}
}

// A version-1 workspace manifest can seed validated version-2 reuse.
func TestBuildManifest_ReusesFromV1Predecessor(t *testing.T) {
	ctx := context.Background()
	bs, err := NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	repoDir := t.TempDir()
	writeTestFile(t, repoDir, "reuse.txt", "reused content\n", 0o644)
	writeTestFile(t, repoDir, "missing.txt", "missing-ref content\n", 0o644)
	writeTestFile(t, repoDir, "malformed.txt", "malformed-ref content\n", 0o644)

	// Count reads to distinguish reuse from fallback.
	reads := map[string]int{}
	readFile := func(rel string) ([]byte, error) {
		reads[rel]++
		return os.ReadFile(filepath.Join(repoDir, rel))
	}

	// Store the reusable file's content in the CAS.
	reuseContent, err := os.ReadFile(filepath.Join(repoDir, "reuse.txt"))
	if err != nil {
		t.Fatal(err)
	}
	reuseBlob, _, err := bs.Put(ctx, reuseContent)
	if err != nil {
		t.Fatal(err)
	}

	v1Entry := func(rel, blob string) ManifestFile {
		fi, err := os.Lstat(filepath.Join(repoDir, rel))
		if err != nil {
			t.Fatal(err)
		}
		return ManifestFile{
			Path: rel, Blob: blob, Size: fi.Size(),
			Mode: fi.Mode() & permMask, ModTimeNs: fi.ModTime().UnixNano(),
		}
	}
	// Round-trip the predecessor through the version-1 wire format.
	v1 := Manifest{Version: 1, CreatedAt: 1, RepoRoot: repoDir, Files: []ManifestFile{
		v1Entry("reuse.txt", reuseBlob),                 // valid + present -> reused
		v1Entry("missing.txt", strings.Repeat("d", 64)), // valid hex, absent -> re-read
		v1Entry("malformed.txt", "not-a-hash"),          // malformed -> re-read
	}}
	rawV1, err := v1.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	parsedV1, err := ParseManifest(rawV1)
	if err != nil {
		t.Fatalf("v1 ParseManifest: %v", err)
	}

	mr, err := BuildManifest(ctx, bs, repoDir, []string{"reuse.txt", "missing.txt", "malformed.txt"}, readFile, parsedV1.Files)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if mr.Manifest.Version != 2 || mr.Manifest.Scope != ScopeWorkspace {
		t.Errorf("result = v%d %q, want version 2 workspace", mr.Manifest.Version, mr.Manifest.Scope)
	}
	byPath := map[string]ManifestFile{}
	for _, f := range mr.Manifest.Files {
		byPath[f.Path] = f
	}

	// The reusable file is not read and keeps its content hash.
	if reads["reuse.txt"] != 0 {
		t.Errorf("reuse.txt read %d times, want 0 (reused)", reads["reuse.txt"])
	}
	if byPath["reuse.txt"].Blob != reuseBlob || reuseBlob != casOf(reuseContent) {
		t.Errorf("reuse.txt blob = %q, want the file's content hash %q", byPath["reuse.txt"].Blob, casOf(reuseContent))
	}
	// Missing and malformed references are read once.
	for _, rel := range []string{"missing.txt", "malformed.txt"} {
		if reads[rel] != 1 {
			t.Errorf("%s read %d times, want 1 (re-read)", rel, reads[rel])
		}
		content, _ := os.ReadFile(filepath.Join(repoDir, rel))
		if byPath[rel].Blob != casOf(content) {
			t.Errorf("%s blob = %q, want the re-read content hash %q", rel, byPath[rel].Blob, casOf(content))
		}
	}
}
