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
	"github.com/semanticash/cli/internal/doctor"
	"github.com/semanticash/cli/internal/store/blobs"
	"github.com/semanticash/cli/internal/toolsnap"
	"github.com/semanticash/cli/internal/util"
)

// toolWindowDeadline bounds one pre- or post-tool capture.
var toolWindowDeadline = 2 * time.Second

// toolWindowPreCapture overrides the pre-capture context in tests.
var toolWindowPreCapture func(context.Context) context.Context

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

// toolWindowCWD returns the command directory, then the session directory.
func toolWindowCWD(event *Event, state *CaptureState) string {
	if event.EffectiveCWD != "" {
		return event.EffectiveCWD
	}
	if event.CWD != "" {
		return event.CWD
	}
	if state != nil {
		return state.CWD
	}
	return ""
}

// effectiveDirProvider reports whether shell windows require persisted
// command-directory routing. Cursor supplies this directory.
func effectiveDirProvider(providerName string) bool {
	return providerName == "cursor"
}

// receiptKeyFor returns the identity used to persist a window target.
func receiptKeyFor(providerName string, event *Event) toolWindowReceiptKey {
	return toolWindowReceiptKey{
		Provider:  providerName,
		SessionID: event.SessionID,
		TurnID:    event.TurnID,
		ToolUseID: event.ToolUseID,
	}
}

// toolWindowBench describes a hook's capture result.
type toolWindowBench struct {
	outcome       string
	partialReason string
	filesChanged  int
	bytesRead     int64
	groupMembers  int
}

// emitToolWindowBench records a tool-window result and optional operation timings.
func emitToolWindowBench(target *toolWindowTarget, event *Event, phase string, start time.Time, bench *toolWindowBench, stages *toolsnap.StageTimes, scope *doctor.BenchScope) {
	if target == nil || bench.outcome == "" {
		return
	}
	rec := doctor.BenchRecord{
		Kind: "toolwindow", Phase: phase, Tool: event.ToolName,
		SessionID: event.SessionID, TurnID: event.TurnID, ToolUseID: event.ToolUseID,
		DurationMS:    doctor.Milliseconds(time.Since(start)),
		Outcome:       bench.outcome,
		PartialReason: bench.partialReason,
		FilesChanged:  bench.filesChanged,
		BytesRead:     bench.bytesRead,
		GroupMembers:  bench.groupMembers,
	}
	if stages != nil {
		rec.StageLeafMS = stages.LeafMillis()
		rec.StageAggMS = stages.AggregateMillis()
		if un := rec.DurationMS - stages.LeafTotal().Milliseconds(); un > 0 {
			rec.UnaccountedMS = un
		}
		// These timings are nested within persist_events.
		if scope != nil {
			st := scope.Snapshot()[target.repoPath]
			detail := map[string]int64{}
			if ms := doctor.Milliseconds(st.DBDuration); ms > 0 {
				detail["event_db_write"] = ms
			}
			if ms := doctor.Milliseconds(st.BlobDuration); ms > 0 {
				detail["event_blob_propagate"] = ms
			}
			if len(detail) > 0 {
				rec.PersistDetailMS = detail
			}
		}
	}
	doctor.EmitBenchRecord(target.repoPath, rec)
}

