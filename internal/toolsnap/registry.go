package toolsnap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/platform"
)

// ToolKey identifies a tool window. Only hook events enter the registry.
type ToolKey struct {
	RepositoryID string `json:"repository_id"`
	Provider     string `json:"provider"`
	SessionID    string `json:"session_id"`
	TurnID       string `json:"turn_id"`
	ToolUseID    string `json:"tool_use_id"`
}

// hash returns the stable file-name identity for the key.
func (k ToolKey) hash() string {
	sum := sha256.Sum256([]byte(k.RepositoryID + "\x00" + k.Provider + "\x00" +
		k.SessionID + "\x00" + k.TurnID + "\x00" + k.ToolUseID))
	return hex.EncodeToString(sum[:16])
}

// validate requires every identity field and rejects the hash delimiter.
func (k ToolKey) validate() error {
	for _, f := range []string{k.RepositoryID, k.Provider, k.SessionID, k.TurnID, k.ToolUseID} {
		if f == "" {
			return fmt.Errorf("toolsnap: incomplete strict key")
		}
		if strings.ContainsRune(f, 0) {
			return fmt.Errorf("toolsnap: strict key contains NUL")
		}
	}
	return nil
}

// less is a total order over strict keys.
func (k ToolKey) less(o ToolKey) bool {
	if k.RepositoryID != o.RepositoryID {
		return k.RepositoryID < o.RepositoryID
	}
	if k.Provider != o.Provider {
		return k.Provider < o.Provider
	}
	if k.SessionID != o.SessionID {
		return k.SessionID < o.SessionID
	}
	if k.TurnID != o.TurnID {
		return k.TurnID < o.TurnID
	}
	return k.ToolUseID < o.ToolUseID
}

// PendingToolSnapshot is one registered tool window.
type PendingToolSnapshot struct {
	Key          ToolKey `json:"key"`
	ToolName     string  `json:"tool_name"`
	SnapshotRef  string  `json:"snapshot_ref"`
	TreeHash     string  `json:"tree_hash"`
	HeadHash     string  `json:"head_hash"`
	ObjectFormat string  `json:"object_format"`
	StartedAt    int64   `json:"started_at"`
	CompletedAt  int64   `json:"completed_at,omitempty"`
	// Seq is the registry-local capture order. Wall clocks can
	// collide within a millisecond; the sequence, assigned under the
	// lock, identifies the earliest captured snapshot.
	Seq int64 `json:"seq"`
	// EventID and CommandSummary are recorded at completion so group
	// closure can link and describe every member without loading its
	// event.
	EventID        string `json:"event_id,omitempty"`
	CommandSummary string `json:"command_summary,omitempty"`
	GroupID        string `json:"group_id"`
	Status         string `json:"status"` // "active" or "complete"
}

// CompletionInfo carries what a post hook knows about its member at
// completion time.
type CompletionInfo struct {
	At             int64
	EventID        string
	CommandSummary string
}

// ErrNoPendingSnapshot reports a post hook without a matching
// registered window; callers degrade to pre_snapshot_missing.
var ErrNoPendingSnapshot = errors.New("toolsnap: no pending snapshot for key")

// ErrWindowTombstoned reports a post hook for an abandoned window.
var ErrWindowTombstoned = errors.New("toolsnap: window tombstoned")

// ErrWindowSealed reports a post hook for a sealed group.
var ErrWindowSealed = errors.New("toolsnap: window group sealed")

// ErrRegistryCorrupt reports invalid persisted window or final state.
var ErrRegistryCorrupt = errors.New("toolsnap: registry state corrupt")

// Registry coordinates repository-scoped tool windows across hook processes.
type Registry struct {
	dir string
	// Test seams around unlocked recovery writes.
	beforeRecoveryWrites func()
	afterRecoveryWrites  func()
}

// OpenRegistryForInspection opens an existing registry without creating or
// repairing state. It rejects missing, non-directory, and symlinked paths.
func OpenRegistryForInspection(semDir string) (*Registry, error) {
	dir := filepath.Join(semDir, "tool-windows")
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrCaptureStorageAbsent
		}
		return nil, fmt.Errorf("toolsnap: probe registry: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("toolsnap: registry directory is a symlink")
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("toolsnap: registry path is not a directory")
	}
	return &Registry{dir: dir}, nil
}

// OpenRegistry prepares the registry directories under semDir.
func OpenRegistry(semDir string) (*Registry, error) {
	dir := filepath.Join(semDir, "tool-windows")
	// Pre-create recovery paths used after registry publication failures.
	for _, d := range []string{dir, filepath.Join(dir, "tombstones"), filepath.Join(dir, "receipts"), filepath.Join(dir, "closures"), filepath.Join(dir, "partials")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("toolsnap: create registry dir: %w", err)
		}
	}
	r := &Registry{dir: dir}
	// Keep receipt locking available when the registry directory is read-only.
	if f, err := os.OpenFile(r.receiptLockPath(), os.O_RDWR|os.O_CREATE, 0o644); err != nil {
		return nil, fmt.Errorf("toolsnap: create receipt lock: %w", err)
	} else if err := f.Close(); err != nil {
		return nil, fmt.Errorf("toolsnap: create receipt lock: %w", err)
	}
	return r, nil
}

func (r *Registry) statePath() string { return filepath.Join(r.dir, "registry.json") }
func (r *Registry) lockPath() string  { return filepath.Join(r.dir, "registry.lock") }

// GroupFinal preserves a group's closing state across finalization retries.
type GroupFinal struct {
	// PostTreeHash identifies the final workspace tree. The caller
	// must have made it reachable (post ref) before returning it.
	PostTreeHash string `json:"post_tree_hash,omitempty"`
	// DeltaHash identifies the canonical delta when it was computed.
	DeltaHash string `json:"delta_hash,omitempty"`
	// PartialReason marks a group whose final capture is permanently
	// unavailable; retries finalize deterministic partial evidence.
	PartialReason string `json:"partial_reason,omitempty"`
	CapturedAt    int64  `json:"captured_at,omitempty"`
}

func (g GroupFinal) isZero() bool { return g == GroupFinal{} }

// mergeFinal preserves the closing identity and permits same-tree delta enrichment.
func mergeFinal(existing, next GroupFinal) (GroupFinal, error) {
	if existing.isZero() {
		return next, nil
	}
	if next.PostTreeHash != "" && next.PostTreeHash != existing.PostTreeHash {
		return GroupFinal{}, fmt.Errorf("toolsnap: final tree %s conflicts with persisted %s", next.PostTreeHash, existing.PostTreeHash)
	}
	if next.PartialReason != "" && next.PartialReason != existing.PartialReason {
		return GroupFinal{}, fmt.Errorf("toolsnap: partial reason %q conflicts with persisted %q", next.PartialReason, existing.PartialReason)
	}
	merged := existing
	if next.DeltaHash != "" {
		// The tree restatement binds the delta to the captured state.
		if next.PostTreeHash != existing.PostTreeHash {
			return GroupFinal{}, fmt.Errorf("toolsnap: delta enrichment must restate captured tree %s", existing.PostTreeHash)
		}
		if merged.DeltaHash != "" && next.DeltaHash != merged.DeltaHash {
			return GroupFinal{}, fmt.Errorf("toolsnap: delta hash %s conflicts with persisted %s", next.DeltaHash, merged.DeltaHash)
		}
		merged.DeltaHash = next.DeltaHash
	}
	return merged, nil
}

