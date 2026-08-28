package carryforward

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/store/blobs"
)

// mustParse asserts a fixture is genuinely ParseManifest-valid and returns the
// parsed manifest, so the selector is exercised against real manifests.
func mustParse(t *testing.T, m blobs.Manifest) blobs.Manifest {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	parsed, err := blobs.ParseManifest(data)
	if err != nil {
		t.Fatalf("fixture is not ParseManifest-valid: %v", err)
	}
	return parsed
}

func hexRepeat(c byte, n int) string { return strings.Repeat(string(c), n) }

// cas returns a valid 64-hex CAS hash seeded by a hex character.
func cas(c byte) string { return hexRepeat(c, 64) }

func commitFixture(t *testing.T, entries ...blobs.ManifestFile) blobs.Manifest {
	return mustParse(t, blobs.Manifest{
		Version: 2, Scope: blobs.ScopeCommit, ObjectFormat: blobs.ObjectFormatSHA1,
		CommitHash: hexRepeat('c', 40), TreeID: hexRepeat('d', 40), Files: entries,
	})
}

func workspaceFixture(t *testing.T, entries ...blobs.ManifestFile) blobs.Manifest {
	return mustParse(t, blobs.Manifest{
		Version: 2, Scope: blobs.ScopeWorkspace, CreatedAt: 1, RepoRoot: "/repo", Files: entries,
	})
}

func commitReg(path, blob string) blobs.ManifestFile {
	return blobs.ManifestFile{Path: path, Blob: blob, Size: 3, EntryType: blobs.EntryRegular, GitMode: "100644", GitObjectID: hexRepeat('1', 40)}
}

func wsReg(path, blob string) blobs.ManifestFile {
	return blobs.ManifestFile{Path: path, Blob: blob, Size: 3}
}

func observation(id string, seq, cursor int64, m blobs.Manifest) Observation {
	return Observation{CheckpointID: id, Sequence: seq, EventCursor: cursor, EventCursorValid: true, Manifest: m}
}

// A modified file whose observed content matches the commit is eligible,
// anchored to the newest observation.
func TestSelectContinuousPaths_ContinuityHolds(t *testing.T) {
	commit := commitFixture(t, commitReg("a.go", cas('a')), commitReg("b.go", cas('b')))
	observations := []Observation{observation("cp-1", 5, 42, workspaceFixture(t, wsReg("a.go", cas('a')), wsReg("b.go", cas('b'))))}
	got := SelectContinuousPaths(commit, []string{"a.go"}, observations)
	if got == nil || got.CheckpointID != "cp-1" || got.EventCursor != 42 {
		t.Fatalf("anchor = %+v, want cp-1/cursor 42", got)
	}
	if len(got.Paths) != 1 || got.Paths[0] != "a.go" {
		t.Fatalf("paths = %v, want [a.go]", got.Paths)
	}
}

