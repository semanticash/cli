package hooks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/agents/api"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/doctor"
	"github.com/semanticash/cli/internal/provenance"
	"github.com/semanticash/cli/internal/store/blobs"
	"github.com/semanticash/cli/internal/util"
)

type captureTimestampKeyType struct{}
type modelKeyType struct{}
type hookEventTypeKeyType struct{}
type hookTimestampKeyType struct{}
type cwdKeyType struct{}
type tokenUsageKeyType struct{}

// CaptureTimestampKey carries the capture state's unix-ms timestamp into
// ReadFromOffset for turn-scoped enrichment.
var CaptureTimestampKey = captureTimestampKeyType{}

// ModelKey carries the hook event's model name into ReadFromOffset.
var ModelKey = modelKeyType{}

// HookEventTypeKey carries the current lifecycle event type into provider
// transcript preparation.
var HookEventTypeKey = hookEventTypeKeyType{}

// HookTimestampKey carries the current hook timestamp into provider transcript
// preparation.
var HookTimestampKey = hookTimestampKeyType{}

// CWDKey carries the working directory from capture state into ReadFromOffset
// for providers whose transcripts don't embed a project path.
var CWDKey = cwdKeyType{}

// TokenUsageKey carries provider-reported turn totals into transcript replay.
var TokenUsageKey = tokenUsageKeyType{}

// ModelFromContext extracts the model name from the context, or "" if absent.
func ModelFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ModelKey).(string); ok {
		return v
	}
	return ""
}

// TokenUsageFromContext returns provider-reported turn totals when available.
func TokenUsageFromContext(ctx context.Context) (TokenUsage, bool) {
	usage, ok := ctx.Value(TokenUsageKey).(TokenUsage)
	return usage, ok
}

// CWDFromContext extracts the working directory from the context, or "" if absent.
func CWDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(CWDKey).(string); ok {
		return v
	}
	return ""
}

// HookEventTypeFromContext extracts the current lifecycle event type from the
// context, or false if absent.
func HookEventTypeFromContext(ctx context.Context) (EventType, bool) {
	v, ok := ctx.Value(HookEventTypeKey).(EventType)
	return v, ok
}

// HookTimestampFromContext extracts the current hook timestamp in unix-ms, or
// 0 if absent.
func HookTimestampFromContext(ctx context.Context) int64 {
	if v, ok := ctx.Value(HookTimestampKey).(int64); ok {
		return v
	}
	return 0
}