// FinalizeResult is the outcome of one finalization attempt.
type FinalizeResult struct {
	// Done means the group's evidence is fully durable; the group is
	// removed in the same registry transaction.
	Done bool
	// Final preserves captured state for retry. A zero value requires
	// deterministic partial evidence instead of another workspace read.
	Final GroupFinal
}

// GroupMeta records an immutable join horizon and seal state.
type GroupMeta struct {
	CreatedAt int64 `json:"created_at"`
	JoinUntil int64 `json:"join_until"`
	// Sealed groups accept no members and produce only partial evidence.
	Sealed bool `json:"sealed,omitempty"`
}

// registryState holds pending windows and closing states awaiting finalization.
type registryState struct {
	Windows []PendingToolSnapshot `json:"windows"`
	Groups  map[string]GroupMeta  `json:"groups,omitempty"`
	Finals  map[string]GroupFinal `json:"finals,omitempty"`
	// NextSeq assigns the capture order under the lock.
	NextSeq int64 `json:"next_seq,omitempty"`
}

// dropGroup removes a group's windows, final, and metadata together.
func dropGroup(state *registryState, gid string) {
	kept := state.Windows[:0]
	for _, w := range state.Windows {
		if w.GroupID != gid {
			kept = append(kept, w)
		}
	}
	state.Windows = kept
	delete(state.Finals, gid)
	delete(state.Groups, gid)
}

// sealExpiredGroups seals expired groups that still have active members.
// It reports whether state changed.
func sealExpiredGroups(state *registryState, now int64) bool {
	active := map[string]bool{}
	for _, w := range state.Windows {
		if w.Status == "active" {
			active[w.GroupID] = true
		}
	}
	changed := false
	for gid, meta := range state.Groups {
		if meta.Sealed || now < meta.JoinUntil || !active[gid] {
			continue
		}
		meta.Sealed = true
		state.Groups[gid] = meta
		changed = true
	}
	return changed
}

// validate checks window, grouping, and final-state invariants.
func (s *registryState) validate() error {
	keys := map[ToolKey]bool{}
	activeGroups := map[string]bool{}
	repo := ""
	for _, w := range s.Windows {
		if err := w.Key.validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrRegistryCorrupt, err)
		}
		if w.Status != "active" && w.Status != "complete" {
			return fmt.Errorf("%w: window status %q", ErrRegistryCorrupt, w.Status)
		}
		// Completed members require an evidence identity.
		if w.Status == "complete" && (w.EventID == "" || w.CompletedAt <= 0) {
			return fmt.Errorf("%w: completed member without evidence identity", ErrRegistryCorrupt)
		}
		if w.GroupID == "" {
			return fmt.Errorf("%w: window without group id", ErrRegistryCorrupt)
		}
		if keys[w.Key] {
			return fmt.Errorf("%w: duplicate strict key", ErrRegistryCorrupt)
		}
		keys[w.Key] = true
		if repo == "" {
			repo = w.Key.RepositoryID
		} else if w.Key.RepositoryID != repo {
			return fmt.Errorf("%w: windows from multiple repositories", ErrRegistryCorrupt)
		}
		if w.Status == "active" {
			activeGroups[w.GroupID] = true
		}
	}
	// Only one group with active members may remain open for joins.
	unsealed := 0
	for gid := range activeGroups {
		if !s.Groups[gid].Sealed {
			unsealed++
		}
	}
	if unsealed > 1 {
		return fmt.Errorf("%w: %d unsealed groups with active members", ErrRegistryCorrupt, unsealed)
	}
	groupsSeen := map[string]bool{}
	for _, w := range s.Windows {
		groupsSeen[w.GroupID] = true
	}
	for gid, meta := range s.Groups {
		if !groupsSeen[gid] {
			return fmt.Errorf("%w: metadata for absent group %s", ErrRegistryCorrupt, gid)
		}
		if meta.CreatedAt <= 0 || meta.JoinUntil <= meta.CreatedAt {
			return fmt.Errorf("%w: group %s has an invalid join horizon", ErrRegistryCorrupt, gid)
		}
	}
	for gid := range groupsSeen {
		if _, ok := s.Groups[gid]; !ok {
			return fmt.Errorf("%w: group %s without metadata", ErrRegistryCorrupt, gid)
		}
	}
	seqs := map[int64]bool{}
	maxSeq := int64(-1)
	for _, w := range s.Windows {
		if w.Seq < 0 || seqs[w.Seq] {
			return fmt.Errorf("%w: invalid or duplicate capture sequence %d", ErrRegistryCorrupt, w.Seq)
		}
		seqs[w.Seq] = true
		if w.Seq > maxSeq {
			maxSeq = w.Seq
		}
	}
	if len(s.Windows) > 0 && s.NextSeq <= maxSeq {
		return fmt.Errorf("%w: next sequence %d not past max %d", ErrRegistryCorrupt, s.NextSeq, maxSeq)
	}
	groups := map[string]bool{}
	for _, w := range s.Windows {
		groups[w.GroupID] = true
	}
	for gid, f := range s.Finals {
		if !groups[gid] {
			return fmt.Errorf("%w: final for absent group %s", ErrRegistryCorrupt, gid)
		}
		if activeGroups[gid] {
			return fmt.Errorf("%w: final for group %s with active members", ErrRegistryCorrupt, gid)
		}
		captured := f.PostTreeHash != ""
		partial := f.PartialReason != ""
		if captured == partial {
			return fmt.Errorf("%w: final for group %s must be exactly one of captured or partial", ErrRegistryCorrupt, gid)
		}
		if f.DeltaHash != "" && !captured {
			return fmt.Errorf("%w: final for group %s has delta without tree", ErrRegistryCorrupt, gid)
		}
	}
	return nil
}

// lockPollInterval bounds lock polling within the capture deadline.
const lockPollInterval = 10 * time.Millisecond

