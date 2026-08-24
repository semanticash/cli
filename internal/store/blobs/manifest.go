package blobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Manifest struct {
	Version int64  `json:"version"`
	Scope   string `json:"scope,omitempty"` // workspace | commit (version 2)

	// Git identity for commit-scoped version-2 manifests.
	ObjectFormat string `json:"object_format,omitempty"` // sha1 | sha256
	CommitHash   string `json:"commit_hash,omitempty"`
	TreeID       string `json:"tree_id,omitempty"`

	// Workspace context for version 1 and workspace-scoped version 2.
	CreatedAt int64  `json:"created_at,omitempty"`
	RepoRoot  string `json:"repo_root,omitempty"`

	Files []ManifestFile `json:"files"`
}

type ManifestFile struct {
	Path string `json:"path"`

	// Semantica CAS hash and uncompressed byte length.
	Blob string `json:"blob,omitempty"`
	Size int64  `json:"size,omitempty"`

	// Git identity for commit-scoped version-2 entries.
	EntryType   string `json:"entry_type,omitempty"` // regular | symlink | gitlink
	GitMode     string `json:"git_mode,omitempty"`
	GitObjectID string `json:"git_object_id,omitempty"`

	// Workspace-scope filesystem observations.
	Mode       os.FileMode `json:"mode,omitempty"`
	ModTimeNs  int64       `json:"mod_time_ns,omitempty"`
	IsSymlink  bool        `json:"is_symlink,omitempty"`
	LinkTarget string      `json:"link_target,omitempty"`
}

// Version-specific wire structs preserve the version-1 byte layout while
// allowing version 2 to omit fields outside its scope. Tags on the exported
// types are used for unmarshaling.

type manifestV1Wire struct {
	Version   int64            `json:"version"`
	CreatedAt int64            `json:"created_at"`
	RepoRoot  string           `json:"repo_root"`
	Files     []manifestV1File `json:"files"`
}

type manifestV1File struct {
	Path       string      `json:"path"`
	Blob       string      `json:"blob"`
	Size       int64       `json:"size"`
	Mode       os.FileMode `json:"mode"`
	ModTimeNs  int64       `json:"mod_time_ns,omitempty"`
	IsSymlink  bool        `json:"is_symlink,omitempty"`
	LinkTarget string      `json:"link_target,omitempty"`
}

type manifestV2Wire struct {
	Version      int64            `json:"version"`
	Scope        string           `json:"scope,omitempty"`
	ObjectFormat string           `json:"object_format,omitempty"`
	CommitHash   string           `json:"commit_hash,omitempty"`
	TreeID       string           `json:"tree_id,omitempty"`
	CreatedAt    int64            `json:"created_at,omitempty"`
	RepoRoot     string           `json:"repo_root,omitempty"`
	Files        []manifestV2File `json:"files"`
}

type manifestV2File struct {
	Path        string      `json:"path"`
	Blob        string      `json:"blob,omitempty"`
	Size        int64       `json:"size,omitempty"`
	EntryType   string      `json:"entry_type,omitempty"`
	GitMode     string      `json:"git_mode,omitempty"`
	GitObjectID string      `json:"git_object_id,omitempty"`
	Mode        os.FileMode `json:"mode,omitempty"`
	ModTimeNs   int64       `json:"mod_time_ns,omitempty"`
	IsSymlink   bool        `json:"is_symlink,omitempty"`
	LinkTarget  string      `json:"link_target,omitempty"`
}

// MarshalJSON emits the wire format for the manifest's version.
func (m Manifest) MarshalJSON() ([]byte, error) {
	if m.Version == 2 {
		w := manifestV2Wire{
			Version: 2, Scope: m.Scope, ObjectFormat: m.ObjectFormat,
			CommitHash: m.CommitHash, TreeID: m.TreeID,
			CreatedAt: m.CreatedAt, RepoRoot: m.RepoRoot,
		}
		if m.Files != nil {
			w.Files = make([]manifestV2File, len(m.Files))
			for i, f := range m.Files {
				// The wire type differs only in its JSON tags.
				w.Files[i] = manifestV2File(f)
			}
		}
		return json.Marshal(w)
	}
	w := manifestV1Wire{Version: m.Version, CreatedAt: m.CreatedAt, RepoRoot: m.RepoRoot}
	if m.Files != nil {
		w.Files = make([]manifestV1File, len(m.Files))
		for i, f := range m.Files {
			w.Files[i] = manifestV1File{
				Path: f.Path, Blob: f.Blob, Size: f.Size, Mode: f.Mode,
				ModTimeNs: f.ModTimeNs, IsSymlink: f.IsSymlink, LinkTarget: f.LinkTarget,
			}
		}
	}
	return json.Marshal(w)
}

