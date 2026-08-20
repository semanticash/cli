package hooks

import (
	"context"
	"errors"
	"testing"

	"github.com/semanticash/cli/internal/store/blobs"
)

// Response metadata survives capture state and turn-context conversion.
func TestApplyResponseCandidateAndBuildTurnContext(t *testing.T) {
	ctx := context.Background()
	bs, err := blobs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	state := &CaptureState{SessionID: "s", Provider: "codex", TurnID: "turn-1"}
	text := "the final answer"
	event := &Event{SessionID: "s", Timestamp: 900, Response: &text}

	applyResponseCandidate(ctx, bs, state, event)
	if state.ResponseStatus != "complete" || state.ResponseHash == "" {
		t.Fatalf("state response = %+v, want complete with hash", state)
	}
	if state.ResponseCompletedAt != 900 {
		t.Errorf("completed_at = %d, want 900", state.ResponseCompletedAt)
	}

	tc := buildTurnContext(state, event, "codex")
	if tc.ResponseCandidate.Status != state.ResponseStatus ||
		tc.ResponseCandidate.Hash != state.ResponseHash ||
		tc.ResponseCandidate.Summary != state.ResponseSummary ||
		tc.ResponseCandidate.CompletedAt != state.ResponseCompletedAt {
		t.Errorf("turn context candidate = %+v, state = %+v", tc.ResponseCandidate, state)
	}

	// A nil response leaves state untouched.
	fresh := &CaptureState{SessionID: "s", Provider: "codex"}
	applyResponseCandidate(ctx, bs, fresh, &Event{})
	if fresh.ResponseStatus != "" || fresh.ResponseHash != "" {
		t.Errorf("nil response mutated state: %+v", fresh)
	}
}

// A capture failure leaves the response candidate available for recovery.
func TestDispatch_AgentCompleted_PersistsResponseWhenCaptureFails(t *testing.T) {
	setupTestCaptureDir(t)
	bs, err := blobs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// A provider whose transcript read fails makes CaptureAndRoute return an
	// error before the state is deleted.
	prov := &fakeProvider{
		name:             "codex",
		transcriptOffset: 10,
		readSequence:     []fakeReadResult{{err: errors.New("boom")}},
	}
	if err := SaveCaptureState(&CaptureState{
		SessionID: "s", Provider: "codex", TurnID: "turn-1", TranscriptRef: "/t.jsonl",
	}); err != nil {
		t.Fatal(err)
	}

	resp := "the final answer"
	event := &Event{Type: AgentCompleted, SessionID: "s", Response: &resp, Timestamp: 500}
	if err := Dispatch(context.Background(), prov, event, nil, bs); err == nil {
		t.Fatal("expected a capture failure error")
	}

	state, err := LoadCaptureState("s")
	if err != nil {
		t.Fatalf("state deleted despite capture failure: %v", err)
	}
	if state.ResponseStatus != "complete" || state.ResponseHash == "" {
		t.Errorf("response candidate not persisted for recovery: %+v", state)
	}
}
