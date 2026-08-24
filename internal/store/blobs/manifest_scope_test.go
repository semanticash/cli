package blobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

var (
	sha1Hex   = strings.Repeat("a", 40)
	sha256Hex = strings.Repeat("b", 64)
	casHex    = strings.Repeat("c", 64)
)

func validCommitManifest() Manifest {
	return Manifest{
		Version:      2,
		Scope:        ScopeCommit,
		ObjectFormat: ObjectFormatSHA1,
		CommitHash:   sha1Hex,
		TreeID:       sha1Hex,
		// Strictly sorted by path.
		Files: []ManifestFile{
			{Path: "a.go", Blob: casHex, Size: 12, EntryType: EntryRegular, GitMode: "100644", GitObjectID: sha1Hex},
			{Path: "link", Blob: casHex, Size: 5, EntryType: EntrySymlink, GitMode: "120000", GitObjectID: sha1Hex},
			{Path: "run.sh", Blob: casHex, Size: 3, EntryType: EntryRegular, GitMode: "100755", GitObjectID: sha1Hex},
			{Path: "sub", EntryType: EntryGitlink, GitMode: "160000", GitObjectID: sha1Hex},
		},
	}
}

func validWorkspaceManifest() Manifest {
	return Manifest{
		Version:   2,
		Scope:     ScopeWorkspace,
		CreatedAt: 1000,
		RepoRoot:  "/repo",
		Files: []ManifestFile{
			{Path: "a.go", Blob: casHex, Size: 12, Mode: 0o644, ModTimeNs: 42},
		},
	}
}

func TestParseManifest_RoundTripCommit(t *testing.T) {
	for _, format := range []struct {
		name, of, hash string
	}{
		{"sha1", ObjectFormatSHA1, sha1Hex},
		{"sha256", ObjectFormatSHA256, sha256Hex},
	} {
		t.Run(format.name, func(t *testing.T) {
			m := validCommitManifest()
			m.ObjectFormat = format.of
			m.CommitHash = format.hash
			m.TreeID = format.hash
			for i := range m.Files {
				m.Files[i].GitObjectID = format.hash
			}
			data, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParseManifest(data)
			if err != nil {
				t.Fatalf("ParseManifest: %v", err)
			}
			if !got.IsCommitScoped() {
				t.Error("IsCommitScoped = false, want true")
			}
			// Commit manifests omit workspace context on the wire.
			if strings.Contains(string(data), "created_at") || strings.Contains(string(data), "repo_root") {
				t.Errorf("commit manifest JSON must omit created_at/repo_root: %s", data)
			}
		})
	}
}

func TestParseManifest_LegacyV1Accepted(t *testing.T) {
	// Version 1 is accepted but never treated as commit-scoped.
	m := Manifest{Version: 1, CreatedAt: 5, RepoRoot: "/repo", Files: []ManifestFile{{Path: "a", Blob: casHex, Size: 1, Mode: 0o644}}}
	data, _ := json.Marshal(m)
	got, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("ParseManifest v1: %v", err)
	}
	if got.IsCommitScoped() {
		t.Error("v1 manifest must never be commit-scoped")
	}
}

func TestParseManifest_WorkspaceV2(t *testing.T) {
	data, _ := json.Marshal(validWorkspaceManifest())
	got, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("ParseManifest workspace: %v", err)
	}
	if got.IsCommitScoped() {
		t.Error("workspace manifest must not be commit-scoped")
	}
}

