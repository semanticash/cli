package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/platform"
)

// toolWindowObservationVersion is the persisted manifest schema version.
const toolWindowObservationVersion = 1

// Pending candidates have no recorded outcome. All other outcomes are terminal.
// Snapshot failure and budget expiry indicate unknown coverage; budget expiry
// means no snapshot was attempted.
const (
	observationPending        = "pending"
	observationObserved       = "observed"
	observationSnapshotFailed = "snapshot_failed"
	observationBudgetExpired  = "budget_expired"
)

// Per-window lock retries are bounded.
var (
	observationLockAttempts   = 20
	observationLockRetryDelay = 5 * time.Millisecond
)

// ObservedCandidate identifies a repository and its observation outcome.
// Snapshot data and refs remain in that repository's registry.
type ObservedCandidate struct {
	RepoPath     string `json:"repo_path"`
	RepositoryID string `json:"repository_id"`
	Outcome      string `json:"outcome"`
}

// ToolWindowObservationRecord stores a window's frozen candidates and primary
// publishing target. Additional candidates are observation-only.
// Budget-expired candidates remain in Candidates; cap omissions are counted separately.
type ToolWindowObservationRecord struct {
	Version             int                 `json:"version"`
	CreatedAt           int64               `json:"created_at"` // unix ms
	Provider            string              `json:"provider"`
	SessionID           string              `json:"session_id"`
	TurnID              string              `json:"turn_id,omitempty"`
	ToolUseID           string              `json:"tool_use_id"`
	PrimaryRepoPath     string              `json:"primary_repo_path,omitempty"`
	PrimaryRepositoryID string              `json:"primary_repository_id,omitempty"`
	Candidates          []ObservedCandidate `json:"candidates"`
	OmittedByCap        int                 `json:"omitted_by_cap"`
	// OmittedUnresolved counts candidates with unresolved lineage identities.
	OmittedUnresolved int `json:"omitted_unresolved,omitempty"`
	TotalActive       int `json:"total_active"`
	// CompletionAttempted prevents later deliveries from taking fresh post-snapshots.
	CompletionAttempted bool `json:"completion_attempted,omitempty"`
}

func validObservationOutcome(o string) bool {
	switch o {
	case observationPending, observationObserved, observationSnapshotFailed, observationBudgetExpired:
		return true
	default:
		return false
	}
}

// validateObservationRecord checks candidate identities, outcomes, and counts.
func validateObservationRecord(rec *ToolWindowObservationRecord) error {
	if rec == nil {
		return errors.New("observation manifest: nil record")
	}
	// An absent primary is valid; a partial identity is not.
	if (rec.PrimaryRepoPath == "") != (rec.PrimaryRepositoryID == "") {
		return errors.New("observation manifest: partially specified primary target")
	}
	if rec.OmittedByCap < 0 || rec.OmittedUnresolved < 0 || rec.TotalActive < 0 {
		return errors.New("observation manifest: negative omission or total count")
	}
	// A manifest must contain candidates or coverage gaps.
	if len(rec.Candidates) == 0 && rec.OmittedByCap == 0 && rec.OmittedUnresolved == 0 {
		return errors.New("observation manifest: empty (no candidates or gaps)")
	}
	// Candidates and omissions must account for the eligible total.
	if len(rec.Candidates)+rec.OmittedByCap+rec.OmittedUnresolved != rec.TotalActive {
		return errors.New("observation manifest: inconsistent omission counts")
	}
	seen := make(map[string]bool, len(rec.Candidates))
	for _, c := range rec.Candidates {
		if c.RepoPath == "" || c.RepositoryID == "" {
			return errors.New("observation manifest: empty candidate identity")
		}
		if seen[c.RepoPath] {
			return fmt.Errorf("observation manifest: duplicate candidate %q", c.RepoPath)
		}
		seen[c.RepoPath] = true
		if !validObservationOutcome(c.Outcome) {
			return fmt.Errorf("observation manifest: unknown outcome %q", c.Outcome)
		}
	}
	return nil
}

func toolWindowObservationPath(key toolWindowReceiptKey) (string, error) {
	dir, err := captureDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "obs-"+key.fileName()), nil
}

// lockObservation acquires a per-window file lock with bounded retries.
// The caller must unlock and close the returned file.
func lockObservation(path string) (*os.File, bool) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false
	}
	for attempt := 0; attempt < observationLockAttempts; attempt++ {
		locked, lerr := platform.TryLockFile(f)
		if lerr != nil {
			_ = f.Close()
			return nil, false
		}
		if locked {
			return f, true
		}
		time.Sleep(observationLockRetryDelay)
	}
	_ = f.Close()
	return nil, false
}

