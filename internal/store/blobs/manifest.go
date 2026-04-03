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
	Version   int64          `json:"version"`
	CreatedAt int64          `json:"created_at"`
	RepoRoot  string         `json:"repo_root"`
	Files     []ManifestFile `json:"files"`
}

type ManifestFile struct {
	Path       string      `json:"path"`
	Blob       string      `json:"blob"`
	Size       int64       `json:"size"`
	Mode       os.FileMode `json:"mode"`
	ModTimeNs  int64       `json:"mod_time_ns,omitempty"`
	IsSymlink  bool        `json:"is_symlink,omitempty"`
	LinkTarget string      `json:"link_target,omitempty"`
}

// ManifestResult holds the output of BuildManifest.
type ManifestResult struct {
	Manifest     Manifest
	ManifestHash string
	TotalBytes   int64
}

// permMask extracts the permission bits we store in the manifest.
const permMask = os.ModeSetuid | os.ModeSetgid | os.ModeSticky | os.ModePerm

// BuildManifest reads each file via readFile, stores blobs, builds a manifest,
// and stores the manifest blob itself. Returns the manifest hash and total size.
//
// If prevFiles is non-nil, unchanged files (same path, size, mode, and mtime)
// reuse the previous blob hash without re-reading or re-hashing the file.
// Symlinks are always re-read regardless of previous state.
func BuildManifest(ctx context.Context, bs *Store, repoRoot string, paths []string, readFile func(rel string) ([]byte, error), prevFiles []ManifestFile) (*ManifestResult, error) {
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

	manifestBytes, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
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