// handleToolStepStarted captures a pre-tool snapshot without failing the provider command.
func handleToolStepStarted(ctx context.Context, providerName string, event *Event, bh *broker.Handle) error {
	// Include repository and database lookup in the capture deadline.
	wctx, cancel := context.WithTimeout(ctx, toolWindowDeadline)
	defer cancel()
	start := time.Now()

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
	if isBackgroundBash(event) {
		// Detached writes occur after PostToolUse. The post hook records a partial.
		return nil
	}

	target, err := resolveToolWindowTarget(wctx, bh, toolWindowCWD(event, state))
	if err != nil {
		slog.Warn("tool window: resolve target", "err", err)
		return nil
	}
	if target == nil {
		// Without a primary repository, the environment controls observation.
		if util.ObservationRoutingGlobalDefault() {
			observeAdditionalCandidates(wctx, providerName, event, bh, nil, toolWindowCWD(event, state), state.CWD)
		}
		return nil
	}
	// Include hooks that do not write database rows in timing output.
	doctor.AddBenchStats(wctx, target.repoPath, doctor.BenchStats{})
	bench := &toolWindowBench{}
	var stages *toolsnap.StageTimes
	if doctor.BenchEnabled(target.repoPath) {
		wctx, stages = toolsnap.WithStageTimes(wctx)
	}
	scope := doctor.BenchScopeFrom(wctx)
	defer func() { emitToolWindowBench(target, event, "pre", start, bench, stages, scope) }()

	// Persist command-directory targets before opening a window so the post hook
	// completes the same repository. A failed write leaves the window unopened.
	if effectiveDirProvider(providerName) {
		key := receiptKeyFor(providerName, event)
		if err := SaveToolWindowTarget(key, target.repoPath, target.repositoryID); err != nil {
			bench.outcome = "error"
			slog.Warn("tool window: persist target", "err", err)
			return nil
		}
		// Remove the target if the window does not open.
		defer func() {
			if bench.outcome != "registered" {
				if derr := DeleteToolWindowTarget(key); derr != nil {
					slog.Warn("tool window: reclaim target", "err", derr)
				}
			}
		}()
	}

	stopSetup := toolsnap.MeasureStage(wctx, "snapshot_store_setup")
	rc, err := toolsnap.ResolveRepoContext(wctx, target.repoPath)
	if err != nil {
		stopSetup()
		bench.outcome = "error"
		slog.Warn("tool window: resolve repository", "err", err)
		return nil
	}
	store, err := toolsnap.OpenStore(wctx, rc, target.semDir)
	if err != nil {
		stopSetup()
		bench.outcome = "error"
		slog.Warn("tool window: open snapshot store", "err", err)
		return nil
	}
	reg, err := toolsnap.OpenRegistry(target.semDir)
	if err != nil {
		stopSetup()
		bench.outcome = "error"
		slog.Warn("tool window: open registry", "err", err)
		return nil
	}
	stopSetup()

	// Registry keys use the same underscore-normalized provider names as events.
	key := toolsnap.ToolKey{
		RepositoryID: target.repositoryID,
		Provider:     strings.ReplaceAll(providerName, "-", "_"),
		SessionID:    event.SessionID,
		TurnID:       event.TurnID,
		ToolUseID:    event.ToolUseID,
	}
	captureCtx := wctx
	if toolWindowPreCapture != nil {
		captureCtx = toolWindowPreCapture(wctx)
	}
	win, err := reg.CaptureAndBegin(captureCtx, store, key, event.ToolName, event.Timestamp)
	switch {
	case err != nil:
		var pe *toolsnap.PartialError
		if errors.As(err, &pe) && pe.Reason == toolsnap.ReasonLockTimeout {
			bench.outcome = "lock_timeout"
			bench.partialReason = pe.Reason
			// The registry is unavailable, so use the independent activity log.
			util.AppendActivityLog(target.semDir,
				"tool-window pre capture skipped (%s): tool_use=%s", pe.Reason, event.ToolUseID)
			return nil
		}
		bench.outcome = "error"
		slog.Warn("tool window: pre capture", "tool_use", event.ToolUseID, "err", err)
		return nil
	case win.Key.ToolUseID == "":
		// Settled key: closed or tombstoned identities never reopen.
		bench.outcome = "skipped_settled"
	default:
		bench.outcome = "registered"
	}

	// Open additional observation windows when enabled.
	if bench.outcome == "registered" && util.ObservationRoutingEnabled(target.semDir) {
		observeAdditionalCandidates(wctx, providerName, event, bh, target, toolWindowCWD(event, state), state.CWD)
	}
	return nil
}

// toolWindowObserveBudget is the shared timeout for candidate snapshots.
var toolWindowObserveBudget = 2 * time.Second

// observeAdditionalCandidates freezes the candidate set and opens observation-only
// windows outside the primary repository. Failures are recorded as coverage gaps.
func observeAdditionalCandidates(ctx context.Context, providerName string, event *Event, bh *broker.Handle, primary *toolWindowTarget, cmdCWD, sessionCWD string) {
	receiptKey := receiptKeyFor(providerName, event)

	// Serialize snapshot creation, checkpoints, and retry reconciliation per window.
	release, ok := acquireObservationAttempt(receiptKey)
	if !ok {
		return
	}
	defer release()

	// Reconcile frozen candidates even if repository registrations have changed.
	existing, lerr := LoadToolWindowObservation(receiptKey)
	if lerr != nil {
		slog.Warn("tool window observe: load manifest", "err", lerr)
		return
	}
	if existing != nil {
		reconcileObservation(ctx, providerName, event, receiptKey, existing)
		return
	}

	repos, err := broker.ListActiveRepos(ctx, bh)
	if err != nil {
		slog.Warn("tool window observe: list repos", "err", err)
		return
	}
	primaryCanonical := ""
	for _, r := range repos {
		if primary != nil && r.Path == primary.repoPath {
			primaryCanonical = r.CanonicalPath
			break
		}
	}
	eligible := make([]broker.RegisteredRepo, 0, len(repos))
	for _, r := range repos {
		if primary != nil && r.CanonicalPath == primaryCanonical {
			continue // the primary publishes; it is not observed
		}
		eligible = append(eligible, r)
	}
	if len(eligible) == 0 {
		return
	}
	set := selectToolWindowCandidates(eligible, cmdCWD, sessionCWD, defaultToolWindowCandidateCap)

	// Count unresolved repository identities as coverage gaps.
	type resolvedCandidate struct{ path, repoID string }
	var resolved []resolvedCandidate
	candidates := make([]ObservedCandidate, 0, len(set.Repos))
	unresolved := 0
	for _, r := range set.Repos {
		repoID, rerr := broker.RepositoryIDForPath(ctx, r.Path)
		if rerr != nil || repoID == "" {
			unresolved++
			slog.Warn("tool window observe: resolve candidate identity", "repo", r.Path, "err", rerr)
			continue
		}
		candidates = append(candidates, ObservedCandidate{RepoPath: r.Path, RepositoryID: repoID, Outcome: observationPending})
		resolved = append(resolved, resolvedCandidate{path: r.Path, repoID: repoID})
	}

	// Preserve coverage gaps even when no identities resolve.
	manifest := &ToolWindowObservationRecord{
		Candidates:        candidates,
		OmittedByCap:      set.OmittedByCap,
		OmittedUnresolved: unresolved,
		TotalActive:       set.TotalActive,
	}
	if primary != nil {
		manifest.PrimaryRepoPath = primary.repoPath
		manifest.PrimaryRepositoryID = primary.repositoryID
	}
	created, err := CreateToolWindowObservation(receiptKey, manifest)
	if err != nil {
		slog.Warn("tool window observe: create manifest", "err", err)
		return
	}
	if !created {
		// Another creator's frozen set takes precedence over this selection.
		if existing, lerr := LoadToolWindowObservation(receiptKey); lerr == nil && existing != nil {
			reconcileObservation(ctx, providerName, event, receiptKey, existing)
		}
		return
	}

	// Share one snapshot budget and record unattempted candidates as budget-expired.
	obsCtx, cancel := context.WithTimeout(ctx, toolWindowObserveBudget)
	defer cancel()
	for _, c := range resolved {
		outcome := observationBudgetExpired
		if obsCtx.Err() == nil {
			outcome = observeOneCandidate(obsCtx, providerName, event, c.path, c.repoID)
		}
		if err := CheckpointToolWindowObservation(receiptKey, map[string]string{c.path: outcome}); err != nil {
			slog.Warn("tool window observe: checkpoint outcome", "repo", c.path, "err", err)
		}
	}
}

