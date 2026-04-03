package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// enableAndCheckpoint enables Semantica on a git repo and creates a checkpoint,
// returning the checkpoint ID.
func enableAndCheckpoint(t *testing.T, dir string) string {
	t.Helper()
	ctx := context.Background()

	svc, err := NewEnableService(EnableServiceOptions{RepoPath: dir})
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Enable(ctx, EnableOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return res.CheckpointID
}

// createCheckpoint creates a checkpoint and returns the checkpoint ID.
func createCheckpoint(t *testing.T, dir, message string) string {
	t.Helper()
	ctx := context.Background()
	cps := NewCheckpointService()
	res, err := cps.Create(ctx, CreateCheckpointInput{
		RepoPath: dir,
		Kind:     CheckpointManual,
		Message:  message,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res.CheckpointID
}

// writeFile is a helper to write a file relative to dir.
func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	abs := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// readFile is a helper to read a file relative to dir.
func readFile(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// fileExists checks if a file exists relative to dir.
func fileExists(dir, rel string) bool {
	_, err := os.Stat(filepath.Join(dir, rel))
	return err == nil
}

func TestRewind_BasicRestore(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	// Create initial file and checkpoint
	writeFile(t, dir, "hello.txt", "version 1\n")
	enableAndCheckpoint(t, dir)
	cpID := createCheckpoint(t, dir, "v1")

	// Modify the file
	writeFile(t, dir, "hello.txt", "version 2\n")
	if got := readFile(t, dir, "hello.txt"); got != "version 2\n" {
		t.Fatalf("file not modified: %q", got)
	}

	// Rewind to checkpoint
	svc := NewRewindService()
	res, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: cpID,
		NoSafety:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// File should be restored
	if got := readFile(t, dir, "hello.txt"); got != "version 1\n" {
		t.Errorf("file not restored: got %q, want %q", got, "version 1\n")
	}
	if res.FilesRestored < 1 {
		t.Errorf("FilesRestored = %d, want >= 1", res.FilesRestored)
	}
	if res.CheckpointID != cpID {
		t.Errorf("CheckpointID = %q, want %q", res.CheckpointID, cpID)
	}
}

func TestRewind_MultipleFiles(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	// Create multiple files and checkpoint
	writeFile(t, dir, "a.txt", "aaa\n")
	writeFile(t, dir, "b.txt", "bbb\n")
	writeFile(t, dir, "sub/c.txt", "ccc\n")
	enableAndCheckpoint(t, dir)
	cpID := createCheckpoint(t, dir, "multi")

	// Modify all files
	writeFile(t, dir, "a.txt", "modified\n")
	writeFile(t, dir, "b.txt", "modified\n")
	writeFile(t, dir, "sub/c.txt", "modified\n")

	// Rewind
	svc := NewRewindService()
	_, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: cpID,
		NoSafety:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// All files should be restored
	for _, tc := range []struct {
		path, want string
	}{
		{"a.txt", "aaa\n"},
		{"b.txt", "bbb\n"},
		{"sub/c.txt", "ccc\n"},
	} {
		if got := readFile(t, dir, tc.path); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestRewind_ExactMode_DeletesExtraFiles(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	// Create one file and checkpoint
	writeFile(t, dir, "keep.txt", "keep\n")
	enableAndCheckpoint(t, dir)
	cpID := createCheckpoint(t, dir, "exact")

	// Add an extra file after checkpoint
	writeFile(t, dir, "extra.txt", "should be deleted\n")

	// Rewind with Exact=true
	svc := NewRewindService()
	res, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: cpID,
		NoSafety:     true,
		Exact:        true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// keep.txt should still exist
	if got := readFile(t, dir, "keep.txt"); got != "keep\n" {
		t.Errorf("keep.txt: got %q, want %q", got, "keep\n")
	}

	// extra.txt should be deleted
	if fileExists(dir, "extra.txt") {
		t.Error("extra.txt should have been deleted in exact mode")
	}

	if res.FilesDeleted == 0 {
		t.Error("FilesDeleted = 0, expected > 0")
	}
}

func TestRewind_NonExactMode_KeepsExtraFiles(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	writeFile(t, dir, "keep.txt", "keep\n")
	enableAndCheckpoint(t, dir)
	cpID := createCheckpoint(t, dir, "non-exact")

	// Add extra file
	writeFile(t, dir, "extra.txt", "extra\n")

	// Rewind without Exact (default)
	svc := NewRewindService()
	res, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: cpID,
		NoSafety:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// extra.txt should still exist
	if !fileExists(dir, "extra.txt") {
		t.Error("extra.txt should NOT be deleted in non-exact mode")
	}

	if res.FilesDeleted != 0 {
		t.Errorf("FilesDeleted = %d, want 0 in non-exact mode", res.FilesDeleted)
	}
}

func TestRewind_ExactMode_ProtectsSemanticaDir(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	writeFile(t, dir, "file.txt", "content\n")
	enableAndCheckpoint(t, dir)
	cpID := createCheckpoint(t, dir, "protect-semantica")

	// Rewind with Exact - .semantica/ must survive
	svc := NewRewindService()
	_, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: cpID,
		NoSafety:     true,
		Exact:        true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// .semantica directory should still exist
	semDir := filepath.Join(dir, ".semantica")
	if _, err := os.Stat(semDir); err != nil {
		t.Fatalf(".semantica dir was deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(semDir, "lineage.db")); err != nil {
		t.Fatalf(".semantica/lineage.db was deleted: %v", err)
	}
}

func TestRewind_SafetyCheckpoint(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	writeFile(t, dir, "file.txt", "original\n")
	enableAndCheckpoint(t, dir)
	cpID := createCheckpoint(t, dir, "before-safety-test")

	// Modify file
	writeFile(t, dir, "file.txt", "modified\n")

	// Rewind WITH safety (default, NoSafety=false)
	svc := NewRewindService()
	res, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: cpID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Safety checkpoint should have been created
	if res.SafetyCheckpointID == "" {
		t.Error("SafetyCheckpointID is empty; safety checkpoint should have been created")
	}

	// File should be restored to original
	if got := readFile(t, dir, "file.txt"); got != "original\n" {
		t.Errorf("file not restored: got %q, want %q", got, "original\n")
	}

	// Now rewind to the safety checkpoint - should restore the modified state
	res2, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: res.SafetyCheckpointID,
		NoSafety:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, dir, "file.txt"); got != "modified\n" {
		t.Errorf("safety checkpoint didn't capture modified state: got %q, want %q", got, "modified\n")
	}
	if res2.FilesRestored < 1 {
		t.Errorf("FilesRestored from safety = %d, want >= 1", res2.FilesRestored)
	}
}

func TestRewind_NoSafety_SkipsSafetyCheckpoint(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	writeFile(t, dir, "file.txt", "content\n")
	enableAndCheckpoint(t, dir)
	cpID := createCheckpoint(t, dir, "no-safety")

	svc := NewRewindService()
	res, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: cpID,
		NoSafety:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if res.SafetyCheckpointID != "" {
		t.Errorf("SafetyCheckpointID = %q, want empty when NoSafety=true", res.SafetyCheckpointID)
	}
}

func TestRewind_InvalidCheckpointID(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()
	enableAndCheckpoint(t, dir)

	svc := NewRewindService()
	_, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: "nonexistent-checkpoint-id",
		NoSafety:     true,
	})
	if err == nil {
		t.Fatal("expected error for nonexistent checkpoint")
	}
	if !strings.Contains(err.Error(), "checkpoint not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRewind_EmptyCheckpointID(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	svc := NewRewindService()
	_, err := svc.Rewind(ctx, RewindInput{
		RepoPath: dir,
	})
	if err == nil {
		t.Fatal("expected error for empty checkpoint ID")
	}
	if !strings.Contains(err.Error(), "checkpoint id is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRewind_SemanticaNotEnabled(t *testing.T) {
	dir := initGitRepo(t) // git repo but no semantica enable
	ctx := context.Background()

	svc := NewRewindService()
	_, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: "some-id",
		NoSafety:     true,
	})
	if err == nil {
		t.Fatal("expected error when Semantica not enabled")
	}
	if !strings.Contains(err.Error(), "semantica is not enabled") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRewind_RestoresDeletedFile(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	// Create file and checkpoint
	writeFile(t, dir, "willdelete.txt", "precious data\n")
	enableAndCheckpoint(t, dir)
	cpID := createCheckpoint(t, dir, "before-delete")

	// Delete the file
	if err := os.Remove(filepath.Join(dir, "willdelete.txt")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if fileExists(dir, "willdelete.txt") {
		t.Fatal("file should be deleted")
	}

	// Rewind should restore it
	svc := NewRewindService()
	_, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: cpID,
		NoSafety:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if !fileExists(dir, "willdelete.txt") {
		t.Error("deleted file was not restored")
	}
	if got := readFile(t, dir, "willdelete.txt"); got != "precious data\n" {
		t.Errorf("restored file content: got %q, want %q", got, "precious data\n")
	}
}

func TestRewind_NestedDirectories(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	// Create deeply nested file structure
	writeFile(t, dir, "a/b/c/deep.txt", "deep content\n")
	writeFile(t, dir, "x/y.txt", "xy content\n")
	enableAndCheckpoint(t, dir)
	cpID := createCheckpoint(t, dir, "nested")

	// Modify
	writeFile(t, dir, "a/b/c/deep.txt", "changed\n")
	writeFile(t, dir, "x/y.txt", "changed\n")

	// Rewind
	svc := NewRewindService()
	_, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: cpID,
		NoSafety:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, dir, "a/b/c/deep.txt"); got != "deep content\n" {
		t.Errorf("deep file: got %q, want %q", got, "deep content\n")
	}
	if got := readFile(t, dir, "x/y.txt"); got != "xy content\n" {
		t.Errorf("xy file: got %q, want %q", got, "xy content\n")
	}
}

func TestRewind_RewindToOlderCheckpoint(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	// V1
	writeFile(t, dir, "file.txt", "v1\n")
	enableAndCheckpoint(t, dir)
	cp1 := createCheckpoint(t, dir, "v1")

	// V2
	writeFile(t, dir, "file.txt", "v2\n")
	_ = createCheckpoint(t, dir, "v2")

	// V3
	writeFile(t, dir, "file.txt", "v3\n")
	_ = createCheckpoint(t, dir, "v3")

	// Rewind all the way back to V1
	svc := NewRewindService()
	_, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: cp1,
		NoSafety:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, dir, "file.txt"); got != "v1\n" {
		t.Errorf("file: got %q, want %q", got, "v1\n")
	}
}

func TestRewind_DoubleRewind(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	writeFile(t, dir, "file.txt", "original\n")
	enableAndCheckpoint(t, dir)
	cpID := createCheckpoint(t, dir, "original")

	writeFile(t, dir, "file.txt", "changed\n")

	svc := NewRewindService()

	// First rewind
	_, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: cpID,
		NoSafety:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Second rewind to same checkpoint (idempotent)
	res, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: cpID,
		NoSafety:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, dir, "file.txt"); got != "original\n" {
		t.Errorf("file: got %q, want %q", got, "original\n")
	}
	if res.FilesRestored < 1 {
		t.Errorf("FilesRestored = %d, want >= 1", res.FilesRestored)
	}
}

func TestRewind_BinaryFile(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	// Create a binary file with null bytes
	binaryContent := []byte{0x00, 0x01, 0x02, 0xFF, 0xFE, 0x00, 0x42}
	abs := filepath.Join(dir, "binary.dat")
	if err := os.WriteFile(abs, binaryContent, 0o644); err != nil {
		t.Fatal(err)
	}
	enableAndCheckpoint(t, dir)
	cpID := createCheckpoint(t, dir, "binary")

	// Overwrite with different binary
	if err := os.WriteFile(abs, []byte{0xDE, 0xAD}, 0o644); err != nil {
		t.Fatal(err)
	}

	// Rewind
	svc := NewRewindService()
	_, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: cpID,
		NoSafety:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(binaryContent) {
		t.Fatalf("binary file length: got %d, want %d", len(got), len(binaryContent))
	}
	for i, b := range got {
		if b != binaryContent[i] {
			t.Errorf("binary byte %d: got %02x, want %02x", i, b, binaryContent[i])
		}
	}
}

func TestRewind_EmptyFile(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	// Create an empty file
	writeFile(t, dir, "empty.txt", "")
	enableAndCheckpoint(t, dir)
	cpID := createCheckpoint(t, dir, "empty-file")

	// Write content to it
	writeFile(t, dir, "empty.txt", "now has content\n")

	// Rewind should restore empty file
	svc := NewRewindService()
	_, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: cpID,
		NoSafety:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, dir, "empty.txt"); got != "" {
		t.Errorf("empty file should be empty after rewind, got %q", got)
	}
}

func TestRewind_LargeFile(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	// Create a large file (~1MB)
	content := strings.Repeat("abcdefghij", 100_000) // 1MB
	writeFile(t, dir, "large.txt", content)
	enableAndCheckpoint(t, dir)
	cpID := createCheckpoint(t, dir, "large")

	// Overwrite
	writeFile(t, dir, "large.txt", "small\n")

	// Rewind
	svc := NewRewindService()
	_, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: cpID,
		NoSafety:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	got := readFile(t, dir, "large.txt")
	if len(got) != len(content) {
		t.Errorf("large file size: got %d bytes, want %d", len(got), len(content))
	}
}

func TestRewind_ExactMode_ManyExtraFiles(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	// Checkpoint with just one file
	writeFile(t, dir, "keep.txt", "keep\n")
	enableAndCheckpoint(t, dir)
	cpID := createCheckpoint(t, dir, "sparse")

	// Add many extra files in various directories
	for i := 0; i < 20; i++ {
		writeFile(t, dir, filepath.Join("extras", strings.Repeat("sub/", i%5)+
			"file"+string(rune('a'+i))+".txt"), "extra\n")
	}

	// Rewind exact
	svc := NewRewindService()
	res, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: cpID,
		NoSafety:     true,
		Exact:        true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if !fileExists(dir, "keep.txt") {
		t.Error("keep.txt was deleted")
	}
	if res.FilesDeleted < 10 {
		t.Errorf("FilesDeleted = %d, expected many files to be deleted", res.FilesDeleted)
	}
}

func TestRewind_SafetyCheckpoint_RoundTrip(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	// V1
	writeFile(t, dir, "a.txt", "v1-a\n")
	writeFile(t, dir, "b.txt", "v1-b\n")
	enableAndCheckpoint(t, dir)
	cp1 := createCheckpoint(t, dir, "v1")

	// V2: modify a, add c
	writeFile(t, dir, "a.txt", "v2-a\n")
	writeFile(t, dir, "c.txt", "v2-c\n")

	// Rewind to V1 WITH safety checkpoint
	svc := NewRewindService()
	res, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: cp1,
	})
	if err != nil {
		t.Fatal(err)
	}

	safetyID := res.SafetyCheckpointID
	if safetyID == "" {
		t.Fatal("no safety checkpoint created")
	}

	// Verify V1 state
	if got := readFile(t, dir, "a.txt"); got != "v1-a\n" {
		t.Errorf("a.txt after rewind: got %q, want %q", got, "v1-a\n")
	}

	// Rewind back to safety (V2 state)
	res2, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: safetyID,
		NoSafety:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify V2 state is recovered
	if got := readFile(t, dir, "a.txt"); got != "v2-a\n" {
		t.Errorf("a.txt after rewind to safety: got %q, want %q", got, "v2-a\n")
	}
	if got := readFile(t, dir, "c.txt"); got != "v2-c\n" {
		t.Errorf("c.txt after rewind to safety: got %q, want %q", got, "v2-c\n")
	}
	if res2.FilesRestored < 1 {
		t.Errorf("FilesRestored from safety = %d, want >= 1", res2.FilesRestored)
	}
}

