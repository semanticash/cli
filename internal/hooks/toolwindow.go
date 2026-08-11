package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/store/blobs"
	"github.com/semanticash/cli/internal/toolsnap"
	"github.com/semanticash/cli/internal/util"
)

// toolWindowDeadline bounds one pre-tool capture.
const toolWindowDeadline = 2 * time.Second

// toolWindowTarget identifies the repository that owns a tool window.
type toolWindowTarget struct {
	repoPath     string
	semDir       string
	repositoryID string
}

// resolveToolWindowTarget returns the deepest enabled repository containing cwd.
func resolveToolWindowTarget(ctx context.Context, bh *broker.Handle, cwd string) (*toolWindowTarget, error) {
	if cwd == "" {
		return nil, nil
	}
	repos, err := broker.ListActiveRepos(ctx, bh)
	if err != nil {
		return nil, err
	}
	var best *broker.RegisteredRepo
	bestLen := 0
	for i := range repos {
		if broker.PathBelongsToRepo(cwd, repos[i].CanonicalPath) && len(repos[i].CanonicalPath) > bestLen {
			best = &repos[i]
			bestLen = len(repos[i].CanonicalPath)
		}
	}
	if best == nil {
		return nil, nil
	}
	repoID, err := broker.RepositoryIDForPath(ctx, best.Path)
	if err != nil {
		return nil, err
	}
	return &toolWindowTarget{
		repoPath:     best.Path,
		semDir:       filepath.Join(best.Path, ".semantica"),
		repositoryID: repoID,
	}, nil
}

// handleToolStepStarted captures a pre-tool snapshot without failing the provider command.
func handleToolStepStarted(ctx context.Context, providerName string, event *Event, bh *broker.Handle) error {
	// Include repository and database lookup in the capture deadline.
	wctx, cancel := context.WithTimeout(ctx, toolWindowDeadline)
	defer cancel()

	state, err := LoadCaptureState(event.SessionID)
	if errors.Is(err, ErrNoCaptureState) {
		return nil
	}
	if err != nil {
		slog.Warn("tool window: load capture state", "err", err)
		return nil
	}
	if event.TurnID == "" {
		event.TurnID = state.TurnID
	}
	if event.ToolUseID == "" || event.TurnID == "" {
		return nil
	}

	cwd := event.CWD
	if cwd == "" {
		cwd = state.CWD
	}
	target, err := resolveToolWindowTarget(wctx, bh, cwd)
	if err != nil || target == nil {
		if err != nil {
			slog.Warn("tool window: resolve target", "err", err)
		}
		return nil
	}

	rc, err := toolsnap.ResolveRepoContext(wctx, target.repoPath)
	if err != nil {
		slog.Warn("tool window: resolve repository", "err", err)
		return nil
	}
	store, err := toolsnap.OpenStore(wctx, rc, target.semDir)
	if err != nil {
		slog.Warn("tool window: open snapshot store", "err", err)
		return nil
	}
	reg, err := toolsnap.OpenRegistry(target.semDir)
	if err != nil {
		slog.Warn("tool window: open registry", "err", err)
		return nil
	}

	// Registry keys use the same underscore-normalized provider names as events.
	key := toolsnap.ToolKey{
		RepositoryID: target.repositoryID,
		Provider:     strings.ReplaceAll(providerName, "-", "_"),
		SessionID:    event.SessionID,
		TurnID:       event.TurnID,
		ToolUseID:    event.ToolUseID,
	}
	if _, err := reg.CaptureAndBegin(wctx, store, key, event.ToolName, event.Timestamp); err != nil {
		var pe *toolsnap.PartialError
		if errors.As(err, &pe) && pe.Reason == toolsnap.ReasonLockTimeout {
			// The registry is unavailable, so use the independent activity log.
			util.AppendActivityLog(target.semDir,
				"tool-window pre capture skipped (%s): tool_use=%s", pe.Reason, event.ToolUseID)
			return nil
		}
		slog.Warn("tool window: pre capture", "tool_use", event.ToolUseID, "err", err)
		return nil
	}
	return nil
}