// reconcileObservation resolves pending candidates from existing registrations.
// It preserves terminal outcomes and never takes new snapshots.
func reconcileObservation(ctx context.Context, providerName string, event *Event, receiptKey toolWindowReceiptKey, rec *ToolWindowObservationRecord) {
	for _, c := range rec.Candidates {
		if c.Outcome != observationPending {
			continue // frozen terminal outcomes are never changed
		}
		outcome := observationSnapshotFailed // no snapshot exists and we must not take one now
		if active, err := observeWindowActive(ctx, providerName, event, c.RepoPath, c.RepositoryID); err == nil && active {
			outcome = observationObserved
		}
		if err := CheckpointToolWindowObservation(receiptKey, map[string]string{c.RepoPath: outcome}); err != nil {
			slog.Warn("tool window observe: reconcile checkpoint", "repo", c.RepoPath, "err", err)
		}
	}
}

// observeWindowActive checks for an existing observation window without creating one.
func observeWindowActive(_ context.Context, providerName string, event *Event, repoPath, repositoryID string) (bool, error) {
	snap, err := toolsnap.InspectRegistry(filepath.Join(repoPath, ".semantica"))
	if err != nil {
		return false, err
	}
	key := toolsnap.ToolKey{
		RepositoryID: repositoryID,
		Provider:     strings.ReplaceAll(providerName, "-", "_"),
		SessionID:    event.SessionID,
		TurnID:       event.TurnID,
		ToolUseID:    event.ToolUseID,
		Mode:         toolsnap.ToolModeObserve,
	}
	for _, w := range snap.Windows {
		if w.Key == key {
			return true, nil
		}
	}
	return false, nil
}

// observeOneCandidate opens an observation window and returns its capture outcome.
func observeOneCandidate(ctx context.Context, providerName string, event *Event, repoPath, repositoryID string) string {
	semDir := filepath.Join(repoPath, ".semantica")
	rc, err := toolsnap.ResolveRepoContext(ctx, repoPath)
	if err != nil {
		slog.Warn("tool window observe: resolve repo", "repo", repoPath, "err", err)
		return observationSnapshotFailed
	}
	store, err := toolsnap.OpenStore(ctx, rc, semDir)
	if err != nil {
		slog.Warn("tool window observe: open store", "repo", repoPath, "err", err)
		return observationSnapshotFailed
	}
	reg, err := toolsnap.OpenRegistry(semDir)
	if err != nil {
		slog.Warn("tool window observe: open registry", "repo", repoPath, "err", err)
		return observationSnapshotFailed
	}
	key := toolsnap.ToolKey{
		RepositoryID: repositoryID,
		Provider:     strings.ReplaceAll(providerName, "-", "_"),
		SessionID:    event.SessionID,
		TurnID:       event.TurnID,
		ToolUseID:    event.ToolUseID,
		Mode:         toolsnap.ToolModeObserve,
	}
	captureCtx := ctx
	if toolWindowPreCapture != nil {
		captureCtx = toolWindowPreCapture(ctx)
	}
	if _, err := reg.CaptureAndBegin(captureCtx, store, key, event.ToolName, event.Timestamp); err != nil {
		slog.Warn("tool window observe: pre capture", "repo", repoPath, "err", err)
		return observationSnapshotFailed
	}
	return observationObserved
}

