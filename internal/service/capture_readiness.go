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
	"github.com/semanticash/cli/internal/turncapture"
)

var captureGrace = 30 * time.Second
var captureRetryDelay = 5 * time.Second

// CaptureGap records missing evidence or unresolved authorship for a checkpoint.
type CaptureGap struct {
	Key     *toolsnap.ToolKey `json:"key,omitempty"`
	GroupID string            `json:"group_id,omitempty"`
	Reason  string            `json:"reason"`
}

// CaptureReadiness covers known tool windows and turn-observed changes.
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
		// Unreadable registry state cannot establish checkpoint membership.
		r.FixedGaps = append(r.FixedGaps, CaptureGap{Reason: "registry_unavailable"})
		return r
	}
	for _, w := range snap.Windows {
		if w.Key.RepositoryID != cp.RepositoryID || w.StartedAt > r.Through {
			continue
		}
		// Registry timestamps cannot resolve event-cursor ties.
		if w.Status == "complete" && w.CompletedAt < r.After {
			continue
		}
		r.Members = append(r.Members, captureMember{Key: w.Key, GroupID: w.GroupID})
	}
	// Tombstones lack a start timestamp. Records created at or after the lower
	// boundary conservatively remain gaps, including recovery after the commit.
	for _, t := range snap.Tombstones {
		if t.Key.RepositoryID == cp.RepositoryID && t.At >= r.After {
			key := t.Key
			r.FixedGaps = append(r.FixedGaps, CaptureGap{Key: &key, Reason: "completion_missing"})
		}
	}
	for _, p := range snap.Partials {
		if p.Key.RepositoryID == cp.RepositoryID && p.Timestamp >= r.After && p.Timestamp <= r.Through {
			key := p.Key
			r.FixedGaps = append(r.FixedGaps, CaptureGap{Key: &key, Reason: p.Reason})
		}
	}
	if len(snap.MalformedTombstones) != 0 {
		r.FixedGaps = append(r.FixedGaps, CaptureGap{Reason: "invalid_capture_tombstone"})
	}
	return r
}

// captureEvidence validates tool deltas and checks turn-observation uncertainty.
// Registry closure alone does not prove evidence was persisted.
func captureEvidence(ctx context.Context, h *sqlstore.Handle, bs *blobs.Store, cp sqldb.Checkpoint, win eventWindow) (map[toolsnap.ToolKey]captureProof, []CaptureGap, error) {
	links, err := h.Queries.ListEvidenceLinksInWindow(ctx, sqldb.ListEvidenceLinksInWindowParams{
		RepositoryID: cp.RepositoryID, UseCursor: win.cursorFlag(), AfterCursor: win.cursorAfter(),
		UpToCursor: win.cursorUpTo(), AfterTs: win.afterTs, UpToTs: win.upToTs,
	})
	if err != nil {
		return nil, nil, err
	}
	resolved := map[toolsnap.ToolKey]captureProof{}
	gaps, err := turnObservationGaps(ctx, h, bs, cp, win)
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	linked := map[string]map[string]sqldb.ListCaptureGroupLinksRow{}
	selected := map[string]map[string]bool{}
	for _, link := range links {
		if linked[link.GroupID] == nil {
			members, err := h.Queries.ListCaptureGroupLinks(ctx, sqldb.ListCaptureGroupLinksParams{GroupID: link.GroupID, RepositoryID: cp.RepositoryID})
			if err != nil {
				return nil, nil, err
			}
			linked[link.GroupID] = map[string]sqldb.ListCaptureGroupLinksRow{}
			selected[link.GroupID] = map[string]bool{}
			for _, member := range members {
				linked[link.GroupID][member.EventID] = member
			}
		}
		selected[link.GroupID][link.EventID] = true
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
		case d.Window.CompletedAt == cp.CreatedAt:
			// Post-state timestamps cannot resolve checkpoint cursor ties.
			reason = "post_state_order_unknown"
		case d.Limits.Truncated:
			reason = "evidence_truncated"
		}
		for _, f := range d.Files {
			if f.Truncated {
				reason = "evidence_truncated"
			}
		}
		groupValid := len(linked[link.GroupID]) == len(d.ToolUses)
		for _, use := range d.ToolUses {
			member, exists := linked[link.GroupID][use.EventID]
			if !exists || member.Provider != d.Actors[use.Actor].Provider || member.EvidenceHash != link.EvidenceHash {
				groupValid = false
			}
		}
		if !groupValid {
			gaps = append(gaps, CaptureGap{GroupID: link.GroupID, Reason: "evidence_member_not_linked"})
			continue
		}
		for _, use := range d.ToolUses {
			if !selected[link.GroupID][use.EventID] {
				continue
			}
			a := d.Actors[use.Actor]
			key := toolsnap.ToolKey{RepositoryID: cp.RepositoryID, Provider: a.Provider, SessionID: a.SessionID, TurnID: a.TurnID, ToolUseID: use.ToolUseID}
			useReason := reason
			if old, exists := resolved[key]; exists && old.GroupID != link.GroupID {
				useReason = "conflicting_capture_groups"
			}
			if old, exists := resolved[key]; !exists || old.Reason == "" || useReason == "conflicting_capture_groups" {
				resolved[key] = captureProof{GroupID: link.GroupID, Reason: useReason}
			}
			if useReason != "" {
				gaps = append(gaps, CaptureGap{Key: &key, GroupID: link.GroupID, Reason: useReason})
			}
		}
	}
	return resolved, gaps, nil
}