// Dispatch routes a normalized hook event.
func Dispatch(ctx context.Context, provider HookProvider, event *Event, bh *broker.Handle, blobStore *blobs.Store) error {
	switch event.Type {
	case PromptSubmitted:
		benchCtx, benchScope := doctor.WithBenchScope(ctx)
		hookStart := time.Now()

		offset, err := provider.TranscriptOffset(benchCtx, event.TranscriptRef)
		if err != nil {
			return fmt.Errorf("transcript offset: %w", err)
		}
		turnID := uuid.NewString()
		event.TurnID = turnID

		newState := &CaptureState{
			SessionID:         event.SessionID,
			Provider:          provider.Name(),
			TranscriptRef:     event.TranscriptRef,
			TranscriptOffset:  offset,
			Timestamp:         event.Timestamp,
			TurnID:            turnID,
			PromptSubmittedAt: event.Timestamp,
			CWD:               event.CWD,
			TurnStartOffset:   offset,
		}
		// Keep unresolved transcript data behind the current EOF.
		if prev, perr := LoadCaptureState(event.SessionID); perr == nil {
			switch {
			case prev.TranscriptRef == event.TranscriptRef && prev.TranscriptOffset < offset:
				// Resume replay at the oldest unresolved boundary.
				newState.TranscriptOffset = prev.TranscriptOffset
				newState.PendingTurns = prev.PendingTurns
				if prev.TurnID != "" {
					newState.PendingTurns = append(newState.PendingTurns, PendingTurnBoundary{
						TurnID:            prev.TurnID,
						PromptSubmittedAt: prev.PromptSubmittedAt,
						StartOffset:       prev.TurnStartOffset,
					})
				}
				newState.ScopedDeferrals = prev.ScopedDeferrals
				newState.LastDeferredAt = prev.LastDeferredAt
			case prev.TranscriptRef != event.TranscriptRef && prev.ScopedDeferrals > 0:
				// Preserve the deferred segment before replacing its
				// active state with the new transcript.
				orphan := *prev
				orphan.StateKey = orphan.SessionID + ".orphan." + uuid.NewString()
				orphan.OrphanedAt = time.Now().UnixMilli()
				if serr := SaveCaptureState(&orphan); serr != nil {
					return fmt.Errorf("preserve deferred capture segment: %w", serr)
				}
			}
		}
		if err := SaveCaptureState(newState); err != nil {
			return err
		}

		// Emit direct prompt event if the provider supports it.
		if emitter, ok := provider.(DirectHookEmitter); ok {
			var bs api.BlobPutter
			if blobStore != nil {
				bs = blobStore
			}
			events, err := emitter.BuildHookEvents(benchCtx, event, bs)
			if err != nil {
				slog.Warn("direct prompt event failed", "err", err)
			} else if len(events) > 0 {
				if err := routeAndWriteEvents(benchCtx, events, bh, blobStore); err != nil {
					slog.Warn("direct prompt write failed", "err", err)
				}
			}
		}

		emitHookBenchRecords(benchScope, event, time.Since(hookStart))

		return nil

	case AgentResponseCaptured:
		// Store the response on the active turn for the later completion hook.
		if event.Response == nil {
			return nil
		}
		state, err := LoadCaptureState(event.SessionID)
		if err != nil || state.Provider != provider.Name() {
			return nil
		}
		applyResponseCandidate(ctx, blobStore, state, event)
		return SaveCaptureState(state)

	case AgentCompleted:
		benchCtx, benchScope := doctor.WithBenchScope(ctx)

		// Snapshot the current turn context before capture advances the offset.
		preState, _ := LoadCaptureState(event.SessionID)

		// Save completion-hook responses before capture so retries retain them.
		if event.Response != nil && preState != nil && preState.Provider == provider.Name() {
			applyResponseCandidate(benchCtx, blobStore, preState, event)
			if err := SaveCaptureState(preState); err != nil {
				slog.Warn("persist response candidate failed", "err", err)
			}
		}
		if preState != nil && preState.Provider == provider.Name() {
			changed, conflict := freezeTurnTokenUsage(preState, event.TokenUsage)
			if conflict {
				slog.Warn("capture: preserving existing turn token usage", "turn", preState.TurnID)
			}
			event.TokenUsage = cloneCapturedTokenUsage(preState.TokenUsage)
			if changed {
				if err := SaveCaptureState(preState); err != nil {
					return fmt.Errorf("save turn token usage: %w", err)
				}
			}
		}

		captureStart := time.Now()
		if err := CaptureAndRoute(benchCtx, provider, event, bh, blobStore); err != nil {
			return err
		}
		finalSubagentSweepAndCleanup(benchCtx, provider, event, bh, blobStore)
		captureDuration := time.Since(captureStart)

		// Package the turn artifacts after capture succeeds.
		// This must happen before DeleteCaptureState because packaging
		// reloads the saved state to get the post-capture transcript offset.
		packageDuration := time.Duration(0)
		if preState != nil && preState.TurnID != "" {
			packageStart := time.Now()
			packageTurnFromState(benchCtx, provider, event, bh, blobStore, preState)
			packageDuration = time.Since(packageStart)
			emitTurnBenchRecords(benchScope, preState.TurnID, captureDuration, packageDuration)
		}

		return DeleteCaptureState(event.SessionID)

	case SubagentCompleted:
		// Providers report subagent activity either through the parent
		// session or through a direct hook from the child session.
		if state, err := LoadCaptureState(event.SessionID); err == nil {
			if emitter, ok := provider.(DirectHookEmitter); ok {
				if event.TurnID == "" {
					event.TurnID = state.TurnID
				}
				var bs api.BlobPutter
				if blobStore != nil {
					bs = blobStore
				}
				events, buildErr := emitter.BuildHookEvents(ctx, event, bs)
				if buildErr != nil {
					slog.Warn("subagent direct event failed", "err", buildErr)
				} else if len(events) > 0 {
					if routeErr := routeAndWriteEvents(ctx, events, bh, blobStore); routeErr != nil {
						slog.Warn("subagent direct write failed", "err", routeErr)
					}
				}
			}
			if err := CaptureAndRoute(ctx, provider, event, bh, blobStore); err != nil {
				slog.Warn("subagent: parent capture failed", "err", err)
			}
			captureSubagentTranscripts(ctx, provider, event, bh, blobStore)
		} else {
			// No parent state means the provider delivered a direct
			// subagent hook from the child session.
			// Read the subagent's own transcript from its saved offset
			// (or from 0 if first encounter).
			captureDirectSubagent(ctx, provider, event, bh, blobStore)
		}
		return nil

	case IncrementalCapture:
		// Mid-turn trigger: scan the transcript from the saved offset and
		// route any new events without running turn-end cleanup. Used by
		// providers that fire per-edit hooks (e.g. Kiro IDE fileEdited)
		// to land events during the turn instead of waiting for stop.
		// agentStop is still expected later as a final sweep.
		if _, err := LoadCaptureState(event.SessionID); err != nil {
			// No capture state means PromptSubmitted has not pinned a turn
			// yet; nothing to capture incrementally.
			return nil
		}
		if err := CaptureAndRoute(ctx, provider, event, bh, blobStore); err != nil {
			slog.Warn("incremental capture failed", "err", err)
		}
		return nil

	case ContextCompacted:
		// Compaction can invalidate saved offsets. Reset to EOF and accept a gap.
		newOffset, err := provider.TranscriptOffset(ctx, event.TranscriptRef)
		if err != nil {
			slog.Warn("compaction: transcript offset failed", "err", err)
			return nil
		}
		if state, err := LoadCaptureState(event.SessionID); err == nil {
			state.TranscriptOffset = newOffset
			state.Timestamp = time.Now().UnixMilli()
			if err := SaveCaptureState(state); err != nil {
				slog.Warn("compaction: save capture state failed", "err", err)
			}
		}
		return nil

	case SessionClosed:
		benchCtx, benchScope := doctor.WithBenchScope(ctx)

		// If the final completion hook was missed, try one last capture.
		preState, _ := LoadCaptureState(event.SessionID)
		if preState != nil {
			captureStart := time.Now()
			if err := CaptureAndRoute(benchCtx, provider, event, bh, blobStore); err == nil {
				finalSubagentSweepAndCleanup(benchCtx, provider, event, bh, blobStore)
				captureDuration := time.Since(captureStart)
				packageDuration := time.Duration(0)

				if preState.TurnID != "" {
					// Keep packaging before DeleteCaptureState so the final
					// transcript reference and turn metadata are still available.
					packageStart := time.Now()
					packageTurnFromState(benchCtx, provider, event, bh, blobStore, preState)
					packageDuration = time.Since(packageStart)
					emitTurnBenchRecords(benchScope, preState.TurnID, captureDuration, packageDuration)
				}

				if err := DeleteCaptureState(event.SessionID); err != nil {
					slog.Warn("delete capture state failed", "session", event.SessionID, "err", err)
				}
			} else {
				slog.Warn("session close: final capture failed", "session", event.SessionID, "err", err)
			}
			// On failure, keep parent and subagent state so the next pass can retry.
		}
		return nil

	case ToolStepStarted:
		benchCtx, benchScope := doctor.WithBenchScope(ctx)
		hookStart := hookStartTime(ctx)
		err := handleToolStepStarted(benchCtx, provider.Name(), event, bh)
		emitHookBenchRecords(benchScope, event, time.Since(hookStart))
		return err

	case ToolStepCompleted:
		benchCtx, benchScope := doctor.WithBenchScope(ctx)
		hookStart := hookStartTime(ctx)

		// Handle state-changing PostToolUse events (Write, Edit, Bash).
		// Route the hook-derived records without advancing transcript state.
		state, err := LoadCaptureState(event.SessionID)
		if errors.Is(err, ErrNoCaptureState) {
			// No active turn; nothing to record for this hook.
			return nil
		}
		if err != nil {
			return fmt.Errorf("load capture state: %w", err)
		}
		emitter, ok := provider.(DirectHookEmitter)
		if !ok {
			return nil
		}
		// Resolve turn from capture state.
		if event.TurnID == "" {
			event.TurnID = state.TurnID
		}
		var bs api.BlobPutter
		if blobStore != nil {
			bs = blobStore
		}
		events, err := emitter.BuildHookEvents(benchCtx, event, bs)
		if err != nil {
			slog.Warn("direct step event failed", "tool", event.ToolName, "err", err)
			emitHookBenchRecords(benchScope, event, time.Since(hookStart))
			return nil
		}
		if len(events) == 0 {
			return nil
		}
		// Bash completion may persist events while closing its tool window.
		if event.ToolName == "Bash" {
			switch completeToolWindow(benchCtx, provider.Name(), event, bh, blobStore, events) {
			case windowHandled:
				emitHookBenchRecords(benchScope, event, time.Since(hookStart))
				return nil
			case windowSuppressed:
				// Do not route an unresolved command to the session repository.
				slog.Warn("tool window: suppressed event after failed completion",
					"provider", provider.Name(), "tool_use", event.ToolUseID)
				emitHookBenchRecords(benchScope, event, time.Since(hookStart))
				return nil
			}
		}
		err = routeAndWriteEvents(benchCtx, events, bh, blobStore)
		if err != nil {
			slog.Warn("direct step write failed", "tool", event.ToolName, "err", err)
		}
		emitHookBenchRecords(benchScope, event, time.Since(hookStart))
		return nil

	case SubagentPromptSubmitted:
		benchCtx, benchScope := doctor.WithBenchScope(ctx)
		hookStart := time.Now()

		// Record the subagent prompt from PreToolUse[Agent].
		// Route the hook-derived records without advancing transcript state.
		state, err := LoadCaptureState(event.SessionID)
		if errors.Is(err, ErrNoCaptureState) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("load capture state: %w", err)
		}
		emitter, ok := provider.(DirectHookEmitter)
		if !ok {
			return nil
		}
		if event.TurnID == "" {
			event.TurnID = state.TurnID
		}
		var bs api.BlobPutter
		if blobStore != nil {
			bs = blobStore
		}
		events, err := emitter.BuildHookEvents(benchCtx, event, bs)
		if err != nil {
			slog.Warn("subagent prompt direct event failed", "err", err)
			emitHookBenchRecords(benchScope, event, time.Since(hookStart))
			return nil
		}
		if len(events) == 0 {
			return nil
		}
		err = routeAndWriteEvents(benchCtx, events, bh, blobStore)
		if err != nil {
			slog.Warn("subagent prompt direct write failed", "err", err)
		}
		emitHookBenchRecords(benchScope, event, time.Since(hookStart))
		return nil

	case SessionOpened, SubagentSpawned:
		// Lightweight lifecycle tracking (metadata only, no transcript reading)
		return nil

	default:
		return nil
	}
}

