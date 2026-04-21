package copilot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/hooks"
)

type fakeBlobPutter struct {
	stored map[string][]byte
}

func newFakeBlobPutter() *fakeBlobPutter {
	return &fakeBlobPutter{stored: make(map[string][]byte)}
}

func (f *fakeBlobPutter) Put(_ context.Context, b []byte) (string, int64, error) {
	h := "hash_" + string(rune(len(f.stored)+'a'))
	f.stored[h] = append([]byte(nil), b...)
	return h, int64(len(b)), nil
}

func TestBuildHookEvents_Write(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()
	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		SessionID: "sess-1",
		TurnID:    "turn-1",
		ToolUseID: "copilot-step-1",
		ToolName:  "Write",
		CWD:       "/repo",
		ToolInput: json.RawMessage(`{"path":"/repo/new.txt","file_text":"hello\n"}`),
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events: got %d, want 1", len(events))
	}
	ev := events[0]
	if ev.ToolName != "Write" {
		t.Errorf("tool_name: got %q, want Write", ev.ToolName)
	}
	if ev.PayloadHash == "" || ev.ProvenanceHash == "" {
		t.Fatal("expected payload and provenance hashes")
	}
	if len(ev.FilePaths) != 1 || ev.FilePaths[0] != "/repo/new.txt" {
		t.Errorf("file_paths: got %v", ev.FilePaths)
	}
	if !strings.Contains(ev.ToolUsesJSON, `"Write"`) {
		t.Errorf("tool_uses should contain Write, got %q", ev.ToolUsesJSON)
	}
}

func TestBuildHookEvents_Edit(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()
	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		SessionID: "sess-1",
		TurnID:    "turn-1",
		ToolUseID: "copilot-step-2",
		ToolName:  "Edit",
		CWD:       "/repo",
		ToolInput: json.RawMessage(`{"path":"/repo/main.go","old_str":"foo","new_str":"bar"}`),
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events: got %d, want 1", len(events))
	}
	var payload struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(bs.stored[events[0].PayloadHash], &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Type != "assistant" {
		t.Fatalf("payload type: got %q, want assistant", payload.Type)
	}
}

func TestBuildHookEvents_Bash(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()
	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		SessionID: "sess-1",
		TurnID:    "turn-1",
		ToolUseID: "copilot-step-3",
		ToolName:  "Bash",
		CWD:       "/repo",
		ToolInput: json.RawMessage(`{"command":"cat file.txt","description":"Print file"}`),
		ToolResponse: json.RawMessage(`{
			"resultType":"success",
			"textResultForLlm":"ok\n<exited with exit code 0>"
		}`),
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events: got %d, want 1", len(events))
	}
	ev := events[0]
	if ev.ToolName != "Bash" {
		t.Errorf("tool_name: got %q, want Bash", ev.ToolName)
	}
	if ev.Summary != "Print file" {
		t.Errorf("summary: got %q, want %q", ev.Summary, "Print file")
	}
}

func TestBuildHookEvents_SubagentPrompt(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()
	event := &hooks.Event{
		Type:      hooks.SubagentPromptSubmitted,
		SessionID: "sess-1",
		TurnID:    "turn-1",
		ToolUseID: "copilot-agent-1",
		ToolName:  "Agent",
		CWD:       "/repo",
		ToolInput: json.RawMessage(`{
			"description":"Create JSON",
			"prompt":"Create the json file",
			"agent_type":"general-purpose",
			"name":"json-creator"
		}`),
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events: got %d, want 1", len(events))
	}
	if events[0].ToolName != "Agent" {
		t.Errorf("tool_name: got %q, want Agent", events[0].ToolName)
	}
	if events[0].PayloadHash == "" {
		t.Fatal("expected payload hash")
	}
}

func TestBuildHookEvents_AgentCompletion(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()
	event := &hooks.Event{
		Type:      hooks.SubagentCompleted,
		SessionID: "sess-1",
		TurnID:    "turn-1",
		ToolUseID: "copilot-agent-2",
		ToolName:  "Agent",
		CWD:       "/repo",
		ToolInput: json.RawMessage(`{
			"description":"Create JSON",
			"prompt":"Create the json file",
			"agent_type":"general-purpose",
			"name":"json-creator"
		}`),
		ToolResponse: json.RawMessage(`{
			"resultType":"success",
			"textResultForLlm":"Created /repo/out.json."
		}`),
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events: got %d, want 1", len(events))
	}
	ev := events[0]
	if ev.ToolName != "Agent" {
		t.Errorf("tool_name: got %q, want Agent", ev.ToolName)
	}
	if !strings.Contains(ev.ToolUsesJSON, `"Agent"`) {
		t.Errorf("tool_uses should contain Agent, got %q", ev.ToolUsesJSON)
	}
	if ev.Summary != "Created /repo/out.json." {
		t.Errorf("summary: got %q", ev.Summary)
	}
}

