// Package broker implements the device-level event routing layer for Semantica.
//
// The broker maintains a registry of enabled repositories in a JSON file
// (~/.semantica/repos.json) and routes agent events into the correct per-repo
// lineage databases based on touched file paths.
package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// RegisteredRepo represents a repository entry in the JSON registry.
type RegisteredRepo struct {
	RepoID        string `json:"repo_id"`
	Path          string `json:"path"`
	CanonicalPath string `json:"canonical_path"`
	EnabledAt     int64  `json:"enabled_at"`
	DisabledAt    *int64 `json:"disabled_at,omitempty"`
	Active        bool   `json:"active"`
}

// registry is the on-disk JSON structure at repos.json.
type registry struct {
	Repos []RegisteredRepo `json:"repos"`
}

// Handle holds the loaded registry and the file path.
type Handle struct {
	path     string
	registry registry
}

// GlobalBase returns the Semantica global directory. Defaults to
// ~/.semantica but can be overridden via the SEMANTICA_HOME env var
// (primarily for test isolation).
func GlobalBase() (string, error) {
	if dir := os.Getenv("SEMANTICA_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".semantica"), nil
}

// DefaultRegistryPath returns the registry file path (<globalBase>/repos.json).
func DefaultRegistryPath() (string, error) {
	base, err := GlobalBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "repos.json"), nil
}

// GlobalObjectsDir returns the path to the global blob store directory
// (<globalBase>/objects). Used by hook capture, worker reconciliation,
// and commit-msg catch-up as the single source blob store before events
// are routed and copied into per-repo stores.
func GlobalObjectsDir() (string, error) {
	base, err := GlobalBase()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "objects"), nil
}

