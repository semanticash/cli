package claude

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/hooks"
)

// fakeBlobPutter records Put calls and returns predictable hashes.
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
		Type:          hooks.ToolStepCompleted,
		SessionID:     "sess-1",
		TurnID:        "turn-1",
		ToolUseID:     "toolu_abc",
		ToolName:      "Write",
		ToolInput:     json.RawMessage(`{"file_path":"/repo/main.go","content":"package main\n"}`),
		TranscriptRef: "/workspace/.claude/projects/test/sess-1.jsonl",
		Timestamp:     1000,
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	ev := events[0]
	if ev.Role != "assistant" {
		t.Errorf("role = %q, want assistant", ev.Role)
	}
	if ev.Kind != "assistant" {
		t.Errorf("kind = %q, want assistant", ev.Kind)
	}
	if ev.TurnID != "turn-1" {
		t.Errorf("turn_id = %q, want turn-1", ev.TurnID)
	}
	if ev.ToolUseID != "toolu_abc" {
		t.Errorf("tool_use_id = %q, want toolu_abc", ev.ToolUseID)
	}
	if ev.ToolName != "Write" {
		t.Errorf("tool_name = %q, want Write", ev.ToolName)
	}
	if ev.EventSource != "hook" {
		t.Errorf("event_source = %q, want hook", ev.EventSource)
	}
	if ev.PayloadHash == "" {
		t.Error("expected non-empty payload_hash")
	}
	if !strings.Contains(ev.ToolUsesJSON, `"Write"`) {
		t.Errorf("tool_uses should contain Write, got %q", ev.ToolUsesJSON)
	}
	if len(ev.FilePaths) != 1 || ev.FilePaths[0] != "/repo/main.go" {
		t.Errorf("file_paths = %v, want [/repo/main.go]", ev.FilePaths)
	}

	// Verify the stored blob is extractClaudeActions-compatible.
	blob := bs.stored[ev.PayloadHash]
	if blob == nil {
		t.Fatal("payload blob not stored")
	}
	var payload struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(blob, &payload); err != nil {
		t.Fatalf("unmarshal blob: %v", err)
	}
	if payload.Type != "assistant" {
		t.Errorf("blob type = %q, want assistant", payload.Type)
	}
	if len(payload.Message.Content) != 1 {
		t.Fatalf("blob content blocks = %d, want 1", len(payload.Message.Content))
	}
	block := payload.Message.Content[0]
	if block.Type != "tool_use" || block.Name != "Write" {
		t.Errorf("blob block: type=%q name=%q", block.Type, block.Name)
	}
}