// completeToolWindow finalizes a Bash window. It returns true when it
// persisted the closing events; otherwise the caller writes them normally.
func completeToolWindow(ctx context.Context, providerName string, event *Event, bh *broker.Handle, globalBlobs *blobs.Store, events []broker.RawEvent) bool {
	if len(events) == 0 || event.ToolUseID == "" {
		return false
	}
	wctx, cancel := context.WithTimeout(ctx, toolWindowDeadline)
	defer cancel()

	state, err := LoadCaptureState(event.SessionID)
	if err != nil {
		return false
	}
	if event.TurnID == "" {
		event.TurnID = state.TurnID
	}
	cwd := event.CWD
	if cwd == "" {
		cwd = state.CWD
	}
	target, err := resolveToolWindowTarget(wctx, bh, cwd)
	if err != nil || target == nil {
		return false
	}
	key := toolsnap.ToolKey{
		RepositoryID: target.repositoryID,
		Provider:     strings.ReplaceAll(providerName, "-", "_"),
		SessionID:    event.SessionID,
		TurnID:       event.TurnID,
		ToolUseID:    event.ToolUseID,
	}

	rc, err := toolsnap.ResolveRepoContext(wctx, target.repoPath)
	if err != nil {
		slog.Warn("tool window: resolve repository", "err", err)
		return false
	}
	store, err := toolsnap.OpenStore(wctx, rc, target.semDir)
	if err != nil {
		slog.Warn("tool window: open snapshot store", "err", err)
		return false
	}
	reg, err := toolsnap.OpenRegistry(target.semDir)
	if err != nil {
		slog.Warn("tool window: open registry", "err", err)
		return false
	}

	// A tombstone prevents a late post hook from reading newer changes.
	if tombstoned, err := reg.HasTombstone(key); err != nil || tombstoned {
		if tombstoned {
			util.AppendActivityLog(target.semDir,
				"tool-window post skipped (tombstoned): tool_use=%s", event.ToolUseID)
		}
		return false
	}

	eventID, ok := closingEventID(events, event.ToolUseID)
	if !ok {
		slog.Warn("tool window: no hook event for closing tool use", "tool_use", event.ToolUseID)
		return false
	}
	repoBlobs, err := blobs.NewStore(filepath.Join(target.semDir, "objects"))
	if err != nil {
		slog.Warn("tool window: open repo blob store", "err", err)
		return false
	}

	info := toolsnap.CompletionInfo{
		At: event.Timestamp, EventID: eventID,
		CommandSummary: commandSummary(event.ToolInput),
	}
	writeEvents := func(toolsnap.PendingToolSnapshot) error {
		_, err := broker.WriteEventsToRepo(wctx, target.repoPath, events, globalBlobs)
		return err
	}
	cleanupRefs := map[string]string{}
	closed, err := reg.Complete(wctx, key, info, writeEvents,
		func(members []toolsnap.PendingToolSnapshot, prior *toolsnap.GroupFinal, retry bool, recordIntent func() error) (toolsnap.FinalizeResult, error) {
			return finalizeGroup(wctx, store, repoBlobs, target, key, info, event, events, globalBlobs, members, prior, retry, recordIntent, cleanupRefs)
		})
	switch {
	case err == nil:
		// Release refs only after registry closure is durable.
		if closed {
			releaseGroupRefs(wctx, store, cleanupRefs)
		}
		return true
	case errors.Is(err, toolsnap.ErrNoPendingSnapshot):
		// Preserve missing-pre evidence without blocking the ordinary write path.
		return persistPartialDelta(wctx, reg, repoBlobs, target, key, event, eventID, "pre_snapshot_missing", events, globalBlobs)
	case errors.Is(err, toolsnap.ErrWindowTombstoned):
		// Preserve the event without creating evidence for an abandoned window.
		util.AppendActivityLog(target.semDir,
			"tool-window post skipped (tombstoned): tool_use=%s", event.ToolUseID)
		return false
	case err != nil:
		var pe *toolsnap.PartialError
		if errors.As(err, &pe) && pe.Reason == toolsnap.ReasonLockTimeout {
			// Prevent a later post hook from capturing newer changes.
			if terr := reg.WriteTombstone(key, event.Timestamp); terr != nil {
				slog.Warn("tool window: tombstone", "err", terr)
			}
			util.AppendActivityLog(target.semDir,
				"tool-window post lock timeout: tool_use=%s tombstoned", event.ToolUseID)
			return false
		}
		slog.Warn("tool window: complete", "tool_use", event.ToolUseID, "err", err)
		return false
	}
	return false
}

