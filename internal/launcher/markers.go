package launcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/semanticash/cli/internal/platform"
)

// markerExt is the committed marker extension.
const markerExt = ".job"

// tempExt is used for in-flight atomic writes.
const tempExt = ".tmp"

// Marker is the handoff written by the hook and read by the drain
// command.
type Marker struct {
	// CheckpointID also becomes the filename stem.
	CheckpointID string `json:"checkpoint_id"`

	// CommitHash is the linked commit.
	CommitHash string `json:"commit_hash"`

	// RepoRoot is the absolute repository path.
	RepoRoot string `json:"repo_root"`

	// WrittenAt is the hook-side Unix millisecond timestamp.
	WrittenAt int64 `json:"written_at"`
}

// Validate checks the on-disk marker contract.
// RepoRoot uses the host OS's absolute-path rules. Plist paths stay
// POSIX-only because launchd consumes them directly.
func (m Marker) Validate() error {
	if m.CheckpointID == "" {
		return errors.New("marker: CheckpointID is empty")
	}
	if strings.ContainsAny(m.CheckpointID, `/\`) {
		return fmt.Errorf(
			"marker: CheckpointID contains a path separator: %q",
			m.CheckpointID,
		)
	}
	if m.CommitHash == "" {
		return errors.New("marker: CommitHash is empty")
	}
	if m.RepoRoot == "" {
		return errors.New("marker: RepoRoot is empty")
	}
	if !filepath.IsAbs(m.RepoRoot) {
		return fmt.Errorf(
			"marker: RepoRoot must be an absolute path, got %q",
			m.RepoRoot,
		)
	}
	if m.WrittenAt == 0 {
		return errors.New("marker: WrittenAt is zero")
	}
	return nil
}

// PendingDir returns <repoRoot>/.semantica/pending.
func PendingDir(repoRoot string) string {
	return filepath.Join(repoRoot, ".semantica", "pending")
}

// MarkerPath returns the canonical marker path.
func MarkerPath(repoRoot, checkpointID string) string {
	return filepath.Join(PendingDir(repoRoot), checkpointID+markerExt)
}

// Write atomically persists a marker to the pending directory.
func Write(m Marker) error {
	if err := m.Validate(); err != nil {
		return err
	}
	dir := PendingDir(m.RepoRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create pending dir: %w", err)
	}

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal marker: %w", err)
	}
	data = append(data, '\n')

	final := MarkerPath(m.RepoRoot, m.CheckpointID)
	tmp := final + tempExt
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write marker tmp: %w", err)
	}
	if err := platform.SafeRename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename marker: %w", err)
	}
	return nil
}

// Read loads and validates one marker. Use ReadInQueue when the
// caller discovered the file from a specific queue directory.
func Read(path string) (Marker, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Marker{}, err
	}
	var m Marker
	if err := json.Unmarshal(data, &m); err != nil {
		return Marker{}, fmt.Errorf("parse marker %s: %w", path, err)
	}
	if err := m.Validate(); err != nil {
		return Marker{}, fmt.Errorf("marker %s invalid: %w", path, err)
	}
	return m, nil
}

// ReadInQueue loads a marker and checks that it matches the queue
// it came from.
func ReadInQueue(repoRoot, path string) (Marker, error) {
	m, err := Read(path)
	if err != nil {
		return Marker{}, err
	}
	if filepath.Clean(m.RepoRoot) != filepath.Clean(repoRoot) {
		return Marker{}, fmt.Errorf(
			"marker %s: RepoRoot %q does not match queue root %q",
			path, m.RepoRoot, repoRoot,
		)
	}
	wantBase := m.CheckpointID + markerExt
	if filepath.Base(path) != wantBase {
		return Marker{}, fmt.Errorf(
			"marker %s: filename does not match CheckpointID (expected basename %q)",
			path, wantBase,
		)
	}
	return m, nil
}

// List returns committed marker paths in lexical order. A missing
// directory is treated as empty.
func List(repoRoot string) ([]string, error) {
	dir := PendingDir(repoRoot)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// filepath.Ext(".job.tmp") is ".tmp", so this filters
		// partial writes as well as unrelated files.
		if filepath.Ext(name) != markerExt {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	sort.Strings(out)
	return out, nil
}

// Delete removes a marker file. Missing files are ignored.
func Delete(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
