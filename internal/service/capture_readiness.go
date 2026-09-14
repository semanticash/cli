package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/toolsnap"
)

var captureGrace = 30 * time.Second
var captureRetryDelay = 5 * time.Second

// CaptureGap identifies evidence unavailable to a checkpoint.
type CaptureGap struct {
	Key     *toolsnap.ToolKey `json:"key,omitempty"`
	GroupID string            `json:"group_id,omitempty"`
	Reason  string            `json:"reason"`
}

// CaptureReadiness covers registered tool windows, not all possible agent activity.
type CaptureReadiness struct {
	Status string       `json:"status"` // pending, complete, incomplete
	Gaps   []CaptureGap `json:"gaps,omitempty"`
}

type captureMember struct {
	Key     toolsnap.ToolKey `json:"key"`
	GroupID string           `json:"group_id"`
}

type captureProof struct {
	GroupID string
	Reason  string
}

type checkpointCapture struct {
	Version      int              `json:"version"`
	CheckpointID string           `json:"checkpoint_id"`
	RepositoryID string           `json:"repository_id"`
	After        int64            `json:"after"`
	Through      int64            `json:"through"`
	Deadline     int64            `json:"deadline"`
	Members      []captureMember  `json:"members"`
	FixedGaps    []CaptureGap     `json:"fixed_gaps,omitempty"`
	Result       CaptureReadiness `json:"result"`
}

type captureNotSettled struct{ At time.Time }

func (e *captureNotSettled) Error() string { return "capture_not_settled" }

func readCheckpointCapture(ctx context.Context, h *sqlstore.Handle, cp sqldb.Checkpoint) (*checkpointCapture, error) {
	raw, err := h.Queries.GetCheckpointCapture(ctx, cp.CheckpointID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r checkpointCapture
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil, fmt.Errorf("read capture readiness: %w", err)
	}
	if r.Version != 1 || r.CheckpointID != cp.CheckpointID || r.RepositoryID != cp.RepositoryID || r.Through != cp.CreatedAt || r.Deadline <= 0 {
		return nil, fmt.Errorf("capture readiness identity mismatch")
	}
	switch r.Result.Status {
	case "pending", "complete", "incomplete":
	default:
		return nil, fmt.Errorf("invalid capture readiness status %q", r.Result.Status)
	}
	if r.After > r.Through || (r.Result.Status == "complete" && (len(r.Result.Gaps) > 0 || len(r.FixedGaps) > 0)) || (r.Result.Status == "incomplete" && len(r.Result.Gaps) == 0) {
		return nil, fmt.Errorf("inconsistent capture readiness record")
	}
	seen := map[toolsnap.ToolKey]bool{}
	for _, m := range r.Members {
		if m.Key.RepositoryID != cp.RepositoryID || m.Key.Provider == "" || m.Key.SessionID == "" || m.Key.TurnID == "" || m.Key.ToolUseID == "" || m.GroupID == "" || seen[m.Key] {
			return nil, fmt.Errorf("invalid or duplicate capture member")
		}
		seen[m.Key] = true
	}
	return &r, nil
}

func saveCheckpointCapture(ctx context.Context, h *sqlstore.Handle, r *checkpointCapture) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return h.Queries.SaveCheckpointCapture(ctx, sqldb.SaveCheckpointCaptureParams{CheckpointID: r.CheckpointID, RecordJson: string(raw)})
}

func freezeCheckpointCapture(cp sqldb.Checkpoint, win eventWindow, snap toolsnap.RegistrySnapshot, inspectErr error, now time.Time) *checkpointCapture {
	r := &checkpointCapture{
		Version: 1, CheckpointID: cp.CheckpointID, RepositoryID: cp.RepositoryID,
		After: win.afterTs, Through: cp.CreatedAt, Deadline: now.Add(captureGrace).UnixMilli(),
		Result: CaptureReadiness{Status: "pending"},
	}
	if inspectErr != nil {
		// An unreadable initial inventory cannot establish a frozen membership.
		r.FixedGaps = append(r.FixedGaps, CaptureGap{Reason: "registry_unavailable"})
		return r
	}
	for _, w := range snap.Windows {
		if w.Key.RepositoryID != cp.RepositoryID || w.StartedAt > r.Through {
			continue
		}
		if w.Status == "complete" && w.CompletedAt <= r.After {
			continue
		}
		r.Members = append(r.Members, captureMember{Key: w.Key, GroupID: w.GroupID})
	}
	// Tombstones lack a start timestamp. Records created after the lower
	// boundary conservatively remain gaps, including recovery after the commit.
	for _, t := range snap.Tombstones {
		if t.Key.RepositoryID == cp.RepositoryID && t.At > r.After {
			key := t.Key
			r.FixedGaps = append(r.FixedGaps, CaptureGap{Key: &key, Reason: "completion_missing"})
		}
	}
	for _, p := range snap.Partials {
		if p.Key.RepositoryID == cp.RepositoryID && p.Timestamp > r.After && p.Timestamp <= r.Through {
			key := p.Key
			r.FixedGaps = append(r.FixedGaps, CaptureGap{Key: &key, Reason: p.Reason})
		}
	}
	if len(snap.MalformedTombstones) != 0 {
		r.FixedGaps = append(r.FixedGaps, CaptureGap{Reason: "invalid_capture_tombstone"})
	}
	return r
}