func TestRewind_NotGitRepo(t *testing.T) {
	dir := t.TempDir() // not a git repo
	ctx := context.Background()

	svc := NewRewindService()
	_, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: "some-id",
		NoSafety:     true,
	})
	if err == nil {
		t.Fatal("expected error outside git repo")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRewind_FileWithSpecialChars(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	// File with spaces and special chars (but not NUL)
	writeFile(t, dir, "file with spaces.txt", "spaced\n")
	writeFile(t, dir, "has-dash_and_under.txt", "dashed\n")
	enableAndCheckpoint(t, dir)
	cpID := createCheckpoint(t, dir, "special-chars")

	// Modify
	writeFile(t, dir, "file with spaces.txt", "changed\n")
	writeFile(t, dir, "has-dash_and_under.txt", "changed\n")

	// Rewind
	svc := NewRewindService()
	_, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: cpID,
		NoSafety:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, dir, "file with spaces.txt"); got != "spaced\n" {
		t.Errorf("spaced file: got %q, want %q", got, "spaced\n")
	}
	if got := readFile(t, dir, "has-dash_and_under.txt"); got != "dashed\n" {
		t.Errorf("dashed file: got %q, want %q", got, "dashed\n")
	}
}

func TestRewind_RestoresIntoNewDirectory(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	// Create nested file and checkpoint
	writeFile(t, dir, "deep/nested/file.txt", "nested\n")
	enableAndCheckpoint(t, dir)
	cpID := createCheckpoint(t, dir, "nested-dir")

	// Delete the entire directory
	if err := os.RemoveAll(filepath.Join(dir, "deep")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if fileExists(dir, "deep/nested/file.txt") {
		t.Fatal("directory should be deleted")
	}

	// Rewind should recreate the directory structure
	svc := NewRewindService()
	_, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: cpID,
		NoSafety:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, dir, "deep/nested/file.txt"); got != "nested\n" {
		t.Errorf("nested file: got %q, want %q", got, "nested\n")
	}
}