// Open loads (or initializes) the registry at registryPath.
func Open(ctx context.Context, registryPath string) (*Handle, error) {
	if registryPath == "" {
		return nil, fmt.Errorf("registry path is empty")
	}

	if err := os.MkdirAll(filepath.Dir(registryPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir registry dir: %w", err)
	}

	h := &Handle{path: registryPath}

	data, err := os.ReadFile(registryPath)
	if err != nil {
		if os.IsNotExist(err) {
			return h, nil
		}
		return nil, fmt.Errorf("read registry: %w", err)
	}
	if err := json.Unmarshal(data, &h.registry); err != nil {
		return nil, fmt.Errorf("parse registry: %w", err)
	}
	return h, nil
}

// Close is a no-op retained for caller symmetry (defer broker.Close(bh)).
func Close(h *Handle) error {
	return nil
}

// Register adds or reactivates a repository in the registry.
// canonicalPath should be the symlink-resolved, cleaned absolute path.
func Register(ctx context.Context, h *Handle, repoPath, canonicalPath string) error {
	return h.mutate(func(repos []RegisteredRepo) []RegisteredRepo {
		now := time.Now().UnixMilli()
		for i, r := range repos {
			if r.CanonicalPath == canonicalPath {
				repos[i].Path = repoPath
				repos[i].EnabledAt = now
				repos[i].DisabledAt = nil
				repos[i].Active = true
				return repos
			}
		}
		return append(repos, RegisteredRepo{
			RepoID:        uuid.NewString(),
			Path:          repoPath,
			CanonicalPath: canonicalPath,
			EnabledAt:     now,
			Active:        true,
		})
	})
}

// Prune removes registry entries whose .semantica directory no longer exists.
// Called best-effort during status checks to clean up after manual deletions.
// Note: drops entries on any os.Stat error, not just ErrNotExist. For
// conservative cleanup, use PruneConfirmedMissing instead.
func Prune(ctx context.Context, h *Handle) error {
	return h.mutate(func(repos []RegisteredRepo) []RegisteredRepo {
		var kept []RegisteredRepo
		for _, r := range repos {
			semDir := filepath.Join(r.Path, ".semantica")
			if _, err := os.Stat(semDir); err == nil {
				kept = append(kept, r)
			}
		}
		return kept
	})
}

// PruneConfirmedMissing removes registry entries only when their .semantica
// directory is confirmed missing (os.ErrNotExist). Permission errors and
// transient I/O failures keep the entry. Returns the number of entries removed.
func PruneConfirmedMissing(ctx context.Context, h *Handle) (int, error) {
	var removed int
	err := h.mutate(func(repos []RegisteredRepo) []RegisteredRepo {
		var kept []RegisteredRepo
		for _, r := range repos {
			semDir := filepath.Join(r.Path, ".semantica")
			_, statErr := os.Stat(semDir)
			if errors.Is(statErr, os.ErrNotExist) {
				removed++
			} else {
				kept = append(kept, r)
			}
		}
		return kept
	})
	return removed, err
}

// Deactivate marks a repository as inactive in the registry.
// Does not delete the entry - allows re-registration later.
func Deactivate(ctx context.Context, h *Handle, canonicalPath string) error {
	return h.mutate(func(repos []RegisteredRepo) []RegisteredRepo {
		now := time.Now().UnixMilli()
		for i, r := range repos {
			if r.CanonicalPath == canonicalPath {
				repos[i].Active = false
				repos[i].DisabledAt = &now
				return repos
			}
		}
		return repos
	})
}

// ListAllRepos returns all registered repositories (active and inactive).
func ListAllRepos(ctx context.Context, h *Handle) ([]RegisteredRepo, error) {
	return append([]RegisteredRepo(nil), h.registry.Repos...), nil
}

// ListActiveRepos returns all currently active registered repositories.
func ListActiveRepos(ctx context.Context, h *Handle) ([]RegisteredRepo, error) {
	var active []RegisteredRepo
	for _, r := range h.registry.Repos {
		if r.Active {
			active = append(active, r)
		}
	}
	return active, nil
}

// mutate applies fn to the registry under an exclusive file lock.
// It re-reads the file before applying fn to prevent lost updates from
// concurrent writers, then writes atomically via temp file + rename.
func (h *Handle) mutate(fn func([]RegisteredRepo) []RegisteredRepo) error {
	lockPath := h.path + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	defer func() { _ = lockFile.Close() }()

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) }()

	// Re-read from disk under the lock to get latest state.
	var reg registry
	if data, err := os.ReadFile(h.path); err == nil {
		if err := json.Unmarshal(data, &reg); err != nil {
			return fmt.Errorf("parse %s: file is corrupt: %w", h.path, err)
		}
	}

	// Apply mutation.
	reg.Repos = fn(reg.Repos)

	// Update in-memory state.
	h.registry = reg

	// Write atomically.
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal registry: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(h.path)
	tmp, err := os.CreateTemp(dir, ".repos-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, h.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}

// CanonicalRepoPath resolves symlinks and cleans the path for use as the
// canonical repo identity in the broker registry.
func CanonicalRepoPath(repoRoot string) string {
	resolved, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return filepath.Clean(repoRoot)
	}
	return filepath.Clean(resolved)
}

// PathBelongsToRepo returns true if path equals repoRoot or is a subdirectory
// of repoRoot, after canonicalization. This handles the common case where a
// tool session is launched from a subdirectory inside a registered repo.
func PathBelongsToRepo(path, repoRoot string) bool {
	cp := canonicalBestEffort(path)
	cr := canonicalBestEffort(repoRoot)
	if cp == cr {
		return true
	}
	prefix := cr
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return strings.HasPrefix(cp, prefix)
}

// canonicalBestEffort resolves symlinks for the longest existing ancestor of
// the path, then appends the remaining (non-existent) tail components.
//
// This is critical for correct path comparison when one path exists on disk
// and the other does not. On macOS, /var is a symlink to /private/var, so
// filepath.EvalSymlinks("/var/folders/.../repo") returns "/private/var/..."
// but fails on "/var/folders/.../repo/subdir" if subdir doesn't exist -
// leaving the /var prefix unresolved. A naive comparison would then fail to
// match /private/var/.../repo against /var/.../repo/subdir even though one
// is clearly a subdirectory of the other.
func canonicalBestEffort(p string) string {
	resolved, err := filepath.EvalSymlinks(p)
	if err == nil {
		return filepath.Clean(resolved)
	}
	dir := filepath.Dir(p)
	base := filepath.Base(p)
	if dir == p {
		return filepath.Clean(p)
	}
	return filepath.Join(canonicalBestEffort(dir), base)
}