// CaptureAndRoute reads a transcript delta, routes the resulting events, and
// advances the saved offset only after every repo write succeeds.
func CaptureAndRoute(ctx context.Context, provider HookProvider, event *Event, bh *broker.Handle, blobStore *blobs.Store) error {
	_, err := captureAndRouteScoped(ctx, provider, event, bh, blobStore, "")
	return err
}

// CaptureAndRouteForRepo routes only when every event belongs to repoRoot.
// A cross-repository result leaves the offset unchanged and returns false.
func CaptureAndRouteForRepo(ctx context.Context, provider HookProvider, event *Event, bh *broker.Handle, blobStore *blobs.Store, repoRoot string) (bool, error) {
	return captureAndRouteScoped(ctx, provider, event, bh, blobStore, repoRoot)
}

func captureAndRouteScoped(ctx context.Context, provider HookProvider, event *Event, bh *broker.Handle, blobStore *blobs.Store, scopeRepo string) (bool, error) {
	state, err := LoadCaptureState(event.SessionID)
	if errors.Is(err, ErrNoCaptureState) {
		// Start from the current end of the transcript rather than backfilling.
		offset, _ := provider.TranscriptOffset(ctx, event.TranscriptRef)
		return true, SaveCaptureState(&CaptureState{
			SessionID:        event.SessionID,
			Provider:         provider.Name(),
			TranscriptRef:    event.TranscriptRef,
			TranscriptOffset: offset,
			Timestamp:        time.Now().UnixMilli(),
		})
	}
	if err != nil {
		return false, fmt.Errorf("load capture state: %w", err)
	}

	readCtx := context.WithValue(ctx, CaptureTimestampKey, state.Timestamp)
	if event.Model != "" {
		readCtx = context.WithValue(readCtx, ModelKey, event.Model)
	}
	if event.TokenUsage != nil {
		readCtx = context.WithValue(readCtx, TokenUsageKey, *event.TokenUsage)
	}
	readCtx = context.WithValue(readCtx, HookEventTypeKey, event.Type)
	readCtx = context.WithValue(readCtx, HookTimestampKey, event.Timestamp)
	if cwd := state.CWD; cwd != "" {
		readCtx = context.WithValue(readCtx, CWDKey, cwd)
	}
	var bs api.BlobPutter
	if blobStore != nil {
		bs = blobStore
	}
	events, newOffset, err := readReplayEvents(readCtx, provider, state, bs)
	if err != nil {
		return false, fmt.Errorf("read from offset: %w", err)
	}
	if len(events) == 0 {
		state.TranscriptOffset = newOffset
		state.Timestamp = time.Now().UnixMilli()
		state.ScopedDeferrals = 0
		state.LastDeferredAt = 0
		state.PendingTurns = nil
		return true, SaveCaptureState(state)
	}

	// Prefer transcript boundaries; timestamps are a conservative fallback.
	if len(state.PendingTurns) > 0 && offsetsAuthoritative(provider) && canProbeOffsets(state, newOffset) {
		if err := assignTurnsByOffset(readCtx, provider, state, events); err != nil {
			slog.Warn("turn ownership probe failed; falling back to timestamps", "err", err)
			stampTurnIDs(events, state)
		}
	} else {
		stampTurnIDs(events, state)
	}

	repos, err := broker.ListActiveRepos(ctx, bh)
	if err != nil {
		return false, fmt.Errorf("list active repos: %w", err)
	}
	matches := computeEventRoutes(events, repos)
	if scopeRepo != "" {
		for _, m := range matches {
			if !sameRepoPath(m.Repo.Path, scopeRepo) {
				// Defer the whole session rather than split its events
				// across repositories under one repository lock.
				state.ScopedDeferrals++
				state.LastDeferredAt = time.Now().UnixMilli()
				if serr := SaveCaptureState(state); serr != nil {
					return false, fmt.Errorf("record scoped deferral: %w", serr)
				}
				return false, nil
			}
		}
	}
	if err := writeRoutedEvents(ctx, matches, blobStore); err != nil {
		return false, fmt.Errorf("route and write: %w", err)
	}

	state.TranscriptOffset = newOffset
	state.Timestamp = time.Now().UnixMilli()
	state.ScopedDeferrals = 0
	state.LastDeferredAt = 0
	state.PendingTurns = nil
	if err := SaveCaptureState(state); err != nil {
		return false, fmt.Errorf("save capture state: %w", err)
	}

	return true, nil
}