// completeObservedCandidates completes registered observations from the manifest,
// regardless of the current gate. It removes the manifest once all candidates
// resolve and never takes new before-snapshots.
func completeObservedCandidates(ctx context.Context, providerName string, event *Event) {
	receiptKey := receiptKeyFor(providerName, event)
	// Avoid locking when no observation manifest exists.
	if rec, err := LoadToolWindowObservation(receiptKey); err != nil || rec == nil {
		if err != nil {
			slog.Warn("tool window observe: load manifest for completion", "err", err)
		}
		return
	}
	// Record arrival before lock contention can interrupt completion.
	firstArrival, aerr := recordCompletionArrival(receiptKey)
	if aerr != nil {
		slog.Warn("tool window observe: record completion arrival", "err", aerr)
	}

	// Serialize completion with pre-hook attempts; retain the manifest on contention.
	release, ok := acquireObservationAttempt(receiptKey)
	if !ok {
		return
	}
	defer release()
	rec, err := LoadToolWindowObservation(receiptKey)
	if err != nil || rec == nil {
		return
	}

	// Recorded or uncertain arrivals must reuse frozen post-state or produce a partial.
	// If both arrival records are lost, a retry can still capture later edits.
	// These deltas are for shadow measurement only, never routing or authorship.
	// See TestObserve_DoubleRecordLossCanCaptureDrift.
	priorAttempt := !firstArrival || aerr != nil || rec.CompletionAttempted
	// Keep a second arrival record in case the sentinel could not be created.
	if !rec.CompletionAttempted {
		if merr := MarkObservationCompletionAttempted(receiptKey); merr != nil {
			slog.Warn("tool window observe: mark completion attempt", "err", merr)
		}
	}

	// Share one completion budget; retain unfinished candidates for recovery.
	obsCtx, cancel := context.WithTimeout(ctx, toolWindowObserveBudget)
	defer cancel()
	allDone := true
	for _, c := range rec.Candidates {
		switch c.Outcome {
		case observationSnapshotFailed, observationBudgetExpired:
			continue // definitive gap: no window to complete
		case observationObserved, observationPending:
			if obsCtx.Err() != nil {
				allDone = false // not attempted within the budget
				continue
			}
			if !completeOneObservedCandidate(obsCtx, providerName, event, receiptKey, c, priorAttempt) {
				allDone = false
			}
		default:
			allDone = false
		}
	}
	// Keep the manifest until every candidate is resolved.
	if allDone {
		if derr := DeleteToolWindowObservation(receiptKey); derr != nil {
			slog.Warn("tool window observe: delete manifest", "err", derr)
		}
	}
}

// completeOneObservedCandidate stores a registered observation without publishing
// events or links. It returns whether the candidate is terminal. With priorAttempt
// set, missing post-state produces a partial instead of a fresh workspace capture.
func completeOneObservedCandidate(ctx context.Context, providerName string, event *Event, receiptKey toolWindowReceiptKey, c ObservedCandidate, priorAttempt bool) bool {
	repoPath, repositoryID := c.RepoPath, c.RepositoryID
	semDir := filepath.Join(repoPath, ".semantica")
	rc, err := toolsnap.ResolveRepoContext(ctx, repoPath)
	if err != nil {
		slog.Warn("tool window observe: resolve repo", "repo", repoPath, "err", err)
		return false
	}
	store, err := toolsnap.OpenStore(ctx, rc, semDir)
	if err != nil {
		slog.Warn("tool window observe: open store", "repo", repoPath, "err", err)
		return false
	}
	reg, err := toolsnap.OpenRegistry(semDir)
	if err != nil {
		slog.Warn("tool window observe: open registry", "repo", repoPath, "err", err)
		return false
	}
	repoBlobs, err := blobs.NewStore(filepath.Join(semDir, "objects"))
	if err != nil {
		slog.Warn("tool window observe: open blob store", "repo", repoPath, "err", err)
		return false
	}
	key := toolsnap.ToolKey{
		RepositoryID: repositoryID,
		Provider:     strings.ReplaceAll(providerName, "-", "_"),
		SessionID:    event.SessionID,
		TurnID:       event.TurnID,
		ToolUseID:    event.ToolUseID,
		Mode:         toolsnap.ToolModeObserve,
	}
	// This synthetic identity is delta metadata, not a published event.
	info := toolsnap.CompletionInfo{
		At: event.Timestamp, EventID: "observe:" + event.ToolUseID,
		CommandSummary: commandSummary(event.ToolInput),
	}
	cleanupRefs := map[string]string{}
	closed, err := reg.Complete(ctx, key, info, nil,
		func(members []toolsnap.PendingToolSnapshot, prior *toolsnap.GroupFinal, retry bool, recordIntent func() error) (toolsnap.FinalizeResult, error) {
			return finalizeObserveGroup(ctx, store, rc.HeadAnchor(), repoBlobs, members, prior, retry || priorAttempt, recordIntent, cleanupRefs, event)
		})
	switch {
	case err == nil && closed:
		// Confirm the pending candidate's registration.
		if c.Outcome == observationPending {
			if cerr := CheckpointToolWindowObservation(receiptKey, map[string]string{repoPath: observationObserved}); cerr != nil {
				slog.Warn("tool window observe: checkpoint observed", "repo", repoPath, "err", cerr)
			}
		}
		releaseGroupRefs(ctx, reg, store, cleanupRefs)
		return true
	case err == nil && !closed:
		return false // group has other active members; complete later
	case errors.Is(err, toolsnap.ErrNoPendingSnapshot):
		// Record the missing registration without taking a new before-snapshot.
		if c.Outcome == observationPending {
			if cerr := CheckpointToolWindowObservation(receiptKey, map[string]string{repoPath: observationSnapshotFailed}); cerr != nil {
				slog.Warn("tool window observe: checkpoint gap", "repo", repoPath, "err", cerr)
			}
		}
		return true
	case errors.Is(err, toolsnap.ErrWindowSealed), errors.Is(err, toolsnap.ErrWindowTombstoned):
		// Registry-terminal states; the sweep reclaims the window.
		return true
	default:
		slog.Warn("tool window observe: complete candidate", "repo", repoPath, "err", err)
		return false
	}
}

