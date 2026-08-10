package toolsnap

import (
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
	GroupID      string  `json:"group_id"`
	Status       string  `json:"status"` // "active" or "complete"
}

// ErrNoPendingSnapshot reports a post hook without a matching
// registered window; callers degrade to pre_snapshot_missing.
var ErrNoPendingSnapshot = errors.New("toolsnap: no pending snapshot for key")

// ErrRegistryCorrupt reports invalid persisted window or final state.
var ErrRegistryCorrupt = errors.New("toolsnap: registry state corrupt")

// Registry coordinates repository-scoped tool windows across hook processes.
type Registry struct {
	dir string
}

// OpenRegistry prepares the registry directories under semDir.
func OpenRegistry(semDir string) (*Registry, error) {
	dir := filepath.Join(semDir, "tool-windows")
	for _, d := range []string{dir, filepath.Join(dir, "tombstones")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("toolsnap: create registry dir: %w", err)
		}
	}
	return &Registry{dir: dir}, nil
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
	// Final, when not Done, is persisted so a retry resumes from the
	// captured identity instead of rereading the workspace. A zero
	// Final with an error means not even a capture identity exists;
	// the caller must convert the group to deterministic partial
	// evidence via PartialReason before line attribution is possible.
	Final GroupFinal
}

// registryState holds pending windows and closing states awaiting finalization.
type registryState struct {
	Windows []PendingToolSnapshot `json:"windows"`
	Finals  map[string]GroupFinal `json:"finals,omitempty"`
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
	if len(activeGroups) > 1 {
		return fmt.Errorf("%w: %d active groups", ErrRegistryCorrupt, len(activeGroups))
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

// withLock loads, validates, mutates, and atomically publishes registry state.
// A callback may request persistence alongside an error so retry state survives.
func (r *Registry) withLock(ctx context.Context, fn func(*registryState) (persist bool, err error)) error {
	if err := ctx.Err(); err != nil {
		return &PartialError{Reason: ReasonLockTimeout, Detail: "capture deadline expired before lock acquisition"}
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
			return &PartialError{Reason: ReasonLockTimeout, Detail: "registry lock not acquired within the capture deadline"}
		case <-time.After(lockPollInterval):
		}
	}
	defer func() { _ = platform.UnlockFile(f) }()
	// Recheck after acquisition because the wait may have consumed the deadline.
	if err := ctx.Err(); err != nil {
		return &PartialError{Reason: ReasonLockTimeout, Detail: "capture deadline expired during lock acquisition"}
	}

	var state registryState
	raw, err := os.ReadFile(r.statePath())
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &state); err != nil {
			return fmt.Errorf("toolsnap: corrupt registry state: %w", err)
		}
	case os.IsNotExist(err):
	default:
		return fmt.Errorf("toolsnap: read registry state: %w", err)
	}
	if err := state.validate(); err != nil {
		return err
	}

	persist, fnErr := fn(&state)
	if !persist {
		return fnErr
	}
	// Never publish state that later operations would reject.
	if err := state.validate(); err != nil {
		return errors.Join(fnErr, err)
	}

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
	return fnErr
}

// Begin registers a tool window. Overlapping windows share the active group.
// Re-registering a retained key returns its existing group.
func (r *Registry) Begin(ctx context.Context, entry PendingToolSnapshot) (string, error) {
	if entry.Status != "" && entry.Status != "active" {
		return "", fmt.Errorf("toolsnap: begin with status %q", entry.Status)
	}
	if err := entry.Key.validate(); err != nil {
		return "", err
	}
	entry.Status = "active"
	groupID := "g-" + entry.Key.hash()
	err := r.withLock(ctx, func(state *registryState) (bool, error) {
		// Retained keys make duplicate delivery idempotent.
		for _, w := range state.Windows {
			if w.Key == entry.Key {
				groupID = w.GroupID
				return false, nil
			}
		}
		// state.validate() guarantees at most one active group.
		for _, w := range state.Windows {
			if w.Status == "active" && w.Key.RepositoryID == entry.Key.RepositoryID {
				groupID = w.GroupID
				break
			}
		}
		entry.GroupID = groupID
		state.Windows = append(state.Windows, entry)
		return true, nil
	})
	if err != nil {
		return "", err
	}
	return groupID, nil
}

// Complete marks a window complete and finalizes its group after the last member.
// Finalization runs under the registry lock with members in deterministic order.
//
// On retry, finalize must resume from prior and remain idempotent against durable
// evidence. Done removes the group; otherwise Final is saved for the next retry.
// closed is true only after the removal is published.
func (r *Registry) Complete(ctx context.Context, key ToolKey, completedAt int64, finalize func(members []PendingToolSnapshot, prior *GroupFinal) (FinalizeResult, error)) (bool, error) {
	removed := false
	err := r.withLock(ctx, func(state *registryState) (bool, error) {
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
			return false, ErrNoPendingSnapshot
		}
		if !retry {
			state.Windows[idx].Status = "complete"
			state.Windows[idx].CompletedAt = completedAt
		}

		groupID := state.Windows[idx].GroupID
		for _, w := range state.Windows {
			if w.GroupID == groupID && w.Status == "active" {
				// Other active members keep the group open.
				return !retry, nil
			}
		}

		var members []PendingToolSnapshot
		for _, w := range state.Windows {
			if w.GroupID == groupID {
				members = append(members, w)
			}
		}
		sort.Slice(members, func(i, j int) bool {
			if members[i].StartedAt != members[j].StartedAt {
				return members[i].StartedAt < members[j].StartedAt
			}
			return members[i].Key.less(members[j].Key)
		})

		var prior *GroupFinal
		if f, ok := state.Finals[groupID]; ok {
			prior = &f
		}
		res, ferr := finalize(members, prior)
		if ferr == nil && res.Done {
			kept := state.Windows[:0]
			for _, w := range state.Windows {
				if w.GroupID != groupID {
					kept = append(kept, w)
				}
			}
			state.Windows = kept
			delete(state.Finals, groupID)
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
		}
		if ferr == nil {
			ferr = fmt.Errorf("toolsnap: finalization incomplete for group %s", groupID)
		}
		return true, ferr
	})
	if err != nil {
		// Failed publication leaves the persisted group open for retry.
		return false, err
	}
	return removed, nil
}

// Stale returns windows whose pre snapshot is older than cutoff, for
// doctor reporting and tidy cleanup.
func (r *Registry) Stale(ctx context.Context, cutoff int64) ([]PendingToolSnapshot, error) {
	var stale []PendingToolSnapshot
	err := r.withLock(ctx, func(state *registryState) (bool, error) {
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

// RemoveGroup deletes stale group members and their pending final identity.
func (r *Registry) RemoveGroup(ctx context.Context, groupID string) error {
	return r.withLock(ctx, func(state *registryState) (bool, error) {
		kept := state.Windows[:0]
		for _, w := range state.Windows {
			if w.GroupID != groupID {
				kept = append(kept, w)
			}
		}
		state.Windows = kept
		delete(state.Finals, groupID)
		return true, nil
	})
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
