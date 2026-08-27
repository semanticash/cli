package blobs

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Manifest scopes (version 2).
const (
	ScopeWorkspace = "workspace" // worktree snapshot at capture time
	ScopeCommit    = "commit"    // immutable tracked Git state of one commit
)

// Commit-scope entry types.
const (
	EntryRegular = "regular"
	EntrySymlink = "symlink"
	EntryGitlink = "gitlink"
)

// Git object formats for commit-scoped manifests. CAS hashes remain SHA-256.
const (
	ObjectFormatSHA1   = "sha1"
	ObjectFormatSHA256 = "sha256"
)

// gitModesForEntry maps each commit entry type to its allowed Git modes.
var gitModesForEntry = map[string]map[string]bool{
	EntryRegular: {"100644": true, "100755": true},
	EntrySymlink: {"120000": true},
	EntryGitlink: {"160000": true},
}

// ParseManifest decodes and validates a manifest. Version 1 is treated as a
// legacy workspace observation. Version 2 requires a valid scope and matching
// fields. Consumers should use this function instead of decoding directly.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("manifest: decode: %w", err)
	}
	if err := m.validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// IsCommitScoped reports whether m is a valid commit-scoped version-2 manifest.
func (m Manifest) IsCommitScoped() bool {
	return m.Version == 2 && m.Scope == ScopeCommit && m.validateCommit() == nil
}

// IsWorkspaceScoped reports whether m is a valid workspace-scoped version-2
// manifest. Version 1 is a legacy workspace observation and is not accepted.
func (m Manifest) IsWorkspaceScoped() bool {
	return m.Version == 2 && m.Scope == ScopeWorkspace && m.validateWorkspace() == nil
}

func (m Manifest) validate() error {
	switch m.Version {
	case 1:
		// Version 1 predates explicit scopes and is never commit-scoped.
		return nil
	case 2:
		switch m.Scope {
		case ScopeCommit:
			return m.validateCommit()
		case ScopeWorkspace:
			return m.validateWorkspace()
		default:
			return fmt.Errorf("manifest: version 2 has unknown scope %q", m.Scope)
		}
	default:
		return fmt.Errorf("manifest: unknown version %d", m.Version)
	}
}

func (m Manifest) validateCommit() error {
	if m.ObjectFormat != ObjectFormatSHA1 && m.ObjectFormat != ObjectFormatSHA256 {
		return fmt.Errorf("manifest: commit scope requires object_format sha1 or sha256, got %q", m.ObjectFormat)
	}
	if !validGitHash(m.CommitHash, m.ObjectFormat) {
		return fmt.Errorf("manifest: commit scope has invalid commit_hash for %s", m.ObjectFormat)
	}
	if !validGitHash(m.TreeID, m.ObjectFormat) {
		return fmt.Errorf("manifest: commit scope has invalid tree_id for %s", m.ObjectFormat)
	}
	if m.CreatedAt != 0 || m.RepoRoot != "" {
		return fmt.Errorf("manifest: commit scope must omit created_at and repo_root")
	}
	for i, f := range m.Files {
		if err := f.validateCommitEntry(m.ObjectFormat); err != nil {
			return err
		}
		// Strict ordering also rejects duplicate paths.
		if i > 0 && m.Files[i-1].Path >= f.Path {
			return fmt.Errorf("manifest: commit entries must be strictly sorted and unique: %q !< %q", m.Files[i-1].Path, f.Path)
		}
	}
	return nil
}

func (m Manifest) validateWorkspace() error {
	if m.CreatedAt == 0 || m.RepoRoot == "" {
		return fmt.Errorf("manifest: workspace scope requires created_at and repo_root")
	}
	if m.ObjectFormat != "" || m.CommitHash != "" || m.TreeID != "" {
		return fmt.Errorf("manifest: workspace scope must omit commit Git identity")
	}
	seen := make(map[string]bool, len(m.Files))
	for _, f := range m.Files {
		if err := f.validateWorkspaceEntry(); err != nil {
			return err
		}
		// Each path must identify one blob.
		if seen[f.Path] {
			return fmt.Errorf("manifest: workspace entry has a duplicate path %q", f.Path)
		}
		seen[f.Path] = true
	}
	return nil
}

func (f ManifestFile) validateCommitEntry(objectFormat string) error {
	if !validPath(f.Path) {
		return fmt.Errorf("manifest: commit entry has an invalid path %q", f.Path)
	}
	modes, ok := gitModesForEntry[f.EntryType]
	if !ok {
		return fmt.Errorf("manifest: commit entry %q has unknown entry_type %q", f.Path, f.EntryType)
	}
	if !modes[f.GitMode] {
		return fmt.Errorf("manifest: commit entry %q has git_mode %q inconsistent with entry_type %q", f.Path, f.GitMode, f.EntryType)
	}
	if !validGitHash(f.GitObjectID, objectFormat) {
		return fmt.Errorf("manifest: commit entry %q has invalid git_object_id", f.Path)
	}
	if f.Mode != 0 || f.ModTimeNs != 0 || f.IsSymlink || f.LinkTarget != "" {
		return fmt.Errorf("manifest: commit entry %q must omit workspace metadata", f.Path)
	}
	if f.EntryType == EntryGitlink {
		if f.Blob != "" || f.Size != 0 {
			return fmt.Errorf("manifest: gitlink entry %q must omit blob and be zero size", f.Path)
		}
		return nil
	}
	// Regular and symlink entries carry canonical committed bytes in the CAS.
	if !validCASHash(f.Blob) {
		return fmt.Errorf("manifest: commit entry %q requires a CAS blob", f.Path)
	}
	if f.Size < 0 {
		return fmt.Errorf("manifest: commit entry %q has negative size", f.Path)
	}
	return nil
}

func (f ManifestFile) validateWorkspaceEntry() error {
	if !validPath(f.Path) {
		return fmt.Errorf("manifest: workspace entry has an invalid path %q", f.Path)
	}
	if f.EntryType != "" || f.GitMode != "" || f.GitObjectID != "" {
		return fmt.Errorf("manifest: workspace entry %q must omit commit Git identity", f.Path)
	}
	if !validCASHash(f.Blob) {
		return fmt.Errorf("manifest: workspace entry %q requires a CAS blob", f.Path)
	}
	return nil
}

// validGitHash validates a lowercase object ID against the declared format.
func validGitHash(s, objectFormat string) bool {
	want := 40
	if objectFormat == ObjectFormatSHA256 {
		want = 64
	}
	return isLowerHex(s, want)
}

// validCASHash reports whether s is a Semantica CAS hash (SHA-256 hex).
func validCASHash(s string) bool {
	return isLowerHex(s, 64)
}

// validPath accepts non-empty UTF-8 paths without NUL bytes.
func validPath(p string) bool {
	return p != "" && utf8.ValidString(p) && strings.IndexByte(p, 0) < 0
}

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