// withLock validates and atomically updates registry state. A callback may
// persist retry state with an error. The return value reports publication.
func (r *Registry) withLock(ctx context.Context, fn func(*registryState) (persist bool, err error)) (published bool, _ error) {
	if err := ctx.Err(); err != nil {
		return false, &PartialError{Reason: ReasonLockTimeout, Detail: "capture deadline expired before lock acquisition"}
	}
	stopLock := measureStage(ctx, "registry_lock_wait", stageLeaf)
	f, err := os.OpenFile(r.lockPath(), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		stopLock()
		return false, fmt.Errorf("toolsnap: open registry lock: %w", err)
	}
	defer func() { _ = f.Close() }()

	for {
		ok, err := platform.TryLockFile(f)
		if err != nil {
			stopLock()
			return false, fmt.Errorf("toolsnap: registry lock: %w", err)
		}
		if ok {
			break
		}
		select {
		case <-ctx.Done():
			stopLock()
			return false, &PartialError{Reason: ReasonLockTimeout, Detail: "registry lock not acquired within the capture deadline"}
		case <-time.After(lockPollInterval):
		}
	}
	defer func() { _ = platform.UnlockFile(f) }()
	// Recheck after acquisition because the wait may have consumed the deadline.
	if err := ctx.Err(); err != nil {
		stopLock()
		return false, &PartialError{Reason: ReasonLockTimeout, Detail: "capture deadline expired during lock acquisition"}
	}
	// Hold the receipt lock through publication and receipt deletion.
	receiptLock, err := r.acquireReceiptLock(ctx)
	if err != nil {
		stopLock()
		return false, err
	}
	defer func() {
		_ = platform.UnlockFile(receiptLock)
		_ = receiptLock.Close()
	}()
	// Registry operations stop if receipt-lock waiting exhausts the deadline.
	if err := ctx.Err(); err != nil {
		stopLock()
		return false, &PartialError{Reason: ReasonLockTimeout, Detail: "capture deadline expired during receipt lock acquisition"}
	}

	stopLock()
	stopRead := measureStage(ctx, "registry_read", stageLeaf)
	state, applied, migrated, err := r.loadState(r.writeClosureMarker)
	stopRead()
	if err != nil {
		return false, err
	}

	// The callback records its own stages.
	persist, fnErr := fn(&state)
	// Publish migrations and recovered receipts even on reads.
	persist = persist || len(applied) > 0 || migrated
	if !persist {
		return false, fnErr
	}
	// Validate every state transition before publication.
	if err := state.validate(); err != nil {
		return false, errors.Join(fnErr, err)
	}

	// Record the stage even when publication fails.
	if werr := func() error {
		stopWrite := measureStage(ctx, "registry_write", stageLeaf)
		defer stopWrite()
		out, err := json.Marshal(state)
		if err != nil {
			return errors.Join(fnErr, fmt.Errorf("toolsnap: encode registry state: %w", err))
		}
		tmp, err := os.CreateTemp(r.dir, "registry-*.tmp")
		if err != nil {
			return errors.Join(fnErr, fmt.Errorf("toolsnap: registry temp: %w", err))
		}
		tmpName := tmp.Name()
		defer func() { _ = os.Remove(tmpName) }()
		if _, err := tmp.Write(out); err != nil {
			_ = tmp.Close()
			return errors.Join(fnErr, fmt.Errorf("toolsnap: write registry state: %w", err))
		}
		if err := tmp.Sync(); err != nil {
			_ = tmp.Close()
			return errors.Join(fnErr, fmt.Errorf("toolsnap: sync registry state: %w", err))
		}
		if err := tmp.Close(); err != nil {
			return errors.Join(fnErr, fmt.Errorf("toolsnap: close registry state: %w", err))
		}
		// os.Rename cannot replace an existing file on Windows.
		if err := platform.ReplaceFile(tmpName, r.statePath()); err != nil {
			return errors.Join(fnErr, fmt.Errorf("toolsnap: publish registry state: %w", err))
		}
		return nil
	}(); werr != nil {
		return false, werr
	}
	stopClosure := measureStage(ctx, "closure_publish", stageLeaf)
	for _, name := range applied {
		_ = os.Remove(filepath.Join(r.receiptsDir(), name))
	}
	stopClosure()
	return true, fnErr
}

// WithCoordinationLock runs fn while holding the registry and receipt locks.
// It performs no registry reads or writes. Callers may inspect snapshot state
// or release closed-group refs, but must not mutate registry state. Lock
// contention returns a ReasonLockTimeout PartialError.
func (r *Registry) WithCoordinationLock(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return &PartialError{Reason: ReasonLockTimeout, Detail: "deadline expired before lock acquisition"}
	}
	f, err := os.OpenFile(r.lockPath(), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("toolsnap: open registry lock: %w", err)
	}
	defer func() { _ = f.Close() }()
	for {
		ok, err := platform.TryLockFile(f)
		if err != nil {
			return fmt.Errorf("toolsnap: registry lock: %w", err)
		}
		if ok {
			break
		}
		select {
		case <-ctx.Done():
			return &PartialError{Reason: ReasonLockTimeout, Detail: "registry lock not acquired within deadline"}
		case <-time.After(lockPollInterval):
		}
	}
	defer func() { _ = platform.UnlockFile(f) }()
	if err := ctx.Err(); err != nil {
		return &PartialError{Reason: ReasonLockTimeout, Detail: "deadline expired during lock acquisition"}
	}
	// Match withLock's order to avoid deadlocks and receipt publication races.
	receiptLock, err := r.acquireReceiptLock(ctx)
	if err != nil {
		return err
	}
	defer func() {
		_ = platform.UnlockFile(receiptLock)
		_ = receiptLock.Close()
	}()
	if err := ctx.Err(); err != nil {
		return &PartialError{Reason: ReasonLockTimeout, Detail: "deadline expired during receipt lock acquisition"}
	}
	return fn()
}

// lockWithPoll acquires an exclusive lock or returns when ctx expires.
func lockWithPoll(ctx context.Context, f *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return &PartialError{Reason: ReasonLockTimeout, Detail: "registry lock not acquired within deadline"}
		}
		ok, err := platform.TryLockFile(f)
		if err != nil {
			return fmt.Errorf("toolsnap: registry lock: %w", err)
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return &PartialError{Reason: ReasonLockTimeout, Detail: "registry lock not acquired within deadline"}
		case <-time.After(lockPollInterval):
		}
	}
}

// WithCoordinationLockReadOnly runs fn while holding existing registry and
// receipt locks. It never creates state and returns ErrCaptureStorageAbsent
// when either lock file is missing.
func (r *Registry) WithCoordinationLockReadOnly(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return &PartialError{Reason: ReasonLockTimeout, Detail: "deadline expired before lock acquisition"}
	}
	lock, err := os.OpenFile(r.lockPath(), os.O_RDWR, 0o644)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrCaptureStorageAbsent
		}
		return fmt.Errorf("toolsnap: open registry lock: %w", err)
	}
	defer func() { _ = lock.Close() }()
	if err := lockWithPoll(ctx, lock); err != nil {
		return err
	}
	defer func() { _ = platform.UnlockFile(lock) }()

	receipt, err := os.OpenFile(r.receiptLockPath(), os.O_RDWR, 0o644)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrCaptureStorageAbsent
		}
		return fmt.Errorf("toolsnap: open receipt lock: %w", err)
	}
	defer func() { _ = receipt.Close() }()
	if err := lockWithPoll(ctx, receipt); err != nil {
		return err
	}
	defer func() { _ = platform.UnlockFile(receipt) }()
	if err := ctx.Err(); err != nil {
		return &PartialError{Reason: ReasonLockTimeout, Detail: "deadline expired before inspection"}
	}
	return fn()
}