func TestBuildHookEvents_Edit(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	event := &hooks.Event{
		Type:          hooks.ToolStepCompleted,
		SessionID:     "sess-1",
		TurnID:        "turn-1",
		ToolUseID:     "toolu_def",
		ToolName:      "Edit",
		ToolInput:     json.RawMessage(`{"file_path":"/repo/main.go","old_string":"foo","new_string":"bar"}`),
		TranscriptRef: "/workspace/.claude/projects/test/sess-1.jsonl",
		Timestamp:     2000,
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	ev := events[0]
	if ev.ToolName != "Edit" {
		t.Errorf("tool_name = %q, want Edit", ev.ToolName)
	}
	if !strings.Contains(ev.ToolUsesJSON, `"Edit"`) {
		t.Errorf("tool_uses should contain Edit, got %q", ev.ToolUsesJSON)
	}
	if ev.PayloadHash == "" {
		t.Error("expected non-empty payload_hash")
	}
}

func TestBuildHookEvents_Bash(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	event := &hooks.Event{
		Type:          hooks.ToolStepCompleted,
		SessionID:     "sess-1",
		TurnID:        "turn-1",
		ToolUseID:     "toolu_ghi",
		ToolName:      "Bash",
		ToolInput:     json.RawMessage(`{"command":"go test ./...","description":"Run tests"}`),
		TranscriptRef: "/workspace/.claude/projects/test/sess-1.jsonl",
		Timestamp:     3000,
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	ev := events[0]
	if ev.ToolName != "Bash" {
		t.Errorf("tool_name = %q, want Bash", ev.ToolName)
	}
	if ev.Summary != "Run tests" {
		t.Errorf("summary = %q, want 'Run tests'", ev.Summary)
	}
	// Bash events should have no file paths.
	if len(ev.FilePaths) != 0 {
		t.Errorf("file_paths = %v, want empty", ev.FilePaths)
	}
	if ev.ProvenanceHash == "" {
		t.Fatal("expected non-empty provenance_hash")
	}
	var prov struct {
		ToolInput struct {
			Description string `json:"description"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(bs.stored[ev.ProvenanceHash], &prov); err != nil {
		t.Fatalf("unmarshal provenance blob: %v", err)
	}
	if prov.ToolInput.Description != "Run tests" {
		t.Fatalf("provenance description = %q, want %q", prov.ToolInput.Description, "Run tests")
	}
}

func TestBuildHookEvents_Prompt(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	event := &hooks.Event{
		Type:          hooks.PromptSubmitted,
		SessionID:     "sess-1",
		TurnID:        "turn-1",
		Prompt:        "Create a hello world program",
		TranscriptRef: "/workspace/.claude/projects/test/sess-1.jsonl",
		Timestamp:     500,
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	ev := events[0]
	if ev.Role != "user" {
		t.Errorf("role = %q, want user", ev.Role)
	}
	if ev.Kind != "user" {
		t.Errorf("kind = %q, want user", ev.Kind)
	}
	if ev.TurnID != "turn-1" {
		t.Errorf("turn_id = %q, want turn-1", ev.TurnID)
	}
	if ev.EventSource != "hook" {
		t.Errorf("event_source = %q, want hook", ev.EventSource)
	}
	if ev.PayloadHash == "" {
		t.Error("expected non-empty payload_hash for prompt")
	}
}

func TestBuildHookEvents_SubagentPrompt(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	event := &hooks.Event{
		Type:          hooks.SubagentPromptSubmitted,
		SessionID:     "sess-1",
		TurnID:        "turn-1",
		ToolUseID:     "toolu_jkl",
		ToolName:      "Agent",
		ToolInput:     json.RawMessage(`{"prompt":"Review the code","description":"Code review task"}`),
		TranscriptRef: "/workspace/.claude/projects/test/sess-1.jsonl",
		Timestamp:     4000,
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	ev := events[0]
	if ev.ToolName != "Agent" {
		t.Errorf("tool_name = %q, want Agent", ev.ToolName)
	}
	if !strings.Contains(ev.Summary, "Review the code") {
		t.Errorf("summary should contain prompt text, got %q", ev.Summary)
	}
	if ev.PayloadHash == "" {
		t.Error("expected non-empty payload_hash for agent prompt")
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
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for empty prompt, got %d", len(events))
	}
}

func TestBuildHookEvents_BashRedaction(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	// Command contains a secret that gitleaks should detect.
	event := &hooks.Event{
		Type:          hooks.ToolStepCompleted,
		SessionID:     "sess-1",
		TurnID:        "turn-1",
		ToolUseID:     "toolu_redact",
		ToolName:      "Bash",
		ToolInput:     json.RawMessage(`{"command":"curl -H 'Authorization: Bearer ghp_1234567890abcdef1234567890abcdef12345678' https://api.github.com/user","description":""}`),
		TranscriptRef: "/workspace/.claude/projects/test/sess-1.jsonl",
		Timestamp:     5000,
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	ev := events[0]
	// The summary should contain the redacted command, not the raw token.
	if strings.Contains(ev.Summary, "ghp_1234567890abcdef") {
		t.Errorf("summary should have redacted the GitHub token, got: %s", ev.Summary)
	}
	if !strings.Contains(ev.Summary, "curl") {
		t.Errorf("summary should still contain the command structure, got: %s", ev.Summary)
	}
}

func TestBuildHookEvents_BashSafeSummary(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	// Command without secrets - description is used as summary when available.
	event := &hooks.Event{
		Type:          hooks.ToolStepCompleted,
		SessionID:     "sess-1",
		TurnID:        "turn-1",
		ToolUseID:     "toolu_safe",
		ToolName:      "Bash",
		ToolInput:     json.RawMessage(`{"command":"go test ./...","description":"Run all tests"}`),
		TranscriptRef: "/workspace/.claude/projects/test/sess-1.jsonl",
		Timestamp:     6000,
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	// When description is present, it takes priority over command.
	if events[0].Summary != "Run all tests" {
		t.Errorf("summary = %q, want 'Run all tests'", events[0].Summary)
	}
}

func TestBuildHookEvents_UnknownType(t *testing.T) {
	p := &Provider{}
	event := &hooks.Event{
		Type:      hooks.AgentCompleted,
		SessionID: "sess-1",
	}

	events, err := p.BuildHookEvents(context.Background(), event, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for AgentCompleted, got %d", len(events))
	}
}