// readObservation validates the manifest's version, identity, age, and contents.
// A missing file returns (nil, nil).
func readObservation(path string, key toolWindowReceiptKey) (*ToolWindowObservationRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read observation manifest: %w", err)
	}
	var rec ToolWindowObservationRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("unmarshal observation manifest: %w", err)
	}
	if rec.Version != toolWindowObservationVersion {
		return nil, fmt.Errorf("observation manifest: unexpected version %d", rec.Version)
	}
	if rec.Provider != key.Provider || rec.SessionID != key.SessionID ||
		rec.TurnID != key.TurnID || rec.ToolUseID != key.ToolUseID {
		return nil, errors.New("observation manifest: identity mismatch")
	}
	if age := toolWindowNow() - rec.CreatedAt; age < 0 || age > toolWindowTargetTTL.Milliseconds() {
		return nil, fmt.Errorf("observation manifest: expired (age %dms)", age)
	}
	if err := validateObservationRecord(&rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// writeObservation atomically replaces the manifest using a temporary file.
func writeObservation(path string, rec *ToolWindowObservationRecord) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir capture dir: %w", err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal observation manifest: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "obs-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp observation manifest: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write observation manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close observation manifest: %w", err)
	}
	if err := platform.SafeRename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename observation manifest: %w", err)
	}
	return nil
}

// CreateToolWindowObservation creates a missing manifest and reports whether it
// was created. Existing valid records are preserved; invalid records return an error.
func CreateToolWindowObservation(key toolWindowReceiptKey, rec *ToolWindowObservationRecord) (created bool, err error) {
	if !key.valid() {
		return false, errors.New("observation manifest: incomplete window identity")
	}
	if rec == nil {
		return false, errors.New("observation manifest: nil record")
	}
	rec.Version = toolWindowObservationVersion
	if rec.CreatedAt == 0 {
		rec.CreatedAt = toolWindowNow()
	}
	rec.Provider, rec.SessionID = key.Provider, key.SessionID
	rec.TurnID, rec.ToolUseID = key.TurnID, key.ToolUseID
	if err := validateObservationRecord(rec); err != nil {
		return false, err
	}
	path, err := toolWindowObservationPath(key)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("mkdir capture dir: %w", err)
	}
	lockFile, ok := lockObservation(path)
	if !ok {
		return false, errors.New("observation manifest: lock unavailable")
	}
	defer func() {
		_ = platform.UnlockFile(lockFile)
		_ = lockFile.Close()
	}()

	// Read errors must not allow replacement of a frozen record.
	existing, rerr := readObservation(path, key)
	if rerr != nil {
		return false, rerr
	}
	if existing != nil {
		return false, nil // valid manifest present: preserve frozen candidates and progress
	}
	if err := writeObservation(path, rec); err != nil {
		return false, err
	}
	return true, nil
}

// acquireObservationAttempt locks the full observation attempt using a separate
// sidecar from manifest operations. It returns a release function on success,
// or (nil, false) if the lock cannot be acquired within the retry limit.
func acquireObservationAttempt(key toolWindowReceiptKey) (func(), bool) {
	path, err := toolWindowObservationPath(key)
	if err != nil {
		return nil, false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false
	}
	f, err := os.OpenFile(path+".attempt.lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false
	}
	for attempt := 0; attempt < observationLockAttempts; attempt++ {
		locked, lerr := platform.TryLockFile(f)
		if lerr != nil {
			_ = f.Close()
			return nil, false
		}
		if locked {
			return func() {
				_ = platform.UnlockFile(f)
				_ = f.Close()
			}, true
		}
		time.Sleep(observationLockRetryDelay)
	}
	_ = f.Close()
	return nil, false
}

// CheckpointToolWindowObservation updates candidate outcomes under the window lock.
// Unknown candidates and changes to terminal outcomes are rejected.
func CheckpointToolWindowObservation(key toolWindowReceiptKey, outcomes map[string]string) error {
	if !key.valid() {
		return errors.New("observation manifest: incomplete window identity")
	}
	for repo, o := range outcomes {
		if !validObservationOutcome(o) {
			return fmt.Errorf("observation manifest: unknown outcome %q for %q", o, repo)
		}
	}
	path, err := toolWindowObservationPath(key)
	if err != nil {
		return err
	}
	lockFile, ok := lockObservation(path)
	if !ok {
		return errors.New("observation manifest: lock unavailable")
	}
	defer func() {
		_ = platform.UnlockFile(lockFile)
		_ = lockFile.Close()
	}()

	rec, err := readObservation(path, key)
	if err != nil {
		return err
	}
	if rec == nil {
		return errors.New("observation manifest: not found for checkpoint")
	}
	known := make(map[string]bool, len(rec.Candidates))
	for _, c := range rec.Candidates {
		known[c.RepoPath] = true
	}
	for repo := range outcomes {
		if !known[repo] {
			return fmt.Errorf("observation manifest: checkpoint for unknown candidate %q", repo)
		}
	}
	for i := range rec.Candidates {
		o, ok := outcomes[rec.Candidates[i].RepoPath]
		if !ok {
			continue
		}
		// Terminal outcomes allow identical retries only.
		cur := rec.Candidates[i].Outcome
		if cur != observationPending && o != cur {
			return fmt.Errorf("observation manifest: cannot change terminal outcome %q -> %q for %q",
				cur, o, rec.Candidates[i].RepoPath)
		}
		rec.Candidates[i].Outcome = o
	}
	if err := validateObservationRecord(rec); err != nil {
		return err
	}
	return writeObservation(path, rec)
}