// maxOwnershipProbes bounds transcript rereads during turn recovery.
const maxOwnershipProbes = 8

func offsetsAuthoritative(provider HookProvider) bool {
	oar, ok := provider.(OffsetAuthoritativeReader)
	return ok && oar.OffsetReadsAuthoritative()
}

// canProbeOffsets rejects boundaries invalidated by transcript compaction.
func canProbeOffsets(state *CaptureState, transcriptEnd int) bool {
	if state.TurnStartOffset <= 0 || state.TurnStartOffset > transcriptEnd {
		return false
	}
	if len(state.PendingTurns) > maxOwnershipProbes {
		return false
	}
	last := -1
	for _, b := range state.PendingTurns {
		if b.StartOffset < 0 || b.StartOffset <= last {
			return false
		}
		last = b.StartOffset
	}
	return last < state.TurnStartOffset
}

// assignTurnsByOffset assigns ownership only after every boundary read succeeds.
func assignTurnsByOffset(ctx context.Context, provider HookProvider, state *CaptureState, events []broker.RawEvent) error {
	owners := map[string]string{}
	for _, ev := range events {
		if ev.TurnID == "" && len(state.PendingTurns) > 0 {
			owners[ev.EventID] = state.PendingTurns[0].TurnID
		}
	}

	type segment struct {
		turnID string
		start  int
	}
	var segs []segment
	for _, b := range state.PendingTurns[1:] {
		segs = append(segs, segment{b.TurnID, b.StartOffset})
	}
	segs = append(segs, segment{state.TurnID, state.TurnStartOffset})

	for _, sg := range segs {
		probe, _, err := provider.ReadFromOffset(ctx, state.TranscriptRef, sg.start, nil)
		if err != nil {
			return fmt.Errorf("probe offset %d: %w", sg.start, err)
		}
		member := make(map[string]bool, len(probe))
		for _, ev := range probe {
			member[ev.EventID] = true
		}
		for id := range owners {
			if member[id] {
				owners[id] = sg.turnID
			}
		}
	}

	for i := range events {
		if events[i].TurnID == "" {
			events[i].TurnID = owners[events[i].EventID]
		}
	}
	return nil
}

// stampTurnIDs leaves events unowned when timestamps cannot identify a turn.
func stampTurnIDs(events []broker.RawEvent, state *CaptureState) {
	if state.TurnID == "" && len(state.PendingTurns) == 0 {
		return
	}
	for i := range events {
		if events[i].TurnID != "" {
			continue
		}
		events[i].TurnID = turnOwnerByTime(events[i].Timestamp, state)
	}
}