// ManifestResult holds the output of BuildManifest.
type ManifestResult struct {
	Manifest     Manifest
	ManifestHash string
	TotalBytes   int64
}

// permMask extracts the permission bits we store in the manifest.
const permMask = os.ModeSetuid | os.ModeSetgid | os.ModeSticky | os.ModePerm

// marshalManifest lets tests cancel during serialization.
var marshalManifest = func(m Manifest) ([]byte, error) { return json.Marshal(m) }

// BuildManifest reads each file via readFile, stores blobs, builds a manifest,
// and stores the manifest blob itself. Returns the manifest hash and total size.
//
// If prevFiles is non-nil, unchanged files (same path, size, mode, and mtime)
// reuse the previous blob hash without re-reading or re-hashing the file.
// Symlinks are always re-read regardless of previous state.
func BuildManifest(ctx context.Context, bs *Store, repoRoot string, paths []string, readFile func(rel string) ([]byte, error), prevFiles []ManifestFile) (*ManifestResult, error) {
	// Reject cancellation before publishing even an empty manifest.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	m := Manifest{
		Version:   1,
		CreatedAt: now,
		RepoRoot:  repoRoot,
		Files:     make([]ManifestFile, 0, len(paths)),
	}

	// Build index from previous manifest for incremental reuse.
	prevIndex := make(map[string]ManifestFile, len(prevFiles))
	for _, pf := range prevFiles {
		prevIndex[pf.Path] = pf
	}

	var totalBytes int64
	for _, rel := range paths {
		// Avoid hashing the remaining files after cancellation.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		absPath := filepath.Join(repoRoot, rel)

		fi, err := os.Lstat(absPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue // Tracked file deleted in the working tree.
			}
			return nil, fmt.Errorf("stat file %s: %w", rel, err)
		}

		mode := fi.Mode() & permMask
		mtimeNs := fi.ModTime().UnixNano()

		mf := ManifestFile{
			Path:      rel,
			Mode:      mode,
			ModTimeNs: mtimeNs,
		}

		if fi.Mode()&os.ModeSymlink != 0 {
			// Always re-read symlinks.
			target, err := os.Readlink(absPath)
			if err != nil {
				return nil, fmt.Errorf("readlink %s: %w", rel, err)
			}
			mf.IsSymlink = true
			mf.LinkTarget = target
			hash, size, err := bs.Put(ctx, []byte{})
			if err != nil {
				return nil, fmt.Errorf("store blob for symlink %s: %w", rel, err)
			}
			mf.Blob = hash
			mf.Size = size
		} else if prev, ok := prevIndex[rel]; ok &&
			!prev.IsSymlink &&
			prev.ModTimeNs != 0 &&
			prev.Size == fi.Size() &&
			prev.Mode == mode &&
			prev.ModTimeNs == mtimeNs {
			// Incremental reuse: metadata matches exactly - skip read+hash.
			mf.Blob = prev.Blob
			mf.Size = prev.Size
			totalBytes += mf.Size
		} else {
			b, err := readFile(rel)
			if err != nil {
				if os.IsNotExist(err) {
					continue // File vanished after stat; skip it.
				}
				return nil, fmt.Errorf("read file %s: %w", rel, err)
			}
			// Store.Put ignores its context, so check after reading.
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			hash, size, err := bs.Put(ctx, b)
			if err != nil {
				return nil, fmt.Errorf("store blob for %s: %w", rel, err)
			}
			mf.Blob = hash
			mf.Size = size
			totalBytes += size
		}

		m.Files = append(m.Files, mf)
	}

	manifestBytes, err := marshalManifest(m)
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}

	// Store.Put ignores its context, so check again after serialization.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	manifestHash, _, err := bs.Put(ctx, manifestBytes)
	if err != nil {
		return nil, fmt.Errorf("store manifest: %w", err)
	}

	return &ManifestResult{
		Manifest:     m,
		ManifestHash: manifestHash,
		TotalBytes:   totalBytes,
	}, nil
}