// loadState normalizes, reconciles, and validates registry state.
// A non-nil publishMarker persists closure markers for completed receipts.
func (r *Registry) loadState(publishMarker func(key ToolKey, groupID string, at int64) error) (state registryState, applied []string, migrated bool, _ error) {
	raw, err := os.ReadFile(r.statePath())
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &state); err != nil {
			return state, nil, false, fmt.Errorf("%w: registry state unreadable: %v", ErrRegistryCorrupt, err)
		}
	case os.IsNotExist(err):
	default:
		return state, nil, false, fmt.Errorf("toolsnap: read registry state: %w", err)
	}
	// Legacy states derive capture order from array order.
	if state.NextSeq == 0 && len(state.Windows) > 0 {
		legacy := true
		for _, w := range state.Windows {
			if w.Seq != 0 {
				legacy = false
				break
			}
		}
		if legacy {
			for i := range state.Windows {
				state.Windows[i].Seq = int64(i)
			}
			state.NextSeq = int64(len(state.Windows))
			migrated = true
		}
	}
	// Derive missing group metadata from the earliest member.
	for _, w := range state.Windows {
		if _, ok := state.Groups[w.GroupID]; ok {
			continue
		}
		created := w.StartedAt
		for _, m := range state.Windows {
			if m.GroupID == w.GroupID && m.StartedAt < created {
				created = m.StartedAt
			}
		}
		if state.Groups == nil {
			state.Groups = map[string]GroupMeta{}
		}
		state.Groups[w.GroupID] = GroupMeta{
			CreatedAt: created,
			JoinUntil: created + DefaultStaleActiveAge.Milliseconds(),
		}
		migrated = true
	}
	// Reconcile recovery receipts before returning state.
	applied, err = r.applyReceipts(&state, publishMarker)
	if err != nil {
		return state, nil, false, err
	}
	if err := state.validate(); err != nil {
		return state, nil, false, err
	}
	return state, applied, migrated, nil
}

// keySettled reports whether a key is closed or abandoned.
func (r *Registry) keySettled(key ToolKey) (bool, error) {
	closedBefore, err := r.hasClosureMarker(key)
	if err != nil || closedBefore {
		return closedBefore, err
	}
	return r.HasTombstone(key)
}

// joinGroup joins an overlapping open group or creates a new one.
func joinGroup(state *registryState, key ToolKey, startedAt int64) string {
	sealExpiredGroups(state, startedAt)
	for _, w := range state.Windows {
		if w.Status == "active" && w.Key.RepositoryID == key.RepositoryID && !state.Groups[w.GroupID].Sealed {
			return w.GroupID
		}
	}
	gid := "g-" + key.hash()
	if state.Groups == nil {
		state.Groups = map[string]GroupMeta{}
	}
	state.Groups[gid] = GroupMeta{
		CreatedAt: startedAt,
		JoinUntil: startedAt + DefaultStaleActiveAge.Milliseconds(),
	}
	return gid
}

// ReclaimedGroup summarizes one removed sealed group.
type ReclaimedGroup struct {
	GroupID string
	// Completed members converted to pending partials.
	Completed int
	// Tombstoned member identities.
	Tombstoned int
}

// ReclaimSealedGroups records partial evidence, tombstones every member,
// and removes sealed groups. Recovery writes occur outside the registry
// lock. Removal revalidates the captured member state before publication.
func (r *Registry) ReclaimSealedGroups(ctx context.Context, now int64) ([]ReclaimedGroup, error) {
	// Seal expired groups and snapshot their members under the lock.
	var sealed map[string][]PendingToolSnapshot
	if _, err := r.withLock(ctx, func(state *registryState) (bool, error) {
		changed := sealExpiredGroups(state, now)
		sealed = map[string][]PendingToolSnapshot{}
		for _, w := range state.Windows {
			if state.Groups[w.GroupID].Sealed {
				sealed[w.GroupID] = append(sealed[w.GroupID], w)
			}
		}
		return changed, nil
	}); err != nil {
		return nil, err
	}
	if len(sealed) == 0 {
		return nil, nil
	}

	// Write idempotent recovery files without holding the registry lock.
	if r.beforeRecoveryWrites != nil {
		r.beforeRecoveryWrites()
	}
	var persistErrs []error
	committed := map[string]ReclaimedGroup{}
	for gid, members := range sealed {
		if ctx.Err() != nil {
			break
		}
		g := ReclaimedGroup{GroupID: gid}
		ok := true
		for _, m := range members {
			if ctx.Err() != nil {
				ok = false
				break
			}
			if m.Status != "complete" || m.EventID == "" {
				continue
			}
			if _, err := r.LoadOrRecordPendingPartial(PendingPartialRecord{
				Key: m.Key, EventID: m.EventID,
				Reason: ReasonStaleActiveWindow, ToolName: m.ToolName,
				CommandSummary: m.CommandSummary, Timestamp: m.CompletedAt,
			}); err != nil {
				persistErrs = append(persistErrs, err)
				ok = false
			} else {
				g.Completed++
			}
		}
		for _, m := range members {
			if ctx.Err() != nil {
				ok = false
				break
			}
			if err := r.WriteTombstone(m.Key, now); err != nil {
				persistErrs = append(persistErrs, err)
				ok = false
			} else {
				g.Tombstoned++
			}
		}
		if ok {
			committed[gid] = g
		}
	}

	if r.afterRecoveryWrites != nil {
		r.afterRecoveryWrites()
	}

	// Revalidate member completion state before removing each group.
	type memberSig struct {
		status      string
		eventID     string
		completedAt int64
	}
	sigOf := func(w PendingToolSnapshot) memberSig {
		return memberSig{status: w.Status, eventID: w.EventID, completedAt: w.CompletedAt}
	}
	var candidates []ReclaimedGroup
	if _, err := r.withLock(ctx, func(state *registryState) (bool, error) {
		candidates = nil
		current := map[string]map[ToolKey]memberSig{}
		for _, w := range state.Windows {
			if state.Groups[w.GroupID].Sealed {
				if current[w.GroupID] == nil {
					current[w.GroupID] = map[ToolKey]memberSig{}
				}
				current[w.GroupID][w.Key] = sigOf(w)
			}
		}
		changed := false
		for gid, g := range committed {
			cur, present := current[gid]
			if !present || len(cur) != len(sealed[gid]) {
				continue // gone or altered; a later pass handles it
			}
			unchanged := true
			for _, m := range sealed[gid] {
				if cur[m.Key] != sigOf(m) {
					unchanged = false
					break
				}
			}
			if !unchanged {
				continue
			}
			dropGroup(state, gid)
			candidates = append(candidates, g)
			changed = true
		}
		return changed, nil
	}); err != nil {
		// Publication failed, so no group was durably reclaimed.
		return nil, errors.Join(append([]error{err}, persistErrs...)...)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].GroupID < candidates[j].GroupID })
	return candidates, errors.Join(persistErrs...)
}

