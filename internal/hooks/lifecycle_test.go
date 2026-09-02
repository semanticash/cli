package hooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/agents/api"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/provenance"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"
)

// fakeProvider implements HookProvider for testing.
type fakeProvider struct {
	name             string
	transcriptOffset int
	events           []broker.RawEvent
	readSequence     []fakeReadResult
	readByOffset     map[int][]broker.RawEvent // offset-keyed reads for ownership probes
	readCalls        int
	readPaths        []string // tracks which transcript paths were read
	readOffsets      []int    // offsets used for each ReadFromOffset call
	tokenUsage       TokenUsage
	tokenUsageSeen   bool
}

type fakeReadResult struct {
	events []broker.RawEvent
	offset int
	err    error
}

type fakeDirectProvider struct {
	fakeProvider
	directEvents []broker.RawEvent
	buildErr     error
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) OffsetReadsAuthoritative() bool { return true }

func (f *fakeProvider) DisplayName() string { return f.name }
func (f *fakeProvider) IsAvailable() bool   { return true }
func (f *fakeProvider) InstallHooks(ctx context.Context, repoRoot string, binaryPath string) (int, error) {
	return 0, nil
}
func (f *fakeProvider) UninstallHooks(ctx context.Context, repoRoot string) error   { return nil }
func (f *fakeProvider) AreHooksInstalled(ctx context.Context, repoRoot string) bool { return false }
func (f *fakeProvider) HookBinary(ctx context.Context, repoRoot string) (string, error) {
	return "semantica", nil
}
func (f *fakeProvider) ParseHookEvent(ctx context.Context, hookName string, stdin io.Reader) (*Event, error) {
	return nil, nil
}
func (f *fakeProvider) TranscriptOffset(ctx context.Context, transcriptRef string) (int, error) {
	return f.transcriptOffset, nil
}
func (f *fakeProvider) ReadFromOffset(ctx context.Context, transcriptRef string, offset int, bs api.BlobPutter) ([]broker.RawEvent, int, error) {
	f.readCalls++
	f.readPaths = append(f.readPaths, transcriptRef)
	f.readOffsets = append(f.readOffsets, offset)
	f.tokenUsage, f.tokenUsageSeen = TokenUsageFromContext(ctx)
	if f.readByOffset != nil {
		return append([]broker.RawEvent(nil), f.readByOffset[offset]...), f.transcriptOffset, nil
	}
	if len(f.readSequence) >= f.readCalls {
		r := f.readSequence[f.readCalls-1]
		return r.events, r.offset, r.err
	}
	return f.events, f.transcriptOffset, nil
}

func (f *fakeDirectProvider) BuildHookEvents(ctx context.Context, event *Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	if f.buildErr != nil {
		return nil, f.buildErr
	}
	return append([]broker.RawEvent(nil), f.directEvents...), nil
}

// fakeSubagentProvider extends fakeProvider with SubagentDiscoverer support.
type fakeSubagentProvider struct {
	fakeProvider
	subagentPaths []string // paths returned by DiscoverSubagentTranscripts
}

func (f *fakeSubagentProvider) DiscoverSubagentTranscripts(ctx context.Context, parentTranscriptRef string, _ DiscoveryContext) ([]string, error) {
	return f.subagentPaths, nil
}

func (f *fakeSubagentProvider) SubagentStateKey(subagentTranscriptRef string) string {
	return extractBasename(subagentTranscriptRef)
}

// fakeFailingSubagentProvider is like fakeSubagentProvider but ReadFromOffset
// returns an error for paths in failPaths.
type fakeFailingSubagentProvider struct {
	fakeSubagentProvider
	failPaths map[string]bool // paths that should fail on ReadFromOffset
}

func (f *fakeFailingSubagentProvider) ReadFromOffset(ctx context.Context, transcriptRef string, offset int, bs api.BlobPutter) ([]broker.RawEvent, int, error) {
	f.readCalls++
	f.readPaths = append(f.readPaths, transcriptRef)
	if f.failPaths[transcriptRef] {
		return nil, offset, fmt.Errorf("simulated read failure")
	}
	return f.events, f.transcriptOffset, nil
}

// extractBasename returns the filename without extension from a path.
func extractBasename(path string) string {
	base := path
	for i := len(base) - 1; i >= 0; i-- {
		if base[i] == '/' {
			base = base[i+1:]
			break
		}
	}
	if len(base) > 6 && base[len(base)-6:] == ".jsonl" {
		base = base[:len(base)-6]
	}
	return base
}