// finalizeGroup persists a group's events, delta, and evidence links.
// Retries use durable state, and refs remain until registry closure succeeds.
func finalizeGroup(ctx context.Context, store *toolsnap.Store, repoBlobs *blobs.Store, target *toolWindowTarget, key toolsnap.ToolKey, info toolsnap.CompletionInfo, event *Event, events []broker.RawEvent, globalBlobs *blobs.Store, members []toolsnap.PendingToolSnapshot, prior *toolsnap.GroupFinal, retry bool, recordIntent func() error, cleanupRefs map[string]string) (toolsnap.FinalizeResult, error) {
	groupID := members[0].GroupID
	earliest := members[0]

	var files []toolsnap.FileDelta
	var bytesRead int64
	truncated := false
	partialReason := ""
	postTree := ""
	capturedAt := event.Timestamp

	switch {
	case prior != nil && prior.DeltaHash != "":
		// Resume with the durable delta instead of reading the workspace.
		if err := persistEvidenceLinks(ctx, target, members, prior.DeltaHash, info.At); err != nil {
			return toolsnap.FinalizeResult{Final: *prior}, err
		}
		collectGroupRefs(cleanupRefs, store, members, groupID, prior.PostTreeHash)
		return toolsnap.FinalizeResult{Done: true}, nil
	case prior != nil && prior.PartialReason != "":
		partialReason = prior.PartialReason
		capturedAt = prior.CapturedAt
	case prior != nil && prior.PostTreeHash != "":
		postTree = prior.PostTreeHash
		capturedAt = prior.CapturedAt
		captureStage("delta_between")
		files, bytesRead, truncated, _ = deltaOrPartial(ctx, store, earliest.TreeHash, postTree, &partialReason)
	default:
		// A post ref preserves a capture whose registry update failed.
		// Store errors degrade to partial evidence rather than recapture.
		postRefName := toolsnap.GroupPostRef(store.WorktreeID(), groupID)
		tree, found, refErr := existingRefTarget(ctx, store, postRefName)
		if refErr != nil {
			partialReason = toolsnap.ReasonStoreUnavailable
			break
		}
		if found {
			postTree = tree
			captureStage("delta_between")
			files, bytesRead, truncated, _ = deltaOrPartial(ctx, store, earliest.TreeHash, postTree, &partialReason)
			break
		}
		if retry {
			// The original post state is gone; do not capture newer changes.
			partialReason = toolsnap.ReasonPostSnapshotLost
			break
		}
		captureStage("capture_after")
		res, err := store.CaptureAfter(ctx, toolsnap.Snapshot{TreeHash: earliest.TreeHash, HeadHash: earliest.HeadHash})
		var pe *toolsnap.PartialError
		switch {
		case err == nil:
			postTree = res.Post.TreeHash
			files, bytesRead, truncated = res.Files, res.BytesRead, res.Truncated
		case errors.As(err, &pe):
			// Preserve the stable partial reason.
			partialReason = pe.Reason
		default:
			// An unknown capture failure cannot be retried against new state.
			partialReason = toolsnap.ReasonTimeout
		}
	}

	// Record completion before other writes so a crash cannot strand the
	// member as active. Failure is non-fatal because capture can continue.
	if err := recordIntent(); err != nil {
		slog.Warn("tool window: finalization intent", "tool_use", key.ToolUseID, "err", err)
	}

	// Events must exist before any group identity that references them.
	if _, err := broker.WriteEventsToRepo(ctx, target.repoPath, events, globalBlobs); err != nil {
		return toolsnap.FinalizeResult{}, err
	}
	if postTree != "" && (prior == nil || prior.PostTreeHash == "") {
		if err := store.EnsureRef(ctx, toolsnap.GroupPostRef(store.WorktreeID(), groupID), postTree); err != nil {
			// An unreachable post tree cannot support a retry.
			partialReason = toolsnap.ReasonAlternateGone
			postTree = ""
			files, bytesRead, truncated = nil, 0, false
		}
	}
	delta := assembleDelta(members, files, bytesRead, truncated, partialReason, capturedAt)
	canonical, err := delta.CanonicalBytes()
	if err != nil {
		return toolsnap.FinalizeResult{Final: finalIdentity(postTree, "", partialReason, capturedAt)}, err
	}
	deltaHash, _, err := repoBlobs.Put(ctx, canonical)
	if err != nil {
		return toolsnap.FinalizeResult{Final: finalIdentity(postTree, "", partialReason, capturedAt)}, err
	}
	// Evidence links must be durable before the group can close.
	if err := persistEvidenceLinks(ctx, target, members, deltaHash, info.At); err != nil {
		return toolsnap.FinalizeResult{Final: finalIdentity(postTree, deltaHash, partialReason, capturedAt)}, err
	}
	collectGroupRefs(cleanupRefs, store, members, groupID, postTree)
	return toolsnap.FinalizeResult{Done: true}, nil
}