func turnOwnerByTime(ts int64, state *CaptureState) string {
	if len(state.PendingTurns) == 0 {
		return state.TurnID
	}
	if ts <= 0 {
		return "" // ambiguous: no position, no time
	}
	if state.PromptSubmittedAt > 0 {
		if ts == state.PromptSubmittedAt {
			return "" // ambiguous: boundary collision
		}
		if ts > state.PromptSubmittedAt {
			return state.TurnID
		}
	}
	for i := len(state.PendingTurns) - 1; i >= 0; i-- {
		if ts == state.PendingTurns[i].PromptSubmittedAt {
			return "" // ambiguous: boundary collision
		}
		if ts > state.PendingTurns[i].PromptSubmittedAt {
			return state.PendingTurns[i].TurnID
		}
	}
	return "" // ambiguous: predates every known boundary
}

func sameRepoPath(a, b string) bool {
	return broker.PathBelongsToRepo(a, b) && broker.PathBelongsToRepo(b, a)
}

func readReplayEvents(ctx context.Context, provider HookProvider, state *CaptureState, bs api.BlobPutter) ([]broker.RawEvent, int, error) {
	if preparer, ok := provider.(TranscriptPreparer); ok {
		if err := preparer.PrepareTranscript(ctx, state.TranscriptRef); err != nil {
			slog.Warn("prepare transcript failed, proceeding anyway", "err", err)
		}
	}

	events, newOffset, err := provider.ReadFromOffset(ctx, state.TranscriptRef, state.TranscriptOffset, bs)
	if err != nil {
		return nil, state.TranscriptOffset, err
	}
	return events, newOffset, nil
}

// finalSubagentSweepAndCleanup removes only the child states that were
// captured successfully. If discovery aborts, all state is preserved.
func finalSubagentSweepAndCleanup(ctx context.Context, provider HookProvider, event *Event, bh *broker.Handle, blobStore *blobs.Store) {
	failedKeys, ran := captureSubagentTranscripts(ctx, provider, event, bh, blobStore)
	if !ran {
		return
	}
	parentRef := event.TranscriptRef
	var promptTime int64
	var parentCWD string
	if state, err := LoadCaptureState(event.SessionID); err == nil {
		parentRef = state.TranscriptRef
		promptTime = state.PromptSubmittedAt
		parentCWD = state.CWD
	}
	if parentCWD == "" {
		parentCWD = event.CWD
	}
	dctx := DiscoveryContext{
		Cwd:             parentCWD,
		PromptTime:      promptTime,
		StopTime:        time.Now().UnixMilli(),
		ParentSessionID: event.SessionID,
	}
	deleteSubagentCaptureStates(provider, parentRef, dctx, failedKeys)
}

// captureSubagentTranscripts reads each discovered child transcript from its
// own saved offset. ran=false means discovery aborted before any child ran.
func captureSubagentTranscripts(ctx context.Context, provider HookProvider, event *Event, bh *broker.Handle, blobStore *blobs.Store) (failedKeys []string, ran bool) {
	disc, ok := provider.(SubagentDiscoverer)
	if !ok {
		return nil, false
	}

	parentRef := event.TranscriptRef
	var turnStartedAt int64
	var parentCWD string
	var parentTurnID string
	if state, err := LoadCaptureState(event.SessionID); err == nil {
		parentRef = state.TranscriptRef
		turnStartedAt = state.PromptSubmittedAt
		parentCWD = state.CWD
		parentTurnID = state.TurnID
	}
	if parentCWD == "" {
		parentCWD = event.CWD
	}

	dctx := DiscoveryContext{
		Cwd:             parentCWD,
		PromptTime:      turnStartedAt,
		StopTime:        time.Now().UnixMilli(),
		ParentSessionID: event.SessionID,
	}

	paths, err := disc.DiscoverSubagentTranscripts(ctx, parentRef, dctx)
	if err != nil {
		slog.Warn("subagent discovery failed", "err", err)
		return nil, false
	}
	if len(paths) == 0 {
		return nil, true
	}

	var bs api.BlobPutter
	if blobStore != nil {
		bs = blobStore
	}

	var repos []broker.RegisteredRepo
	if bh != nil {
		repos, err = broker.ListActiveRepos(ctx, bh)
		if err != nil {
			slog.Warn("subagent capture: list active repos failed", "err", err)
			return nil, false
		}
	}

	for _, path := range paths {
		ok := captureOneSubagent(ctx, provider, disc, path, event.SessionID, parentTurnID, turnStartedAt, bs, blobStore, repos)
		if !ok {
			failedKeys = append(failedKeys, disc.SubagentStateKey(path))
		}
	}
	return failedKeys, true
}