// --- Prompt ---
//
// Copilot's prompt-summary truncation diverges from the raw-string
// "s[:200] + ..." rule used by the other providers. It normalizes
// whitespace (TrimSpace, replace newlines with spaces, drop
// carriage returns) and truncates without appending an ellipsis.
// These tests pin that behavior as the current baseline so the
// direct-emit unification cannot collapse it by accident.

func TestBuildHookEvents_Prompt(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	event := &hooks.Event{
		Type:      hooks.PromptSubmitted,
		SessionID: "sess-1",
		TurnID:    "turn-1",
		Prompt:    "Refactor the retry handler in payments.go",
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events: got %d, want 1", len(events))
	}

	ev := events[0]
	if ev.Role != "user" || ev.Kind != "user" {
		t.Errorf("role/kind = %q/%q, want user/user", ev.Role, ev.Kind)
	}
	if ev.TurnID != "turn-1" {
		t.Errorf("turn_id = %q, want turn-1", ev.TurnID)
	}
	if ev.EventSource != "hook" {
		t.Errorf("event_source = %q, want hook", ev.EventSource)
	}
	if ev.PayloadHash == "" {
		t.Error("expected non-empty payload_hash")
	}
	if ev.ProvenanceHash != ev.PayloadHash {
		t.Errorf("provenance_hash = %q, want equal to payload_hash %q",
			ev.ProvenanceHash, ev.PayloadHash)
	}
	if ev.Summary != "Refactor the retry handler in payments.go" {
		t.Errorf("summary = %q, want the prompt text", ev.Summary)
	}
}

func TestBuildHookEvents_PromptTruncationNoEllipsis(t *testing.T) {
	// Copilot's truncate does NOT append "..." on overflow. The raw
	// 200-char prefix is what ships. Locking this in so the
	// unification refactor cannot introduce an ellipsis silently.
	p := &Provider{}
	bs := newFakeBlobPutter()

	long := strings.Repeat("a", 250)
	event := &hooks.Event{
		Type:      hooks.PromptSubmitted,
		SessionID: "sess-1",
		Prompt:    long,
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events: got %d, want 1", len(events))
	}
	if len(events[0].Summary) != 200 {
		t.Errorf("summary length = %d, want 200 (no ellipsis)", len(events[0].Summary))
	}
	if strings.HasSuffix(events[0].Summary, "...") {
		t.Errorf("summary should not end with '...', got %q", events[0].Summary)
	}
}

func TestBuildHookEvents_PromptWhitespaceNormalization(t *testing.T) {
	// Copilot's truncate: TrimSpace on both ends, then replace '\n'
	// with a single space, then strip '\r' entirely (no substitution).
	// Note the asymmetry: '\n' becomes ' ' but '\r' is dropped, so
	// "world\rand" collapses to "worldand" with no space between them.
	// That is current behavior and this test locks it in.
	p := &Provider{}
	bs := newFakeBlobPutter()

	event := &hooks.Event{
		Type:      hooks.PromptSubmitted,
		SessionID: "sess-1",
		Prompt:    "  hello\nworld\rand more  ",
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events: got %d, want 1", len(events))
	}
	got := events[0].Summary
	want := "hello worldand more"
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestBuildHookEvents_EmptyPrompt(t *testing.T) {
	p := &Provider{}
	event := &hooks.Event{
		Type:      hooks.PromptSubmitted,
		SessionID: "sess-1",
		Prompt:    "",
	}

	events, err := p.BuildHookEvents(context.Background(), event, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for empty prompt, got %d", len(events))
	}
}

// --- Subagent prompt summary (same truncate rule as top-level prompt) ---

func TestBuildHookEvents_SubagentPromptWhitespaceNormalization(t *testing.T) {
	// buildSubagentPromptEvent also calls agentcopilot.Truncate, so
	// subagent summaries follow the same trim-and-collapse rule.
	// Input "package\r please" drops the '\r' entirely; the space
	// that was already after '\r' is preserved. Expected output has
	// a single space between "package" and "please".
	p := &Provider{}
	bs := newFakeBlobPutter()

	event := &hooks.Event{
		Type:      hooks.SubagentPromptSubmitted,
		SessionID: "sess-1",
		ToolName:  "Agent",
		ToolInput: json.RawMessage(`{
			"description":"Run review",
			"prompt":"  review\nthe auth package\r please  "
		}`),
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events: got %d, want 1", len(events))
	}
	got := events[0].Summary
	want := "review the auth package please"
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}
