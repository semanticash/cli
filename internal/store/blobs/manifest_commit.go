package blobs

import (
	"context"
	"fmt"
	"sort"
)

// CommitTreeEntry describes a tracked entry in a Git tree.
type CommitTreeEntry struct {
	Path        string
	GitMode     string // 100644 | 100755 | 120000 | 160000
	GitObjectID string
	GitType     string // blob | commit
}

// CommitObjectReader streams object contents in request order.
type CommitObjectReader func(ctx context.Context, oids []string, fn func(oid string, content []byte) error) error

// CommitManifestInput contains the Git state used to build a commit manifest.
type CommitManifestInput struct {
	ObjectFormat string // sha1 | sha256
	CommitHash   string
	TreeID       string
	Entries      []CommitTreeEntry
	ReadObjects  CommitObjectReader
	// Previous may provide reusable CAS entries from an earlier commit manifest.
	Previous *Manifest
	// PreviousCommitLink must match Previous.CommitHash to permit reuse.
	PreviousCommitLink string
}

// BuildCommitManifest builds an immutable, commit-scoped version-2 manifest from
// a Git tree. Regular files and symlinks store committed bytes in the CAS;
// gitlinks store identity only. Reuse requires matching Git identity, a verified
// previous commit link, and an existing CAS blob.
func BuildCommitManifest(ctx context.Context, bs *Store, in CommitManifestInput) (*ManifestResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in.ObjectFormat != ObjectFormatSHA1 && in.ObjectFormat != ObjectFormatSHA256 {
		return nil, fmt.Errorf("commit manifest: unsupported object format %q", in.ObjectFormat)
	}
	if !validGitHash(in.CommitHash, in.ObjectFormat) || !validGitHash(in.TreeID, in.ObjectFormat) {
		return nil, fmt.Errorf("commit manifest: invalid commit/tree id for %s", in.ObjectFormat)
	}

	// Index entries from a verified, compatible previous manifest.
	prevByPath := make(map[string]ManifestFile)
	if in.Previous != nil && in.Previous.IsCommitScoped() &&
		in.Previous.ObjectFormat == in.ObjectFormat &&
		in.PreviousCommitLink != "" && in.Previous.CommitHash == in.PreviousCommitLink {
		for _, pf := range in.Previous.Files {
			prevByPath[pf.Path] = pf
		}
	}

	files := make([]ManifestFile, 0, len(in.Entries))
	pending := make(map[string][]int) // oid -> indexes in files awaiting content
	var readOrder []string            // unique oids to read, first-seen order
	seenPaths := make(map[string]bool, len(in.Entries))

	for _, e := range in.Entries {
		if !validPath(e.Path) {
			return nil, fmt.Errorf("commit manifest: invalid path %q", e.Path)
		}
		// Reject duplicates before storing object content.
		if seenPaths[e.Path] {
			return nil, fmt.Errorf("commit manifest: duplicate path %q", e.Path)
		}
		seenPaths[e.Path] = true
		entryType, err := entryTypeForMode(e.GitMode, e.GitType)
		if err != nil {
			return nil, fmt.Errorf("commit manifest: %q: %w", e.Path, err)
		}
		if !validGitHash(e.GitObjectID, in.ObjectFormat) {
			return nil, fmt.Errorf("commit manifest: %q has invalid object id", e.Path)
		}

		mf := ManifestFile{
			Path:        e.Path,
			EntryType:   entryType,
			GitMode:     e.GitMode,
			GitObjectID: e.GitObjectID,
		}
		if entryType == EntryGitlink {
			files = append(files, mf) // no content, size 0
			continue
		}

		// Reuse a matching, still-present CAS blob.
		if prev, ok := prevByPath[e.Path]; ok &&
			prev.EntryType == entryType &&
			prev.GitMode == e.GitMode &&
			prev.GitObjectID == e.GitObjectID &&
			prev.Blob != "" && bs.Exists(prev.Blob) {
			mf.Blob = prev.Blob
			mf.Size = prev.Size
			files = append(files, mf)
			continue
		}

		// Otherwise read the current object.
		idx := len(files)
		files = append(files, mf)
		if _, seen := pending[e.GitObjectID]; !seen {
			readOrder = append(readOrder, e.GitObjectID)
		}
		pending[e.GitObjectID] = append(pending[e.GitObjectID], idx)
	}

	if len(readOrder) > 0 {
		if in.ReadObjects == nil {
			return nil, fmt.Errorf("commit manifest: no object reader for %d object(s)", len(readOrder))
		}
		err := in.ReadObjects(ctx, readOrder, func(oid string, content []byte) error {
			// Reject unexpected and duplicate responses before storing content.
			idxs, ok := pending[oid]
			if !ok {
				return fmt.Errorf("reader returned unexpected or duplicate object %s", oid)
			}
			hash, size, err := bs.Put(ctx, content)
			if err != nil {
				return fmt.Errorf("store blob for %s: %w", oid, err)
			}
			for _, idx := range idxs {
				files[idx].Blob = hash
				files[idx].Size = size
			}
			delete(pending, oid)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("commit manifest: read objects: %w", err)
		}
		if len(pending) > 0 {
			return nil, fmt.Errorf("commit manifest: %d object(s) not returned by the reader", len(pending))
		}
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m := Manifest{
		Version:      2,
		Scope:        ScopeCommit,
		ObjectFormat: in.ObjectFormat,
		CommitHash:   in.CommitHash,
		TreeID:       in.TreeID,
		Files:        files,
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("commit manifest: %w", err)
	}

	var totalBytes int64
	for _, f := range m.Files {
		totalBytes += f.Size
	}

	data, err := marshalManifest(m)
	if err != nil {
		return nil, fmt.Errorf("commit manifest: marshal: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	manifestHash, _, err := bs.Put(ctx, data)
	if err != nil {
		return nil, fmt.Errorf("commit manifest: store: %w", err)
	}
	return &ManifestResult{Manifest: m, ManifestHash: manifestHash, TotalBytes: totalBytes}, nil
}

// entryTypeForMode maps a supported Git mode and object type to a manifest type.
func entryTypeForMode(gitMode, gitType string) (string, error) {
	switch gitMode {
	case "100644", "100755":
		if gitType != "blob" {
			return "", fmt.Errorf("mode %s expects a blob, got %q", gitMode, gitType)
		}
		return EntryRegular, nil
	case "120000":
		if gitType != "blob" {
			return "", fmt.Errorf("symlink mode expects a blob, got %q", gitType)
		}
		return EntrySymlink, nil
	case "160000":
		if gitType != "commit" {
			return "", fmt.Errorf("gitlink mode expects a commit, got %q", gitType)
		}
		return EntryGitlink, nil
	default:
		return "", fmt.Errorf("unsupported git mode %q", gitMode)
	}
}