// toolWindowDisposition is completeToolWindow's outcome for the caller.
type toolWindowDisposition int

const (
	// windowPassthrough lets the caller route events normally.
	windowPassthrough toolWindowDisposition = iota
	// windowHandled means the closing events were persisted.
	windowHandled
	// windowSuppressed prevents routing to an unverified repository.
	windowSuppressed
)

// completeToolWindow finalizes a Bash window and tells the caller whether to
// stop, continue, or suppress event routing.
func completeToolWindow(ctx context.Context, providerName string, event *Event, bh *broker.Handle, globalBlobs *blobs.Store, events []broker.RawEvent) toolWindowDisposition {
	// Command-directory providers suppress events when completion fails.
	failDisp := windowPassthrough
	if effectiveDirProvider(providerName) {
		failDisp = windowSuppressed
	}
	if len(events) == 0 || event.ToolUseID == "" {
		return failDisp
	}
	wctx, cancel := context.WithTimeout(ctx, toolWindowDeadline)
	defer cancel()
	start := time.Now()

	state, err := LoadCaptureState(event.SessionID)
	if err != nil {
		return failDisp
	}
	if event.TurnID == "" {
		event.TurnID = state.TurnID
	}
	// Complete frozen observations even if the gate changes or no primary resolves.
	defer completeObservedCandidates(ctx, providerName, event)
	// Command-directory providers complete the target selected by the pre hook.
	// Missing or invalid targets suppress routing to the session repository.
	var target *toolWindowTarget
	if effectiveDirProvider(providerName) {
		receiptKey := receiptKeyFor(providerName, event)
		defer func() {
			if derr := DeleteToolWindowTarget(receiptKey); derr != nil {
				slog.Warn("tool window: delete target", "err", derr)
			}
		}()
		rec, lerr := LoadToolWindowTarget(receiptKey)
		if lerr != nil {
			slog.Warn("tool window: load target", "err", lerr)
			return failDisp
		}
		if rec == nil {
			// Never replace a missing target with the session repository.
			slog.Warn("tool window: routing receipt missing", "tool_use", event.ToolUseID)
			return failDisp
		}
		// Verify the repository identity before using the persisted path.
		resolvedID, ridErr := broker.RepositoryIDForPath(wctx, rec.RepoPath)
		if ridErr != nil || resolvedID != rec.RepositoryID {
			slog.Warn("tool window: target identity mismatch", "repo", rec.RepoPath, "err", ridErr)
			return failDisp
		}
		target = &toolWindowTarget{
			repoPath:     rec.RepoPath,
			semDir:       filepath.Join(rec.RepoPath, ".semantica"),
			repositoryID: rec.RepositoryID,
		}
	} else {
		target, err = resolveToolWindowTarget(wctx, bh, toolWindowCWD(event, state))
		if err != nil || target == nil {
			return failDisp
		}
	}
	bench := &toolWindowBench{}
	var stages *toolsnap.StageTimes
	if doctor.BenchEnabled(target.repoPath) {
		wctx, stages = toolsnap.WithStageTimes(wctx)
	}
	scope := doctor.BenchScopeFrom(wctx)
	defer func() { emitToolWindowBench(target, event, "post", start, bench, stages, scope) }()

	key := toolsnap.ToolKey{
		RepositoryID: target.repositoryID,
		Provider:     strings.ReplaceAll(providerName, "-", "_"),
		SessionID:    event.SessionID,
		TurnID:       event.TurnID,
		ToolUseID:    event.ToolUseID,
	}

	stopSetup := toolsnap.MeasureStage(wctx, "snapshot_store_setup")
	rc, err := toolsnap.ResolveRepoContext(wctx, target.repoPath)
	if err != nil {
		stopSetup()
		bench.outcome = "error"
		slog.Warn("tool window: resolve repository", "err", err)
		return failDisp
	}
	store, err := toolsnap.OpenStore(wctx, rc, target.semDir)
	if err != nil {
		stopSetup()
		bench.outcome = "error"
		slog.Warn("tool window: open snapshot store", "err", err)
		return failDisp
	}
	reg, err := toolsnap.OpenRegistry(target.semDir)
	if err != nil {
		stopSetup()
		bench.outcome = "error"
		slog.Warn("tool window: open registry", "err", err)
		return failDisp
	}
	stopSetup()

	// A tombstone prevents a late post hook from reading newer changes.
	if tombstoned, err := reg.HasTombstone(key); err != nil || tombstoned {
		if tombstoned {
			bench.outcome = "tombstoned"
			util.AppendActivityLog(target.semDir,
				"tool-window post skipped (tombstoned): tool_use=%s", event.ToolUseID)
		} else {
			bench.outcome = "error"
		}
		return failDisp
	}

	eventID, ok := closingEventID(events, event.ToolUseID)
	if !ok {
		bench.outcome = "error"
		slog.Warn("tool window: no hook event for closing tool use", "tool_use", event.ToolUseID)
		return failDisp
	}
	repoBlobs, err := blobs.NewStore(filepath.Join(target.semDir, "objects"))
	if err != nil {
		bench.outcome = "error"
		slog.Warn("tool window: open repo blob store", "err", err)
		return failDisp
	}

	if isBackgroundBash(event) {
		// Record the detached command as a partial without file evidence.
		bench.outcome = "background_command"
		return partialDisposition(persistPartialDelta(wctx, reg, repoBlobs, target, key, event, eventID, toolsnap.ReasonBackgroundCommand, events, globalBlobs), failDisp)
	}

	info := toolsnap.CompletionInfo{
		At: event.Timestamp, EventID: eventID,
		CommandSummary: commandSummary(event.ToolInput),
	}
	writeEvents := func(toolsnap.PendingToolSnapshot) error {
		stop := toolsnap.MeasureStage(wctx, "persist_events")
		_, err := broker.WriteEventsToRepo(wctx, target.repoPath, events, globalBlobs)
		stop()
		return err
	}
	cleanupRefs := map[string]string{}
	closed, err := reg.Complete(wctx, key, info, writeEvents,
		func(members []toolsnap.PendingToolSnapshot, prior *toolsnap.GroupFinal, retry bool, recordIntent func() error) (toolsnap.FinalizeResult, error) {
			return finalizeGroup(wctx, store, rc.HeadAnchor(), repoBlobs, target, key, info, event, events, globalBlobs, members, prior, retry, recordIntent, cleanupRefs, bench)
		})
	switch {
	case err == nil:
		if closed {
			if bench.partialReason != "" {
				bench.outcome = "closed_partial"
			} else {
				bench.outcome = "closed_complete"
			}
			// Release refs only after registry closure is durable.
			stopRel := toolsnap.MeasureStage(wctx, "ref_release")
			releaseGroupRefs(wctx, reg, store, cleanupRefs)
			stopRel()
		} else {
			bench.outcome = "non_final"
		}
		return windowHandled
	case errors.Is(err, toolsnap.ErrNoPendingSnapshot):
		bench.outcome = "missing_pre"
		bench.partialReason = "pre_snapshot_missing"
		// Preserve missing-pre evidence without blocking the ordinary write path.
		return partialDisposition(persistPartialDelta(wctx, reg, repoBlobs, target, key, event, eventID, "pre_snapshot_missing", events, globalBlobs), failDisp)
	case errors.Is(err, toolsnap.ErrWindowTombstoned):
		bench.outcome = "tombstoned"
		// Preserve the event without creating evidence for an abandoned window.
		util.AppendActivityLog(target.semDir,
			"tool-window post skipped (tombstoned): tool_use=%s", event.ToolUseID)
		return failDisp
	case errors.Is(err, toolsnap.ErrWindowSealed):
		bench.outcome = "sealed"
		// Reclamation records partial evidence without reading the workspace.
		util.AppendActivityLog(target.semDir,
			"tool-window post sealed (join horizon passed): tool_use=%s", event.ToolUseID)
		return failDisp
	case err != nil:
		var pe *toolsnap.PartialError
		if errors.As(err, &pe) && pe.Reason == toolsnap.ReasonLockTimeout {
			bench.outcome = "lock_timeout"
			bench.partialReason = pe.Reason
			// Prevent a later post hook from capturing newer changes.
			if terr := reg.WriteTombstone(key, event.Timestamp); terr != nil {
				slog.Warn("tool window: tombstone", "err", terr)
			}
			util.AppendActivityLog(target.semDir,
				"tool-window post lock timeout: tool_use=%s tombstoned", event.ToolUseID)
			return failDisp
		}
		bench.outcome = "error"
		slog.Warn("tool window: complete", "tool_use", event.ToolUseID, "err", err)
		return failDisp
	}
	return failDisp
}