// captureEvidence uses the same event bounds as attribution. A closed registry
// entry alone does not prove that usable evidence reached the repository.
func captureEvidence(ctx context.Context, h *sqlstore.Handle, bs *blobs.Store, cp sqldb.Checkpoint, win eventWindow) (map[toolsnap.ToolKey]captureProof, []CaptureGap, error) {
	links, err := h.Queries.ListEvidenceLinksInWindow(ctx, sqldb.ListEvidenceLinksInWindowParams{
		RepositoryID: cp.RepositoryID, UseCursor: win.cursorFlag(), AfterCursor: win.cursorAfter(),
		UpToCursor: win.cursorUpTo(), AfterTs: win.afterTs, UpToTs: win.upToTs,
	})
	if err != nil {
		return nil, nil, err
	}
	resolved := map[toolsnap.ToolKey]captureProof{}
	var gaps []CaptureGap
	seen := map[string]bool{}
	linked := map[string]map[string]string{}
	for _, link := range links {
		if linked[link.GroupID] == nil {
			linked[link.GroupID] = map[string]string{}
		}
		linked[link.GroupID][link.EventID] = link.Provider
	}
	for _, link := range links {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		identity := link.GroupID + ":" + link.EvidenceHash
		if seen[identity] {
			continue
		}
		seen[identity] = true
		decodedHash, hashErr := hex.DecodeString(link.EvidenceHash)
		if hashErr != nil || len(decodedHash) != sha256.Size {
			gaps = append(gaps, CaptureGap{GroupID: link.GroupID, Reason: "invalid_evidence_hash"})
			continue
		}
		raw, err := bs.Get(ctx, link.EvidenceHash)
		var d *toolsnap.Delta
		if err == nil {
			sum := sha256.Sum256(raw)
			if hex.EncodeToString(sum[:]) != link.EvidenceHash {
				err = fmt.Errorf("evidence hash mismatch")
			} else {
				d, err = toolsnap.ParseDelta(raw)
			}
		}
		if err != nil {
			gaps = append(gaps, CaptureGap{GroupID: link.GroupID, Reason: "evidence_unavailable"})
			continue
		}
		reason := ""
		switch {
		case d.Status == "partial":
			reason = d.Reason
		case d.Window.CompletedAt > cp.CreatedAt:
			reason = "post_state_after_checkpoint"
		case d.Limits.Truncated:
			reason = "evidence_truncated"
		}
		for _, f := range d.Files {
			if f.Truncated {
				reason = "evidence_truncated"
			}
		}
		for _, use := range d.ToolUses {
			a := d.Actors[use.Actor]
			key := toolsnap.ToolKey{RepositoryID: cp.RepositoryID, Provider: a.Provider, SessionID: a.SessionID, TurnID: a.TurnID, ToolUseID: use.ToolUseID}
			if linked[link.GroupID][use.EventID] != a.Provider {
				gaps = append(gaps, CaptureGap{Key: &key, GroupID: link.GroupID, Reason: "evidence_member_not_linked"})
				continue
			}
			if old, exists := resolved[key]; exists && old.GroupID != link.GroupID {
				reason = "conflicting_capture_groups"
			}
			if old, exists := resolved[key]; !exists || old.Reason == "" || reason == "conflicting_capture_groups" {
				resolved[key] = captureProof{GroupID: link.GroupID, Reason: reason}
			}
			if reason != "" {
				gaps = append(gaps, CaptureGap{Key: &key, GroupID: link.GroupID, Reason: reason})
			}
		}
	}
	return resolved, gaps, nil
}

func evaluateCheckpointCapture(r *checkpointCapture, snap toolsnap.RegistrySnapshot, inspectErr error, evidence map[toolsnap.ToolKey]captureProof, evidenceGaps []CaptureGap, now time.Time) {
	gaps := append([]CaptureGap(nil), r.FixedGaps...)
	gaps = append(gaps, evidenceGaps...)
	current := map[toolsnap.ToolKey]toolsnap.PendingToolSnapshot{}
	for _, w := range snap.Windows {
		current[w.Key] = w
	}
	pending := false
	for _, m := range r.Members {
		proof, proven := evidence[m.Key]
		if proven && proof.Reason == "" && proof.GroupID == m.GroupID {
			continue
		}
		reason := "completion_evidence_missing"
		if w, ok := current[m.Key]; ok {
			if w.Status == "active" {
				reason = "completion_missing"
			} else {
				reason = "group_evidence_pending"
			}
		}
		if inspectErr != nil {
			reason = "registry_unavailable"
		}
		if proven && proof.GroupID != m.GroupID {
			reason = "capture_group_mismatch"
		} else if proven && proof.Reason != "" {
			reason = proof.Reason
		} else if now.UnixMilli() < r.Deadline {
			pending = true
		}
		key := m.Key
		gaps = append(gaps, CaptureGap{Key: &key, GroupID: m.GroupID, Reason: reason})
	}
	if inspectErr != nil {
		gaps = append(gaps, CaptureGap{Reason: "registry_unavailable"})
	}
	r.Result = CaptureReadiness{Status: "complete", Gaps: uniqueCaptureGaps(gaps)}
	if pending {
		r.Result.Status = "pending"
	} else if len(r.Result.Gaps) > 0 {
		r.Result.Status = "incomplete"
	}
}

