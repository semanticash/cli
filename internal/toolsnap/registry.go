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

// ErrRegistryCorrupt reports invalid persisted window or final state.
var ErrRegistryCorrupt = errors.New("toolsnap: registry state corrupt")

// Registry coordinates repository-scoped tool windows across hook processes.
type Registry struct {
	dir string
}

// OpenRegistry prepares the registry directories under semDir.
func OpenRegistry(semDir string) (*Registry, error) {
	dir := filepath.Join(semDir, "tool-windows")
	// Recovery directories must exist before registry publication fails.
	for _, d := range []string{dir, filepath.Join(dir, "tombstones"), filepath.Join(dir, "receipts"), filepath.Join(dir, "closures")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("toolsnap: create registry dir: %w", err)
		}
	}
	r := &Registry{dir: dir}
	// Pre-create the lock so recovery does not depend on directory writes.
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
	// NextSeq assigns the capture order under the lock.
	NextSeq int64 `json:"next_seq,omitempty"`
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
	if len(activeGroups) > 1 {
		return fmt.Errorf("%w: %d active groups", ErrRegistryCorrupt, len(activeGroups))
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
	f, err := os.OpenFile(r.lockPath(), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return false, fmt.Errorf("toolsnap: open registry lock: %w", err)
	}
	defer func() { _ = f.Close() }()

	for {
		ok, err := platform.TryLockFile(f)
		if err != nil {
			return false, fmt.Errorf("toolsnap: registry lock: %w", err)
		}
		if ok {
			break
		}
		select {
		case <-ctx.Done():
			return false, &PartialError{Reason: ReasonLockTimeout, Detail: "registry lock not acquired within the capture deadline"}
		case <-time.After(lockPollInterval):
		}
	}
	defer func() { _ = platform.UnlockFile(f) }()
	// Recheck after acquisition because the wait may have consumed the deadline.
	if err := ctx.Err(); err != nil {
		return false, &PartialError{Reason: ReasonLockTimeout, Detail: "capture deadline expired during lock acquisition"}
	}
	// Hold the receipt lock through publication and receipt deletion.
	receiptLock, err := r.acquireReceiptLock(ctx)
	if err != nil {
		return false, err
	}
	defer func() {
		_ = platform.UnlockFile(receiptLock)
		_ = receiptLock.Close()
	}()
	// Registry passes never use the standalone write deadline exception.
	if err := ctx.Err(); err != nil {
		return false, &PartialError{Reason: ReasonLockTimeout, Detail: "capture deadline expired during receipt lock acquisition"}
	}

	var state registryState
	raw, err := os.ReadFile(r.statePath())
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &state); err != nil {
			return false, fmt.Errorf("toolsnap: corrupt registry state: %w", err)
		}
	case os.IsNotExist(err):
	default:
		return false, fmt.Errorf("toolsnap: read registry state: %w", err)
	}
	// States without sequences use array order. Mixed sequence states fail validation.
	migrated := false
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
	// Apply recovery receipts before exposing state to the operation.
	applied, err := r.applyReceipts(&state)
	if err != nil {
		return false, err
	}
	if err := state.validate(); err != nil {
		return false, err
	}

	persist, fnErr := fn(&state)
	// Publish migrations and recovered receipts even on reads.
	persist = persist || len(applied) > 0 || migrated
	if !persist {
		return false, fnErr
	}
	// Never publish state that later operations would reject.
	if err := state.validate(); err != nil {
		return false, errors.Join(fnErr, err)
	}

	out, err := json.Marshal(state)
	if err != nil {
		return false, errors.Join(fnErr, fmt.Errorf("toolsnap: encode registry state: %w", err))
	}
	tmp, err := os.CreateTemp(r.dir, "registry-*.tmp")
	if err != nil {
		return false, errors.Join(fnErr, fmt.Errorf("toolsnap: registry temp: %w", err))
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return false, errors.Join(fnErr, fmt.Errorf("toolsnap: write registry state: %w", err))
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false, errors.Join(fnErr, fmt.Errorf("toolsnap: sync registry state: %w", err))
	}
	if err := tmp.Close(); err != nil {
		return false, errors.Join(fnErr, fmt.Errorf("toolsnap: close registry state: %w", err))
	}
	// os.Rename cannot replace an existing file on Windows.
	if err := platform.ReplaceFile(tmpName, r.statePath()); err != nil {
		return false, errors.Join(fnErr, fmt.Errorf("toolsnap: publish registry state: %w", err))
	}
	for _, name := range applied {
		_ = os.Remove(filepath.Join(r.receiptsDir(), name))
	}
	return true, fnErr
}