// captureOneSubagent reads one child transcript and advances its offset only
// after all routed writes succeed. parentSessionID and parentTurnID are
// stamped onto child events that left those fields empty, so the lineage
// join works without each provider deriving parent context itself.
func captureOneSubagent(
	ctx context.Context,
	provider HookProvider,
	disc SubagentDiscoverer,
	transcriptPath string,
	parentSessionID string,
	parentTurnID string,
	turnStartedAt int64,
	bs api.BlobPutter,
	blobStore *blobs.Store,
	repos []broker.RegisteredRepo,
) bool {
	stateKey := disc.SubagentStateKey(transcriptPath)

	state, err := LoadCaptureStateByKey(stateKey)
	if errors.Is(err, ErrNoCaptureState) {
		initialOffset := 0
		if shouldSeedSubagentAtEOF(transcriptPath, turnStartedAt) {
			offset, offErr := provider.TranscriptOffset(ctx, transcriptPath)
			if offErr != nil {
				slog.Warn("subagent: transcript offset failed", "path", transcriptPath, "err", offErr)
				return false
			}
			initialOffset = offset
		}
		state = &CaptureState{
			SessionID:        parentSessionID,
			StateKey:         stateKey,
			Provider:         provider.Name(),
			TranscriptRef:    transcriptPath,
			TranscriptOffset: initialOffset,
			Timestamp:        time.Now().UnixMilli(),
		}
	} else if err != nil {
		slog.Warn("subagent: load state failed", "key", stateKey, "err", err)
		return false
	}

	if preparer, ok := provider.(TranscriptPreparer); ok {
		if err := preparer.PrepareTranscript(ctx, transcriptPath); err != nil {
			slog.Warn("subagent: prepare transcript failed", "path", transcriptPath, "err", err)
		}
	}

	// Read from subagent's own offset.
	events, newOffset, err := provider.ReadFromOffset(ctx, transcriptPath, state.TranscriptOffset, bs)
	if err != nil {
		slog.Warn("subagent: read failed", "path", transcriptPath, "err", err)
		return false
	}

	if len(events) == 0 {
		state.TranscriptOffset = newOffset
		state.Timestamp = time.Now().UnixMilli()
		if err := SaveCaptureState(state); err != nil {
			slog.Warn("subagent: save state failed", "key", stateKey, "err", err)
		}
		return true
	}

	for i := range events {
		if events[i].ParentSessionID == "" {
			events[i].ParentSessionID = parentSessionID
		}
		if events[i].TurnID == "" {
			events[i].TurnID = parentTurnID
		}
	}

	if err := routeAndWriteEventsToRepos(ctx, events, repos, blobStore); err != nil {
		slog.Warn("subagent: offset not advanced due to write failure", "key", stateKey)
		return false
	}

	state.TranscriptOffset = newOffset
	state.Timestamp = time.Now().UnixMilli()
	if err := SaveCaptureState(state); err != nil {
		slog.Warn("subagent: save state failed", "key", stateKey, "err", err)
		return false
	}
	return true
}

func shouldSeedSubagentAtEOF(transcriptPath string, turnStartedAt int64) bool {
	if turnStartedAt <= 0 {
		return false
	}
	info, err := os.Stat(transcriptPath)
	if err != nil {
		return false
	}
	return info.ModTime().UnixMilli() < turnStartedAt
}

// captureDirectSubagent handles providers whose subagents fire their own hooks.
func captureDirectSubagent(ctx context.Context, provider HookProvider, event *Event, bh *broker.Handle, blobStore *blobs.Store) {
	state, err := LoadCaptureState(event.SessionID)
	if errors.Is(err, ErrNoCaptureState) {
		state = &CaptureState{
			SessionID:        event.SessionID,
			Provider:         provider.Name(),
			TranscriptRef:    event.TranscriptRef,
			TranscriptOffset: 0,
			Timestamp:        time.Now().UnixMilli(),
		}
	} else if err != nil {
		slog.Warn("direct subagent: load state failed", "session", event.SessionID, "err", err)
		return
	}

	if preparer, ok := provider.(TranscriptPreparer); ok {
		if err := preparer.PrepareTranscript(ctx, state.TranscriptRef); err != nil {
			slog.Warn("direct subagent: prepare transcript failed", "err", err)
		}
	}

	var bs api.BlobPutter
	if blobStore != nil {
		bs = blobStore
	}

	events, newOffset, err := provider.ReadFromOffset(ctx, state.TranscriptRef, state.TranscriptOffset, bs)
	if err != nil {
		slog.Warn("direct subagent: read failed", "path", state.TranscriptRef, "err", err)
		return
	}

	if len(events) == 0 {
		state.TranscriptOffset = newOffset
		state.Timestamp = time.Now().UnixMilli()
		if err := SaveCaptureState(state); err != nil {
			slog.Warn("direct subagent: save state failed", "session", event.SessionID, "err", err)
		}
		return
	}

	var repos []broker.RegisteredRepo
	if bh != nil {
		repos, err = broker.ListActiveRepos(ctx, bh)
		if err != nil {
			slog.Warn("direct subagent: list active repos failed", "err", err)
			return
		}
	}

	if err := routeAndWriteEventsToRepos(ctx, events, repos, blobStore); err != nil {
		return
	}

	state.TranscriptOffset = newOffset
	state.Timestamp = time.Now().UnixMilli()
	if err := SaveCaptureState(state); err != nil {
		slog.Warn("direct subagent: save state failed", "session", event.SessionID, "err", err)
	}
}

// turnCWD returns the event CWD, falling back to the prompt-time capture state.
func turnCWD(event *Event, preState *CaptureState) string {
	if event.CWD != "" {
		return event.CWD
	}
	if preState != nil {
		return preState.CWD
	}
	return ""
}