// CaptureAndBegin captures and registers a pre-tool snapshot while
// holding the registry lock. Duplicate deliveries reuse the existing entry.
func (r *Registry) CaptureAndBegin(ctx context.Context, s *Store, key ToolKey, toolName string, startedAt int64) (PendingToolSnapshot, error) {
	if err := key.validate(); err != nil {
		return PendingToolSnapshot{}, err
	}
	// Do not capture an already settled identity.
	if settled, err := r.keySettled(key); err != nil {
		return PendingToolSnapshot{}, err
	} else if settled {
		return PendingToolSnapshot{}, nil
	}
	// The registry lock protects only the store in the same .semantica directory.
	if filepath.Dir(r.dir) != filepath.Dir(s.Dir) {
		return PendingToolSnapshot{}, fmt.Errorf("toolsnap: registry %s and store %s belong to different directories",
			filepath.Dir(r.dir), filepath.Dir(s.Dir))
	}
	var result PendingToolSnapshot
	_, err := r.withLock(ctx, func(state *registryState) (bool, error) {
		// Recheck after acquiring the registry lock.
		settled, serr := r.keySettled(key)
		if serr != nil {
			return false, serr
		}
		if settled {
			return false, nil
		}
		for _, w := range state.Windows {
			if w.Key == key {
				result = w
				return false, nil
			}
		}
		groupID := joinGroup(state, key, startedAt)
		snap, err := s.CaptureBefore(ctx)
		if err != nil {
			return false, err
		}
		ref := SnapshotRef(s.repo.WorktreeID, groupID, key.ToolUseID)
		if err := s.CreateRef(ctx, ref, snap.TreeHash); err != nil {
			return false, err
		}
		// If registry publication fails, maintenance retains the orphaned
		// ref until it becomes stale.
		result = PendingToolSnapshot{
			Key: key, ToolName: toolName,
			SnapshotRef: ref, TreeHash: snap.TreeHash, HeadHash: snap.HeadHash,
			ObjectFormat: s.repo.ObjectFormat,
			StartedAt:    startedAt, Seq: state.NextSeq,
			GroupID: groupID, Status: "active",
		}
		state.NextSeq++
		state.Windows = append(state.Windows, result)
		return true, nil
	})
	if err != nil {
		return PendingToolSnapshot{}, err
	}
	return result, nil
}

// Begin registers a pre-existing snapshot. Hook callers use
// CaptureAndBegin so capture and registration share one lock.
func (r *Registry) Begin(ctx context.Context, entry PendingToolSnapshot) (string, error) {
	if entry.Status != "" && entry.Status != "active" {
		return "", fmt.Errorf("toolsnap: begin with status %q", entry.Status)
	}
	if err := entry.Key.validate(); err != nil {
		return "", err
	}
	entry.Status = "active"
	groupID := "g-" + entry.Key.hash()
	_, err := r.withLock(ctx, func(state *registryState) (bool, error) {
		// Retained keys make duplicate delivery idempotent.
		for _, w := range state.Windows {
			if w.Key == entry.Key {
				groupID = w.GroupID
				return false, nil
			}
		}
		groupID = joinGroup(state, entry.Key, entry.StartedAt)
		entry.GroupID = groupID
		entry.Seq = state.NextSeq
		state.NextSeq++
		state.Windows = append(state.Windows, entry)
		return true, nil
	})
	if err != nil {
		return "", err
	}
	return groupID, nil
}

// Complete persists a member and finalizes its group in capture order.
// Retries must use durable state rather than recapturing the workspace.
func (r *Registry) Complete(ctx context.Context, key ToolKey, info CompletionInfo, persistMember func(PendingToolSnapshot) error, finalize func(members []PendingToolSnapshot, prior *GroupFinal, retry bool, recordIntent func() error) (FinalizeResult, error)) (bool, error) {
	if info.EventID == "" || info.At <= 0 {
		return false, fmt.Errorf("toolsnap: completion without event identity or timestamp")
	}
	removed := false
	memberPersisted := false
	sealedHit := false
	var finalDone bool
	var finalIdentity *GroupFinal
	finalGroupID := ""
	published, err := r.withLock(ctx, func(state *registryState) (bool, error) {
		// Do not capture an expired window's unbounded span.
		sealedNow := sealExpiredGroups(state, info.At)
		idx := -1
		retry := false
		for i, w := range state.Windows {
			if w.Key == key {
				idx = i
				retry = w.Status == "complete"
				break
			}
		}
		if idx < 0 {
			// Closure markers make late duplicate posts idempotent.
			closedBefore, merr := r.hasClosureMarker(key)
			if merr != nil {
				return sealedNow, merr
			}
			if closedBefore {
				removed = true
				return sealedNow, nil
			}
			// Distinguish abandonment from a missing pre snapshot.
			tombstoned, terr := r.HasTombstone(key)
			if terr != nil {
				return sealedNow, terr
			}
			if tombstoned {
				return sealedNow, ErrWindowTombstoned
			}
			return sealedNow, ErrNoPendingSnapshot
		}
		if !retry {
			state.Windows[idx].Status = "complete"
			state.Windows[idx].CompletedAt = info.At
			state.Windows[idx].EventID = info.EventID
			state.Windows[idx].CommandSummary = info.CommandSummary
		}
		if state.Groups[state.Windows[idx].GroupID].Sealed {
			// Persist the event before exposing a sealed completion.
			sealedHit = true
			finalGroupID = state.Windows[idx].GroupID
			if !retry && persistMember != nil {
				if err := persistMember(state.Windows[idx]); err != nil {
					return false, err
				}
				memberPersisted = true
			}
			return !retry, nil
		}

		groupID := state.Windows[idx].GroupID
		final := true
		for _, w := range state.Windows {
			if w.GroupID == groupID && w.Status == "active" {
				final = false
				break
			}
		}
		if !final {
			if persistMember != nil {
				if err := persistMember(state.Windows[idx]); err != nil {
					// Persist the event before exposing completion.
					return false, err
				}
				memberPersisted = true
			}
			return !retry, nil
		}

		var members []PendingToolSnapshot
		for _, w := range state.Windows {
			if w.GroupID == groupID {
				members = append(members, w)
			}
		}
		// Sequence order resolves wall-clock ties.
		sort.Slice(members, func(i, j int) bool {
			if members[i].Seq != members[j].Seq {
				return members[i].Seq < members[j].Seq
			}
			return members[i].Key.less(members[j].Key)
		})

		var prior *GroupFinal
		if f, ok := state.Finals[groupID]; ok {
			prior = &f
		}
		res, ferr := finalize(members, prior, retry, r.recordIntentFor(key, info))
		finalGroupID = groupID
		if ferr == nil && res.Done {
			finalDone = true
			// Mark members closed before removing the group.
			for _, m := range members {
				if err := r.writeClosureMarker(m.Key, groupID, info.At); err != nil {
					return true, err
				}
			}
			dropGroup(state, groupID)
			removed = true
			return true, nil
		}
		// Preserve the first closing identity for later retries.
		if !res.Final.isZero() {
			var existing GroupFinal
			if state.Finals != nil {
				existing = state.Finals[groupID]
			}
			merged, mergeErr := mergeFinal(existing, res.Final)
			if mergeErr != nil {
				return !retry, errors.Join(ferr, mergeErr)
			}
			if state.Finals == nil {
				state.Finals = map[string]GroupFinal{}
			}
			state.Finals[groupID] = merged
			finalIdentity = &merged
		}
		if ferr == nil {
			ferr = fmt.Errorf("toolsnap: finalization incomplete for group %s", groupID)
		}
		return true, ferr
	})
	if err != nil {
		// Preserve durable work that registry publication did not record.
		var rec *completionReceipt
		switch {
		case finalDone:
			rec = &completionReceipt{Key: key, Info: info, GroupID: finalGroupID, Done: true}
		case finalIdentity != nil && !published:
			rec = &completionReceipt{Key: key, Info: info, GroupID: finalGroupID, GroupFinal: finalIdentity}
		case sealedHit && memberPersisted && !published:
			rec = &completionReceipt{Key: key, Info: info, GroupID: finalGroupID, Sealed: true}
		case memberPersisted && !published:
			rec = &completionReceipt{Key: key, Info: info}
		}
		if rec != nil {
			if rerr := r.writeReceipt(ctx, *rec); rerr != nil {
				return false, errors.Join(err, rerr)
			}
		}
		return false, err
	}
	if sealedHit {
		return false, ErrWindowSealed
	}
	return removed, nil
}

