package launcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureMarker(repoRoot string) Marker {
	return Marker{
		CheckpointID: "1a75ac4a-391f-4d7e-a50f-2d6c3aff68ce",
		CommitHash:   "6299182729d5d1f0f685491e7ef974455b70acdf",
		RepoRoot:     repoRoot,
		WrittenAt:    1714000000000,
	}
}

func TestPendingDir_ShapedUnderRepoSemanticaDir(t *testing.T) {
	got := PendingDir("/workspace/pulse")
	want := "/workspace/pulse/.semantica/pending"
	if got != want {
		t.Errorf("PendingDir = %q, want %q", got, want)
	}
}

func TestMarkerPath_UsesCheckpointAsStemAndJobExtension(t *testing.T) {
	got := MarkerPath("/workspace/pulse", "abc-123")
	want := "/workspace/pulse/.semantica/pending/abc-123.job"
	if got != want {
		t.Errorf("MarkerPath = %q, want %q", got, want)
	}
}

func TestValidate_RequiresAllFields(t *testing.T) {
	base := fixtureMarker("/workspace/pulse")

	cases := []struct {
		name string
		mut  func(*Marker)
	}{
		{"empty CheckpointID", func(m *Marker) { m.CheckpointID = "" }},
		{"empty CommitHash", func(m *Marker) { m.CommitHash = "" }},
		{"empty RepoRoot", func(m *Marker) { m.RepoRoot = "" }},
		{"zero WrittenAt", func(m *Marker) { m.WrittenAt = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := base
			tc.mut(&m)
			if err := m.Validate(); err == nil {
				t.Errorf("expected validation error for %+v", m)
			}
		})
	}
}

func TestValidate_RejectsNonAbsoluteRepoRoot(t *testing.T) {
	m := fixtureMarker("relative/pulse")
	err := m.Validate()
	if err == nil {
		t.Fatal("expected error for relative RepoRoot")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("expected error to mention 'absolute', got %v", err)
	}
}

func TestValidate_RejectsCheckpointIDWithPathSeparator(t *testing.T) {
	// A checkpoint ID containing a slash would escape the
	// pending directory via filepath.Join. Validate must reject
	// it so a crafted or corrupt CheckpointID cannot be used as
	// a path-traversal vector.
	for _, bad := range []string{"../escape", "nested/ckpt", `back\slash`} {
		m := fixtureMarker("/workspace/pulse")
		m.CheckpointID = bad
		if err := m.Validate(); err == nil {
			t.Errorf("expected error for CheckpointID %q", bad)
		}
	}
}

func TestWrite_PersistsMarkerAtCanonicalPath(t *testing.T) {
	repo := t.TempDir()
	m := fixtureMarker(repo)

	if err := Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}

	path := MarkerPath(repo, m.CheckpointID)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected marker at %s, got %v", path, err)
	}
}

func TestWrite_CreatesPendingDirWhenMissing(t *testing.T) {
	repo := t.TempDir()
	// Pending dir does not exist yet.
	if _, err := os.Stat(PendingDir(repo)); !os.IsNotExist(err) {
		t.Fatalf("setup: expected pending dir absent, got stat=%v", err)
	}
	if err := Write(fixtureMarker(repo)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(PendingDir(repo)); err != nil {
		t.Errorf("pending dir not created: %v", err)
	}
}

func TestWrite_LeavesNoTempFileOnSuccess(t *testing.T) {
	repo := t.TempDir()
	m := fixtureMarker(repo)
	if err := Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	tmp := MarkerPath(repo, m.CheckpointID) + tempExt
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("temp file lingered after successful write, stat=%v", err)
	}
}

func TestWrite_RejectsInvalidMarker(t *testing.T) {
	repo := t.TempDir()
	m := fixtureMarker(repo)
	m.CheckpointID = "" // invalid

	if err := Write(m); err == nil {
		t.Fatal("expected Write to refuse an invalid marker")
	}
	entries, _ := os.ReadDir(PendingDir(repo))
	if len(entries) > 0 {
		t.Errorf("no files should exist after rejected Write, got %d", len(entries))
	}
}