// partialDisposition marks a stored partial as handled.
func partialDisposition(persisted bool, failDisp toolWindowDisposition) toolWindowDisposition {
	if persisted {
		return windowHandled
	}
	return failDisp
}

// finalizeGroup persists a group's events, delta, and evidence links.
// Retries use durable state, and refs remain until registry closure succeeds.
func finalizeGroup(ctx context.Context, store *toolsnap.Store, postAnchor toolsnap.HeadAnchor, repoBlobs *blobs.Store, target *toolWindowTarget, key toolsnap.ToolKey, info toolsnap.CompletionInfo, event *Event, events []broker.RawEvent, globalBlobs *blobs.Store, members []toolsnap.PendingToolSnapshot, prior *toolsnap.GroupFinal, retry bool, recordIntent func() error, cleanupRefs map[string]string, bench *toolWindowBench) (toolsnap.FinalizeResult, error) {
	groupID := members[0].GroupID
	earliest := members[0]
	bench.groupMembers = len(members)

	// Resume evidence publication from the stored delta without reading the workspace.
	if prior != nil && prior.DeltaHash != "" {
		stopLinks := toolsnap.MeasureStage(ctx, "persist_evidence_links")
		err := persistEvidenceLinks(ctx, target, members, prior.DeltaHash, info.At)
		stopLinks()
		if err != nil {
			return toolsnap.FinalizeResult{Final: *prior}, err
		}
		collectGroupRefs(cleanupRefs, store, members, groupID, prior.PostTreeHash)
		return toolsnap.FinalizeResult{Done: true}, nil
	}

	d := captureGroupDelta(ctx, store, earliest, groupID, postAnchor, prior, retry, event.Timestamp)

	// Record completion before other writes so a crash cannot strand the
	// member as active. Failure is non-fatal because capture can continue.
	if err := recordIntent(); err != nil {
		slog.Warn("tool window: finalization intent", "tool_use", key.ToolUseID, "err", err)
	}

	// Events must exist before any group identity that references them.
	stopEvents := toolsnap.MeasureStage(ctx, "persist_events")
	_, evErr := broker.WriteEventsToRepo(ctx, target.repoPath, events, globalBlobs)
	stopEvents()
	if evErr != nil {
		return toolsnap.FinalizeResult{}, evErr
	}

	ensureGroupPostRef(ctx, store, groupID, prior, &d)

	bench.filesChanged, bench.bytesRead, bench.partialReason = len(d.files), d.bytesRead, d.partialReason
	deltaHash, err := storeGroupDelta(ctx, repoBlobs, members, d)
	if err != nil {
		return toolsnap.FinalizeResult{Final: finalIdentity(d.postTree, "", d.partialReason, d.capturedAt)}, err
	}
	// Evidence links must be durable before the group can close.
	stopLinks := toolsnap.MeasureStage(ctx, "persist_evidence_links")
	linkErr := persistEvidenceLinks(ctx, target, members, deltaHash, info.At)
	stopLinks()
	if linkErr != nil {
		return toolsnap.FinalizeResult{Final: finalIdentity(d.postTree, deltaHash, d.partialReason, d.capturedAt)}, linkErr
	}
	collectGroupRefs(cleanupRefs, store, members, groupID, d.postTree)
	return toolsnap.FinalizeResult{Done: true}, nil
}