// completionReceipt preserves work not recorded in registry state.
type completionReceipt struct {
	Key        ToolKey        `json:"key"`
	Info       CompletionInfo `json:"info"`
	GroupID    string         `json:"group_id,omitempty"`
	Done       bool           `json:"done,omitempty"`
	GroupFinal *GroupFinal    `json:"group_final,omitempty"`
	// Sealed restores the group seal with this completion.
	Sealed bool `json:"sealed,omitempty"`
}

func (r *Registry) receiptsDir() string     { return filepath.Join(r.dir, "receipts") }
func (r *Registry) receiptLockPath() string { return filepath.Join(r.dir, "receipt.lock") }
func (r *Registry) closuresDir() string     { return filepath.Join(r.dir, "closures") }

// acquireReceiptLock serializes receipt writes, upgrades, and consumption.
// Registry passes acquire it after the registry lock; standalone writers
// never acquire the registry lock.
func (r *Registry) acquireReceiptLock(ctx context.Context) (*os.File, error) {
	f, err := os.OpenFile(r.receiptLockPath(), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("toolsnap: open receipt lock: %w", err)
	}
	for attempt := 0; ; attempt++ {
		// Allow one uncontended recovery write after deadline expiry.
		if attempt > 0 {
			if ctx.Err() != nil {
				_ = f.Close()
				return nil, &PartialError{Reason: ReasonLockTimeout, Detail: "receipt lock not acquired within the capture deadline"}
			}
		}
		ok, err := platform.TryLockFile(f)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("toolsnap: receipt lock: %w", err)
		}
		if ok {
			return f, nil
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, &PartialError{Reason: ReasonLockTimeout, Detail: "receipt lock not acquired within the capture deadline"}
		case <-time.After(lockPollInterval):
		}
	}
}

// validateShape accepts member, sealed, final, and done receipts.
func (rec completionReceipt) validateShape() error {
	switch {
	case rec.Sealed:
		if rec.GroupID != "" && !rec.Done && rec.GroupFinal == nil {
			return nil // sealed member completion
		}
	case rec.GroupID == "" && !rec.Done && rec.GroupFinal == nil:
		return nil // member completion
	case rec.GroupID != "" && rec.Done && rec.GroupFinal == nil:
		return nil // group done
	case rec.GroupID != "" && !rec.Done && rec.GroupFinal != nil:
		return nil // group final identity
	}
	return fmt.Errorf("toolsnap: receipt has invalid shape")
}

// closureMarker records that a member's group closed with durable
// evidence, keeping duplicate hooks under the closed key idempotent.
type closureMarker struct {
	Key     ToolKey `json:"key"`
	GroupID string  `json:"group_id"`
	At      int64   `json:"at"`
}

// writeClosureMarker persists a marker before its group is removed.
func (r *Registry) writeClosureMarker(key ToolKey, groupID string, at int64) error {
	payload, err := json.Marshal(closureMarker{Key: key, GroupID: groupID, At: at})
	if err != nil {
		return fmt.Errorf("toolsnap: encode closure marker: %w", err)
	}
	path := filepath.Join(r.closuresDir(), key.hash())
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return fmt.Errorf("toolsnap: write closure marker: %w", err)
	}
	if err := platform.ReplaceFile(tmp, path); err != nil {
		return fmt.Errorf("toolsnap: publish closure marker: %w", err)
	}
	return nil
}

// hasClosureMarker reports whether the keyed member's group already
// closed with durable evidence. Marker content is validated; a
// malformed marker is corruption, not a signal in either direction.
func (r *Registry) hasClosureMarker(key ToolKey) (bool, error) {
	raw, err := os.ReadFile(filepath.Join(r.closuresDir(), key.hash()))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("toolsnap: read closure marker: %w", err)
	}
	var m closureMarker
	if json.Unmarshal(raw, &m) != nil || m.Key != key || m.At <= 0 || m.GroupID == "" {
		return false, fmt.Errorf("%w: malformed closure marker for %s", ErrRegistryCorrupt, key.hash())
	}
	return true, nil
}

// recordIntentFor returns a finalize-scoped writer for the closing member.
// The intent makes an interrupted completion recoverable. A later outcome
// receipt may upgrade it.
func (r *Registry) recordIntentFor(key ToolKey, info CompletionInfo) func() error {
	return func() error {
		return r.writeReceiptLocked(completionReceipt{Key: key, Info: info})
	}
}

// writeReceipt persists an outcome outside a registry pass.
func (r *Registry) writeReceipt(ctx context.Context, rec completionReceipt) error {
	lock, err := r.acquireReceiptLock(ctx)
	if err != nil {
		return err
	}
	defer func() {
		_ = platform.UnlockFile(lock)
		_ = lock.Close()
	}()
	return r.writeReceiptLocked(rec)
}

