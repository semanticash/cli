package hooks

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/agents/api"
	"github.com/semanticash/cli/internal/broker"
)

// fakeProvider implements HookProvider for testing.
type fakeProvider struct {
	name             string
	transcriptOffset int
	events           []broker.RawEvent
	readSequence     []fakeReadResult
	readCalls        int
	readPaths        []string // tracks which transcript paths were read
	readOffsets      []int    // offsets used for each ReadFromOffset call
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

func (f *fakeProvider) Name() string        { return f.name }
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