// A change after the observation diverges the whole-file identity: no carry.
func TestSelectContinuousPaths_DivergedContentFailsClosed(t *testing.T) {
	commit := commitFixture(t, commitReg("a.go", cas('e')))
	observations := []Observation{observation("cp-1", 5, 1, workspaceFixture(t, wsReg("a.go", cas('a'))))}
	if got := SelectContinuousPaths(commit, []string{"a.go"}, observations); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

// The newest observation is the only anchor: a path missing there (re-added by
// the commit) is not rescued by an older observation that had it.
func TestSelectContinuousPaths_MissingInNewestObservation(t *testing.T) {
	commit := commitFixture(t, commitReg("a.go", cas('a')))
	observations := []Observation{
		observation("old", 3, 1, workspaceFixture(t, wsReg("a.go", cas('a')))),
		observation("new", 7, 2, workspaceFixture(t, wsReg("b.go", cas('b')))),
	}
	if got := SelectContinuousPaths(commit, []string{"a.go"}, observations); got != nil {
		t.Fatalf("expected nil (newest observation lacks the path), got %+v", got)
	}
}

// A path absent from the commit manifest is not eligible.
func TestSelectContinuousPaths_MissingInCommit(t *testing.T) {
	commit := commitFixture(t, commitReg("a.go", cas('a')))
	observations := []Observation{observation("cp", 5, 1, workspaceFixture(t, wsReg("a.go", cas('a')), wsReg("c.go", cas('c'))))}
	if got := SelectContinuousPaths(commit, []string{"c.go"}, observations); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

// A legacy (version 1) or commit-scoped newest observation cannot anchor.
func TestSelectContinuousPaths_NonWorkspaceObservationFailsClosed(t *testing.T) {
	commit := commitFixture(t, commitReg("a.go", cas('a')))
	legacy := mustParse(t, blobs.Manifest{Version: 1, Files: []blobs.ManifestFile{{Path: "a.go", Blob: cas('a')}}})
	if got := SelectContinuousPaths(commit, []string{"a.go"}, []Observation{observation("cp", 5, 1, legacy)}); got != nil {
		t.Fatalf("legacy observation anchored: %+v", got)
	}
	commitScoped := commitFixture(t, commitReg("a.go", cas('a')))
	if got := SelectContinuousPaths(commit, []string{"a.go"}, []Observation{observation("cp", 5, 1, commitScoped)}); got != nil {
		t.Fatalf("commit-scoped observation anchored: %+v", got)
	}
}

// A non-commit-scope commit manifest fails closed.
func TestSelectContinuousPaths_InvalidCommitManifest(t *testing.T) {
	ws := workspaceFixture(t, wsReg("a.go", cas('a')))
	observations := []Observation{observation("cp", 5, 1, workspaceFixture(t, wsReg("a.go", cas('a'))))}
	if got := SelectContinuousPaths(ws, []string{"a.go"}, observations); got != nil {
		t.Fatalf("workspace-scoped commit manifest accepted: %+v", got)
	}
}

// No observations means no anchor.
func TestSelectContinuousPaths_NoObservations(t *testing.T) {
	commit := commitFixture(t, commitReg("a.go", cas('a')))
	if got := SelectContinuousPaths(commit, []string{"a.go"}, nil); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

// Symlink entries never establish continuity, on either side.
func TestSelectContinuousPaths_NonRegularEntriesExcluded(t *testing.T) {
	// Commit entry is a symlink; a matching blob must not carry forward.
	commitSym := commitFixture(t, blobs.ManifestFile{
		Path: "link", Blob: cas('a'), EntryType: blobs.EntrySymlink, GitMode: "120000", GitObjectID: hexRepeat('2', 40),
	})
	obsReg := []Observation{observation("cp", 5, 1, workspaceFixture(t, wsReg("link", cas('a'))))}
	if got := SelectContinuousPaths(commitSym, []string{"link"}, obsReg); got != nil {
		t.Fatalf("symlink commit entry carried forward: %+v", got)
	}

	// Observation entry is a symlink; a matching blob must not carry forward.
	commitReg2 := commitFixture(t, commitReg("link", cas('a')))
	obsSym := []Observation{observation("cp", 5, 1, workspaceFixture(t, blobs.ManifestFile{Path: "link", Blob: cas('a'), IsSymlink: true, LinkTarget: "t"}))}
	if got := SelectContinuousPaths(commitReg2, []string{"link"}, obsSym); got != nil {
		t.Fatalf("symlink observation entry carried forward: %+v", got)
	}
}

// Duplicate and empty modified paths are ignored.
func TestSelectContinuousPaths_DedupesModifiedPaths(t *testing.T) {
	commit := commitFixture(t, commitReg("a.go", cas('a')))
	observations := []Observation{observation("cp", 5, 1, workspaceFixture(t, wsReg("a.go", cas('a'))))}
	got := SelectContinuousPaths(commit, []string{"", "a.go", "a.go"}, observations)
	if got == nil || len(got.Paths) != 1 || got.Paths[0] != "a.go" {
		t.Fatalf("paths = %v", got)
	}
}

// The single newest observation anchors even when older ones also match.
func TestSelectContinuousPaths_NewestObservationWins(t *testing.T) {
	commit := commitFixture(t, commitReg("a.go", cas('a')))
	observations := []Observation{
		observation("old", 3, 10, workspaceFixture(t, wsReg("a.go", cas('a')))),
		observation("new", 9, 20, workspaceFixture(t, wsReg("a.go", cas('a')))),
		observation("mid", 6, 15, workspaceFixture(t, wsReg("a.go", cas('a')))),
	}
	got := SelectContinuousPaths(commit, []string{"a.go"}, observations)
	if got == nil || got.CheckpointID != "new" || got.Sequence != 9 || got.EventCursor != 20 {
		t.Fatalf("anchor = %+v, want newest (new/9/20)", got)
	}
}

// A tie on the highest sequence is ambiguous and fails closed.
func TestSelectContinuousPaths_AmbiguousNewestFailsClosed(t *testing.T) {
	commit := commitFixture(t, commitReg("a.go", cas('a')))
	observations := []Observation{
		observation("x", 9, 1, workspaceFixture(t, wsReg("a.go", cas('a')))),
		observation("y", 9, 2, workspaceFixture(t, wsReg("a.go", cas('a')))),
	}
	if got := SelectContinuousPaths(commit, []string{"a.go"}, observations); got != nil {
		t.Fatalf("ambiguous newest anchored: %+v", got)
	}
}

// An observation manifest with a duplicate path is not a valid workspace
// manifest, so it fails closed rather than resolving the path to one of the
// conflicting blobs.
func TestSelectContinuousPaths_DuplicateObservationPathFailsClosed(t *testing.T) {
	commit := commitFixture(t, commitReg("a.go", cas('a')))
	// Constructed directly: this manifest cannot pass ParseManifest.
	dup := blobs.Manifest{
		Version: 2, Scope: blobs.ScopeWorkspace, CreatedAt: 1, RepoRoot: "/repo",
		Files: []blobs.ManifestFile{wsReg("a.go", cas('a')), wsReg("a.go", cas('e'))},
	}
	if dup.IsWorkspaceScoped() {
		t.Fatal("duplicate-path manifest unexpectedly validated")
	}
	if got := SelectContinuousPaths(commit, []string{"a.go"}, []Observation{observation("cp", 5, 1, dup)}); got != nil {
		t.Fatalf("duplicate-path observation anchored: %+v", got)
	}
}

// Invalid anchor metadata is rejected before it could be used to load events.
func TestSelectContinuousPaths_InvalidAnchorMetadataFailsClosed(t *testing.T) {
	commit := commitFixture(t, commitReg("a.go", cas('a')))
	ws := workspaceFixture(t, wsReg("a.go", cas('a')))
	cases := []struct {
		name string
		obs  Observation
	}{
		{"empty id", Observation{CheckpointID: "", Sequence: 5, EventCursor: 1, EventCursorValid: true, Manifest: ws}},
		{"non-positive sequence", Observation{CheckpointID: "cp", Sequence: 0, EventCursor: 1, EventCursorValid: true, Manifest: ws}},
		{"missing cursor", Observation{CheckpointID: "cp", Sequence: 5, EventCursorValid: false, Manifest: ws}},
		{"negative cursor", Observation{CheckpointID: "cp", Sequence: 5, EventCursor: -1, EventCursorValid: true, Manifest: ws}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SelectContinuousPaths(commit, []string{"a.go"}, []Observation{c.obs}); got != nil {
				t.Fatalf("anchored on invalid metadata: %+v", got)
			}
		})
	}
}