// CaptureAndBegin captures and registers a pre-tool snapshot while
// holding the registry lock. Duplicate deliveries reuse the existing entry.
func (r *Registry) CaptureAndBegin(ctx context.Context, s *Store, key ToolKey, toolName string, startedAt int64) (PendingToolSnapshot, error) {
	if err := key.validate(); err != nil {
		return PendingToolSnapshot{}, err
	}
	// Avoid store work for an already-closed key.
	if closedBefore, err := r.hasClosureMarker(key); err != nil {
		return PendingToolSnapshot{}, err
	} else if closedBefore {
		return PendingToolSnapshot{}, nil
	}
	// The registry lock protects only the store in the same .semantica directory.
	if filepath.Dir(r.dir) != filepath.Dir(s.Dir) {
		return PendingToolSnapshot{}, fmt.Errorf("toolsnap: registry %s and store %s belong to different directories",
			filepath.Dir(r.dir), filepath.Dir(s.Dir))
	}
	var result PendingToolSnapshot
	_, err := r.withLock(ctx, func(state *registryState) (bool, error) {
		// Closure may occur after the unlocked fast check.
		closedBefore, merr := r.hasClosureMarker(key)
		if merr != nil {
			return false, merr
		}
		if closedBefore {
			return false, nil
		}
		for _, w := range state.Windows {
			if w.Key == key {
				result = w
				return false, nil
			}
		}
		groupID := "g-" + key.hash()
		for _, w := range state.Windows {
			if w.Status == "active" && w.Key.RepositoryID == key.RepositoryID {
				groupID = w.GroupID
				break
			}
		}
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
		// state.validate() guarantees at most one active group.
		for _, w := range state.Windows {
			if w.Status == "active" && w.Key.RepositoryID == entry.Key.RepositoryID {
				groupID = w.GroupID
				break
			}
		}
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
// persistMember runs before a non-final completion becomes visible.
// finalize must not recapture when retry is true. recordIntent is valid
// only during finalize. Done removes the group; Final preserves retry state.
func (r *Registry) Complete(ctx context.Context, key ToolKey, info CompletionInfo, persistMember func(PendingToolSnapshot) error, finalize func(members []PendingToolSnapshot, prior *GroupFinal, retry bool, recordIntent func() error) (FinalizeResult, error)) (bool, error) {
	if info.EventID == "" || info.At <= 0 {
		return false, fmt.Errorf("toolsnap: completion without event identity or timestamp")
	}
	removed := false
	memberPersisted := false
	var finalDone bool
	var finalIdentity *GroupFinal
	finalGroupID := ""
	published, err := r.withLock(ctx, func(state *registryState) (bool, error) {
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
				return false, merr
			}
			if closedBefore {
				removed = true
				return false, nil
			}
			return false, ErrNoPendingSnapshot
		}
		if !retry {
			state.Windows[idx].Status = "complete"
			state.Windows[idx].CompletedAt = info.At
			state.Windows[idx].EventID = info.EventID
			state.Windows[idx].CommandSummary = info.CommandSummary
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
	return removed, nil
}

// completionReceipt preserves work not recorded in registry state.
type completionReceipt struct {
	Key        ToolKey        `json:"key"`
	Info       CompletionInfo `json:"info"`
	GroupID    string         `json:"group_id,omitempty"`
	Done       bool           `json:"done,omitempty"`
	GroupFinal *GroupFinal    `json:"group_final,omitempty"`
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

// validateShape enforces the three receipt forms: member completion
// (no group outcome), group done, or group final identity.
func (rec completionReceipt) validateShape() error {
	switch {
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

// receiptRank orders receipt shapes along the finalization state
// machine: intent, then persisted identity, then done.
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

// applyReceipts merges recovery receipts into state. The caller deletes
// them only after publishing that state.
func (r *Registry) applyReceipts(state *registryState) ([]string, error) {
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
		case rec.Done:
			// Mark members closed before removing durable group state.
			for _, w := range state.Windows {
				if w.GroupID == rec.GroupID {
					if err := r.writeClosureMarker(w.Key, rec.GroupID, rec.Info.At); err != nil {
						return nil, err
					}
				}
			}
			kept := state.Windows[:0]
			for _, w := range state.Windows {
				if w.GroupID != rec.GroupID {
					kept = append(kept, w)
				}
			}
			state.Windows = kept
			delete(state.Finals, rec.GroupID)
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
			if activeGroups[gid] {
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

// ResumeFinalization retries a complete group without another post hook.
// The callback receives retry=true and must never recapture the workspace.
// Without durable post state, it must produce terminal partial evidence.
func (r *Registry) ResumeFinalization(ctx context.Context, groupID string, finalize func(members []PendingToolSnapshot, prior *GroupFinal, retry bool, recordIntent func() error) (FinalizeResult, error)) (bool, error) {
	removed := false
	var finalDone bool
	var finalIdentity *GroupFinal
	var closingKey ToolKey
	var closingInfo CompletionInfo
	published, err := r.withLock(ctx, func(state *registryState) (bool, error) {
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