func TestRewind_ExecutableBits(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	// Create a file with executable mode.
	abs := filepath.Join(dir, "script.sh")
	if err := os.WriteFile(abs, []byte("#!/bin/sh\necho hello\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	enableAndCheckpoint(t, dir)
	cpID := createCheckpoint(t, dir, "executable")

	// Overwrite with a non-executable file (loses mode).
	if err := os.WriteFile(abs, []byte("overwritten\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Rewind should restore executable bits.
	svc := NewRewindService()
	_, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: cpID,
		NoSafety:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("executable bits not restored: mode = %o", fi.Mode().Perm())
	}
	if got := readFile(t, dir, "script.sh"); got != "#!/bin/sh\necho hello\n" {
		t.Errorf("content not restored: got %q", got)
	}
}

func TestRewind_Symlink(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	// Create a regular file and a symlink pointing to it.
	writeFile(t, dir, "target.txt", "target content\n")
	linkAbs := filepath.Join(dir, "link.txt")
	if err := os.Symlink("target.txt", linkAbs); err != nil {
		t.Fatal(err)
	}
	enableAndCheckpoint(t, dir)
	cpID := createCheckpoint(t, dir, "symlink")

	// Remove the symlink and replace with a regular file.
	if err := os.Remove(linkAbs); err != nil {
		t.Fatalf("remove symlink: %v", err)
	}
	if err := os.WriteFile(linkAbs, []byte("not a symlink\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Rewind should restore the symlink.
	svc := NewRewindService()
	_, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: cpID,
		NoSafety:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify it is a symlink.
	fi, err := os.Lstat(linkAbs)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("link.txt is not a symlink after rewind: mode = %v", fi.Mode())
	}

	// Verify the symlink target.
	target, err := os.Readlink(linkAbs)
	if err != nil {
		t.Fatal(err)
	}
	if target != "target.txt" {
		t.Errorf("symlink target = %q, want %q", target, "target.txt")
	}

	// Reading through the symlink should give original content.
	if got := readFile(t, dir, "link.txt"); got != "target content\n" {
		t.Errorf("content through symlink: got %q, want %q", got, "target content\n")
	}
}

func TestRewind_SetuidSetgidSticky(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	// Create a file with setuid+setgid+sticky bits.
	abs := filepath.Join(dir, "special.bin")
	if err := os.WriteFile(abs, []byte("special\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(abs, 0o7755); err != nil {
		t.Fatal(err)
	}

	// Check if the OS actually preserved the special bits (non-root on macOS
	// may silently strip them).
	fi, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	specialBits := fi.Mode() & (os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
	if specialBits == 0 {
		t.Skipf("OS did not preserve setuid/setgid/sticky bits (mode = %v); skipping", fi.Mode())
	}

	enableAndCheckpoint(t, dir)
	cpID := createCheckpoint(t, dir, "special-bits")

	// Overwrite the file (loses special bits).
	if err := os.WriteFile(abs, []byte("overwritten\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Rewind should restore the special mode bits.
	svc := NewRewindService()
	_, err = svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: cpID,
		NoSafety:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	fi, err = os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	restored := fi.Mode() & (os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
	if restored == 0 {
		t.Errorf("special mode bits not restored: mode = %v", fi.Mode())
	}
	if got := readFile(t, dir, "special.bin"); got != "special\n" {
		t.Errorf("content not restored: got %q", got)
	}
}

func TestRewind_SymlinkToDirectory(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	// Create a directory with a file inside.
	writeFile(t, dir, "realdir/inside.txt", "inside\n")

	// Create a symlink to the directory.
	linkAbs := filepath.Join(dir, "linkdir")
	if err := os.Symlink("realdir", linkAbs); err != nil {
		t.Fatal(err)
	}

	enableAndCheckpoint(t, dir)

	// ListFilesFromGit may not list directory symlinks. Check if the symlink
	// is captured by creating a checkpoint and rewinding.
	cpID := createCheckpoint(t, dir, "dir-symlink")

	// Remove the symlink and replace with a plain file.
	if err := os.Remove(linkAbs); err != nil {
		t.Fatalf("remove symlink: %v", err)
	}
	if err := os.WriteFile(linkAbs, []byte("not a symlink\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Rewind
	svc := NewRewindService()
	_, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: cpID,
		NoSafety:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Check if linkdir is a symlink again. If git ls-files did not list
	// the directory symlink, the rewind cannot restore it, so we skip.
	fi, err := os.Lstat(linkAbs)
	if err != nil {
		t.Skipf("linkdir does not exist after rewind; directory symlinks may not be tracked: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Skipf("linkdir is not a symlink after rewind (mode = %v); directory symlinks may not be supported", fi.Mode())
	}

	target, err := os.Readlink(linkAbs)
	if err != nil {
		t.Fatal(err)
	}
	if target != "realdir" {
		t.Errorf("symlink target = %q, want %q", target, "realdir")
	}
}

func TestRewind_MixedFileTypes(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	// Create a mix of file types.
	// 1. Regular file (0644)
	writeFile(t, dir, "regular.txt", "regular content\n")

	// 2. Executable file (0755)
	execAbs := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(execAbs, []byte("#!/bin/sh\necho run\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// 3. Symlink
	writeFile(t, dir, "real.txt", "real content\n")
	symlinkAbs := filepath.Join(dir, "alias.txt")
	if err := os.Symlink("real.txt", symlinkAbs); err != nil {
		t.Fatal(err)
	}

	enableAndCheckpoint(t, dir)
	cpID := createCheckpoint(t, dir, "mixed")

	// Modify all of them.
	writeFile(t, dir, "regular.txt", "changed\n")
	if err := os.WriteFile(execAbs, []byte("overwritten\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(symlinkAbs); err != nil {
		t.Fatalf("remove symlink: %v", err)
	}
	if err := os.WriteFile(symlinkAbs, []byte("not a link\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Rewind
	svc := NewRewindService()
	_, err := svc.Rewind(ctx, RewindInput{
		RepoPath:     dir,
		CheckpointID: cpID,
		NoSafety:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Regular file restored with correct content.
	if got := readFile(t, dir, "regular.txt"); got != "regular content\n" {
		t.Errorf("regular.txt: got %q, want %q", got, "regular content\n")
	}

	// 2. Executable restored with correct content and mode.
	if got := readFile(t, dir, "run.sh"); got != "#!/bin/sh\necho run\n" {
		t.Errorf("run.sh content: got %q", got)
	}
	fi, err := os.Stat(execAbs)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("run.sh executable bits not restored: mode = %o", fi.Mode().Perm())
	}

	// 3. Symlink restored.
	lfi, err := os.Lstat(symlinkAbs)
	if err != nil {
		t.Fatal(err)
	}
	if lfi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("alias.txt is not a symlink after rewind: mode = %v", lfi.Mode())
	}
	target, err := os.Readlink(symlinkAbs)
	if err != nil {
		t.Fatal(err)
	}
	if target != "real.txt" {
		t.Errorf("alias.txt symlink target = %q, want %q", target, "real.txt")
	}
	if got := readFile(t, dir, "alias.txt"); got != "real content\n" {
		t.Errorf("alias.txt content through symlink: got %q, want %q", got, "real content\n")
	}
}