// writeReceiptLocked persists a receipt while the caller holds the receipt lock.
func (r *Registry) writeReceiptLocked(rec completionReceipt) error {
	dir := r.receiptsDir()
	// OpenRegistry normally creates this directory.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("toolsnap: receipts dir: %w", err)
	}
	if err := rec.validateShape(); err != nil {
		return err
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("toolsnap: encode receipt: %w", err)
	}
	key := rec.Key
	path := filepath.Join(dir, key.hash())
	f, err := os.CreateTemp(dir, key.hash()+".tmp-*")
	if err != nil {
		return fmt.Errorf("toolsnap: receipt temp: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		return fmt.Errorf("toolsnap: write receipt: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("toolsnap: sync receipt: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("toolsnap: close receipt: %w", err)
	}
	if err := os.Link(tmp, path); err != nil {
		if !os.IsExist(err) {
			return fmt.Errorf("toolsnap: publish receipt: %w", err)
		}
		// Receipts may advance from intent to final identity to done.
		existing, rerr := os.ReadFile(path)
		if rerr != nil {
			return fmt.Errorf("toolsnap: verify existing receipt: %w", rerr)
		}
		if bytes.Equal(existing, payload) {
			return nil
		}
		var prev completionReceipt
		if json.Unmarshal(existing, &prev) != nil || prev.validateShape() != nil {
			return fmt.Errorf("%w: conflicting receipt for %s", ErrRegistryCorrupt, key.hash())
		}
		if prev.Key != rec.Key || prev.Info != rec.Info {
			return fmt.Errorf("%w: conflicting receipt for %s", ErrRegistryCorrupt, key.hash())
		}
		switch {
		case receiptRank(rec) > receiptRank(prev):
			if err := platform.ReplaceFile(tmp, path); err != nil {
				return fmt.Errorf("toolsnap: upgrade receipt: %w", err)
			}
		case receiptRank(rec) < receiptRank(prev):
			// Do not replace a newer outcome with an older intent.
		default:
			// Equal-rank divergence is conflicting evidence.
			return fmt.Errorf("%w: conflicting receipt for %s", ErrRegistryCorrupt, key.hash())
		}
	}
	return nil
}

// receiptRank orders recoverable outcomes after member-only receipts.
func receiptRank(rec completionReceipt) int {
	switch {
	case rec.Done:
		return 2
	case rec.GroupFinal != nil:
		return 1
	default:
		return 0
	}
}

// applyReceipts overlays recovery receipts on registry state. A non-nil
// publishMarker records closures before completed groups are removed.
func (r *Registry) applyReceipts(state *registryState, publishMarker func(key ToolKey, groupID string, at int64) error) ([]string, error) {
	entries, err := os.ReadDir(r.receiptsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: receipts unreadable: %v", ErrRegistryCorrupt, err)
	}
	var applied []string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(r.receiptsDir(), e.Name()))
		if err != nil {
			return nil, fmt.Errorf("%w: receipt %s unreadable: %v", ErrRegistryCorrupt, e.Name(), err)
		}
		var rec completionReceipt
		if json.Unmarshal(raw, &rec) != nil || rec.Key.validate() != nil ||
			rec.Key.hash() != e.Name() || rec.Info.EventID == "" || rec.Info.At <= 0 ||
			rec.validateShape() != nil {
			return nil, fmt.Errorf("%w: receipt %s malformed", ErrRegistryCorrupt, e.Name())
		}
		found := false
		for i, w := range state.Windows {
			if w.Key != rec.Key {
				continue
			}
			found = true
			// Group outcomes must match the member's group.
			if rec.GroupID != "" && rec.GroupID != w.GroupID {
				return nil, fmt.Errorf("%w: receipt %s names group %s but member belongs to %s",
					ErrRegistryCorrupt, e.Name(), rec.GroupID, w.GroupID)
			}
			switch w.Status {
			case "active":
				state.Windows[i].Status = "complete"
				state.Windows[i].CompletedAt = rec.Info.At
				state.Windows[i].EventID = rec.Info.EventID
				state.Windows[i].CommandSummary = rec.Info.CommandSummary
			case "complete":
				// Completion identities are immutable.
				if w.EventID != rec.Info.EventID || w.CompletedAt != rec.Info.At {
					return nil, fmt.Errorf("%w: receipt %s conflicts with completed member", ErrRegistryCorrupt, e.Name())
				}
			}
			break
		}
		if !found {
			if rec.GroupID == "" {
				// A closure marker proves the missing member already settled.
				closedBefore, merr := r.hasClosureMarker(rec.Key)
				if merr != nil {
					return nil, merr
				}
				if closedBefore {
					applied = append(applied, e.Name())
					continue
				}
				return nil, fmt.Errorf("%w: receipt %s for unknown member", ErrRegistryCorrupt, e.Name())
			}
			// A missing member cannot affect a group that still exists.
			groupExists := false
			for _, w := range state.Windows {
				if w.GroupID == rec.GroupID {
					groupExists = true
					break
				}
			}
			if groupExists {
				return nil, fmt.Errorf("%w: receipt %s names existing group %s without its member",
					ErrRegistryCorrupt, e.Name(), rec.GroupID)
			}
			applied = append(applied, e.Name())
			continue
		}
		switch {
		case rec.Sealed:
			// Restore the seal lost with the completion update.
			meta := state.Groups[rec.GroupID]
			if !meta.Sealed {
				meta.Sealed = true
				if state.Groups == nil {
					state.Groups = map[string]GroupMeta{}
				}
				state.Groups[rec.GroupID] = meta
			}
		case rec.Done:
			// Mark members closed before removing durable group state.
			if publishMarker != nil {
				for _, w := range state.Windows {
					if w.GroupID == rec.GroupID {
						if err := publishMarker(w.Key, rec.GroupID, rec.Info.At); err != nil {
							return nil, err
						}
					}
				}
			}
			dropGroup(state, rec.GroupID)
		case rec.GroupFinal != nil:
			var existing GroupFinal
			if state.Finals != nil {
				existing = state.Finals[rec.GroupID]
			}
			merged, err := mergeFinal(existing, *rec.GroupFinal)
			if err != nil {
				return nil, fmt.Errorf("%w: receipt %s final conflicts: %v", ErrRegistryCorrupt, e.Name(), err)
			}
			if state.Finals == nil {
				state.Finals = map[string]GroupFinal{}
			}
			state.Finals[rec.GroupID] = merged
		}
		applied = append(applied, e.Name())
	}
	return applied, nil
}