func TestManifestValidate_Rejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"unknown version", func(m *Manifest) { m.Version = 3 }},
		{"unknown scope", func(m *Manifest) { m.Scope = "bogus" }},
		{"commit missing object_format", func(m *Manifest) { m.ObjectFormat = "" }},
		{"commit bad object_format", func(m *Manifest) { m.ObjectFormat = "md5" }},
		{"commit bad commit_hash", func(m *Manifest) { m.CommitHash = "xyz" }},
		{"commit bad tree_id", func(m *Manifest) { m.TreeID = "short" }},
		{"commit hash wrong length for format", func(m *Manifest) { m.ObjectFormat = ObjectFormatSHA256 }}, // hashes stay sha1-length
		{"commit has created_at", func(m *Manifest) { m.CreatedAt = 1 }},
		{"commit has repo_root", func(m *Manifest) { m.RepoRoot = "/repo" }},
		{"entry unknown type", func(m *Manifest) { m.Files[0].EntryType = "weird" }},
		{"entry mode inconsistent", func(m *Manifest) { m.Files[0].GitMode = "120000" }}, // regular with symlink mode
		{"entry bad object id", func(m *Manifest) { m.Files[0].GitObjectID = "nope" }},
		{"entry has workspace mode", func(m *Manifest) { m.Files[0].Mode = 0o644 }},
		{"entry has modtime", func(m *Manifest) { m.Files[0].ModTimeNs = 1 }},
		{"entry has link target", func(m *Manifest) { m.Files[0].LinkTarget = "x" }},
		{"regular entry missing blob", func(m *Manifest) { m.Files[0].Blob = "" }},
		{"regular entry bad blob", func(m *Manifest) { m.Files[0].Blob = "short" }},
		{"gitlink entry has blob", func(m *Manifest) { m.Files[3].Blob = casHex }},
		{"gitlink entry nonzero size", func(m *Manifest) { m.Files[3].Size = 1 }},
		{"empty path", func(m *Manifest) { m.Files[0].Path = "" }},
		{"non-utf8 path", func(m *Manifest) { m.Files[0].Path = string([]byte{0xff, 0xfe}) }},
		{"nul in path", func(m *Manifest) { m.Files[0].Path = "a\x00.go" }},
		{"duplicate path", func(m *Manifest) { m.Files[1].Path = m.Files[0].Path }},
		{"unordered paths", func(m *Manifest) { m.Files[0].Path = "zzz.go" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := validCommitManifest()
			tt.mutate(&m)
			if err := m.validate(); err == nil {
				t.Errorf("validate() = nil, want error for %q", tt.name)
			}
		})
	}
}

func TestWorkspaceValidate_Rejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"missing created_at", func(m *Manifest) { m.CreatedAt = 0 }},
		{"missing repo_root", func(m *Manifest) { m.RepoRoot = "" }},
		{"has object_format", func(m *Manifest) { m.ObjectFormat = ObjectFormatSHA1 }},
		{"has commit_hash", func(m *Manifest) { m.CommitHash = sha1Hex }},
		{"entry has entry_type", func(m *Manifest) { m.Files[0].EntryType = EntryRegular }},
		{"entry has git_mode", func(m *Manifest) { m.Files[0].GitMode = "100644" }},
		{"entry has git_object_id", func(m *Manifest) { m.Files[0].GitObjectID = sha1Hex }},
		{"entry missing blob", func(m *Manifest) { m.Files[0].Blob = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := validWorkspaceManifest()
			tt.mutate(&m)
			if err := m.validate(); err == nil {
				t.Errorf("validate() = nil, want error for %q", tt.name)
			}
		})
	}
}

func TestParseManifest_MalformedJSON(t *testing.T) {
	if _, err := ParseManifest([]byte("{not json")); err == nil {
		t.Error("ParseManifest = nil error for malformed JSON")
	}
}

// Pin the complete version-1 wire format, including optional fields.
func TestManifest_V1WireFormatStable(t *testing.T) {
	m := Manifest{Version: 1, CreatedAt: 5, RepoRoot: "/r", Files: []ManifestFile{
		{Path: "empty.txt", Blob: casHex, Size: 0, Mode: 0o644},
		{Path: "regular.txt", Blob: casHex, Size: 12, Mode: 0o644, ModTimeNs: 1234567890},
		{Path: "link", Blob: casHex, Size: 5, Mode: 0o777, ModTimeNs: 42, IsSymlink: true, LinkTarget: "regular.txt"},
	}}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	const wantJSON = `{"version":1,"created_at":5,"repo_root":"/r","files":[` +
		`{"path":"empty.txt","blob":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","size":0,"mode":420},` +
		`{"path":"regular.txt","blob":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","size":12,"mode":420,"mod_time_ns":1234567890},` +
		`{"path":"link","blob":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","size":5,"mode":511,"mod_time_ns":42,"is_symlink":true,"link_target":"regular.txt"}` +
		`]}`
	if string(data) != wantJSON {
		t.Errorf("v1 manifest JSON drift:\n got: %s\nwant: %s", data, wantJSON)
	}
	const wantHash = "293db8dbbd4856e7720b15538ae2bc0f0f710a24e95674234da9f57234ef5fe6"
	if got := hex.EncodeToString(sha256Sum(data)); got != wantHash {
		t.Errorf("v1 manifest hash = %s, want pinned %s", got, wantHash)
	}
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// A scope claim without valid identity fields is not commit-scoped.
func TestIsCommitScoped_RequiresValidation(t *testing.T) {
	bogus := Manifest{Version: 2, Scope: ScopeCommit}
	if bogus.IsCommitScoped() {
		t.Error("IsCommitScoped = true for a commit manifest with no identity fields")
	}
	if !validCommitManifest().IsCommitScoped() {
		t.Error("IsCommitScoped = false for a valid commit manifest")
	}
}