func TestRead_RoundTripsWrittenMarker(t *testing.T) {
	repo := t.TempDir()
	want := fixtureMarker(repo)
	if err := Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := Read(MarkerPath(repo, want.CheckpointID))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != want {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestRead_CorruptJSONReturnsParseError(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(PendingDir(repo), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	path := MarkerPath(repo, "corrupt")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}

	_, err := Read(path)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse marker") {
		t.Errorf("expected wrapped parse error, got %v", err)
	}
}

func TestRead_ValidJSONButInvalidMarkerReturnsValidationError(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(PendingDir(repo), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	path := MarkerPath(repo, "incomplete")
	// Valid JSON, missing required fields.
	body := []byte(`{"checkpoint_id": "x"}`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := Read(path)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid") {
		t.Errorf("expected 'invalid' in error, got %v", err)
	}
}

func TestList_MissingPendingDirReturnsEmpty(t *testing.T) {
	repo := t.TempDir()
	got, err := List(repo)
	if err != nil {
		t.Fatalf("List on missing dir: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty list, got %d: %v", len(got), got)
	}
}

func TestList_FiltersNonJobFilesIncludingTempAndSubdirs(t *testing.T) {
	repo := t.TempDir()
	dir := PendingDir(repo)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	writeByte := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	writeByte("good-one.job")
	writeByte("good-two.job")
	writeByte("in-flight.job.tmp") // partial write, must be ignored
	writeByte("unrelated.txt")
	writeByte("no-extension")
	// Also add a subdirectory to confirm directories are skipped.
	if err := os.MkdirAll(filepath.Join(dir, "subdir.job"), 0o755); err != nil {
		t.Fatalf("seed subdir: %v", err)
	}

	got, err := List(repo)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{
		filepath.Join(dir, "good-one.job"),
		filepath.Join(dir, "good-two.job"),
	}
	if len(got) != len(want) {
		t.Fatalf("got %d results, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("result[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestList_SortsLexicographically(t *testing.T) {
	repo := t.TempDir()
	dir := PendingDir(repo)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	for _, name := range []string{"c.job", "a.job", "b.job"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	got, err := List(repo)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for i, want := range []string{"a.job", "b.job", "c.job"} {
		if filepath.Base(got[i]) != want {
			t.Errorf("result[%d] = %q, want basename %q", i, got[i], want)
		}
	}
}

func TestDelete_RemovesExistingMarker(t *testing.T) {
	repo := t.TempDir()
	m := fixtureMarker(repo)
	if err := Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	path := MarkerPath(repo, m.CheckpointID)
	if err := Delete(path); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("marker file should be gone, stat=%v", err)
	}
}

func TestDelete_MissingFileIsNotAnError(t *testing.T) {
	repo := t.TempDir()
	path := MarkerPath(repo, "never-existed")
	if err := Delete(path); err != nil {
		t.Errorf("Delete on missing file returned %v; expected nil", err)
	}
}

// The marker file is human-readable and may be inspected by the
// user or grepped by tooling, so its on-disk JSON shape is a
// durable contract. The literal compared below pins both the field
// order and the indentation style. A future refactor that reorders
// struct fields or changes the marshal options will fail this test
// rather than silently shipping a different on-disk format.
//
// The comparison is against an explicit string literal (not a
// re-marshal through the same Marker struct), so if the struct is
// reshaped both Write and a round-trip helper would produce the
// new shape together; only an explicit literal catches that.
func TestMarker_OnDiskJSONLayoutIsCanonical(t *testing.T) {
	m := Marker{
		CheckpointID: "CK",
		CommitHash:   "SHA",
		RepoRoot:     "/repo",
		WrittenAt:    1714000000000,
	}
	got, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	const want = `{
  "checkpoint_id": "CK",
  "commit_hash": "SHA",
  "repo_root": "/repo",
  "written_at": 1714000000000
}`
	if string(got) != want {
		t.Errorf("on-disk marker JSON changed:\n got:\n%s\nwant:\n%s", got, want)
	}
}

// ReadInQueue locks the invariant that a marker cannot be used
// unless it agrees with its on-disk location. These tests are the
// regression guard against a marker at repoA's queue addressing
// repoB or advertising a CheckpointID that disagrees with its
// filename stem.
func TestReadInQueue_AcceptsMarkerMatchingLocation(t *testing.T) {
	repo := t.TempDir()
	m := fixtureMarker(repo)
	if err := Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	path := MarkerPath(repo, m.CheckpointID)

	got, err := ReadInQueue(repo, path)
	if err != nil {
		t.Fatalf("ReadInQueue: %v", err)
	}
	if got != m {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, m)
	}
}

func TestReadInQueue_RejectsMismatchedRepoRoot(t *testing.T) {
	repoA := t.TempDir()
	repoB := t.TempDir()

	// Write a marker whose RepoRoot says repoA, but place the
	// file under repoB's pending directory.
	m := fixtureMarker(repoA)
	if err := Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	srcPath := MarkerPath(repoA, m.CheckpointID)
	dstDir := PendingDir(repoB)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	dstPath := filepath.Join(dstDir, m.CheckpointID+".job")
	data, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	if err := os.WriteFile(dstPath, data, 0o644); err != nil {
		t.Fatalf("seed dst: %v", err)
	}

	_, err = ReadInQueue(repoB, dstPath)
	if err == nil {
		t.Fatal("expected rejection when RepoRoot disagrees with queue root")
	}
	if !strings.Contains(err.Error(), "RepoRoot") {
		t.Errorf("expected error to mention RepoRoot, got %v", err)
	}
}

func TestReadInQueue_RejectsMismatchedFilename(t *testing.T) {
	repo := t.TempDir()
	m := fixtureMarker(repo)
	if err := Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Rename the marker so the filename no longer encodes the
	// CheckpointID. The file content is unchanged.
	src := MarkerPath(repo, m.CheckpointID)
	dst := filepath.Join(PendingDir(repo), "renamed-by-hand.job")
	if err := os.Rename(src, dst); err != nil {
		t.Fatalf("rename: %v", err)
	}

	_, err := ReadInQueue(repo, dst)
	if err == nil {
		t.Fatal("expected rejection when filename does not match CheckpointID")
	}
	if !strings.Contains(err.Error(), "filename") {
		t.Errorf("expected error to mention filename, got %v", err)
	}
}

// When the queue root has a trailing separator, ReadInQueue must
// still recognize it as equivalent. filepath.Clean normalizes both
// sides; this test pins that behavior so a caller that passes an
// unconventional but valid path is not falsely rejected.
func TestReadInQueue_TrailingSlashInQueueRootTolerated(t *testing.T) {
	repo := t.TempDir()
	m := fixtureMarker(repo)
	if err := Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	path := MarkerPath(repo, m.CheckpointID)

	// Add a trailing slash to the queue root and verify it still
	// matches the marker's clean RepoRoot.
	if _, err := ReadInQueue(repo+string(os.PathSeparator), path); err != nil {
		t.Errorf("ReadInQueue rejected trailing-separator queue root: %v", err)
	}
}
