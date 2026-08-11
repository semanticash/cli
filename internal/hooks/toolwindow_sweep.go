package hooks

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/store/blobs"
	"github.com/semanticash/cli/internal/toolsnap"
	"github.com/semanticash/cli/internal/util"
)

// ToolWindowStatus summarizes a repository's tool-window state.
type ToolWindowStatus struct {
	ActiveWindows        int
	StaleWindows         int
	PendingFinalizations int
	PendingPartials      int
	Tombstones           int
	MalformedTombstones  int
}

// InspectToolWindows reads tool-window state without modifying it.
func InspectToolWindows(_ context.Context, repoPath string) (ToolWindowStatus, error) {
	snap, err := toolsnap.InspectRegistry(filepath.Join(repoPath, ".semantica"))
	if err != nil {
		return ToolWindowStatus{}, err
	}
	var status ToolWindowStatus
	staleCutoff := time.Now().Add(-toolsnap.DefaultStaleWindowAge).UnixMilli()
	for _, w := range snap.Windows {
		if w.Status == "active" {
			status.ActiveWindows++
		}
		if w.StartedAt < staleCutoff {
			status.StaleWindows++
		}
	}
	status.PendingFinalizations = len(snap.CompleteGroups())
	status.PendingPartials = len(snap.Partials)
	status.Tombstones = len(snap.Tombstones)
	status.MalformedTombstones = len(snap.MalformedTombstones)
	return status, nil
}

// SweepReport summarizes one tool-window recovery pass.
type SweepReport struct {
	PartialsReplayed   int
	GroupsResumed      int
	GroupsTerminal     int
	LinksSkipped       int
	Errors             int
	Maintenance        toolsnap.MaintenanceReport
	MaintenanceSkipped bool
}

// SweepToolWindows recovers pending evidence before running maintenance.
// Item failures are counted and do not stop later recovery work.
func SweepToolWindows(ctx context.Context, repoPath string) (SweepReport, error) {
	var report SweepReport
	semDir := filepath.Join(repoPath, ".semantica")
	if !util.IsEnabled(semDir) {
		return report, fmt.Errorf("repository not enabled: %s", repoPath)
	}
	reg, err := toolsnap.OpenRegistry(semDir)
	if err != nil {
		return report, err
	}
	rc, err := toolsnap.ResolveRepoContext(ctx, repoPath)
	if err != nil {
		return report, err
	}
	store, err := toolsnap.OpenStore(ctx, rc, semDir)
	if err != nil {
		return report, err
	}
	repoBlobs, err := blobs.NewStore(filepath.Join(semDir, "objects"))
	if err != nil {
		return report, err
	}
	target := &toolWindowTarget{repoPath: repoPath, semDir: semDir}

	sweepPendingPartials(ctx, reg, repoBlobs, target, &report)
	sweepPendingFinalizations(ctx, reg, store, repoBlobs, target, &report)

	// Run maintenance after recovery.
	m, err := store.Maintain(ctx, reg, 0)
	if err != nil {
		report.Errors++
		report.MaintenanceSkipped = true
		slog.Warn("tool window sweep: maintenance", "repo", repoPath, "err", err)
		return report, nil
	}
	report.Maintenance = m
	return report, nil
}

// sweepPendingPartials persists recorded partial deltas and their links.
func sweepPendingPartials(ctx context.Context, reg *toolsnap.Registry, repoBlobs *blobs.Store, target *toolWindowTarget, report *SweepReport) {
	recs, err := reg.PendingPartialRecords()
	if err != nil {
		report.Errors++
		slog.Warn("tool window sweep: pending partials", "err", err)
		return
	}
	for _, rec := range recs {
		delta := partialDeltaFromRecord(rec)
		canonical, err := delta.CanonicalBytes()
		if err != nil {
			report.Errors++
			slog.Warn("tool window sweep: partial delta", "event", rec.EventID, "err", err)
			continue
		}
		deltaHash, _, err := repoBlobs.Put(ctx, canonical)
		if err != nil {
			report.Errors++
			slog.Warn("tool window sweep: persist partial delta", "event", rec.EventID, "err", err)
			continue
		}
		if err := broker.WriteEvidenceLinksToRepo(ctx, target.repoPath, []broker.EvidenceLink{{
			EventID: rec.EventID, EvidenceKind: evidenceKindToolDelta,
			EvidenceHash: deltaHash, GroupID: rec.Reason + ":" + rec.EventID,
			CreatedAt: rec.Timestamp,
		}}); err != nil {
			report.Errors++
			slog.Warn("tool window sweep: partial delta link", "event", rec.EventID, "err", err)
			continue
		}
		if err := reg.RemovePendingPartial(rec.EventID); err != nil {
			report.Errors++
			slog.Warn("tool window sweep: remove partial record", "event", rec.EventID, "err", err)
			continue
		}
		report.PartialsReplayed++
		util.AppendActivityLog(target.semDir,
			"tool-window sweep replayed partial (%s): event=%s", rec.Reason, rec.EventID)
	}
}

