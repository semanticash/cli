package builder

import (
	"testing"

	"github.com/semanticash/cli/internal/hooks"
)

// --- ComputeEventID ---

// Same inputs must produce the same event ID. Downstream dedup
// depends on this: a replayed hook delivery for the same step
// produces the same event_id and INSERT OR IGNORE suppresses it.
func TestComputeEventID_Deterministic(t *testing.T) {
	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		ToolName:  "Write",
		ToolUseID: "toolu_abc",
	}
	a := ComputeEventID("src-1", event)
	b := ComputeEventID("src-1", event)
	if a != b {
		t.Errorf("event ID is not deterministic: %q vs %q", a, b)
	}
	if a == "" {
		t.Error("event ID should never be empty")
	}
}

// Different source keys yield different IDs even when the event is
// otherwise identical, so two providers processing the same tool
// step on different sessions cannot collide.
func TestComputeEventID_SourceKeySensitive(t *testing.T) {
	event := &hooks.Event{Type: hooks.ToolStepCompleted, ToolName: "Write", ToolUseID: "toolu_abc"}
	a := ComputeEventID("src-1", event)
	b := ComputeEventID("src-2", event)
	if a == b {
		t.Errorf("distinct source keys produced the same event ID: %q", a)
	}
}

// Different tool use IDs yield different event IDs.
func TestComputeEventID_ToolUseIDSensitive(t *testing.T) {
	base := &hooks.Event{Type: hooks.ToolStepCompleted, ToolName: "Write", ToolUseID: "toolu_abc"}
	other := &hooks.Event{Type: hooks.ToolStepCompleted, ToolName: "Write", ToolUseID: "toolu_xyz"}
	if ComputeEventID("src-1", base) == ComputeEventID("src-1", other) {
		t.Error("distinct tool_use_ids produced the same event ID")
	}
}

// When ToolUseID is absent, TurnID is used as the stable key. This
// covers prompt events that have no tool_use_id but do carry a turn.
func TestComputeEventID_FallsBackToTurnID(t *testing.T) {
	event := &hooks.Event{Type: hooks.PromptSubmitted, TurnID: "turn-1"}
	id := ComputeEventID("src-1", event)
	if id == "" {
		t.Error("event ID should be non-empty when TurnID is set")
	}
	// Two events with the same TurnID but no ToolUseID collide
	// (by design: they represent the same logical prompt step).
	other := &hooks.Event{Type: hooks.PromptSubmitted, TurnID: "turn-1"}
	if ComputeEventID("src-1", event) != ComputeEventID("src-1", other) {
		t.Error("same TurnID must produce the same event ID")
	}
}

// --- BaseRawEvent ---

func TestBaseRawEvent_WiresAllFields(t *testing.T) {
	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		ToolName:  "Write",
		ToolUseID: "toolu_1",
		Timestamp: 1714001234000,
		Model:     "claude-opus-4-7",
	}
	out := BaseRawEvent(BaseInput{
		Event:             event,
		SourceKey:         "src-key",
		Provider:          "test-provider",
		ProviderSessionID: "prov-sess-1",
		ParentSessionID:   "parent-sess-1",
		SessionMetaJSON:   `{"source_key":"src-key"}`,
		SourceProjectPath: "/repo",
	})

	if out.EventID == "" {
		t.Error("EventID should be set")
	}
	if out.SourceKey != "src-key" {
		t.Errorf("SourceKey = %q, want src-key", out.SourceKey)
	}
	if out.Provider != "test-provider" {
		t.Errorf("Provider = %q, want test-provider", out.Provider)
	}
	if out.ProviderSessionID != "prov-sess-1" {
		t.Errorf("ProviderSessionID = %q", out.ProviderSessionID)
	}
	if out.ParentSessionID != "parent-sess-1" {
		t.Errorf("ParentSessionID = %q", out.ParentSessionID)
	}
	if out.Timestamp != 1714001234000 {
		t.Errorf("Timestamp = %d", out.Timestamp)
	}
	if out.SessionStartedAt != 1714001234000 {
		t.Errorf("SessionStartedAt should mirror Timestamp, got %d", out.SessionStartedAt)
	}
	if out.Model != "claude-opus-4-7" {
		t.Errorf("Model = %q", out.Model)
	}
	if out.SourceProjectPath != "/repo" {
		t.Errorf("SourceProjectPath = %q", out.SourceProjectPath)
	}
	if out.SessionMetaJSON != `{"source_key":"src-key"}` {
		t.Errorf("SessionMetaJSON = %q", out.SessionMetaJSON)
	}

	// The base does not set per-event fields; callers fill these.
	if out.EventSource != "" {
		t.Errorf("EventSource must be empty in base; caller sets it")
	}
	if out.Kind != "" || out.Role != "" {
		t.Errorf("Kind/Role must be empty in base")
	}
	if out.PayloadHash != "" || out.ProvenanceHash != "" {
		t.Errorf("Hash fields must be empty in base")
	}
}

// ParentSessionID is optional; providers that do not track parent
// relationships pass "" and the field ships as empty.
func TestBaseRawEvent_ParentSessionOptional(t *testing.T) {
	out := BaseRawEvent(BaseInput{
		Event:     &hooks.Event{Type: hooks.PromptSubmitted, TurnID: "turn-1"},
		SourceKey: "src",
		Provider:  "p",
	})
	if out.ParentSessionID != "" {
		t.Errorf("ParentSessionID = %q, want empty", out.ParentSessionID)
	}
}