func TestDispatch_PromptSubmitted_SavesState(t *testing.T) {
	setupTestCaptureDir(t)

	prov := &fakeProvider{name: "test", transcriptOffset: 100}
	event := &Event{
		Type:          PromptSubmitted,
		SessionID:     "sess-1",
		TranscriptRef: "/transcript.jsonl",
		Timestamp:     time.Now().UnixMilli(),
	}

	if err := Dispatch(context.Background(), prov, event, nil, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	state, err := LoadCaptureState("sess-1")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.TranscriptOffset != 100 {
		t.Errorf("offset: got %d, want 100", state.TranscriptOffset)
	}
	if state.Provider != "test" {
		t.Errorf("provider: got %q, want %q", state.Provider, "test")
	}
}

func TestDispatch_AgentCompleted_DeletesState(t *testing.T) {
	setupTestCaptureDir(t)

	prov := &fakeProvider{name: "test", transcriptOffset: 150}

	// Pre-create capture state.
	if err := SaveCaptureState(&CaptureState{
		SessionID:        "sess-2",
		Provider:         "test",
		TranscriptRef:    "/transcript.jsonl",
		TranscriptOffset: 50,
		Timestamp:        1000,
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}

	event := &Event{
		Type:          AgentCompleted,
		SessionID:     "sess-2",
		TranscriptRef: "/transcript.jsonl",
	}

	if err := Dispatch(context.Background(), prov, event, nil, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	_, err := LoadCaptureState("sess-2")
	if err != ErrNoCaptureState {
		t.Errorf("state should be deleted, got: %v", err)
	}
}

func TestDispatch_AgentCompleted_PassesTokenUsageToReplay(t *testing.T) {
	setupTestCaptureDir(t)
	prov := &fakeProvider{name: "cursor", transcriptOffset: 1}
	if err := SaveCaptureState(&CaptureState{
		SessionID:        "usage-session",
		Provider:         "cursor",
		TranscriptRef:    "/transcript.jsonl",
		TranscriptOffset: 0,
		Timestamp:        1,
		TurnID:           "turn-1",
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}
	usage := TokenUsage{TokensIn: 4, TokensOut: 2, TokensCacheRead: 8, TokensCacheCreate: 1}
	event := &Event{
		Type:          AgentCompleted,
		SessionID:     "usage-session",
		TranscriptRef: "/transcript.jsonl",
		TokenUsage:    &usage,
	}
	if err := Dispatch(context.Background(), prov, event, nil, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !prov.tokenUsageSeen || prov.tokenUsage != usage {
		t.Fatalf("replay usage = %+v, seen=%v", prov.tokenUsage, prov.tokenUsageSeen)
	}
}

func TestDispatch_AgentCompleted_FreezesFirstUsageBeforeCapture(t *testing.T) {
	setupTestCaptureDir(t)
	provider := &fakeProvider{
		name:             "cursor",
		transcriptOffset: 1,
		readSequence: []fakeReadResult{
			{err: fmt.Errorf("capture failed")},
			{err: fmt.Errorf("capture failed again")},
		},
	}
	if err := SaveCaptureState(&CaptureState{
		SessionID:        "usage-session",
		Provider:         "cursor",
		TranscriptRef:    "/transcript.jsonl",
		TranscriptOffset: 0,
		Timestamp:        1,
		TurnID:           "turn-1",
	}); err != nil {
		t.Fatal(err)
	}
	first := TokenUsage{TokensIn: 4, TokensOut: 2, TokensCacheRead: 8, TokensCacheCreate: 1}
	event := &Event{Type: AgentCompleted, SessionID: "usage-session", TranscriptRef: "/transcript.jsonl", TokenUsage: &first}
	if err := Dispatch(context.Background(), provider, event, nil, nil); err == nil {
		t.Fatal("Dispatch() error = nil, want capture failure")
	}
	state, err := LoadCaptureState("usage-session")
	if err != nil {
		t.Fatal(err)
	}
	if state.TokenUsage == nil || *state.TokenUsage != first {
		t.Fatalf("frozen usage = %+v, want %+v", state.TokenUsage, first)
	}

	second := TokenUsage{TokensIn: 40, TokensOut: 20, TokensCacheRead: 80, TokensCacheCreate: 10}
	event.TokenUsage = &second
	if err := Dispatch(context.Background(), provider, event, nil, nil); err == nil {
		t.Fatal("retry Dispatch() error = nil, want capture failure")
	}
	state, err = LoadCaptureState("usage-session")
	if err != nil {
		t.Fatal(err)
	}
	if state.TokenUsage == nil || *state.TokenUsage != first {
		t.Fatalf("usage after conflicting retry = %+v, want first value %+v", state.TokenUsage, first)
	}
	if !provider.tokenUsageSeen || provider.tokenUsage != first {
		t.Fatalf("retry replay usage = %+v, want frozen first value %+v", provider.tokenUsage, first)
	}
}

func TestPromptSubmitted_DoesNotCarryTokenUsageToNextTurn(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	world := newToolWindowWorld(t, home, "repo")
	defer func() { _ = broker.Close(world.bh) }()

	const sessionID = "cursor-session"
	if err := SaveCaptureState(&CaptureState{
		SessionID:        sessionID,
		Provider:         "cursor",
		TranscriptRef:    "/transcript.jsonl",
		TranscriptOffset: 0,
		Timestamp:        1,
		TurnID:           "turn-1",
		TokenUsage: &TokenUsage{
			TokensIn: 10, TokensOut: 20, TokensCacheRead: 30, TokensCacheCreate: 40,
		},
	}); err != nil {
		t.Fatal(err)
	}

	provider := &fakeProvider{name: "cursor", transcriptOffset: 1}
	prompt := &Event{
		Type: PromptSubmitted, SessionID: sessionID, TranscriptRef: "/transcript.jsonl",
		Prompt: "next turn", Timestamp: 2, CWD: world.repoPath,
	}
	if err := Dispatch(ctx, provider, prompt, world.bh, nil); err != nil {
		t.Fatal(err)
	}
	state, err := LoadCaptureState(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state.TokenUsage != nil {
		t.Fatalf("new turn inherited token usage: %+v", state.TokenUsage)
	}

	if _, err := broker.WriteEventsToRepo(ctx, world.repoPath, []broker.RawEvent{{
		EventID: "turn-2-prompt", SourceKey: "/transcript.jsonl", Provider: "cursor",
		ProviderSessionID: sessionID, SessionStartedAt: 2,
		Timestamp: 2, Kind: "user", Role: "user", TurnID: state.TurnID, EventSource: "hook",
	}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := Dispatch(ctx, provider, &Event{
		Type: AgentCompleted, SessionID: sessionID, TranscriptRef: "/transcript.jsonl", CWD: world.repoPath,
	}, world.bh, nil); err != nil {
		t.Fatal(err)
	}

	h, err := sqlstore.Open(ctx, filepath.Join(world.semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	var input, output, cacheRead, cacheWrite sql.NullInt64
	if err := h.DB.QueryRowContext(ctx,
		"select tokens_in, tokens_out, tokens_cache_read, tokens_cache_create from provenance_manifests where turn_id = ?",
		state.TurnID,
	).Scan(&input, &output, &cacheRead, &cacheWrite); err != nil {
		t.Fatal(err)
	}
	if input.Valid || output.Valid || cacheRead.Valid || cacheWrite.Valid {
		t.Fatalf("new turn usage = %v/%v/%v/%v, want unavailable", input, output, cacheRead, cacheWrite)
	}
}

func TestSelectTurnTokenUsage(t *testing.T) {
	persisted := &provenance.TurnTokenUsage{InputUncached: 1, Output: 2, CacheRead: 3, CacheWrite: 4}
	frozen := &TokenUsage{TokensIn: 10, TokensOut: 20, TokensCacheRead: 30, TokensCacheCreate: 40}
	if got := selectTurnTokenUsage(persisted, provenance.TurnTokenUsageValid, frozen); got != persisted {
		t.Fatalf("valid persisted usage = %+v, want persisted value", got)
	}
	wantFallback := provenance.TurnTokenUsage{InputUncached: 10, Output: 20, CacheRead: 30, CacheWrite: 40}
	if got := selectTurnTokenUsage(nil, provenance.TurnTokenUsageAbsent, frozen); got == nil || *got != wantFallback {
		t.Fatalf("absent persisted usage = %+v, want frozen %+v", got, wantFallback)
	}
	if got := selectTurnTokenUsage(nil, provenance.TurnTokenUsageInvalid, frozen); got != nil {
		t.Fatalf("invalid persisted usage = %+v, want unavailable", got)
	}
}

func TestDispatch_AgentCompleted_MissingState_SnapshotsToEOF(t *testing.T) {
	setupTestCaptureDir(t)

	prov := &fakeProvider{name: "test", transcriptOffset: 200}

	event := &Event{
		Type:          AgentCompleted,
		SessionID:     "sess-missing",
		TranscriptRef: "/transcript.jsonl",
	}

	// Missing state should snapshot to EOF without failing.
	if err := Dispatch(context.Background(), prov, event, nil, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	_, err := LoadCaptureState("sess-missing")
	if err != ErrNoCaptureState {
		t.Errorf("state should be deleted after snapshot, got: %v", err)
	}
}

// TestDispatch_IncrementalCapture_AdvancesOffsetWithoutCleanup checks the
// mid-turn semantics: an IncrementalCapture event advances the transcript
// offset without deleting capture state.
func TestDispatch_IncrementalCapture_AdvancesOffsetWithoutCleanup(t *testing.T) {
	setupTestCaptureDir(t)

	prov := &fakeProvider{name: "test", transcriptOffset: 50}

	if err := SaveCaptureState(&CaptureState{
		SessionID:        "sess-incr",
		Provider:         "test",
		TranscriptRef:    "/transcript.jsonl",
		TranscriptOffset: 10,
		TurnID:           "turn-1",
		Timestamp:        1000,
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}

	event := &Event{
		Type:          IncrementalCapture,
		SessionID:     "sess-incr",
		TranscriptRef: "/transcript.jsonl",
	}

	if err := Dispatch(context.Background(), prov, event, nil, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	state, err := LoadCaptureState("sess-incr")
	if err != nil {
		t.Fatalf("state should still exist mid-turn, got: %v", err)
	}
	if state.TranscriptOffset != 50 {
		t.Errorf("offset = %d, want 50 (advanced by incremental scan)", state.TranscriptOffset)
	}
	if state.TurnID != "turn-1" {
		t.Errorf("TurnID = %q, want turn-1 (preserved)", state.TurnID)
	}
}

// TestDispatch_IncrementalCapture_NoStateNoOp checks that IncrementalCapture
// without saved state is a no-op, not an error.
func TestDispatch_IncrementalCapture_NoStateNoOp(t *testing.T) {
	setupTestCaptureDir(t)

	prov := &fakeProvider{name: "test", transcriptOffset: 50}

	event := &Event{
		Type:          IncrementalCapture,
		SessionID:     "sess-no-state",
		TranscriptRef: "/transcript.jsonl",
	}

	if err := Dispatch(context.Background(), prov, event, nil, nil); err != nil {
		t.Fatalf("dispatch should not error without state, got: %v", err)
	}

	// State should still not exist.
	_, err := LoadCaptureState("sess-no-state")
	if err != ErrNoCaptureState {
		t.Errorf("expected ErrNoCaptureState, got: %v", err)
	}
	if prov.readCalls != 0 {
		t.Errorf("readCalls = %d, want 0 (no work to do without state)", prov.readCalls)
	}
}

func TestDispatch_SubagentCompleted_AdvancesOffset(t *testing.T) {
	setupTestCaptureDir(t)

	prov := &fakeProvider{name: "test", transcriptOffset: 75}

	if err := SaveCaptureState(&CaptureState{
		SessionID:        "sess-sub",
		Provider:         "test",
		TranscriptRef:    "/transcript.jsonl",
		TranscriptOffset: 30,
		Timestamp:        1000,
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}

	event := &Event{
		Type:          SubagentCompleted,
		SessionID:     "sess-sub",
		TranscriptRef: "/transcript.jsonl",
	}

	if err := Dispatch(context.Background(), prov, event, nil, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// State should still exist (not deleted) with advanced offset.
	state, err := LoadCaptureState("sess-sub")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.TranscriptOffset != 75 {
		t.Errorf("offset: got %d, want 75", state.TranscriptOffset)
	}
}

func TestDispatch_ContextCompacted_ResetsOffset(t *testing.T) {
	setupTestCaptureDir(t)

	prov := &fakeProvider{name: "test", transcriptOffset: 10}

	// State had a high offset (pre-compaction).
	if err := SaveCaptureState(&CaptureState{
		SessionID:        "sess-compact",
		Provider:         "test",
		TranscriptRef:    "/transcript.jsonl",
		TranscriptOffset: 500,
		TurnID:           "turn-compact",
		Timestamp:        1000,
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}

	event := &Event{
		Type:          ContextCompacted,
		SessionID:     "sess-compact",
		TranscriptRef: "/transcript.jsonl",
	}

	if err := Dispatch(context.Background(), prov, event, nil, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	state, err := LoadCaptureState("sess-compact")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.TranscriptOffset != 10 {
		t.Errorf("offset: got %d, want 10 (reset to EOF)", state.TranscriptOffset)
	}
}

func TestDispatch_SessionClosed_FlushesIfStateExists(t *testing.T) {
	setupTestCaptureDir(t)

	prov := &fakeProvider{name: "test", transcriptOffset: 60}

	if err := SaveCaptureState(&CaptureState{
		SessionID:        "sess-close",
		Provider:         "test",
		TranscriptRef:    "/transcript.jsonl",
		TranscriptOffset: 20,
		Timestamp:        1000,
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}

	event := &Event{
		Type:          SessionClosed,
		SessionID:     "sess-close",
		TranscriptRef: "/transcript.jsonl",
	}

	if err := Dispatch(context.Background(), prov, event, nil, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// State should be cleaned up after successful capture.
	_, err := LoadCaptureState("sess-close")
	if err != ErrNoCaptureState {
		t.Errorf("state should be deleted after session close capture, got: %v", err)
	}
}

func TestDispatch_SessionClosed_NoopIfNoState(t *testing.T) {
	setupTestCaptureDir(t)

	prov := &fakeProvider{name: "test"}
	event := &Event{
		Type:      SessionClosed,
		SessionID: "sess-nostate",
	}

	// Should not error when no state exists.
	if err := Dispatch(context.Background(), prov, event, nil, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
}

func TestDispatch_SessionOpened_Noop(t *testing.T) {
	setupTestCaptureDir(t)

	prov := &fakeProvider{name: "test"}
	event := &Event{
		Type:      SessionOpened,
		SessionID: "sess-open",
	}

	if err := Dispatch(context.Background(), prov, event, nil, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// No state should be created.
	_, err := LoadCaptureState("sess-open")
	if err != ErrNoCaptureState {
		t.Errorf("session opened should not create state, got: %v", err)
	}
}

func TestDispatch_ToolStepCompleted_BuildErrorDoesNotFail(t *testing.T) {
	setupTestCaptureDir(t)

	prov := &fakeDirectProvider{
		fakeProvider: fakeProvider{name: "test"},
		buildErr:     fmt.Errorf("boom"),
	}

	if err := SaveCaptureState(&CaptureState{
		SessionID:        "sess-step-build",
		Provider:         "test",
		TranscriptRef:    "/transcript.jsonl",
		TranscriptOffset: 10,
		TurnID:           "turn-step-build",
		Timestamp:        1000,
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}

	event := &Event{
		Type:      ToolStepCompleted,
		SessionID: "sess-step-build",
		ToolName:  "Write",
	}

	if err := Dispatch(context.Background(), prov, event, nil, nil); err != nil {
		t.Fatalf("dispatch should not return an error, got: %v", err)
	}
}

func TestDispatch_ToolStepCompleted_WriteErrorDoesNotFail(t *testing.T) {
	setupTestCaptureDir(t)
	// Redirect the global config dir so the broker write failure
	// this test deliberately provokes lands in a temp hook-errors.log
	// rather than the developer's real one.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoPath, ".semantica"), 0o755); err != nil {
		t.Fatalf("mkdir .semantica: %v", err)
	}

	registryPath := filepath.Join(t.TempDir(), "repos.json")
	bh, err := broker.Open(context.Background(), registryPath)
	if err != nil {
		t.Fatalf("open broker: %v", err)
	}
	t.Cleanup(func() { _ = broker.Close(bh) })
	if err := broker.Register(context.Background(), bh, repoPath, repoPath); err != nil {
		t.Fatalf("register repo: %v", err)
	}

	prov := &fakeDirectProvider{
		fakeProvider: fakeProvider{name: "test"},
		directEvents: []broker.RawEvent{{
			EventID:           "evt-step-write-error",
			Provider:          "test",
			SourceKey:         "test-source",
			ProviderSessionID: "provider-sess-1",
			SourceProjectPath: repoPath,
			Timestamp:         1,
			Role:              "assistant",
			Kind:              "tool",
		}},
	}

	if err := SaveCaptureState(&CaptureState{
		SessionID:        "sess-step-write",
		Provider:         "test",
		TranscriptRef:    "/transcript.jsonl",
		TranscriptOffset: 10,
		TurnID:           "turn-step-write",
		Timestamp:        1000,
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}

	event := &Event{
		Type:      ToolStepCompleted,
		SessionID: "sess-step-write",
		ToolName:  "Write",
	}

	if err := Dispatch(context.Background(), prov, event, bh, nil); err != nil {
		t.Fatalf("dispatch should not return an error, got: %v", err)
	}
}

func TestDispatch_SubagentPromptSubmitted_BuildErrorDoesNotFail(t *testing.T) {
	setupTestCaptureDir(t)

	prov := &fakeDirectProvider{
		fakeProvider: fakeProvider{name: "test"},
		buildErr:     fmt.Errorf("boom"),
	}

	if err := SaveCaptureState(&CaptureState{
		SessionID:        "sess-agent-build",
		Provider:         "test",
		TranscriptRef:    "/transcript.jsonl",
		TranscriptOffset: 10,
		TurnID:           "turn-agent-build",
		Timestamp:        1000,
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}

	event := &Event{
		Type:      SubagentPromptSubmitted,
		SessionID: "sess-agent-build",
		ToolName:  "Agent",
	}

	if err := Dispatch(context.Background(), prov, event, nil, nil); err != nil {
		t.Fatalf("dispatch should not return an error, got: %v", err)
	}
}

func TestDispatch_SubagentPromptSubmitted_WriteErrorDoesNotFail(t *testing.T) {
	setupTestCaptureDir(t)
	// Redirect the global config dir so the broker write failure
	// this test deliberately provokes lands in a temp hook-errors.log
	// rather than the developer's real one.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repoPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoPath, ".semantica"), 0o755); err != nil {
		t.Fatalf("mkdir .semantica: %v", err)
	}

	registryPath := filepath.Join(t.TempDir(), "repos.json")
	bh, err := broker.Open(context.Background(), registryPath)
	if err != nil {
		t.Fatalf("open broker: %v", err)
	}
	t.Cleanup(func() { _ = broker.Close(bh) })
	if err := broker.Register(context.Background(), bh, repoPath, repoPath); err != nil {
		t.Fatalf("register repo: %v", err)
	}

	prov := &fakeDirectProvider{
		fakeProvider: fakeProvider{name: "test"},
		directEvents: []broker.RawEvent{{
			EventID:           "evt-agent-write-error",
			Provider:          "test",
			SourceKey:         "test-source",
			ProviderSessionID: "provider-sess-1",
			SourceProjectPath: repoPath,
			Timestamp:         1,
			Role:              "assistant",
			Kind:              "tool",
		}},
	}

	if err := SaveCaptureState(&CaptureState{
		SessionID:        "sess-agent-write",
		Provider:         "test",
		TranscriptRef:    "/transcript.jsonl",
		TranscriptOffset: 10,
		TurnID:           "turn-agent-write",
		Timestamp:        1000,
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}

	event := &Event{
		Type:      SubagentPromptSubmitted,
		SessionID: "sess-agent-write",
		ToolName:  "Agent",
	}

	if err := Dispatch(context.Background(), prov, event, bh, nil); err != nil {
		t.Fatalf("dispatch should not return an error, got: %v", err)
	}
}

func TestCaptureAndRoute_ReadsFromOffset(t *testing.T) {
	setupTestCaptureDir(t)

	prov := &fakeProvider{
		name:             "test",
		transcriptOffset: 100,
		events:           nil, // No events - just verifies the read path.
	}

	if err := SaveCaptureState(&CaptureState{
		SessionID:        "sess-read",
		Provider:         "test",
		TranscriptRef:    "/transcript.jsonl",
		TranscriptOffset: 50,
		Timestamp:        1000,
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}

	event := &Event{
		Type:          AgentCompleted,
		SessionID:     "sess-read",
		TranscriptRef: "/transcript.jsonl",
	}

	if err := CaptureAndRoute(context.Background(), prov, event, nil, nil); err != nil {
		t.Fatalf("capture: %v", err)
	}

	if prov.readCalls != 1 {
		t.Errorf("read calls: got %d, want 1", prov.readCalls)
	}

	// Offset should be advanced.
	state, err := LoadCaptureState("sess-read")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.TranscriptOffset != 100 {
		t.Errorf("offset: got %d, want 100", state.TranscriptOffset)
	}
}

func TestSubagentCompleted_ScansChildTranscripts(t *testing.T) {
	setupTestCaptureDir(t)

	prov := &fakeSubagentProvider{
		fakeProvider: fakeProvider{
			name:             "test",
			transcriptOffset: 50,
			events:           nil,
		},
		subagentPaths: []string{
			"/project/parent-uuid/subagents/agent-abc.jsonl",
			"/project/parent-uuid/subagents/agent-def.jsonl",
		},
	}

	// Pre-create parent capture state.
	if err := SaveCaptureState(&CaptureState{
		SessionID:        "parent-sess",
		Provider:         "test",
		TranscriptRef:    "/project/parent-uuid.jsonl",
		TranscriptOffset: 10,
		Timestamp:        1000,
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}

	event := &Event{
		Type:          SubagentCompleted,
		SessionID:     "parent-sess",
		TranscriptRef: "/project/parent-uuid.jsonl",
	}

	if err := Dispatch(context.Background(), prov, event, nil, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if prov.readCalls != 3 {
		t.Errorf("read calls: got %d, want 3", prov.readCalls)
	}

	pathSet := make(map[string]bool)
	for _, p := range prov.readPaths {
		pathSet[p] = true
	}
	if !pathSet["/project/parent-uuid/subagents/agent-abc.jsonl"] {
		t.Error("agent-abc transcript not read")
	}
	if !pathSet["/project/parent-uuid/subagents/agent-def.jsonl"] {
		t.Error("agent-def transcript not read")
	}

	stateABC, err := LoadCaptureStateByKey("agent-abc")
	if err != nil {
		t.Fatalf("load subagent state abc: %v", err)
	}
	if stateABC.TranscriptOffset != 50 {
		t.Errorf("subagent abc offset: got %d, want 50", stateABC.TranscriptOffset)
	}
	if stateABC.StateKey != "agent-abc" {
		t.Errorf("subagent abc state key: got %q, want %q", stateABC.StateKey, "agent-abc")
	}

	stateDEF, err := LoadCaptureStateByKey("agent-def")
	if err != nil {
		t.Fatalf("load subagent state def: %v", err)
	}
	if stateDEF.TranscriptOffset != 50 {
		t.Errorf("subagent def offset: got %d, want 50", stateDEF.TranscriptOffset)
	}
}

// Child events without parent linkage are stamped from lifecycle
// capture state before routing.
func TestSubagentCompleted_StampsParentSessionAndTurn(t *testing.T) {
	setupTestCaptureDir(t)

	// Call 1 (parent capture) returns no events so it exits before
	// broker writes. Call 2 (child capture) returns the event under
	// test; the shared slice lets the assertion observe in-place
	// stamping by the lifecycle.
	childEvents := []broker.RawEvent{
		{
			ProviderSessionID: "child-uuid",
			ToolName:          "Write",
			EventSource:       "transcript",
		},
	}

	prov := &fakeSubagentProvider{
		fakeProvider: fakeProvider{
			name:             "test",
			transcriptOffset: 5,
			readSequence: []fakeReadResult{
				{events: nil, offset: 5},         // parent capture call
				{events: childEvents, offset: 5}, // child capture call
			},
		},
		subagentPaths: []string{"/project/parent-uuid/subagents/child.jsonl"},
	}

	if err := SaveCaptureState(&CaptureState{
		SessionID:     "parent-sess",
		Provider:      "test",
		TranscriptRef: "/project/parent-uuid.jsonl",
		Timestamp:     1000,
		TurnID:        "turn-parent",
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}

	event := &Event{
		Type:          SubagentCompleted,
		SessionID:     "parent-sess",
		TranscriptRef: "/project/parent-uuid.jsonl",
	}

	if err := Dispatch(context.Background(), prov, event, nil, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if got := childEvents[0].ParentSessionID; got != "parent-sess" {
		t.Errorf("ParentSessionID = %q, want parent-sess", got)
	}
	if got := childEvents[0].TurnID; got != "turn-parent" {
		t.Errorf("TurnID = %q, want turn-parent", got)
	}
	if got := childEvents[0].ProviderSessionID; got != "child-uuid" {
		t.Errorf("ProviderSessionID = %q, want child-uuid (stamping must be additive only)", got)
	}
}

// Provider-supplied parent linkage is preserved when already present.
func TestSubagentCompleted_DoesNotOverstampParent(t *testing.T) {
	setupTestCaptureDir(t)

	childEvents := []broker.RawEvent{
		{
			ProviderSessionID: "child-uuid",
			ParentSessionID:   "provider-set-parent",
			TurnID:            "provider-set-turn",
			ToolName:          "Write",
		},
	}

	prov := &fakeSubagentProvider{
		fakeProvider: fakeProvider{
			name:             "test",
			transcriptOffset: 5,
			readSequence: []fakeReadResult{
				{events: nil, offset: 5},
				{events: childEvents, offset: 5},
			},
		},
		subagentPaths: []string{"/project/parent-uuid/subagents/child.jsonl"},
	}

	if err := SaveCaptureState(&CaptureState{
		SessionID:     "parent-sess",
		Provider:      "test",
		TranscriptRef: "/project/parent-uuid.jsonl",
		TurnID:        "turn-parent",
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}

	event := &Event{
		Type:          SubagentCompleted,
		SessionID:     "parent-sess",
		TranscriptRef: "/project/parent-uuid.jsonl",
	}

	if err := Dispatch(context.Background(), prov, event, nil, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if got := childEvents[0].ParentSessionID; got != "provider-set-parent" {
		t.Errorf("ParentSessionID = %q, want provider-set-parent (must not overstamp)", got)
	}
	if got := childEvents[0].TurnID; got != "provider-set-turn" {
		t.Errorf("TurnID = %q, want provider-set-turn (must not overstamp)", got)
	}
}

func TestSubagentCompleted_OldChildTranscriptStartsAtEOF(t *testing.T) {
	setupTestCaptureDir(t)

	prov := &fakeSubagentProvider{
		fakeProvider: fakeProvider{
			name:             "test",
			transcriptOffset: 50,
		},
	}

	dir := t.TempDir()
	childPath := filepath.Join(dir, "agent-old.jsonl")
	if err := os.WriteFile(childPath, []byte("{\"type\":\"assistant\"}\n"), 0o644); err != nil {
		t.Fatalf("write child transcript: %v", err)
	}
	oldTime := time.UnixMilli(1_000)
	if err := os.Chtimes(childPath, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes child transcript: %v", err)
	}
	prov.subagentPaths = []string{childPath}

	if err := SaveCaptureState(&CaptureState{
		SessionID:         "parent-old-child",
		Provider:          "test",
		TranscriptRef:     "/project/parent-uuid.jsonl",
		TranscriptOffset:  10,
		Timestamp:         2_000,
		TurnID:            "turn-1",
		PromptSubmittedAt: 2_000,
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}

	event := &Event{
		Type:          SubagentCompleted,
		SessionID:     "parent-old-child",
		TranscriptRef: "/project/parent-uuid.jsonl",
	}

	if err := Dispatch(context.Background(), prov, event, nil, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if len(prov.readOffsets) < 2 {
		t.Fatalf("read offsets: got %d calls, want at least 2", len(prov.readOffsets))
	}
	if prov.readOffsets[1] != 50 {
		t.Fatalf("child read offset: got %d, want 50", prov.readOffsets[1])
	}

	state, err := LoadCaptureStateByKey("agent-old")
	if err != nil {
		t.Fatalf("load child state: %v", err)
	}
	if state.TranscriptOffset != 50 {
		t.Fatalf("child offset: got %d, want 50", state.TranscriptOffset)
	}
}

func TestSubagentCompleted_NewChildTranscriptStartsAtZero(t *testing.T) {
	setupTestCaptureDir(t)

	prov := &fakeSubagentProvider{
		fakeProvider: fakeProvider{
			name:             "test",
			transcriptOffset: 50,
		},
	}

	dir := t.TempDir()
	childPath := filepath.Join(dir, "agent-new.jsonl")
	if err := os.WriteFile(childPath, []byte("{\"type\":\"assistant\"}\n"), 0o644); err != nil {
		t.Fatalf("write child transcript: %v", err)
	}
	newTime := time.UnixMilli(3_000)
	if err := os.Chtimes(childPath, newTime, newTime); err != nil {
		t.Fatalf("chtimes child transcript: %v", err)
	}
	prov.subagentPaths = []string{childPath}

	if err := SaveCaptureState(&CaptureState{
		SessionID:         "parent-new-child",
		Provider:          "test",
		TranscriptRef:     "/project/parent-uuid.jsonl",
		TranscriptOffset:  10,
		Timestamp:         2_000,
		TurnID:            "turn-1",
		PromptSubmittedAt: 2_000,
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}

	event := &Event{
		Type:          SubagentCompleted,
		SessionID:     "parent-new-child",
		TranscriptRef: "/project/parent-uuid.jsonl",
	}

	if err := Dispatch(context.Background(), prov, event, nil, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if len(prov.readOffsets) < 2 {
		t.Fatalf("read offsets: got %d calls, want at least 2", len(prov.readOffsets))
	}
	if prov.readOffsets[1] != 0 {
		t.Fatalf("child read offset: got %d, want 0", prov.readOffsets[1])
	}

	state, err := LoadCaptureStateByKey("agent-new")
	if err != nil {
		t.Fatalf("load child state: %v", err)
	}
	if state.TranscriptOffset != 50 {
		t.Fatalf("child offset: got %d, want 50", state.TranscriptOffset)
	}
}

func TestAgentCompleted_CleansUpSubagentStates(t *testing.T) {
	setupTestCaptureDir(t)

	prov := &fakeSubagentProvider{
		fakeProvider: fakeProvider{
			name:             "test",
			transcriptOffset: 100,
			events:           nil,
		},
		subagentPaths: []string{
			"/project/parent-uuid/subagents/agent-xyz.jsonl",
		},
	}

	// Pre-create parent and subagent capture states.
	if err := SaveCaptureState(&CaptureState{
		SessionID:        "parent-cleanup",
		Provider:         "test",
		TranscriptRef:    "/project/parent-uuid.jsonl",
		TranscriptOffset: 20,
		Timestamp:        1000,
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}
	if err := SaveCaptureState(&CaptureState{
		SessionID:        "parent-cleanup",
		StateKey:         "agent-xyz",
		Provider:         "test",
		TranscriptRef:    "/project/parent-uuid/subagents/agent-xyz.jsonl",
		TranscriptOffset: 30,
		Timestamp:        1000,
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}

	event := &Event{
		Type:          AgentCompleted,
		SessionID:     "parent-cleanup",
		TranscriptRef: "/project/parent-uuid.jsonl",
	}

	if err := Dispatch(context.Background(), prov, event, nil, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if _, err := LoadCaptureState("parent-cleanup"); err != ErrNoCaptureState {
		t.Error("parent state should be deleted after AgentCompleted")
	}

	if _, err := LoadCaptureStateByKey("agent-xyz"); err != ErrNoCaptureState {
		t.Error("subagent state should be deleted after AgentCompleted")
	}
}

func TestCaptureState_StateKey(t *testing.T) {
	setupTestCaptureDir(t)

	state := &CaptureState{
		SessionID:        "parent-id",
		StateKey:         "agent-child-123",
		Provider:         "test",
		TranscriptRef:    "/child/transcript.jsonl",
		TranscriptOffset: 42,
		Timestamp:        1000,
	}

	if err := SaveCaptureState(state); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := LoadCaptureStateByKey("agent-child-123")
	if err != nil {
		t.Fatalf("load by key: %v", err)
	}
	if loaded.SessionID != "parent-id" {
		t.Errorf("session ID: got %q, want %q", loaded.SessionID, "parent-id")
	}
	if loaded.StateKey != "agent-child-123" {
		t.Errorf("state key: got %q, want %q", loaded.StateKey, "agent-child-123")
	}
	if loaded.TranscriptOffset != 42 {
		t.Errorf("offset: got %d, want 42", loaded.TranscriptOffset)
	}

	if _, err := LoadCaptureState("parent-id"); err != ErrNoCaptureState {
		t.Error("should not be loadable by parent session ID")
	}
}

func TestAgentCompleted_NilBrokerPreservesSubagentState(t *testing.T) {
	setupTestCaptureDir(t)

	prov := &fakeSubagentProvider{
		fakeProvider: fakeProvider{
			name:             "test",
			transcriptOffset: 80,
			events:           nil,
		},
		subagentPaths: []string{
			"/project/parent-uuid/subagents/agent-keep.jsonl",
		},
	}

	// Pre-create parent and subagent capture states.
	if err := SaveCaptureState(&CaptureState{
		SessionID:        "sess-nilbh",
		Provider:         "test",
		TranscriptRef:    "/project/parent-uuid.jsonl",
		TranscriptOffset: 10,
		Timestamp:        1000,
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}
	if err := SaveCaptureState(&CaptureState{
		SessionID:        "sess-nilbh",
		StateKey:         "agent-keep",
		Provider:         "test",
		TranscriptRef:    "/project/parent-uuid/subagents/agent-keep.jsonl",
		TranscriptOffset: 5,
		Timestamp:        1000,
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}

	event := &Event{
		Type:          AgentCompleted,
		SessionID:     "sess-nilbh",
		TranscriptRef: "/project/parent-uuid.jsonl",
	}

	// A nil broker skips repo routing, but successful capture still advances
	// offsets and should clean up subagent state.
	if err := Dispatch(context.Background(), prov, event, nil, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if _, err := LoadCaptureState("sess-nilbh"); err != ErrNoCaptureState {
		t.Error("parent state should be deleted")
	}

	if _, err := LoadCaptureStateByKey("agent-keep"); err != ErrNoCaptureState {
		t.Error("subagent state should be cleaned up after successful capture with nil broker")
	}
}

func TestAgentCompleted_FailedChildPreservesItsState(t *testing.T) {
	setupTestCaptureDir(t)

	prov := &fakeFailingSubagentProvider{
		fakeSubagentProvider: fakeSubagentProvider{
			fakeProvider: fakeProvider{
				name:             "test",
				transcriptOffset: 90,
				events:           nil,
			},
			subagentPaths: []string{
				"/project/parent-uuid/subagents/agent-ok.jsonl",
				"/project/parent-uuid/subagents/agent-fail.jsonl",
			},
		},
		failPaths: map[string]bool{
			"/project/parent-uuid/subagents/agent-fail.jsonl": true,
		},
	}

	// Pre-create parent and both subagent states.
	if err := SaveCaptureState(&CaptureState{
		SessionID:        "sess-partial",
		Provider:         "test",
		TranscriptRef:    "/project/parent-uuid.jsonl",
		TranscriptOffset: 10,
		Timestamp:        1000,
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}
	if err := SaveCaptureState(&CaptureState{
		SessionID:        "sess-partial",
		StateKey:         "agent-ok",
		Provider:         "test",
		TranscriptRef:    "/project/parent-uuid/subagents/agent-ok.jsonl",
		TranscriptOffset: 5,
		Timestamp:        1000,
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}
	if err := SaveCaptureState(&CaptureState{
		SessionID:        "sess-partial",
		StateKey:         "agent-fail",
		Provider:         "test",
		TranscriptRef:    "/project/parent-uuid/subagents/agent-fail.jsonl",
		TranscriptOffset: 3,
		Timestamp:        1000,
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}

	event := &Event{
		Type:          AgentCompleted,
		SessionID:     "sess-partial",
		TranscriptRef: "/project/parent-uuid.jsonl",
	}

	if err := Dispatch(context.Background(), prov, event, nil, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if _, err := LoadCaptureState("sess-partial"); err != ErrNoCaptureState {
		t.Error("parent state should be deleted")
	}

	if _, err := LoadCaptureStateByKey("agent-ok"); err != ErrNoCaptureState {
		t.Error("agent-ok state should be deleted after successful capture")
	}

	state, err := LoadCaptureStateByKey("agent-fail")
	if err != nil {
		t.Fatalf("agent-fail state should be preserved, got: %v", err)
	}
	if state.TranscriptOffset != 3 {
		t.Errorf("agent-fail offset: got %d, want 3 (unchanged)", state.TranscriptOffset)
	}
}

func TestSubagentCompleted_DirectSubagent_ReadsFromZero(t *testing.T) {
	setupTestCaptureDir(t)

	// No SubagentDiscoverer simulates providers that emit direct subagent events.
	prov := &fakeProvider{
		name:             "cursor",
		transcriptOffset: 40,
		events:           nil,
	}

	event := &Event{
		Type:          SubagentCompleted,
		SessionID:     "subagent-conv-123",
		TranscriptRef: "/cursor/transcripts/subagent-conv-123.jsonl",
	}

	if err := Dispatch(context.Background(), prov, event, nil, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if prov.readCalls != 1 {
		t.Errorf("read calls: got %d, want 1", prov.readCalls)
	}

	state, err := LoadCaptureState("subagent-conv-123")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.TranscriptOffset != 40 {
		t.Errorf("offset: got %d, want 40", state.TranscriptOffset)
	}
	if state.TranscriptRef != "/cursor/transcripts/subagent-conv-123.jsonl" {
		t.Errorf("transcript ref: got %q", state.TranscriptRef)
	}
}

func TestSessionClosed_SweepsSubagentTranscripts(t *testing.T) {
	setupTestCaptureDir(t)

	prov := &fakeSubagentProvider{
		fakeProvider: fakeProvider{
			name:             "test",
			transcriptOffset: 70,
			events:           nil,
		},
		subagentPaths: []string{
			"/project/parent-uuid/subagents/agent-sess.jsonl",
		},
	}

	// Pre-create parent and subagent state (simulating a session that
	// missed its AgentCompleted hook).
	if err := SaveCaptureState(&CaptureState{
		SessionID:        "sess-closed",
		Provider:         "test",
		TranscriptRef:    "/project/parent-uuid.jsonl",
		TranscriptOffset: 15,
		Timestamp:        1000,
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}
	if err := SaveCaptureState(&CaptureState{
		SessionID:        "sess-closed",
		StateKey:         "agent-sess",
		Provider:         "test",
		TranscriptRef:    "/project/parent-uuid/subagents/agent-sess.jsonl",
		TranscriptOffset: 8,
		Timestamp:        1000,
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}

	event := &Event{
		Type:          SessionClosed,
		SessionID:     "sess-closed",
		TranscriptRef: "/project/parent-uuid.jsonl",
	}

	if err := Dispatch(context.Background(), prov, event, nil, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if _, err := LoadCaptureState("sess-closed"); err != ErrNoCaptureState {
		t.Error("parent state should be deleted after SessionClosed")
	}

	if _, err := LoadCaptureStateByKey("agent-sess"); err != ErrNoCaptureState {
		t.Error("subagent state should be deleted after SessionClosed sweep")
	}
}

func TestBuildTurnContext_PopulatesFields(t *testing.T) {
	preState := &CaptureState{
		SessionID:         "sess-parent",
		Provider:          "claude-code",
		TranscriptRef:     "/tmp/parent.jsonl",
		TranscriptOffset:  48,
		TurnID:            "turn-123",
		PromptSubmittedAt: 1234,
	}
	event := &Event{
		SessionID: "provider-session-1",
		CWD:       "/repo",
	}

	ctx := buildTurnContext(preState, event, "claude-code")
	if ctx.TurnID != "turn-123" {
		t.Fatalf("turn id: got %q, want %q", ctx.TurnID, "turn-123")
	}
	if ctx.TranscriptRef != "/tmp/parent.jsonl" {
		t.Fatalf("transcript ref: got %q", ctx.TranscriptRef)
	}
	if ctx.CWD != "/repo" {
		t.Fatalf("cwd: got %q", ctx.CWD)
	}
}

// TestRouteAndWriteEventsToRepos_SilentDropOnStaleRepo verifies that
// stale repo writes do not produce hook-errors.log entries.
func TestRouteAndWriteEventsToRepos_SilentDropOnStaleRepo(t *testing.T) {
	// Isolate the global config dir so the test never touches the
	// developer's real hook-errors.log.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// Repo path exists but has no .semantica/.
	repoDir := t.TempDir()
	canonical := repoDir
	if resolved, err := filepath.EvalSymlinks(repoDir); err == nil {
		canonical = resolved
	}

	ev := broker.RawEvent{
		EventID:   "evt-1",
		SourceKey: "default",
		Provider:  "codex",
		Timestamp: time.Now().UnixMilli(),
		Kind:      "user",
		Role:      "user",
		FilePaths: []string{filepath.Join(canonical, "src", "foo.go")},
	}
	repos := []broker.RegisteredRepo{{
		RepoID:        "test-repo",
		Path:          canonical,
		CanonicalPath: canonical,
		Active:        true,
	}}

	if err := routeAndWriteEventsToRepos(context.Background(), []broker.RawEvent{ev}, repos, nil); err != nil {
		t.Errorf("expected silent drop, got error: %v", err)
	}

	entries, readErr := util.ReadHookErrorTail(10)
	if readErr != nil {
		t.Fatalf("read hook-errors.log: %v", readErr)
	}
	for _, e := range entries {
		if e.Hook == "broker-write" {
			t.Errorf("stale-repo drop must not touch hook-errors.log; got entry: %+v", e)
		}
	}
}

// Non-stale write failures should still surface through hook-errors.log.
func TestRouteAndWriteEventsToRepos_LogsRealFailureToHookErrors(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// Corrupt lineage DB produces RepoStateUnknown, not ErrRepoStale.
	repoDir := t.TempDir()
	canonical := repoDir
	if resolved, err := filepath.EvalSymlinks(repoDir); err == nil {
		canonical = resolved
	}
	semDir := filepath.Join(canonical, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Write garbage at lineage.db to force a sqlite open error.
	if err := os.WriteFile(filepath.Join(semDir, "lineage.db"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}

	ev := broker.RawEvent{
		EventID:   "evt-1",
		SourceKey: "default",
		Provider:  "claude-code",
		Timestamp: time.Now().UnixMilli(),
		Kind:      "user",
		Role:      "user",
		FilePaths: []string{filepath.Join(canonical, "src", "foo.go")},
	}
	repos := []broker.RegisteredRepo{{
		RepoID:        "test-repo",
		Path:          canonical,
		CanonicalPath: canonical,
		Active:        true,
	}}

	err := routeAndWriteEventsToRepos(context.Background(), []broker.RawEvent{ev}, repos, nil)
	if err == nil {
		t.Fatal("expected write failure to surface as error; got nil")
	}

	entries, readErr := util.ReadHookErrorTail(10)
	if readErr != nil {
		t.Fatalf("read hook-errors.log: %v", readErr)
	}
	var found bool
	for _, e := range entries {
		if e.Hook == "broker-write" && e.Provider == "claude-code" && strings.Contains(e.Message, canonical) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected hook-errors.log entry hook=broker-write provider=claude-code referencing %q; got: %+v",
			canonical, entries)
	}
}

func TestTurnCWD_FallsBackToCaptureState(t *testing.T) {
	if got := turnCWD(&Event{CWD: "/from-event"}, &CaptureState{CWD: "/from-state"}); got != "/from-event" {
		t.Errorf("event CWD must win, got %q", got)
	}
	if got := turnCWD(&Event{}, &CaptureState{CWD: "/from-state"}); got != "/from-state" {
		t.Errorf("empty event CWD must fall back to state, got %q", got)
	}
	if got := turnCWD(&Event{}, nil); got != "" {
		t.Errorf("nil state with empty event must yield empty, got %q", got)
	}
	if got := turnCWD(&Event{}, &CaptureState{}); got != "" {
		t.Errorf("both empty must yield empty, got %q", got)
	}
}

func TestBuildTurnContext_CWDFallback(t *testing.T) {
	preState := &CaptureState{
		TurnID:            "turn-456",
		TranscriptRef:     "/tmp/t.jsonl",
		PromptSubmittedAt: 123,
		CWD:               "/enabled-repo",
	}
	event := &Event{SessionID: "s-1"}

	ctx := buildTurnContext(preState, event, "gemini-cli")
	if ctx.CWD != "/enabled-repo" {
		t.Fatalf("cwd: got %q, want fallback to state CWD", ctx.CWD)
	}
	if ctx.TurnID != "turn-456" {
		t.Fatalf("turn id: got %q", ctx.TurnID)
	}
}

func TestPackageTurnFromState_CWDFallbackReachesEnabledRepo(t *testing.T) {
	ctx := context.Background()

	repoDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	semDir := filepath.Join(repoDir, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Seed the repository and session records required by PackageTurn.
	dbPath := filepath.Join(semDir, "lineage.db")
	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: "repo-1", RootPath: repoDir, CreatedAt: 1, EnabledAt: 1,
	}); err != nil {
		t.Fatalf("insert repository: %v", err)
	}
	if _, err := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: "src-1", RepositoryID: "repo-1", Provider: "fake-prov",
		SourceKey: "key-1", LastSeenAt: 1, CreatedAt: 1,
	}); err != nil {
		t.Fatalf("upsert source: %v", err)
	}
	if _, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: "sess-1", ProviderSessionID: "ps-1", RepositoryID: "repo-1",
		Provider: "fake-prov", SourceID: "src-1", StartedAt: 1, LastSeenAt: 1,
		MetadataJson: "{}",
	}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := sqlstore.Close(h); err != nil {
		t.Fatalf("close db: %v", err)
	}

	bh, err := broker.Open(ctx, filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatalf("open broker: %v", err)
	}
	defer func() { _ = broker.Close(bh) }()
	if err := broker.Register(ctx, bh, repoDir, repoDir); err != nil {
		t.Fatalf("register repo: %v", err)
	}

	provider := &fakeProvider{name: "fake-prov"}
	preState := &CaptureState{
		TurnID:            "turn-cwd-fallback",
		TranscriptRef:     "",
		PromptSubmittedAt: 1,
		CWD:               repoDir,
		TokenUsage: &TokenUsage{
			TokensIn: 10, TokensOut: 20, TokensCacheRead: 0, TokensCacheCreate: 0,
		},
	}
	event := &Event{SessionID: "ps-1"}

	packageTurnFromState(ctx, provider, event, bh, nil, preState)

	manifestCount := func(turnID string) int {
		h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
		if err != nil {
			t.Fatalf("reopen db: %v", err)
		}
		defer func() { _ = sqlstore.Close(h) }()
		var n int
		if err := h.DB.QueryRowContext(ctx,
			"select count(*) from provenance_manifests where turn_id = ?", turnID,
		).Scan(&n); err != nil {
			t.Fatalf("count manifests: %v", err)
		}
		return n
	}

	if got := manifestCount("turn-cwd-fallback"); got != 1 {
		t.Fatalf("packaging did not reach the enabled repo: %d manifests for turn, want 1", got)
	}
	usageDB, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	var input, output, cacheRead, cacheWrite sql.NullInt64
	if err := usageDB.DB.QueryRowContext(ctx,
		"select tokens_in, tokens_out, tokens_cache_read, tokens_cache_create from provenance_manifests where turn_id = ?",
		"turn-cwd-fallback",
	).Scan(&input, &output, &cacheRead, &cacheWrite); err != nil {
		t.Fatal(err)
	}
	if err := sqlstore.Close(usageDB); err != nil {
		t.Fatal(err)
	}
	if !input.Valid || input.Int64 != 10 || !output.Valid || output.Int64 != 20 ||
		!cacheRead.Valid || cacheRead.Int64 != 0 || !cacheWrite.Valid || cacheWrite.Int64 != 0 {
		t.Fatalf("manifest usage = %v/%v/%v/%v, want 10/20/0/0", input, output, cacheRead, cacheWrite)
	}

	// Packaging requires a CWD from either the event or capture state.
	packageTurnFromState(ctx, provider, &Event{SessionID: "ps-1"}, bh, nil, &CaptureState{
		TurnID: "turn-no-cwd", PromptSubmittedAt: 1,
	})
	if got := manifestCount("turn-no-cwd"); got != 0 {
		t.Fatalf("packaging ran without any CWD: %d manifests, want 0", got)
	}
}

func TestPackageTurnFromState_PackagesRepositoriesWithTurnEvents(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)

	a := newToolWindowWorld(t, home, "repoA")
	b := newToolWindowWorldAt(t, a.bh, filepath.Join(t.TempDir(), "repoB"))
	c := newToolWindowWorldAt(t, a.bh, filepath.Join(t.TempDir(), "repoC"))

	source, err := blobs.NewStore(filepath.Join(home, "hook-objects"))
	if err != nil {
		t.Fatal(err)
	}
	promptBytes := []byte(`{"version":1,"kind":"prompt","text":"update generated code"}`)
	promptHash, _, err := source.Put(ctx, promptBytes)
	if err != nil {
		t.Fatal(err)
	}
	responseBytes := []byte(`{"version":1,"kind":"turn_response","text":"done"}`)
	responseHash, _, err := source.Put(ctx, responseBytes)
	if err != nil {
		t.Fatal(err)
	}

	const (
		provider  = "cursor"
		sessionID = "cursor-session"
		turnID    = "turn-cross-repo"
	)
	baseEvent := broker.RawEvent{
		Provider: provider, SourceKey: "/data/cursor-session.jsonl",
		ProviderSessionID: sessionID, SessionStartedAt: 100,
		SessionMetaJSON: `{}`, SourceProjectPath: a.repoPath,
		Timestamp: 1000, TurnID: turnID, EventSource: "hook",
	}
	promptEvent := baseEvent
	promptEvent.EventID = "prompt-event"
	promptEvent.Kind = "user"
	promptEvent.Role = "user"
	promptEvent.PayloadHash = promptHash
	if _, err := broker.WriteEventsToRepo(ctx, a.repoPath, []broker.RawEvent{promptEvent}, source); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	usageEvent := baseEvent
	usageEvent.EventID = "usage-event"
	usageEvent.Kind = "assistant"
	usageEvent.Role = "assistant"
	usageEvent.TokenUsageValid = true
	usageEvent.TokensIn = 10
	usageEvent.TokensOut = 20
	usageEvent.TokensCacheRead = 30
	usageEvent.TokensCacheCreate = 40
	if _, err := broker.WriteEventsToRepo(ctx, a.repoPath, []broker.RawEvent{usageEvent}, source); err != nil {
		t.Fatalf("write token usage: %v", err)
	}

	stepEvent := baseEvent
	stepEvent.EventID = "step-event"
	stepEvent.Kind = "assistant"
	stepEvent.Role = "assistant"
	stepEvent.ToolName = "Bash"
	stepEvent.ToolUseID = "tool-call"
	if _, err := broker.WriteEventsToRepo(ctx, b.repoPath, []broker.RawEvent{stepEvent}, source); err != nil {
		t.Fatalf("write routed step: %v", err)
	}

	otherTurn := baseEvent
	otherTurn.EventID = "other-turn-event"
	otherTurn.TurnID = "other-turn"
	otherTurn.Kind = "assistant"
	otherTurn.Role = "assistant"
	otherTurn.ToolName = "Bash"
	otherTurn.ToolUseID = "other-tool"
	if _, err := broker.WriteEventsToRepo(ctx, c.repoPath, []broker.RawEvent{otherTurn}, source); err != nil {
		t.Fatalf("write unrelated turn: %v", err)
	}
	if err := broker.Deactivate(ctx, a.bh, b.repoPath); err != nil {
		t.Fatalf("deactivate routed repository: %v", err)
	}

	packageTurnFromState(ctx, &fakeProvider{name: provider}, &Event{
		SessionID: sessionID,
		CWD:       a.repoPath,
	}, a.bh, source, &CaptureState{
		TurnID:              turnID,
		PromptSubmittedAt:   1000,
		CWD:                 a.repoPath,
		ResponseStatus:      "complete",
		ResponseHash:        responseHash,
		ResponseSummary:     "done",
		ResponseEventID:     "response-event",
		ResponseCompletedAt: 1100,
	})

	type bundleView struct {
		Prompt *struct {
			EventID  string `json:"event_id"`
			BlobHash string `json:"blob_hash"`
		} `json:"prompt"`
		Steps []struct {
			EventID string `json:"event_id"`
		} `json:"steps"`
		Response *struct {
			EventID string `json:"event_id"`
			Hash    string `json:"hash"`
			Status  string `json:"status"`
		} `json:"response"`
	}
	readBundle := func(repo *toolWindowWorld) (bundleView, bool) {
		t.Helper()
		h, err := sqlstore.Open(ctx, filepath.Join(repo.semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = sqlstore.Close(h) }()
		var hash string
		err = h.DB.QueryRowContext(ctx,
			`select provenance_bundle_hash from provenance_manifests where turn_id = ? and kind = 'turn_bundle'`, turnID,
		).Scan(&hash)
		if err == sql.ErrNoRows {
			return bundleView{}, false
		}
		if err != nil {
			t.Fatal(err)
		}
		store, err := blobs.NewStore(filepath.Join(repo.semDir, "objects"))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := store.Get(ctx, hash)
		if err != nil {
			t.Fatal(err)
		}
		var bundle bundleView
		if err := json.Unmarshal(raw, &bundle); err != nil {
			t.Fatal(err)
		}
		return bundle, true
	}

	if _, ok := readBundle(a); !ok {
		t.Fatal("source repository has no turn bundle")
	}
	bundle, ok := readBundle(b)
	if !ok {
		t.Fatal("routed repository has no turn bundle")
	}
	if bundle.Prompt == nil || bundle.Prompt.EventID != "prompt-event" || bundle.Prompt.BlobHash != promptHash {
		t.Fatalf("routed prompt = %+v, want source prompt", bundle.Prompt)
	}
	if len(bundle.Steps) != 1 || bundle.Steps[0].EventID != "step-event" {
		t.Fatalf("routed steps = %+v, want step-event", bundle.Steps)
	}
	if bundle.Response == nil || bundle.Response.EventID != "response-event" || bundle.Response.Hash != responseHash || bundle.Response.Status != "complete" {
		t.Fatalf("routed response = %+v, want source response", bundle.Response)
	}
	if _, ok := readBundle(c); ok {
		t.Fatal("unrelated repository received a turn bundle")
	}
	assertUsage := func(repo *toolWindowWorld) {
		t.Helper()
		h, err := sqlstore.Open(ctx, filepath.Join(repo.semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = sqlstore.Close(h) }()
		var in, out, read, write sql.NullInt64
		if err := h.DB.QueryRowContext(ctx,
			`select tokens_in, tokens_out, tokens_cache_read, tokens_cache_create from provenance_manifests where turn_id = ?`, turnID,
		).Scan(&in, &out, &read, &write); err != nil {
			t.Fatal(err)
		}
		if !in.Valid || in.Int64 != 10 || out.Int64 != 20 || read.Int64 != 30 || write.Int64 != 40 {
			t.Fatalf("%s usage = %v/%v/%v/%v", repo.repoPath, in, out, read, write)
		}
	}
	assertUsage(a)
	assertUsage(b)

	bStore, err := blobs.NewStore(filepath.Join(b.semDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	gotPrompt, err := bStore.Get(ctx, promptHash)
	if err != nil || string(gotPrompt) != string(promptBytes) {
		t.Fatalf("routed prompt object = %q, %v", gotPrompt, err)
	}
	gotResponse, err := bStore.Get(ctx, responseHash)
	if err != nil || string(gotResponse) != string(responseBytes) {
		t.Fatalf("routed response object = %q, %v", gotResponse, err)
	}

	bh, err := sqlstore.Open(ctx, filepath.Join(b.semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(bh) }()
	coverage, err := bh.Queries.CountWindowTurnProvenance(ctx, sqldb.CountWindowTurnProvenanceParams{
		RepositoryID: b.repoID,
		AfterTs:      0,
		UpToTs:       2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if coverage.TotalTurns != 1 || coverage.PackagedTurns != 1 {
		t.Fatalf("routed coverage = %+v, want one packaged turn", coverage)
	}
	sess, err := bh.Queries.GetAgentSessionByProviderID(ctx, sqldb.GetAgentSessionByProviderIDParams{
		RepositoryID:      b.repoID,
		Provider:          provider,
		ProviderSessionID: sessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sess.SourceRepoPath.Valid || !sameRepoPath(sess.SourceRepoPath.String, a.repoPath) {
		t.Fatalf("source_repo_path = %q, want %q", sess.SourceRepoPath.String, a.repoPath)
	}
}

func TestPackageTurnFromState_PackagesCrossRepoTurnWithoutUsablePrompt(t *testing.T) {
	tests := []struct {
		name       string
		withPrompt bool
	}{
		{name: "no source prompt"},
		{name: "prompt without payload", withPrompt: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			home := t.TempDir()
			t.Setenv("SEMANTICA_HOME", home)

			a := newToolWindowWorld(t, home, "repoA")
			b := newToolWindowWorldAt(t, a.bh, filepath.Join(t.TempDir(), "repoB"))
			source, err := blobs.NewStore(filepath.Join(home, "hook-objects"))
			if err != nil {
				t.Fatal(err)
			}

			const (
				provider  = "cursor"
				sessionID = "cursor-session"
				turnID    = "turn-without-prompt"
			)
			baseEvent := broker.RawEvent{
				Provider: provider, SourceKey: "/data/cursor-session.jsonl",
				ProviderSessionID: sessionID, SessionStartedAt: 100,
				SessionMetaJSON: `{}`, SourceProjectPath: a.repoPath,
				Timestamp: 1000, TurnID: turnID, EventSource: "hook",
			}
			originEvent := baseEvent
			originEvent.EventID = "origin-event"
			originEvent.Kind = "assistant"
			originEvent.Role = "assistant"
			if tt.withPrompt {
				originEvent.EventID = "prompt-event"
				originEvent.Kind = "user"
				originEvent.Role = "user"
			}
			if _, err := broker.WriteEventsToRepo(ctx, a.repoPath, []broker.RawEvent{originEvent}, source); err != nil {
				t.Fatalf("write origin event: %v", err)
			}

			stepEvent := baseEvent
			stepEvent.EventID = "step-event"
			stepEvent.Kind = "assistant"
			stepEvent.Role = "assistant"
			stepEvent.ToolName = "Bash"
			stepEvent.ToolUseID = "tool-call"
			if _, err := broker.WriteEventsToRepo(ctx, b.repoPath, []broker.RawEvent{stepEvent}, source); err != nil {
				t.Fatalf("write routed step: %v", err)
			}

			packageTurnFromState(ctx, &fakeProvider{name: provider}, &Event{
				SessionID: sessionID,
				CWD:       a.repoPath,
			}, a.bh, source, &CaptureState{
				TurnID:            turnID,
				PromptSubmittedAt: 1000,
				CWD:               a.repoPath,
			})

			h, err := sqlstore.Open(ctx, filepath.Join(b.semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sqlstore.Close(h) }()
			var bundleHash string
			if err := h.DB.QueryRowContext(ctx,
				`select provenance_bundle_hash from provenance_manifests where turn_id = ? and kind = 'turn_bundle'`, turnID,
			).Scan(&bundleHash); err != nil {
				t.Fatalf("read routed bundle: %v", err)
			}
			store, err := blobs.NewStore(filepath.Join(b.semDir, "objects"))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := store.Get(ctx, bundleHash)
			if err != nil {
				t.Fatal(err)
			}
			var bundle struct {
				Prompt json.RawMessage `json:"prompt"`
				Steps  []struct {
					EventID string `json:"event_id"`
				} `json:"steps"`
			}
			if err := json.Unmarshal(raw, &bundle); err != nil {
				t.Fatal(err)
			}
			if len(bundle.Prompt) != 0 {
				t.Fatalf("routed prompt = %s, want omitted", bundle.Prompt)
			}
			if len(bundle.Steps) != 1 || bundle.Steps[0].EventID != "step-event" {
				t.Fatalf("routed steps = %+v, want step-event", bundle.Steps)
			}
		})
	}
}

func TestCaptureAndRouteForRepo_DefersCrossRepoEvents(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	repoA, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repoB, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bh, err := broker.Open(ctx, filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = broker.Close(bh) }()
	for _, r := range []string{repoA, repoB} {
		if err := broker.Register(ctx, bh, r, r); err != nil {
			t.Fatal(err)
		}
	}

	if err := SaveCaptureState(&CaptureState{
		SessionID: "scoped-1", Provider: "fake", TranscriptRef: "t",
		TranscriptOffset: 7, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}

	provider := &fakeProvider{
		name: "fake",
		readSequence: []fakeReadResult{{
			events: []broker.RawEvent{{
				EventID: "ev1", Provider: "fake", Timestamp: 1,
				Kind: "assistant", FilePaths: []string{filepath.Join(repoB, "main.go")},
			}},
			offset: 99,
		}},
	}

	captured, err := CaptureAndRouteForRepo(ctx, provider,
		&Event{SessionID: "scoped-1", TranscriptRef: "t"}, bh, nil, repoA)
	if err != nil {
		t.Fatalf("scoped capture: %v", err)
	}
	if captured {
		t.Fatal("events routing to another repository must defer, not capture")
	}
	state, err := LoadCaptureState("scoped-1")
	if err != nil {
		t.Fatal(err)
	}
	if state.TranscriptOffset != 7 {
		t.Fatalf("offset advanced to %d on a deferred session; events would be lost", state.TranscriptOffset)
	}
	if state.ScopedDeferrals != 1 || state.LastDeferredAt == 0 {
		t.Fatalf("deferral not recorded: deferrals=%d lastDeferredAt=%d",
			state.ScopedDeferrals, state.LastDeferredAt)
	}
}

func TestDispatch_PromptPreservesDeferredCaptureState(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	bh, err := broker.Open(ctx, filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = broker.Close(bh) }()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "defer-1", Provider: "fake", TranscriptRef: "t",
		TranscriptOffset: 7, Timestamp: 1,
		ScopedDeferrals: 2, LastDeferredAt: 99,
	}); err != nil {
		t.Fatal(err)
	}

	provider := &fakeProvider{name: "fake", transcriptOffset: 42} // current EOF
	if err := Dispatch(ctx, provider, &Event{
		Type: PromptSubmitted, SessionID: "defer-1", TranscriptRef: "t",
		Timestamp: 123, CWD: "/some/repo",
	}, bh, nil); err != nil {
		t.Fatalf("dispatch prompt: %v", err)
	}

	state, err := LoadCaptureState("defer-1")
	if err != nil {
		t.Fatal(err)
	}
	if state.TranscriptOffset != 7 {
		t.Fatalf("offset = %d, want preserved 7: resetting to EOF skips the deferred segment", state.TranscriptOffset)
	}
	if state.ScopedDeferrals != 2 || state.LastDeferredAt != 99 {
		t.Fatalf("deferral record lost: deferrals=%d lastDeferredAt=%d", state.ScopedDeferrals, state.LastDeferredAt)
	}
	if state.TurnID == "" || state.PromptSubmittedAt != 123 {
		t.Fatalf("prompt fields should refresh: turnID=%q promptAt=%d", state.TurnID, state.PromptSubmittedAt)
	}
}

func TestDispatch_PromptResetsOffsetForNewTranscript(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	bh, err := broker.Open(ctx, filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = broker.Close(bh) }()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "defer-2", Provider: "fake", TranscriptRef: "old-t",
		TranscriptOffset: 7, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}

	provider := &fakeProvider{name: "fake", transcriptOffset: 42}
	if err := Dispatch(ctx, provider, &Event{
		Type: PromptSubmitted, SessionID: "defer-2", TranscriptRef: "new-t",
		Timestamp: 123,
	}, bh, nil); err != nil {
		t.Fatalf("dispatch prompt: %v", err)
	}

	state, err := LoadCaptureState("defer-2")
	if err != nil {
		t.Fatal(err)
	}
	if state.TranscriptOffset != 42 {
		t.Fatalf("offset = %d, want 42: a new transcript starts at its own EOF", state.TranscriptOffset)
	}
}

func TestCaptureAndRoute_RecoveredSegmentKeepsOldTurn(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	repoDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	semDir := filepath.Join(repoDir, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(semDir, "lineage.db")
	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatal(err)
	}
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: "repo-rec", RootPath: repoDir, CreatedAt: 1, EnabledAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := sqlstore.Close(h); err != nil {
		t.Fatal(err)
	}

	bh, err := broker.Open(ctx, filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = broker.Close(bh) }()
	if err := broker.Register(ctx, bh, repoDir, repoDir); err != nil {
		t.Fatal(err)
	}

	if err := SaveCaptureState(&CaptureState{
		SessionID: "s-rec", Provider: "fake", TranscriptRef: "t",
		TranscriptOffset: 0, Timestamp: 100,
		TurnID: "turn-old", PromptSubmittedAt: 100, CWD: repoDir,
	}); err != nil {
		t.Fatal(err)
	}

	provider := &fakeProvider{name: "fake", transcriptOffset: 5}
	if err := Dispatch(ctx, provider, &Event{
		Type: PromptSubmitted, SessionID: "s-rec", TranscriptRef: "t",
		Timestamp: 200, CWD: repoDir,
	}, bh, nil); err != nil {
		t.Fatal(err)
	}
	state, err := LoadCaptureState("s-rec")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.PendingTurns) != 1 || state.PendingTurns[0].TurnID != "turn-old" || state.TranscriptOffset != 0 {
		t.Fatalf("prompt did not preserve the interrupted turn: %+v", state)
	}
	newTurnID := state.TurnID

	eOld := broker.RawEvent{
		EventID: "e-old", SourceKey: "sk", Provider: "fake",
		ProviderSessionID: "ps-rec", Timestamp: 150,
		Kind: "assistant", Role: "assistant",
		FilePaths: []string{filepath.Join(repoDir, "f.go")},
	}
	eNew := broker.RawEvent{
		EventID: "e-new", SourceKey: "sk", Provider: "fake",
		ProviderSessionID: "ps-rec", Timestamp: 250,
		Kind: "assistant", Role: "assistant",
		FilePaths: []string{filepath.Join(repoDir, "f.go")},
	}
	provider.transcriptOffset = 10 // post-capture EOF
	provider.readByOffset = map[int][]broker.RawEvent{
		0: {eOld, eNew}, // full unresolved segment
		5: {eNew},       // the new turn's segment
	}
	if err := CaptureAndRoute(ctx, provider, &Event{
		SessionID: "s-rec", TranscriptRef: "t",
	}, bh, nil); err != nil {
		t.Fatalf("completion capture: %v", err)
	}

	h, err = sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	turnFor := func(eventID string) string {
		var turn string
		if err := h.DB.QueryRowContext(ctx,
			"select coalesce(turn_id, '') from agent_events where event_id like ?", "%"+eventID+"%",
		).Scan(&turn); err != nil {
			t.Fatalf("read %s: %v", eventID, err)
		}
		return turn
	}
	if got := turnFor("e-old"); got != "turn-old" {
		t.Errorf("recovered event turn = %q, want turn-old (interrupted turn keeps its evidence)", got)
	}
	if got := turnFor("e-new"); got != newTurnID {
		t.Errorf("new-turn event turn = %q, want %q", got, newTurnID)
	}

	state, err = LoadCaptureState("s-rec")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.PendingTurns) != 0 || state.TranscriptOffset != 10 {
		t.Errorf("post-capture state: %+v, want cleared pending turns and offset 10", state)
	}
}

func TestDispatch_PromptOrphansDeferredSegmentOnNewTranscript(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	bh, err := broker.Open(ctx, filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = broker.Close(bh) }()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "s-orph", Provider: "fake", TranscriptRef: "old-t",
		TranscriptOffset: 7, Timestamp: 1,
		ScopedDeferrals: 2, LastDeferredAt: 99,
	}); err != nil {
		t.Fatal(err)
	}

	provider := &fakeProvider{name: "fake", transcriptOffset: 42}
	if err := Dispatch(ctx, provider, &Event{
		Type: PromptSubmitted, SessionID: "s-orph", TranscriptRef: "new-t", Timestamp: 123,
	}, bh, nil); err != nil {
		t.Fatal(err)
	}

	state, err := LoadCaptureState("s-orph")
	if err != nil {
		t.Fatal(err)
	}
	if state.ScopedDeferrals != 0 || state.TranscriptOffset != 42 {
		t.Fatalf("new-transcript state should be fresh: %+v", state)
	}

	active, err := LoadActiveCaptureStates()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range active {
		if s.OrphanedAt != 0 {
			t.Fatalf("orphan leaked into active states: %+v", s)
		}
	}
	orphans, err := LoadOrphanedCaptureStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 {
		t.Fatalf("orphan snapshot missing: %v", orphans)
	}
	orphan := orphans[0]
	if orphan.TranscriptRef != "old-t" || orphan.TranscriptOffset != 7 || orphan.ScopedDeferrals != 2 {
		t.Fatalf("orphan must keep the unresolved segment: %+v", orphan)
	}
}

func TestStampTurnIDs_BoundaryOwnership(t *testing.T) {
	state := &CaptureState{
		TurnID: "t3", PromptSubmittedAt: 300,
		PendingTurns: []PendingTurnBoundary{
			{TurnID: "t1", PromptSubmittedAt: 100},
			{TurnID: "t2", PromptSubmittedAt: 200},
		},
	}
	events := []broker.RawEvent{
		{EventID: "ancient", Timestamp: 50}, // predates every boundary
		{EventID: "in-t1", Timestamp: 150},
		{EventID: "in-t2", Timestamp: 250},
		{EventID: "at-prompt", Timestamp: 300},
		{EventID: "in-t3", Timestamp: 350},
		{EventID: "no-ts"},
		{EventID: "own-turn", Timestamp: 150, TurnID: "already"},
	}
	stampTurnIDs(events, state)
	want := map[string]string{
		"ancient": "", "in-t1": "t1", "in-t2": "t2",
		"at-prompt": "", "in-t3": "t3", "no-ts": "", "own-turn": "already",
	}
	for _, ev := range events {
		if ev.TurnID != want[ev.EventID] {
			t.Errorf("%s: turn = %q, want %q", ev.EventID, ev.TurnID, want[ev.EventID])
		}
	}
}

func TestDispatch_ThreePromptsPreserveEveryTurnBoundary(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	bh, err := broker.Open(ctx, filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = broker.Close(bh) }()

	provider := &fakeProvider{name: "fake"}
	prompt := func(ts int64, eof int) *CaptureState {
		t.Helper()
		provider.transcriptOffset = eof
		if err := Dispatch(ctx, provider, &Event{
			Type: PromptSubmitted, SessionID: "s-three", TranscriptRef: "t", Timestamp: ts,
		}, bh, nil); err != nil {
			t.Fatal(err)
		}
		state, err := LoadCaptureState("s-three")
		if err != nil {
			t.Fatal(err)
		}
		return state
	}

	s1 := prompt(100, 0)  // turn 1 starts at offset 0
	s2 := prompt(200, 10) // turn 1 never captured; EOF moved to 10
	s3 := prompt(300, 20) // turn 2 never captured either

	if len(s3.PendingTurns) != 2 {
		t.Fatalf("pending turns = %+v, want two preserved boundaries", s3.PendingTurns)
	}
	if s3.PendingTurns[0].TurnID != s1.TurnID || s3.PendingTurns[1].TurnID != s2.TurnID {
		t.Fatalf("boundaries %+v must keep turn1 %s then turn2 %s", s3.PendingTurns, s1.TurnID, s2.TurnID)
	}
	if s3.TranscriptOffset != 0 {
		t.Fatalf("offset = %d, want 0 (oldest unresolved segment)", s3.TranscriptOffset)
	}

	events := []broker.RawEvent{
		{EventID: "e1", Timestamp: 150},
		{EventID: "e2", Timestamp: 250},
		{EventID: "e3", Timestamp: 350},
	}
	stampTurnIDs(events, s3)
	if events[0].TurnID != s1.TurnID || events[1].TurnID != s2.TurnID || events[2].TurnID != s3.TurnID {
		t.Fatalf("stamped turns [%s %s %s], want [%s %s %s]",
			events[0].TurnID, events[1].TurnID, events[2].TurnID,
			s1.TurnID, s2.TurnID, s3.TurnID)
	}
}

func TestDispatch_PromptFailsClosedWhenOrphanSnapshotFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("read-only directory permissions are unreliable on Windows")
	}
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)

	bh, err := broker.Open(ctx, filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = broker.Close(bh) }()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "s-fail", Provider: "fake", TranscriptRef: "old-t",
		TranscriptOffset: 7, Timestamp: 1, ScopedDeferrals: 2, LastDeferredAt: 99,
	}); err != nil {
		t.Fatal(err)
	}

	captureDirPath := filepath.Join(home, "capture")
	if err := os.Chmod(captureDirPath, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(captureDirPath, 0o755) })

	provider := &fakeProvider{name: "fake", transcriptOffset: 42}
	err = Dispatch(ctx, provider, &Event{
		Type: PromptSubmitted, SessionID: "s-fail", TranscriptRef: "new-t", Timestamp: 123,
	}, bh, nil)
	if err == nil {
		t.Fatal("prompt must fail closed when the orphan snapshot cannot be written")
	}

	_ = os.Chmod(captureDirPath, 0o755)
	state, lerr := LoadCaptureState("s-fail")
	if lerr != nil {
		t.Fatal(lerr)
	}
	if state.TranscriptRef != "old-t" || state.TranscriptOffset != 7 || state.ScopedDeferrals != 2 {
		t.Fatalf("old state must survive a failed orphan snapshot: %+v", state)
	}
}

func TestCaptureAndRoute_SameMillisecondPromptsResolveByOffset(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	repoDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	semDir := filepath.Join(repoDir, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(semDir, "lineage.db")
	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatal(err)
	}
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: "repo-ms", RootPath: repoDir, CreatedAt: 1, EnabledAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := sqlstore.Close(h); err != nil {
		t.Fatal(err)
	}
	bh, err := broker.Open(ctx, filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = broker.Close(bh) }()
	if err := broker.Register(ctx, bh, repoDir, repoDir); err != nil {
		t.Fatal(err)
	}

	const sharedTs = int64(500)
	provider := &fakeProvider{name: "fake"}
	prompt := func(eof int) string {
		t.Helper()
		provider.transcriptOffset = eof
		if err := Dispatch(ctx, provider, &Event{
			Type: PromptSubmitted, SessionID: "s-ms", TranscriptRef: "t",
			Timestamp: sharedTs, CWD: repoDir,
		}, bh, nil); err != nil {
			t.Fatal(err)
		}
		state, err := LoadCaptureState("s-ms")
		if err != nil {
			t.Fatal(err)
		}
		return state.TurnID
	}
	turn1 := prompt(0)
	turn2 := prompt(10)
	turn3 := prompt(20)

	mkEvent := func(id string) broker.RawEvent {
		return broker.RawEvent{
			EventID: id, SourceKey: "sk", Provider: "fake",
			ProviderSessionID: "ps-ms", Timestamp: sharedTs,
			Kind: "assistant", Role: "assistant",
			FilePaths: []string{filepath.Join(repoDir, "f.go")},
		}
	}
	e1, e2, e3 := mkEvent("e-seg1"), mkEvent("e-seg2"), mkEvent("e-seg3")
	provider.transcriptOffset = 30
	provider.readByOffset = map[int][]broker.RawEvent{
		0:  {e1, e2, e3},
		10: {e2, e3},
		20: {e3},
	}

	if err := CaptureAndRoute(ctx, provider, &Event{
		SessionID: "s-ms", TranscriptRef: "t",
	}, bh, nil); err != nil {
		t.Fatalf("capture: %v", err)
	}

	h, err = sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	turnFor := func(eventID string) string {
		var turn string
		if err := h.DB.QueryRowContext(ctx,
			"select coalesce(turn_id, '') from agent_events where event_id like ?", "%"+eventID+"%",
		).Scan(&turn); err != nil {
			t.Fatalf("read %s: %v", eventID, err)
		}
		return turn
	}
	if got := turnFor("e-seg1"); got != turn1 {
		t.Errorf("segment-1 event turn = %q, want %q", got, turn1)
	}
	if got := turnFor("e-seg2"); got != turn2 {
		t.Errorf("segment-2 event turn = %q, want %q", got, turn2)
	}
	if got := turnFor("e-seg3"); got != turn3 {
		t.Errorf("segment-3 event turn = %q, want %q", got, turn3)
	}
}

func TestCaptureAndRoute_EmptyReplayClearsRecoveryState(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	bh, err := broker.Open(ctx, filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = broker.Close(bh) }()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "s-empty", Provider: "fake", TranscriptRef: "t",
		TranscriptOffset: 3, Timestamp: 1,
		TurnID: "turn-now", PromptSubmittedAt: 200, TurnStartOffset: 5,
		PendingTurns:    []PendingTurnBoundary{{TurnID: "turn-old", PromptSubmittedAt: 100, StartOffset: 1}},
		ScopedDeferrals: 2, LastDeferredAt: 99,
	}); err != nil {
		t.Fatal(err)
	}

	provider := &fakeProvider{name: "fake", readSequence: []fakeReadResult{{events: nil, offset: 9}}}
	if err := CaptureAndRoute(ctx, provider, &Event{
		SessionID: "s-empty", TranscriptRef: "t",
	}, bh, nil); err != nil {
		t.Fatalf("empty capture: %v", err)
	}

	state, err := LoadCaptureState("s-empty")
	if err != nil {
		t.Fatal(err)
	}
	if state.TranscriptOffset != 9 {
		t.Errorf("offset = %d, want 9", state.TranscriptOffset)
	}
	if len(state.PendingTurns) != 0 || state.ScopedDeferrals != 0 || state.LastDeferredAt != 0 {
		t.Errorf("recovery state not cleared by empty replay: %+v", state)
	}
}

func TestDispatch_RepeatedOrphansDoNotOverwrite(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	bh, err := broker.Open(ctx, filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = broker.Close(bh) }()

	provider := &fakeProvider{name: "fake", transcriptOffset: 42}
	orphanRound := func(oldRef, newRef string) {
		t.Helper()
		if err := SaveCaptureState(&CaptureState{
			SessionID: "s-multi", Provider: "fake", TranscriptRef: oldRef,
			TranscriptOffset: 7, Timestamp: 1, ScopedDeferrals: 1, LastDeferredAt: 9,
		}); err != nil {
			t.Fatal(err)
		}
		if err := Dispatch(ctx, provider, &Event{
			Type: PromptSubmitted, SessionID: "s-multi", TranscriptRef: newRef, Timestamp: 123,
		}, bh, nil); err != nil {
			t.Fatal(err)
		}
	}
	orphanRound("t-1", "t-2")
	orphanRound("t-2", "t-3")

	orphans, err := LoadOrphanedCaptureStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 2 {
		t.Fatalf("orphans = %d, want 2 distinct snapshots", len(orphans))
	}
}

type fakeUnpositionedProvider struct {
	fakeProvider
}

func (f *fakeUnpositionedProvider) OffsetReadsAuthoritative() bool { return false }

func TestStampFallback_NonAuthoritativeProviderNeverProbes(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	repoDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	semDir := filepath.Join(repoDir, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(semDir, "lineage.db")
	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatal(err)
	}
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: "repo-np", RootPath: repoDir, CreatedAt: 1, EnabledAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := sqlstore.Close(h); err != nil {
		t.Fatal(err)
	}
	bh, err := broker.Open(ctx, filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = broker.Close(bh) }()
	if err := broker.Register(ctx, bh, repoDir, repoDir); err != nil {
		t.Fatal(err)
	}

	const sharedTs = int64(500)
	if err := SaveCaptureState(&CaptureState{
		SessionID: "s-np", Provider: "fake", TranscriptRef: "t",
		TranscriptOffset: 0, Timestamp: 1, CWD: repoDir,
		TurnID: "turn-new", PromptSubmittedAt: sharedTs, TurnStartOffset: 10,
		PendingTurns: []PendingTurnBoundary{{TurnID: "turn-old", PromptSubmittedAt: sharedTs, StartOffset: 1}},
	}); err != nil {
		t.Fatal(err)
	}

	ev := broker.RawEvent{
		EventID: "e-ambig", SourceKey: "sk", Provider: "fake",
		ProviderSessionID: "ps-np", Timestamp: sharedTs,
		Kind: "assistant", Role: "assistant",
		FilePaths: []string{filepath.Join(repoDir, "f.go")},
	}
	provider := &fakeUnpositionedProvider{fakeProvider{
		name:             "fake",
		transcriptOffset: 20,
		readByOffset:     map[int][]broker.RawEvent{0: {ev}, 1: {ev}, 10: {ev}},
	}}

	if err := CaptureAndRoute(ctx, provider, &Event{
		SessionID: "s-np", TranscriptRef: "t",
	}, bh, nil); err != nil {
		t.Fatal(err)
	}
	if provider.readCalls != 1 {
		t.Fatalf("non-authoritative provider probed: %d reads beyond canonical", provider.readCalls-1)
	}

	h, err = sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	var turn string
	if err := h.DB.QueryRowContext(ctx,
		"select coalesce(turn_id, '') from agent_events where event_id like '%e-ambig%'",
	).Scan(&turn); err != nil {
		t.Fatalf("read e-ambig: %v", err)
	}
	if turn != "" {
		t.Fatalf("ambiguous event turn = %q, want unowned (timestamp collision, no offset authority)", turn)
	}
}

func TestCanProbeOffsets_IncoherentChainsFallBack(t *testing.T) {
	base := func() *CaptureState {
		return &CaptureState{
			TurnStartOffset: 30,
			PendingTurns: []PendingTurnBoundary{
				{TurnID: "t1", StartOffset: 0},
				{TurnID: "t2", StartOffset: 10},
			},
		}
	}
	if s := base(); !canProbeOffsets(s, 40) {
		t.Error("coherent chain should probe")
	}
	if s := base(); canProbeOffsets(s, 20) {
		t.Error("current turn start beyond transcript end (compaction) must fall back")
	}
	s := base()
	s.PendingTurns[1].StartOffset = 35 // exceeds the current turn's start
	if canProbeOffsets(s, 40) {
		t.Error("boundary beyond current turn start must fall back")
	}
	s = base()
	s.PendingTurns[1].StartOffset = 0 // not strictly increasing
	if canProbeOffsets(s, 40) {
		t.Error("non-monotonic boundaries must fall back")
	}
	s = base()
	s.PendingTurns = make([]PendingTurnBoundary, maxOwnershipProbes+1)
	for i := range s.PendingTurns {
		s.PendingTurns[i] = PendingTurnBoundary{TurnID: "t", StartOffset: i + 1}
	}
	if canProbeOffsets(s, 40) {
		t.Error("probe count beyond the bound must fall back")
	}
}