// packageTurnFromState writes the packaged turn artifacts after capture succeeds.
func packageTurnFromState(ctx context.Context, provider HookProvider, event *Event, bh *broker.Handle, blobStore *blobs.Store, preState *CaptureState) {
	cwd := turnCWD(event, preState)
	if cwd == "" {
		return
	}

	repos, err := broker.ListActiveRepos(ctx, bh)
	if err != nil {
		return
	}
	var repoPath string
	var bestLen int
	for _, r := range repos {
		matchPath := r.CanonicalPath
		if matchPath == "" {
			matchPath = r.Path
		}
		if broker.PathBelongsToRepo(cwd, matchPath) && len(matchPath) > bestLen {
			repoPath = r.Path
			bestLen = len(matchPath)
		}
	}
	if repoPath == "" {
		return
	}

	tc := buildTurnContext(preState, event, provider.Name())
	// For providers that derive the session ID from the transcript path
	// (e.g., Gemini CLI uses the filename stem), resolve the provider session
	// ID using the same ReadFromOffset path so the DB lookup matches.
	if emitter, ok := provider.(interface {
		DeriveProviderSessionID(transcriptRef string) string
	}); ok {
		if derived := emitter.DeriveProviderSessionID(preState.TranscriptRef); derived != "" {
			tc.SessionID = derived
		}
	}
	prompt, promptErr := provenance.LoadTurnPrompt(ctx, repoPath, tc.Provider, tc.SessionID, tc.TurnID)
	if promptErr != nil {
		slog.Debug("provenance: load turn prompt failed", "repo", repoPath, "err", promptErr)
		prompt = provenance.PromptCandidate{}
	}
	tc.Prompt = prompt
	usage, usageState, usageErr := provenance.LoadTurnTokenUsage(ctx, repoPath, tc.Provider, tc.SessionID, tc.TurnID)
	if usageErr != nil {
		slog.Debug("provenance: load turn token usage failed", "repo", repoPath, "err", usageErr)
	} else {
		tc.TokenUsage = selectTurnTokenUsage(usage, usageState, preState.TokenUsage)
		if usageState == provenance.TurnTokenUsageInvalid {
			slog.Warn("provenance: invalid persisted turn token usage", "repo", repoPath, "turn", tc.TurnID)
		}
	}

	candidates, err := broker.ListAllRepos(ctx, bh)
	if err != nil {
		candidates = repos
	}
	targets := []string{repoPath}
	for _, repo := range candidates {
		if sameRepoPath(repo.Path, repoPath) {
			continue
		}
		if !repo.Active && (tc.StartedAt <= 0 || repo.DisabledAt == nil || *repo.DisabledAt < tc.StartedAt) {
			continue
		}
		if state := broker.CheckRepoState(ctx, repo.Path); state.Verdict != broker.RepoStateOK {
			continue
		}
		recorded, rerr := provenance.TurnRecorded(ctx, repo.Path, tc.Provider, tc.SessionID, tc.TurnID)
		if rerr != nil {
			slog.Debug("provenance: inspect turn repository failed", "repo", repo.Path, "err", rerr)
			continue
		}
		if recorded {
			targets = append(targets, repo.Path)
		}
	}
	provenance.PackageTurn(ctx, repoPath, tc, blobStore)
	if len(targets) == 1 {
		return
	}
	packageSource := blobStore
	if originStore, oerr := blobs.NewStore(filepath.Join(repoPath, ".semantica", "objects")); oerr == nil {
		packageSource = originStore
	}
	for _, target := range targets[1:] {
		provenance.PackageTurn(ctx, target, tc, packageSource)
	}
}

// freezeTurnTokenUsage keeps the first valid usage report for the active turn.
func freezeTurnTokenUsage(state *CaptureState, incoming *TokenUsage) (changed, conflict bool) {
	if state == nil || state.TurnID == "" || projectCapturedTokenUsage(incoming) == nil {
		return false, false
	}
	if state.TokenUsage == nil {
		usage := *incoming
		state.TokenUsage = &usage
		return true, false
	}
	return false, *state.TokenUsage != *incoming
}

// projectCapturedTokenUsage validates and converts captured usage.
func projectCapturedTokenUsage(usage *TokenUsage) *provenance.TurnTokenUsage {
	if usage == nil || usage.TokensIn < 0 || usage.TokensOut < 0 || usage.TokensCacheRead < 0 || usage.TokensCacheCreate < 0 {
		return nil
	}
	return &provenance.TurnTokenUsage{
		InputUncached: usage.TokensIn,
		Output:        usage.TokensOut,
		CacheRead:     usage.TokensCacheRead,
		CacheWrite:    usage.TokensCacheCreate,
	}
}

// cloneCapturedTokenUsage returns a copy of valid captured usage.
func cloneCapturedTokenUsage(usage *TokenUsage) *TokenUsage {
	if projectCapturedTokenUsage(usage) == nil {
		return nil
	}
	cloned := *usage
	return &cloned
}

// selectTurnTokenUsage uses captured usage only when persisted usage is absent.
func selectTurnTokenUsage(persisted *provenance.TurnTokenUsage, state provenance.TurnTokenUsageState, captured *TokenUsage) *provenance.TurnTokenUsage {
	switch state {
	case provenance.TurnTokenUsageValid:
		return persisted
	case provenance.TurnTokenUsageAbsent:
		return projectCapturedTokenUsage(captured)
	default:
		return nil
	}
}

func buildTurnContext(preState *CaptureState, event *Event, providerName string) provenance.TurnContext {
	return provenance.TurnContext{
		TurnID:        preState.TurnID,
		SessionID:     event.SessionID,
		Provider:      providerName,
		TranscriptRef: preState.TranscriptRef,
		StartedAt:     preState.PromptSubmittedAt,
		CompletedAt:   time.Now().UnixMilli(),
		CWD:           turnCWD(event, preState),
		ResponseCandidate: provenance.ResponseCandidate{
			Status:      preState.ResponseStatus,
			EventID:     preState.ResponseEventID,
			Hash:        preState.ResponseHash,
			Summary:     preState.ResponseSummary,
			CompletedAt: preState.ResponseCompletedAt,
		},
	}
}

// applyResponseCandidate stores a redacted response and indexes its metadata.
func applyResponseCandidate(ctx context.Context, blobStore *blobs.Store, state *CaptureState, event *Event) {
	if blobStore == nil || event.Response == nil {
		return
	}
	cand := provenance.RedactAndStoreResponse(ctx, blobStore, "", *event.Response, event.Timestamp)
	state.ResponseStatus = cand.Status
	state.ResponseHash = cand.Hash
	state.ResponseSummary = cand.Summary
	state.ResponseCompletedAt = cand.CompletedAt
	state.ResponseEventID = cand.EventID
}