func uniqueCaptureGaps(gaps []CaptureGap) []CaptureGap {
	byID := map[string]CaptureGap{}
	for _, gap := range gaps {
		raw, _ := json.Marshal(gap)
		byID[string(raw)] = gap
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]CaptureGap, 0, len(ids))
	for _, id := range ids {
		result = append(result, byID[id])
	}
	return result
}

// settleCheckpointCapture runs under the repository worker lock. Membership and
// the deadline are saved before recovery can remove registry entries.
func settleCheckpointCapture(ctx context.Context, wctx *workerContext, win eventWindow) error {
	r, err := readCheckpointCapture(ctx, wctx.h, wctx.cp)
	if err != nil {
		return err
	}
	if r != nil && r.Result.Status != "pending" {
		return nil
	}
	snap, inspectErr := toolsnap.InspectRegistry(wctx.semDir)
	if r == nil {
		r = freezeCheckpointCapture(wctx.cp, win, snap, inspectErr, time.Now())
		if err := saveCheckpointCapture(ctx, wctx.h, r); err != nil {
			return err
		}
	}
	if inspectErr == nil && (len(r.Members) > 0 || len(snap.Partials) > 0) {
		recoveryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, recoveryErr := hooks.RecoverToolWindows(recoveryCtx, filepath.Dir(wctx.semDir))
		cancel()
		if recoveryErr != nil {
			wlog("worker: capture recovery: %v\n", recoveryErr)
		}
		snap, inspectErr = toolsnap.InspectRegistry(wctx.semDir)
	}
	evidence, gaps, err := captureEvidence(ctx, wctx.h, wctx.blobStore, wctx.cp, win)
	if err != nil {
		return err
	}
	now := time.Now()
	evaluateCheckpointCapture(r, snap, inspectErr, evidence, gaps, now)
	if err := saveCheckpointCapture(ctx, wctx.h, r); err != nil {
		return err
	}
	if r.Result.Status == "pending" {
		at := now.Add(captureRetryDelay)
		if at.UnixMilli() > r.Deadline {
			at = time.UnixMilli(r.Deadline)
		}
		return &captureNotSettled{At: at}
	}
	if r.Result.Status == "incomplete" {
		wlog("worker: checkpoint %s capture incomplete (%d gaps); unmatched lines are unattributed\n", wctx.cp.CheckpointID, len(r.Result.Gaps))
	}
	return nil
}

// attributionCapture retains terminal gaps even if evidence arrives later.
// Legacy checkpoints receive a read-only assessment without claiming historical
// capture was fully observed.
func attributionCapture(ctx context.Context, h *sqlstore.Handle, bs *blobs.Store, semDir string, cp sqldb.Checkpoint, win eventWindow) (*CaptureReadiness, error) {
	r, err := readCheckpointCapture(ctx, h, cp)
	if err != nil {
		return nil, err
	}
	if r != nil {
		return &r.Result, nil
	}
	snap, inspectErr := toolsnap.InspectRegistry(semDir)
	now := time.Now()
	r = freezeCheckpointCapture(cp, win, snap, inspectErr, now)
	r.Deadline = now.UnixMilli()
	evidence, gaps, err := captureEvidence(ctx, h, bs, cp, win)
	if err != nil {
		return nil, err
	}
	evaluateCheckpointCapture(r, snap, inspectErr, evidence, gaps, now)
	if r.Result.Status == "complete" {
		return nil, nil
	}
	return &r.Result, nil
}

func applyCaptureReadiness(result *AttributionResult, capture *CaptureReadiness) {
	result.Capture = capture
	if capture == nil || capture.Status == "complete" {
		return
	}
	result.UnattributedLines = result.HumanLines
	result.HumanLines = 0
	for i := range result.Files {
		f := &result.Files[i]
		f.UnattributedLines = f.HumanLines
		f.HumanLines = 0
		if f.Classification == "human" {
			f.Classification = "unattributed"
		}
	}
	result.Diagnostics.Notes = append(result.Diagnostics.Notes, "Capture is incomplete. Unmatched lines are unattributed, not confirmed human changes.")
}