// finalizeObserveGroup stores an observation delta without publishing events or
// evidence links. It returns the closure identity and collects refs for cleanup.
func finalizeObserveGroup(ctx context.Context, store *toolsnap.Store, postAnchor toolsnap.HeadAnchor, repoBlobs *blobs.Store, members []toolsnap.PendingToolSnapshot, prior *toolsnap.GroupFinal, retry bool, recordIntent func() error, cleanupRefs map[string]string, event *Event) (toolsnap.FinalizeResult, error) {
	groupID := members[0].GroupID
	earliest := members[0]

	// Reuse the stored delta without recapturing the workspace.
	if prior != nil && prior.DeltaHash != "" {
		collectGroupRefs(cleanupRefs, store, members, groupID, prior.PostTreeHash)
		return toolsnap.FinalizeResult{Done: true, Final: *prior}, nil
	}

	if err := recordIntent(); err != nil {
		slog.Warn("tool window: observe finalization intent", "group", groupID, "err", err)
	}

	// Store the delta before closing the group and releasing refs.
	d := captureGroupDelta(ctx, store, earliest, groupID, postAnchor, prior, retry, event.Timestamp)
	ensureGroupPostRef(ctx, store, groupID, prior, &d)
	deltaHash, err := storeGroupDelta(ctx, repoBlobs, members, d)
	if err != nil {
		return toolsnap.FinalizeResult{Final: finalIdentity(d.postTree, "", d.partialReason, d.capturedAt)}, err
	}
	collectGroupRefs(cleanupRefs, store, members, groupID, d.postTree)
	// Partial observations also need a retrievable blob identity.
	return toolsnap.FinalizeResult{Done: true, Final: observeFinal(d.postTree, deltaHash, d.partialReason, d.capturedAt)}, nil
}

// ensureGroupPostRef ensures the post-tree ref when d has a post tree and prior
// finalization has none recorded. Failure marks d partial and clears file evidence.
func ensureGroupPostRef(ctx context.Context, store *toolsnap.Store, groupID string, prior *toolsnap.GroupFinal, d *groupDelta) {
	if d.postTree == "" || (prior != nil && prior.PostTreeHash != "") {
		return
	}
	stopRef := toolsnap.MeasureStage(ctx, "ref_publish")
	refErr := store.EnsureRef(ctx, toolsnap.GroupPostRef(store.WorktreeID(), groupID), d.postTree)
	stopRef()
	if refErr != nil {
		d.partialReason = toolsnap.ReasonAlternateGone
		d.postTree = ""
		d.files, d.bytesRead, d.truncated = nil, 0, false
	}
}

