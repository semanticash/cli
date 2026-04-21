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

// markerExt is the on-disk filename extension for committed
// markers. Kept as a private constant because List also needs to
// recognize the extension when filtering the directory listing.
const markerExt = ".job"

// tempExt is the extension used for in-flight atomic writes. A
// marker file at <name>.job.tmp represents a Write that started
// but did not rename; List ignores it so a partial write does not
// feed a half-initialized job into the drain loop.
const tempExt = ".tmp"

// Marker is the handoff envelope written by the post-commit hook
// and read by the drain command. Each marker describes exactly
// one pending worker job for a specific checkpoint in a specific
// repository.
type Marker struct {
	// CheckpointID identifies the checkpoint row the worker
	// should process. It is also the filename stem of the
	// marker file, so it must be filesystem-safe. Production
	// callers pass a UUID, which is always safe.
	CheckpointID string `json:"checkpoint_id"`

	// CommitHash is the commit the checkpoint is linked to. It
	// is carried here even though the worker can re-derive it
	// from the checkpoint row, so a reader that wants to log
	// human-readable context without touching the lineage
	// database can do so.
	CommitHash string `json:"commit_hash"`

	// RepoRoot is the absolute path of the repository the
	// worker should run against. Required to be absolute
	// because the drain command has no cwd guarantee and must
	// not resolve this against its own working directory.
	RepoRoot string `json:"repo_root"`

	// WrittenAt is the Unix millisecond timestamp captured at
	// the moment the hook persisted the marker. Caller-provided
	// rather than synthesized inside Write so the persisted
	// value is deterministic for tests and auditable for
	// diagnostics.
	WrittenAt int64 `json:"written_at"`
}

// Validate checks the required invariants on a Marker before it
// is persisted or acted on. Returns nil for a fully-populated
// Marker whose RepoRoot is POSIX-absolute.
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
	if !isPOSIXAbsolute(m.RepoRoot) {
		return fmt.Errorf(
			"marker: RepoRoot must be absolute, got %q",
			m.RepoRoot,
		)
	}
	if m.WrittenAt == 0 {
		return errors.New("marker: WrittenAt is zero")
	}
	return nil
}

// PendingDir returns the path of the pending-marker directory for
// a given repository: <repoRoot>/.semantica/pending/. The caller
// is responsible for ensuring the directory exists before reads
// that need it to be readable; Write creates it on demand.
func PendingDir(repoRoot string) string {
	return filepath.Join(repoRoot, ".semantica", "pending")
}

// MarkerPath returns the canonical on-disk path of the marker for
// a given checkpoint in a given repository. Used both by Write
// (as the destination) and by callers that want to address a
// specific marker directly.
func MarkerPath(repoRoot, checkpointID string) string {
	return filepath.Join(PendingDir(repoRoot), checkpointID+markerExt)
}

// Write persists m to the repository's pending directory,
// atomically via a temp-rename. Creates the pending directory if
// it does not exist. Rejects markers that fail Validate so no
// half-complete file is ever written to disk.
//
// On rename failure the temp file is removed so a crashed write
// does not leave a stale tmp file for List to trip over. (List
// already filters tmp files, but the cleanup keeps the directory
// tidy for anyone inspecting it by hand.)
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

// Read loads and validates a single marker from an absolute path.
// Returns an error when the file is missing, malformed, or
// describes an invalid Marker; the drain loop uses these signals
// to decide whether to retry, log, or drop a marker.
//
// Read only verifies the shape of the JSON and the self-consistency
// of the Marker. It does not verify that the marker agrees with
// its on-disk location. Callers that reach a marker by iterating a
// specific repository's pending directory should prefer
// ReadInQueue, which adds those contextual checks.
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

// ReadInQueue loads a marker from path and additionally verifies
// that its contents agree with the queue location it was read
// from: the marker's RepoRoot must be equal (after filepath.Clean)
// to repoRoot, and its CheckpointID must equal the file's basename
// with the .job extension stripped. Either mismatch rejects the
// marker so the drain loop cannot act on a marker that was moved,
// corrupted, or deliberately crafted to address a different
// repository than the queue it ended up in.
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

// List returns the absolute paths of every committed marker in
// the repository's pending directory, sorted lexicographically
// for determinism. Files that are not *.job (including the
// *.job.tmp files from in-flight writes) are ignored. A missing
// pending directory is treated as an empty listing rather than
// an error because the common case for a newly-opted-in repo is
// "no pending work and no directory yet."
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
		// Reject anything whose full extension is not ".job",
		// which rules out ".job.tmp" (filepath.Ext returns
		// ".tmp" there) as well as unrelated files.
		if filepath.Ext(name) != markerExt {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	sort.Strings(out)
	return out, nil
}

// Delete removes a marker file. A missing file is not an error
// because the drain loop always deletes on successful completion
// and may race against a second drain pass that already processed
// the same marker; treating ErrNotExist as failure would turn a
// benign race into noisy log output.
func Delete(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
