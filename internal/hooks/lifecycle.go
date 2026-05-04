package hooks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/agents/api"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/doctor"
	"github.com/semanticash/cli/internal/provenance"
	"github.com/semanticash/cli/internal/store/blobs"
)

type captureTimestampKeyType struct{}
type modelKeyType struct{}
type hookEventTypeKeyType struct{}
type hookTimestampKeyType struct{}
type cwdKeyType struct{}

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

// ModelFromContext extracts the model name from the context, or "" if absent.
func ModelFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ModelKey).(string); ok {
		return v
	}
	return ""
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

		if err := SaveCaptureState(&CaptureState{
			SessionID:         event.SessionID,
			Provider:          provider.Name(),
			TranscriptRef:     event.TranscriptRef,
			TranscriptOffset:  offset,
			Timestamp:         event.Timestamp,
			TurnID:            turnID,
			PromptSubmittedAt: event.Timestamp,
			CWD:               event.CWD,
		}); err != nil {
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

	case AgentCompleted:
		benchCtx, benchScope := doctor.WithBenchScope(ctx)

		// Snapshot the current turn context before capture advances the offset.
		preState, _ := LoadCaptureState(event.SessionID)

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
			packageTurnFromState(benchCtx, provider, event, bh, preState)
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
					packageTurnFromState(benchCtx, provider, event, bh, preState)
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

	case ToolStepCompleted:
		benchCtx, benchScope := doctor.WithBenchScope(ctx)
		hookStart := time.Now()

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
	state, err := LoadCaptureState(event.SessionID)
	if errors.Is(err, ErrNoCaptureState) {
		// Start from the current end of the transcript rather than backfilling.
		offset, _ := provider.TranscriptOffset(ctx, event.TranscriptRef)
		return SaveCaptureState(&CaptureState{
			SessionID:        event.SessionID,
			Provider:         provider.Name(),
			TranscriptRef:    event.TranscriptRef,
			TranscriptOffset: offset,
			Timestamp:        time.Now().UnixMilli(),
		})
	}
	if err != nil {
		return fmt.Errorf("load capture state: %w", err)
	}

	readCtx := context.WithValue(ctx, CaptureTimestampKey, state.Timestamp)
	if event.Model != "" {
		readCtx = context.WithValue(readCtx, ModelKey, event.Model)
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
		return fmt.Errorf("read from offset: %w", err)
	}
	if len(events) == 0 {
		state.TranscriptOffset = newOffset
		state.Timestamp = time.Now().UnixMilli()
		return SaveCaptureState(state)
	}

	// Stamp TurnID from capture state onto replayed events so the dedup
	// check in WriteEventsToRepo can skip events already captured directly.
	if state.TurnID != "" {
		for i := range events {
			if events[i].TurnID == "" {
				events[i].TurnID = state.TurnID
			}
		}
	}

	if err := routeAndWriteEvents(ctx, events, bh, blobStore); err != nil {
		return fmt.Errorf("route and write: %w", err)
	}

	state.TranscriptOffset = newOffset
	state.Timestamp = time.Now().UnixMilli()
	if err := SaveCaptureState(state); err != nil {
		return fmt.Errorf("save capture state: %w", err)
	}

	return nil
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

// packageTurnFromState writes the packaged turn artifacts after capture succeeds.
func packageTurnFromState(ctx context.Context, provider HookProvider, event *Event, bh *broker.Handle, preState *CaptureState) {
	if event.CWD == "" {
		return
	}

	repos, err := broker.ListActiveRepos(ctx, bh)
	if err != nil {
		return
	}
	var repoPath string
	var bestLen int
	for _, r := range repos {
		if broker.PathBelongsToRepo(event.CWD, r.Path) && len(r.CanonicalPath) > bestLen {
			repoPath = r.Path
			bestLen = len(r.CanonicalPath)
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
	provenance.PackageTurn(ctx, repoPath, tc)
}

func buildTurnContext(preState *CaptureState, event *Event, providerName string) provenance.TurnContext {
	return provenance.TurnContext{
		TurnID:        preState.TurnID,
		SessionID:     event.SessionID,
		Provider:      providerName,
		TranscriptRef: preState.TranscriptRef,
		StartedAt:     preState.PromptSubmittedAt,
		CompletedAt:   time.Now().UnixMilli(),
		CWD:           event.CWD,
	}
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

	var writeFailed bool
	for _, match := range matches {
		if _, err := broker.WriteEventsToRepo(ctx, match.Repo.Path, match.Events, blobStore); err != nil {
			slog.Warn("write events to repo failed",
				"repo", match.Repo.Path, "events", len(match.Events), "err", err)
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

func emitHookBenchRecords(scope *doctor.BenchScope, event *Event, duration time.Duration) {
	for repoPath, stats := range scope.Snapshot() {
		doctor.EmitBenchRecord(repoPath, doctor.BenchRecord{
			Kind:       "hook",
			Event:      benchEventName(event.Type),
			Tool:       event.ToolName,
			TurnID:     event.TurnID,
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