// storeGroupDelta stores the canonical group delta and returns its hash.
// It does not publish events or evidence links.
func storeGroupDelta(ctx context.Context, repoBlobs *blobs.Store, members []toolsnap.PendingToolSnapshot, d groupDelta) (string, error) {
	delta := assembleDelta(members, d.files, d.bytesRead, d.truncated, d.partialReason, d.capturedAt)
	canonical, err := delta.CanonicalBytes()
	if err != nil {
		return "", err
	}
	stopBlob := toolsnap.MeasureStage(ctx, "persist_delta_blob")
	deltaHash, _, err := repoBlobs.Put(ctx, canonical)
	stopBlob()
	if err != nil {
		return "", err
	}
	return deltaHash, nil
}

// groupDelta is the file-change result for a tool-window group, produced
// without any event or evidence-link publication.
type groupDelta struct {
	files         []toolsnap.FileDelta
	bytesRead     int64
	truncated     bool
	partialReason string
	postTree      string
	capturedAt    int64
}

// captureGroupDelta computes or resumes a group delta without publishing events
// or evidence links. The caller handles prior.DeltaHash before calling.
func captureGroupDelta(ctx context.Context, store *toolsnap.Store, earliest toolsnap.PendingToolSnapshot, groupID string, postAnchor toolsnap.HeadAnchor, prior *toolsnap.GroupFinal, retry bool, defaultCapturedAt int64) groupDelta {
	d := groupDelta{capturedAt: defaultCapturedAt}
	switch {
	case prior != nil && prior.PartialReason != "":
		d.partialReason = prior.PartialReason
		d.capturedAt = prior.CapturedAt
	case prior != nil && prior.PostTreeHash != "":
		d.postTree = prior.PostTreeHash
		d.capturedAt = prior.CapturedAt
		captureStage("delta_between")
		d.files, d.bytesRead, d.truncated, _ = deltaOrPartial(ctx, store, earliest.TreeHash, d.postTree, &d.partialReason)
	default:
		// A post ref preserves a capture whose registry update failed. Store
		// errors degrade to partial evidence rather than recapture.
		postRefName := toolsnap.GroupPostRef(store.WorktreeID(), groupID)
		tree, found, refErr := existingRefTarget(ctx, store, postRefName)
		switch {
		case refErr != nil:
			d.partialReason = toolsnap.ReasonStoreUnavailable
		case found:
			d.postTree = tree
			captureStage("delta_between")
			d.files, d.bytesRead, d.truncated, _ = deltaOrPartial(ctx, store, earliest.TreeHash, d.postTree, &d.partialReason)
		case retry:
			// The original post state is gone; do not capture newer changes.
			d.partialReason = toolsnap.ReasonPostSnapshotLost
		default:
			captureStage("capture_after")
			res, err := store.CaptureAfter(ctx, toolsnap.Snapshot{TreeHash: earliest.TreeHash, HeadHash: earliest.HeadHash}, postAnchor)
			var pe *toolsnap.PartialError
			switch {
			case err == nil:
				d.postTree = res.Post.TreeHash
				d.files, d.bytesRead, d.truncated = res.Files, res.BytesRead, res.Truncated
			case errors.As(err, &pe):
				// Preserve the stable partial reason.
				d.partialReason = pe.Reason
			default:
				// An unknown capture failure cannot be retried against new state.
				d.partialReason = toolsnap.ReasonTimeout
			}
		}
	}
	return d
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

// toolWindowRefReleaseWait bounds post-closure locking and ref deletion before
// maintenance takes over.
var toolWindowRefReleaseWait = 250 * time.Millisecond

// releaseGroupRefs removes unchanged refs under the registry coordination lock
// so a concurrent inspection or maintenance pass sees a consistent ref set.
// It is best-effort and bounded: leftovers are handled by maintenance.
func releaseGroupRefs(ctx context.Context, reg *toolsnap.Registry, store *toolsnap.Store, refs map[string]string) {
	if len(refs) == 0 {
		return
	}
	lockCtx, cancel := context.WithTimeout(ctx, toolWindowRefReleaseWait)
	defer cancel()
	if err := reg.WithCoordinationLock(lockCtx, func() error {
		// Conflicts leave the full batch for maintenance.
		if err := store.DeleteRefs(lockCtx, refs); err != nil {
			slog.Warn("tool window: release refs", "err", err)
		}
		return nil
	}); err != nil {
		slog.Warn("tool window: release refs deferred to maintenance", "err", err)
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

// toolWindowCaptureSeam records capture operations in tests.
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

// observeFinal preserves the blob identity for complete and partial observations.
func observeFinal(postTree, deltaHash, partialReason string, capturedAt int64) toolsnap.GroupFinal {
	return toolsnap.GroupFinal{
		PostTreeHash:  postTree,
		DeltaHash:     deltaHash,
		PartialReason: partialReason,
		CapturedAt:    capturedAt,
	}
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

// isBackgroundBash reports whether Bash was launched with run_in_background.
func isBackgroundBash(event *Event) bool {
	if event == nil || event.ToolName != "Bash" {
		return false
	}
	var in struct {
		RunInBackground bool `json:"run_in_background"`
	}
	if json.Unmarshal(event.ToolInput, &in) != nil {
		return false
	}
	return in.RunInBackground
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