// sweepPendingFinalizations resumes complete groups from durable state.
func sweepPendingFinalizations(ctx context.Context, reg *toolsnap.Registry, store *toolsnap.Store, repoBlobs *blobs.Store, target *toolWindowTarget, report *SweepReport) {
	pending, err := reg.PendingFinalizations(ctx)
	if err != nil {
		report.Errors++
		slog.Warn("tool window sweep: pending finalizations", "err", err)
		return
	}
	for _, p := range pending {
		cleanupRefs := map[string]string{}
		terminal := false
		closed, err := reg.ResumeFinalization(ctx, p.GroupID,
			func(members []toolsnap.PendingToolSnapshot, prior *toolsnap.GroupFinal, _ bool, _ func() error) (toolsnap.FinalizeResult, error) {
				res, term, ferr := sweepFinalizeGroup(ctx, store, repoBlobs, target, members, prior, cleanupRefs, report)
				terminal = term
				return res, ferr
			})
		if err != nil || !closed {
			report.Errors++
			slog.Warn("tool window sweep: resume finalization", "group", p.GroupID, "closed", closed, "err", err)
			continue
		}
		releaseGroupRefs(ctx, store, cleanupRefs)
		if terminal {
			report.GroupsTerminal++
		} else {
			report.GroupsResumed++
		}
		util.AppendActivityLog(target.semDir,
			"tool-window sweep closed group %s (terminal=%v)", p.GroupID, terminal)
	}
}

// sweepFinalizeGroup persists evidence for a recovered complete group.
func sweepFinalizeGroup(ctx context.Context, store *toolsnap.Store, repoBlobs *blobs.Store, target *toolWindowTarget, members []toolsnap.PendingToolSnapshot, prior *toolsnap.GroupFinal, cleanupRefs map[string]string, report *SweepReport) (toolsnap.FinalizeResult, bool, error) {
	groupID := members[0].GroupID
	earliest := members[0]
	last := members[len(members)-1]

	var files []toolsnap.FileDelta
	var bytesRead int64
	truncated := false
	partialReason := ""
	postTree := ""
	capturedAt := last.CompletedAt

	switch {
	case prior != nil && prior.DeltaHash != "":
		// The delta is durable; persist links and release refs.
		if err := sweepEvidenceLinks(ctx, target, members, prior.DeltaHash, last.CompletedAt, report); err != nil {
			return toolsnap.FinalizeResult{Final: *prior}, prior.PartialReason != "", err
		}
		collectGroupRefs(cleanupRefs, store, members, groupID, prior.PostTreeHash)
		return toolsnap.FinalizeResult{Done: true}, prior.PartialReason != "", nil
	case prior != nil && prior.PartialReason != "":
		partialReason = prior.PartialReason
		capturedAt = prior.CapturedAt
	case prior != nil && prior.PostTreeHash != "":
		postTree = prior.PostTreeHash
		capturedAt = prior.CapturedAt
		files, bytesRead, truncated, _ = deltaOrPartial(ctx, store, earliest.TreeHash, postTree, &partialReason)
	default:
		postRefName := toolsnap.GroupPostRef(store.WorktreeID(), groupID)
		tree, found, refErr := existingRefTarget(ctx, store, postRefName)
		switch {
		case refErr != nil:
			partialReason = toolsnap.ReasonStoreUnavailable
		case found:
			postTree = tree
			files, bytesRead, truncated, _ = deltaOrPartial(ctx, store, earliest.TreeHash, postTree, &partialReason)
		default:
			// Missing post state can only produce partial evidence.
			partialReason = toolsnap.ReasonPostSnapshotLost
		}
	}
	// Report any degraded recovery as terminal partial evidence.
	terminal := partialReason != ""

	delta := assembleDelta(members, files, bytesRead, truncated, partialReason, capturedAt)
	canonical, err := delta.CanonicalBytes()
	if err != nil {
		return toolsnap.FinalizeResult{Final: finalIdentity(postTree, "", partialReason, capturedAt)}, terminal, err
	}
	deltaHash, _, err := repoBlobs.Put(ctx, canonical)
	if err != nil {
		return toolsnap.FinalizeResult{Final: finalIdentity(postTree, "", partialReason, capturedAt)}, terminal, err
	}
	if err := sweepEvidenceLinks(ctx, target, members, deltaHash, capturedAt, report); err != nil {
		return toolsnap.FinalizeResult{Final: finalIdentity(postTree, deltaHash, partialReason, capturedAt)}, terminal, err
	}
	collectGroupRefs(cleanupRefs, store, members, groupID, postTree)
	return toolsnap.FinalizeResult{Done: true}, terminal, nil
}

// sweepEvidenceLinks links a delta to each durable member event.
func sweepEvidenceLinks(ctx context.Context, target *toolWindowTarget, members []toolsnap.PendingToolSnapshot, deltaHash string, at int64, report *SweepReport) error {
	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.EventID)
	}
	missing, err := broker.MissingEventIDs(ctx, target.repoPath, ids)
	if err != nil {
		return err
	}
	links := make([]broker.EvidenceLink, 0, len(members))
	for _, m := range members {
		if missing[m.EventID] {
			report.LinksSkipped++
			slog.Warn("tool window sweep: member event missing, link skipped",
				"event", m.EventID, "group", m.GroupID)
			continue
		}
		links = append(links, broker.EvidenceLink{
			EventID:      m.EventID,
			EvidenceKind: evidenceKindToolDelta,
			EvidenceHash: deltaHash,
			GroupID:      m.GroupID,
			CreatedAt:    at,
		})
	}
	return broker.WriteEvidenceLinksToRepo(ctx, target.repoPath, links)
}