// MarkObservationCompletionAttempted records completion arrival before processing
// candidates. Once set, the marker is never cleared.
func MarkObservationCompletionAttempted(key toolWindowReceiptKey) error {
	if !key.valid() {
		return errors.New("observation manifest: incomplete window identity")
	}
	path, err := toolWindowObservationPath(key)
	if err != nil {
		return err
	}
	lockFile, ok := lockObservation(path)
	if !ok {
		return errors.New("observation manifest: lock unavailable")
	}
	defer func() {
		_ = platform.UnlockFile(lockFile)
		_ = lockFile.Close()
	}()
	rec, err := readObservation(path, key)
	if err != nil {
		return err
	}
	if rec == nil {
		return errors.New("observation manifest: not found for completion mark")
	}
	if rec.CompletionAttempted {
		return nil
	}
	rec.CompletionAttempted = true
	return writeObservation(path, rec)
}

// observeArrivalFault forces completion-arrival recording to fail. Tests only.
var observeArrivalFault error

// recordCompletionArrival exclusively creates an arrival sentinel without the
// attempt lock. It returns true when created, false when present, or an error.
// Creation does not prove first delivery if both earlier arrival records were lost;
// see completeObservedCandidates for that limitation.
func recordCompletionArrival(key toolWindowReceiptKey) (firstArrival bool, err error) {
	if observeArrivalFault != nil {
		return false, observeArrivalFault
	}
	path, perr := toolWindowObservationPath(key)
	if perr != nil {
		return false, perr
	}
	if merr := os.MkdirAll(filepath.Dir(path), 0o755); merr != nil {
		return false, merr
	}
	f, cerr := os.OpenFile(path+".arrived", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if cerr == nil {
		_ = f.Close()
		return true, nil
	}
	if errors.Is(cerr, os.ErrExist) {
		return false, nil // a prior delivery already arrived
	}
	return false, cerr // uncertain: the caller must fail closed
}

// removeCompletionArrival deletes the arrival sentinel for a resolved window.
// Removal is best-effort; leftover sentinels prevent fresh capture for the same key.
func removeCompletionArrival(manifestPath string) {
	_ = os.Remove(manifestPath + ".arrived")
}

// LoadToolWindowObservation returns a validated manifest or (nil, nil) if absent.
// Loading does not depend on whether observation is currently enabled.
func LoadToolWindowObservation(key toolWindowReceiptKey) (*ToolWindowObservationRecord, error) {
	path, err := toolWindowObservationPath(key)
	if err != nil {
		return nil, err
	}
	return readObservation(path, key)
}

// SweepToolWindowObservations removes expired or malformed manifests under
// their window locks. Unavailable locks cause manifests to be skipped.
//
// Lock files are retained to preserve mutual exclusion. File age does not
// establish whether another process is using a lock.
func SweepToolWindowObservations() (int, error) {
	dir, err := captureDir()
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read capture dir: %w", err)
	}
	removed := 0
	var errs []error
	cutoff := toolWindowNow() - toolWindowTargetTTL.Milliseconds()
	for _, e := range entries {
		name := e.Name()
		// Skip lock files and temporary files.
		if !e.Type().IsRegular() || !strings.HasPrefix(name, "obs-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)

		// Avoid locking manifests that do not need cleanup.
		if !observationFileStale(path, cutoff) {
			continue
		}
		if n, err := sweepStaleObservation(path, cutoff); err != nil {
			errs = append(errs, fmt.Errorf("sweep %s: %w", name, err))
		} else {
			removed += n
		}
	}
	return removed, errors.Join(errs...)
}

// observationFileStale checks for invalid JSON, a zero timestamp, or expiry.
// Missing or unreadable files return false.
func observationFileStale(path string, cutoff int64) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var rec ToolWindowObservationRecord
	return json.Unmarshal(data, &rec) != nil || rec.CreatedAt == 0 || rec.CreatedAt < cutoff
}

// sweepStaleObservation rechecks and removes a stale manifest under its lock.
// An unavailable lock returns (0, nil). The lock file is retained.
func sweepStaleObservation(path string, cutoff int64) (int, error) {
	lockFile, ok := lockObservation(path)
	if !ok {
		return 0, nil // cannot coordinate; leave for a later sweep
	}
	defer func() {
		_ = platform.UnlockFile(lockFile)
		_ = lockFile.Close()
	}()
	// Recheck after locking to preserve any intervening refresh.
	if !observationFileStale(path, cutoff) {
		return 0, nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	removeCompletionArrival(path)
	return 1, nil
}

// DeleteToolWindowObservation removes the manifest under its window lock.
// The lock file is retained to preserve lock identity for concurrent callers.
func DeleteToolWindowObservation(key toolWindowReceiptKey) error {
	path, err := toolWindowObservationPath(key)
	if err != nil {
		return err
	}
	lockFile, ok := lockObservation(path)
	if !ok {
		return errors.New("observation manifest: lock unavailable")
	}
	defer func() {
		_ = platform.UnlockFile(lockFile)
		_ = lockFile.Close()
	}()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove observation manifest: %w", err)
	}
	removeCompletionArrival(path)
	return nil
}