// turnObservationGaps finds observed changes without proof of authorship.
func turnObservationGaps(ctx context.Context, h *sqlstore.Handle, bs *blobs.Store, cp sqldb.Checkpoint, win eventWindow) ([]CaptureGap, error) {
	rows, err := h.Queries.ListTurnObservationsForCheckpoint(ctx, sqldb.ListTurnObservationsForCheckpointParams{
		RepositoryID: cp.RepositoryID, UseCursor: win.cursorFlag(), AfterCursor: win.cursorAfter(),
		UpToCursor: win.cursorUpTo(), AfterTs: win.afterTs, UpToTs: win.upToTs, CheckpointID: cp.CheckpointID,
	})
	if err != nil {
		return nil, err
	}
	links, err := h.Queries.GetCommitLinksByCheckpoint(ctx, cp.CheckpointID)
	if err != nil {
		return nil, err
	}
	commits := make(map[string]bool, len(links))
	for _, link := range links {
		if link.RepositoryID == cp.RepositoryID {
			commits[link.CommitHash] = true
		}
	}
	var gaps []CaptureGap
	for _, row := range rows {
		raw, readErr := bs.Get(ctx, row.EvidenceHash.String)
		sum := sha256.Sum256(raw)
		var record turncapture.Record
		if readErr != nil || hex.EncodeToString(sum[:]) != row.EvidenceHash.String || json.Unmarshal(raw, &record) != nil ||
			record.Version != 1 || record.TurnID != row.TurnID.String || record.End == nil || record.End.FinishedAt.IsZero() ||
			len(record.Repositories) != 1 || len(record.End.Repositories) != 1 || record.Repositories[0].Subject.RepositoryID != cp.RepositoryID {
			gaps = append(gaps, CaptureGap{GroupID: row.EventID, Reason: "turn_observation_unavailable"})
			continue
		}
		inWindow := row.Ts > win.afterTs && row.Ts <= win.upToTs
		if win.useCursor {
			inWindow = (row.Ts > win.afterTs || (row.Ts == win.afterTs && row.InsertSeq.Int64 > win.afterCursor)) &&
				(row.Ts < win.upToTs || (row.Ts == win.upToTs && row.InsertSeq.Int64 <= win.upToCursor))
		}
		if turnObservationApplies(record.End.Repositories[0], commits, inWindow) {
			gaps = append(gaps, CaptureGap{GroupID: row.EventID, Reason: "turn_observation_authorship_unknown"})
		}
	}
	return gaps, nil
}

func turnObservationApplies(observation toolsnap.TurnObservation, commits map[string]bool, inWindow bool) bool {
	if observation.State != "changed" {
		return false
	}
	lastCommitTree := ""
	for _, change := range observation.Changes {
		if change.Commit != "" {
			if commits[change.Commit] && len(change.Files) > 0 {
				return true
			}
			lastCommitTree = change.Tree
			continue
		}
		// The final delta includes committed changes. Only remaining dirty state
		// belongs to the publication window rather than a recorded commit.
		if inWindow && ((lastCommitTree == "" && len(change.Files) > 0) || (lastCommitTree != "" && change.Tree != lastCommitTree)) {
			return true
		}
	}
	return false
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

// settleCheckpointCapture requires the repository worker lock and saves
// membership before recovery can remove registry entries.
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

// attributionCapture preserves recorded gaps. Legacy checkpoints are assessed
// from surviving evidence without persisting a capture result.
func attributionCapture(ctx context.Context, h *sqlstore.Handle, bs *blobs.Store, semDir string, cp sqldb.Checkpoint, win eventWindow) (*CaptureReadiness, error) {
	r, err := readCheckpointCapture(ctx, h, cp)
	if err != nil {
		return nil, err
	}
	if r != nil {
		// Include observations published after the capture result was saved.
		gaps, err := turnObservationGaps(ctx, h, bs, cp, win)
		if err != nil {
			return nil, err
		}
		if len(gaps) > 0 {
			r.Result.Gaps = uniqueCaptureGaps(append(r.Result.Gaps, gaps...))
			if r.Result.Status == "complete" {
				r.Result.Status = "incomplete"
			}
		}
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