// Stale returns windows whose pre snapshot is older than cutoff, for
// doctor reporting and tidy cleanup.
func (r *Registry) Stale(ctx context.Context, cutoff int64) ([]PendingToolSnapshot, error) {
	var stale []PendingToolSnapshot
	_, err := r.withLock(ctx, func(state *registryState) (bool, error) {
		for _, w := range state.Windows {
			if w.StartedAt < cutoff {
				stale = append(stale, w)
			}
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return stale, nil
}

// PendingFinalization is a group whose members are all complete but
// whose finalization has not durably finished.
type PendingFinalization struct {
	GroupID string
	Members []PendingToolSnapshot
	Final   *GroupFinal
}

// PendingFinalizations lists complete groups awaiting finalization.
func (r *Registry) PendingFinalizations(ctx context.Context) ([]PendingFinalization, error) {
	var pending []PendingFinalization
	_, err := r.withLock(ctx, func(state *registryState) (bool, error) {
		byGroup := map[string][]PendingToolSnapshot{}
		activeGroups := map[string]bool{}
		for _, w := range state.Windows {
			byGroup[w.GroupID] = append(byGroup[w.GroupID], w)
			if w.Status == "active" {
				activeGroups[w.GroupID] = true
			}
		}
		for gid, members := range byGroup {
			// Sealed groups are reclaimed as partial evidence.
			if activeGroups[gid] || state.Groups[gid].Sealed {
				continue
			}
			sort.Slice(members, func(i, j int) bool {
				if members[i].Seq != members[j].Seq {
					return members[i].Seq < members[j].Seq
				}
				return members[i].Key.less(members[j].Key)
			})
			p := PendingFinalization{GroupID: gid, Members: members}
			if f, ok := state.Finals[gid]; ok {
				fc := f
				p.Final = &fc
			}
			pending = append(pending, p)
		}
		sort.Slice(pending, func(i, j int) bool { return pending[i].GroupID < pending[j].GroupID })
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return pending, nil
}

// ResumeFinalization retries a complete group from durable state.
func (r *Registry) ResumeFinalization(ctx context.Context, groupID string, finalize func(members []PendingToolSnapshot, prior *GroupFinal, retry bool, recordIntent func() error) (FinalizeResult, error)) (bool, error) {
	removed := false
	var finalDone bool
	var finalIdentity *GroupFinal
	var closingKey ToolKey
	var closingInfo CompletionInfo
	published, err := r.withLock(ctx, func(state *registryState) (bool, error) {
		if state.Groups[groupID].Sealed {
			return false, fmt.Errorf("toolsnap: group %s is sealed; only reclamation may finalize it", groupID)
		}
		var members []PendingToolSnapshot
		for _, w := range state.Windows {
			if w.GroupID != groupID {
				continue
			}
			if w.Status == "active" {
				return false, fmt.Errorf("toolsnap: group %s still has active members", groupID)
			}
			members = append(members, w)
		}
		if len(members) == 0 {
			return false, ErrNoPendingSnapshot
		}
		sort.Slice(members, func(i, j int) bool {
			if members[i].Seq != members[j].Seq {
				return members[i].Seq < members[j].Seq
			}
			return members[i].Key.less(members[j].Key)
		})
		last := members[len(members)-1]
		closingKey = last.Key
		closingInfo = CompletionInfo{At: last.CompletedAt, EventID: last.EventID, CommandSummary: last.CommandSummary}

		var prior *GroupFinal
		if f, ok := state.Finals[groupID]; ok {
			prior = &f
		}
		// Recorded completions always resume as retries.
		res, ferr := finalize(members, prior, true, r.recordIntentFor(closingKey, closingInfo))
		if ferr == nil && res.Done {
			finalDone = true
			// Markers precede removal, as in Complete.
			for _, m := range members {
				if err := r.writeClosureMarker(m.Key, groupID, last.CompletedAt); err != nil {
					return true, err
				}
			}
			dropGroup(state, groupID)
			removed = true
			return true, nil
		}
		if !res.Final.isZero() {
			var existing GroupFinal
			if state.Finals != nil {
				existing = state.Finals[groupID]
			}
			merged, mergeErr := mergeFinal(existing, res.Final)
			if mergeErr != nil {
				return true, errors.Join(ferr, mergeErr)
			}
			if state.Finals == nil {
				state.Finals = map[string]GroupFinal{}
			}
			state.Finals[groupID] = merged
			finalIdentity = &merged
		}
		if ferr == nil {
			ferr = fmt.Errorf("toolsnap: finalization incomplete for group %s", groupID)
		}
		return true, ferr
	})
	if err != nil {
		var rec *completionReceipt
		switch {
		case finalDone:
			rec = &completionReceipt{Key: closingKey, Info: closingInfo, GroupID: groupID, Done: true}
		case finalIdentity != nil && !published:
			rec = &completionReceipt{Key: closingKey, Info: closingInfo, GroupID: groupID, GroupFinal: finalIdentity}
		}
		if rec != nil {
			if rerr := r.writeReceipt(ctx, *rec); rerr != nil {
				return false, errors.Join(err, rerr)
			}
		}
		return false, err
	}
	return removed, nil
}

// RemoveGroup deletes stale group members and their pending final identity.
func (r *Registry) RemoveGroup(ctx context.Context, groupID string) error {
	_, err := r.withLock(ctx, func(state *registryState) (bool, error) {
		dropGroup(state, groupID)
		return true, nil
	})
	return err
}

// WriteTombstone atomically marks a window ineligible without the registry lock.
// The first complete record wins; interrupted writes remain temporary files.
func (r *Registry) WriteTombstone(key ToolKey, at int64) error {
	t := Tombstone{Key: key, At: at}
	if err := t.validate(); err != nil {
		return err
	}
	dir := filepath.Join(r.dir, "tombstones")
	path := filepath.Join(dir, key.hash())
	payload, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("toolsnap: encode tombstone: %w", err)
	}
	f, err := os.CreateTemp(dir, key.hash()+".tmp-*")
	if err != nil {
		return fmt.Errorf("toolsnap: tombstone temp: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		return fmt.Errorf("toolsnap: write tombstone: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("toolsnap: sync tombstone: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("toolsnap: close tombstone: %w", err)
	}
	if err := os.Link(tmp, path); err != nil {
		if os.IsExist(err) {
			return nil
		}
		return fmt.Errorf("toolsnap: publish tombstone: %w", err)
	}
	return nil
}

// Tombstone records when a tool window became ineligible.
type Tombstone struct {
	Key ToolKey `json:"key"`
	At  int64   `json:"at"`
}

// validate applies the tombstone contract on write and read.
func (t Tombstone) validate() error {
	if err := t.Key.validate(); err != nil {
		return err
	}
	if t.At <= 0 {
		return fmt.Errorf("toolsnap: tombstone with invalid timestamp %d", t.At)
	}
	return nil
}

// HasTombstone reports whether a window is ineligible. Probe errors fail closed.
func (r *Registry) HasTombstone(key ToolKey) (bool, error) {
	if err := key.validate(); err != nil {
		return false, err
	}
	_, err := os.Stat(filepath.Join(r.dir, "tombstones", key.hash()))
	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, fmt.Errorf("toolsnap: tombstone probe: %w", err)
	}
}

// ListTombstones returns valid records and the names of malformed entries.
// Each record must match its filename identity.
func (r *Registry) ListTombstones() (tombs []Tombstone, malformed []string, err error) {
	dir := filepath.Join(r.dir, "tombstones")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("toolsnap: list tombstones: %w", err)
	}
	for _, e := range entries {
		// Ignore temporary files left by interrupted publication.
		if strings.Contains(e.Name(), ".tmp-") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			malformed = append(malformed, e.Name())
			continue
		}
		var t Tombstone
		if json.Unmarshal(raw, &t) != nil || t.validate() != nil || t.Key.hash() != e.Name() {
			malformed = append(malformed, e.Name())
			continue
		}
		tombs = append(tombs, t)
	}
	return tombs, malformed, nil
}

// RemoveTombstone deletes a tombstone after its partial evidence is
// durable.
func (r *Registry) RemoveTombstone(key ToolKey) error {
	if err := key.validate(); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(r.dir, "tombstones", key.hash()))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("toolsnap: remove tombstone: %w", err)
	}
	return nil
}