// routeAndWriteEvents routes events to registered repos and writes them.
// Shared by transcript replay and direct hook event dispatch.
func routeAndWriteEvents(ctx context.Context, events []broker.RawEvent, bh *broker.Handle, blobStore *blobs.Store) error {
	repos, err := broker.ListActiveRepos(ctx, bh)
	if err != nil {
		return fmt.Errorf("list active repos: %w", err)
	}
	return routeAndWriteEventsToRepos(ctx, events, repos, blobStore)
}

func routeAndWriteEventsToRepos(ctx context.Context, events []broker.RawEvent, repos []broker.RegisteredRepo, blobStore *blobs.Store) error {
	return writeRoutedEvents(ctx, computeEventRoutes(events, repos), blobStore)
}

// computeEventRoutes maps events to their target repositories,
// including the no-file-path fallback via source project path.
func computeEventRoutes(events []broker.RawEvent, repos []broker.RegisteredRepo) []broker.RepoMatch {
	matches := broker.RouteEvents(events, repos)

	// Fallback: route events without file paths via source project path.
	var noPathEvents []broker.RawEvent
	var sourceProjectPath string
	for _, ev := range events {
		if len(ev.FilePaths) == 0 {
			noPathEvents = append(noPathEvents, ev)
			if sourceProjectPath == "" {
				sourceProjectPath = ev.SourceProjectPath
			}
		}
	}
	if len(noPathEvents) > 0 {
		if m := broker.RouteNoPathEvents(noPathEvents, repos, sourceProjectPath); m != nil {
			matches = append(matches, *m)
		}
	}
	return matches
}

func writeRoutedEvents(ctx context.Context, matches []broker.RepoMatch, blobStore *blobs.Store) error {
	var writeFailed bool
	for _, match := range matches {
		if _, err := broker.WriteEventsToRepo(ctx, match.Repo.Path, match.Events, blobStore); err != nil {
			// Confirmed-stale repo state is skipped here. Doctor and
			// status report the stale entry with cleanup guidance.
			var stale *broker.ErrRepoStale
			if errors.As(err, &stale) {
				slog.Debug("broker: skipping stale repo",
					"repo", match.Repo.Path, "reason", string(stale.Reason))
				continue
			}
			slog.Warn("write events to repo failed",
				"repo", match.Repo.Path, "events", len(match.Events), "err", err)
			// Other write failures may lose capture data, so record
			// them for doctor diagnostics.
			provider := ""
			if len(match.Events) > 0 {
				provider = match.Events[0].Provider
			}
			util.AppendHookError(provider, "broker-write",
				fmt.Sprintf("write events to repo %s failed (%d events): %v",
					match.Repo.Path, len(match.Events), err))
			writeFailed = true
		}
	}

	if writeFailed {
		return fmt.Errorf("one or more repo writes failed")
	}
	return nil
}

// deleteSubagentCaptureStates removes child state files except those
// explicitly preserved for retry.
func deleteSubagentCaptureStates(provider HookProvider, parentTranscriptRef string, dctx DiscoveryContext, failedKeys []string) {
	disc, ok := provider.(SubagentDiscoverer)
	if !ok {
		return
	}

	skip := make(map[string]bool, len(failedKeys))
	for _, k := range failedKeys {
		skip[k] = true
	}

	paths, err := disc.DiscoverSubagentTranscripts(context.Background(), parentTranscriptRef, dctx)
	if err != nil {
		return
	}

	for _, path := range paths {
		key := disc.SubagentStateKey(path)
		if skip[key] {
			continue
		}
		if err := DeleteCaptureStateByKey(key); err != nil {
			slog.Warn("subagent: delete stale state failed", "key", key, "err", err)
		}
	}
}

type hookStartKey struct{}

// WithHookStart attaches the timing origin for a hook invocation.
func WithHookStart(ctx context.Context, start time.Time) context.Context {
	return context.WithValue(ctx, hookStartKey{}, start)
}

func hookStartTime(ctx context.Context) time.Time {
	if start, ok := ctx.Value(hookStartKey{}).(time.Time); ok {
		return start
	}
	return time.Now()
}

func emitHookBenchRecords(scope *doctor.BenchScope, event *Event, duration time.Duration) {
	for repoPath, stats := range scope.Snapshot() {
		doctor.EmitBenchRecord(repoPath, doctor.BenchRecord{
			Kind:       "hook",
			Event:      benchEventName(event.Type),
			Tool:       event.ToolName,
			TurnID:     event.TurnID,
			SessionID:  event.SessionID,
			ToolUseID:  event.ToolUseID,
			DurationMS: doctor.Milliseconds(duration),
			DBMS:       doctor.Milliseconds(stats.DBDuration),
			BlobMS:     doctor.Milliseconds(stats.BlobDuration),
		})
	}
}

func emitTurnBenchRecords(scope *doctor.BenchScope, turnID string, captureDuration, packageDuration time.Duration) {
	for repoPath, stats := range scope.Snapshot() {
		doctor.EmitBenchRecord(repoPath, doctor.BenchRecord{
			Kind:         "turn",
			TurnID:       turnID,
			CaptureMS:    doctor.Milliseconds(captureDuration),
			PackageMS:    doctor.Milliseconds(packageDuration),
			RowsWritten:  stats.RowsWritten,
			BlobsWritten: stats.BlobsWritten,
			BytesWritten: stats.BytesWritten,
		})
	}
}

func benchEventName(eventType EventType) string {
	switch eventType {
	case PromptSubmitted:
		return "PromptSubmitted"
	case ToolStepStarted:
		return "ToolStepStarted"
	case ToolStepCompleted:
		return "ToolStepCompleted"
	case SubagentPromptSubmitted:
		return "SubagentPromptSubmitted"
	case AgentCompleted:
		return "AgentCompleted"
	case SessionClosed:
		return "SessionClosed"
	default:
		return "Unknown"
	}
}