const evidenceKindToolDelta = "tool_delta"

// persistEvidenceLinks attaches one canonical delta to every member event.
func persistEvidenceLinks(ctx context.Context, target *toolWindowTarget, members []toolsnap.PendingToolSnapshot, deltaHash string, at int64) error {
	links := make([]broker.EvidenceLink, 0, len(members))
	for _, m := range members {
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

// collectGroupRefs records refs for release after registry closure.
// Until then, the refs preserve retry state.
func collectGroupRefs(dst map[string]string, store *toolsnap.Store, members []toolsnap.PendingToolSnapshot, groupID, postTree string) {
	for _, m := range members {
		if m.SnapshotRef != "" && m.TreeHash != "" {
			dst[m.SnapshotRef] = m.TreeHash
		}
	}
	if postTree != "" {
		dst[toolsnap.GroupPostRef(store.WorktreeID(), groupID)] = postTree
	}
}

// releaseGroupRefs removes unchanged refs; maintenance handles leftovers.
func releaseGroupRefs(ctx context.Context, store *toolsnap.Store, refs map[string]string) {
	for ref, tree := range refs {
		if err := store.DeleteRef(ctx, ref, tree); err != nil {
			slog.Warn("tool window: release ref", "ref", ref, "err", err)
		}
	}
}

// existingRefTarget distinguishes a missing ref from a store failure.
func existingRefTarget(ctx context.Context, store *toolsnap.Store, ref string) (string, bool, error) {
	refs, err := store.ListRefs(ctx)
	if err != nil {
		return "", false, err
	}
	tree, ok := refs[ref]
	return tree, ok, nil
}

// toolWindowCaptureSeam records capture stages in tests.
var toolWindowCaptureSeam func(stage string)

func captureStage(stage string) {
	if toolWindowCaptureSeam != nil {
		toolWindowCaptureSeam(stage)
	}
}

// deltaOrPartial computes a delta or records why it is unavailable.
func deltaOrPartial(ctx context.Context, store *toolsnap.Store, preTree, postTree string, partialReason *string) ([]toolsnap.FileDelta, int64, bool, error) {
	files, bytesRead, truncated, err := store.DeltaBetweenTrees(ctx, preTree, postTree)
	if err != nil {
		var pe *toolsnap.PartialError
		if errors.As(err, &pe) {
			*partialReason = pe.Reason
		} else {
			*partialReason = toolsnap.ReasonAlternateGone
		}
		return nil, 0, false, err
	}
	return files, bytesRead, truncated, nil
}

func finalIdentity(postTree, deltaHash, partialReason string, capturedAt int64) toolsnap.GroupFinal {
	if partialReason != "" {
		return toolsnap.GroupFinal{PartialReason: partialReason, CapturedAt: capturedAt}
	}
	return toolsnap.GroupFinal{PostTreeHash: postTree, DeltaHash: deltaHash, CapturedAt: capturedAt}
}

// assembleDelta builds the canonical delta for a closed group from
// its registry members.
func assembleDelta(members []toolsnap.PendingToolSnapshot, files []toolsnap.FileDelta, bytesRead int64, truncated bool, partialReason string, capturedAt int64) *toolsnap.Delta {
	actorIndex := map[toolsnap.Actor]int{}
	var actors []toolsnap.Actor
	var uses []toolsnap.ToolUse
	startedAt := members[0].StartedAt
	completedAt := capturedAt
	for _, m := range members {
		a := toolsnap.Actor{Provider: m.Key.Provider, SessionID: m.Key.SessionID, TurnID: m.Key.TurnID}
		idx, ok := actorIndex[a]
		if !ok {
			idx = len(actors)
			actorIndex[a] = idx
			actors = append(actors, a)
		}
		// Each member contributes its own completion metadata.
		uses = append(uses, toolsnap.ToolUse{
			ToolUseID: m.Key.ToolUseID, ToolName: m.ToolName,
			CommandSummary: m.CommandSummary,
			EventID:        m.EventID, Actor: idx,
		})
		if m.StartedAt < startedAt {
			startedAt = m.StartedAt
		}
		if m.CompletedAt > completedAt {
			completedAt = m.CompletedAt
		}
	}
	scope := "tool"
	if len(members) > 1 {
		scope = "concurrent_group"
	}
	status := "complete"
	if partialReason != "" {
		status = "partial"
		files, bytesRead, truncated = nil, 0, false
	}
	return &toolsnap.Delta{
		Scope: scope, Status: status, Reason: partialReason,
		Window: toolsnap.Window{
			StartedAt: startedAt, CompletedAt: completedAt,
			DurationMS: completedAt - startedAt,
		},
		Actors: actors, ToolUses: uses, Files: files,
		Limits: toolsnap.Limits{FilesObserved: len(files), BytesRead: bytesRead, Truncated: truncated},
	}
}

// persistPartialDelta records linked evidence when no pre snapshot exists.
// A first-delivery record keeps retries and duplicate hooks deterministic.
func persistPartialDelta(ctx context.Context, reg *toolsnap.Registry, repoBlobs *blobs.Store, target *toolWindowTarget, key toolsnap.ToolKey, event *Event, eventID, reason string, events []broker.RawEvent, globalBlobs *blobs.Store) bool {
	// A durable link makes duplicate delivery a no-op.
	if exists, err := broker.HasEvidenceLink(ctx, target.repoPath, broker.EvidenceLink{
		EventID: eventID, EvidenceKind: evidenceKindToolDelta, GroupID: reason + ":" + eventID,
	}); err == nil && exists {
		return true
	}
	if _, err := broker.WriteEventsToRepo(ctx, target.repoPath, events, globalBlobs); err != nil {
		slog.Warn("tool window: partial delta events", "err", err)
		return false
	}
	rec, err := reg.LoadOrRecordPendingPartial(toolsnap.PendingPartialRecord{
		Key: key, EventID: eventID, Reason: reason, ToolName: event.ToolName,
		CommandSummary: commandSummary(event.ToolInput), Timestamp: event.Timestamp,
	})
	if err != nil {
		slog.Warn("tool window: pending partial record", "err", err)
		return false
	}
	delta := partialDeltaFromRecord(rec)
	canonical, err := delta.CanonicalBytes()
	if err != nil {
		slog.Warn("tool window: partial delta", "err", err)
		return false
	}
	deltaHash, _, err := repoBlobs.Put(ctx, canonical)
	if err != nil {
		slog.Warn("tool window: persist partial delta", "err", err)
		return false
	}
	// Groupless partials use a deterministic reason-and-event key.
	if err := broker.WriteEvidenceLinksToRepo(ctx, target.repoPath, []broker.EvidenceLink{{
		EventID: rec.EventID, EvidenceKind: evidenceKindToolDelta,
		EvidenceHash: deltaHash, GroupID: rec.Reason + ":" + rec.EventID,
		CreatedAt: rec.Timestamp,
	}}); err != nil {
		slog.Warn("tool window: partial delta link", "err", err)
		return false
	}
	// The durable link supersedes the recovery record.
	if err := reg.RemovePendingPartial(rec.EventID); err != nil {
		slog.Warn("tool window: remove partial record", "err", err)
	}
	util.AppendActivityLog(target.semDir,
		"tool-window partial (%s): tool_use=%s", reason, key.ToolUseID)
	return true
}

// partialDeltaFromRecord rebuilds a groupless partial from durable inputs.
func partialDeltaFromRecord(rec toolsnap.PendingPartialRecord) *toolsnap.Delta {
	return &toolsnap.Delta{
		Scope: "tool", Status: "partial", Reason: rec.Reason,
		Window: toolsnap.Window{StartedAt: rec.Timestamp, CompletedAt: rec.Timestamp},
		Actors: []toolsnap.Actor{{Provider: rec.Key.Provider, SessionID: rec.Key.SessionID, TurnID: rec.Key.TurnID}},
		ToolUses: []toolsnap.ToolUse{{
			ToolUseID: rec.Key.ToolUseID, ToolName: rec.ToolName,
			EventID: rec.EventID, CommandSummary: rec.CommandSummary,
		}},
	}
}

// closingEventID returns the hook event for the closing tool use.
func closingEventID(events []broker.RawEvent, toolUseID string) (string, bool) {
	for _, ev := range events {
		if ev.ToolUseID == toolUseID && ev.EventSource == "hook" {
			return ev.EventID, true
		}
	}
	return "", false
}

// commandSummary extracts the Bash command from the tool input.
func commandSummary(input json.RawMessage) string {
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	const maxSummary = 200
	if len(in.Command) > maxSummary {
		return in.Command[:maxSummary]
	}
	return in.Command
}
