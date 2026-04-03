package cursor

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

const testTranscriptRef = "/workspace/.cursor/projects/tmp-demo/agent-transcripts/conv-123/conv-123.jsonl"

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
		SessionID:     "conv-123",
		TranscriptRef: testTranscriptRef,
		Timestamp:     1000,
		TurnID:        "turn-123",
		ToolUseID:     "cursor-step-1",
		ToolName:      "Write",
		ToolInput: json.RawMessage(`{
			"conversation_id":"conv-123",
			"file_path":"/repo/new.txt",
			"edits":[{"old_string":"","new_string":"hello\n"}]
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
	if ev.ToolName != "Write" {
		t.Errorf("tool_name: got %q, want Write", ev.ToolName)
	}
	if ev.ProvenanceHash == "" {
		t.Fatal("expected provenance hash")
	}
	if ev.PayloadHash == "" {
		t.Fatal("expected payload hash")
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
		Type:          hooks.ToolStepCompleted,
		SessionID:     "conv-123",
		TranscriptRef: testTranscriptRef,
		Timestamp:     1000,
		TurnID:        "turn-123",
		ToolUseID:     "cursor-step-2",
		ToolName:      "Edit",
		ToolInput: json.RawMessage(`{
			"conversation_id":"conv-123",
			"file_path":"/repo/main.go",
			"edits":[{"old_string":"foo","new_string":"bar"}]
		}`),
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events: got %d, want 1", len(events))
	}
	if !strings.Contains(events[0].ToolUsesJSON, `"Edit"`) {
		t.Errorf("tool_uses should contain Edit, got %q", events[0].ToolUsesJSON)
	}
	if events[0].PayloadHash == "" {
		t.Fatal("expected payload hash")
	}
	var payload struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(bs.stored[events[0].PayloadHash], &payload); err != nil {
		t.Fatalf("unmarshal payload blob: %v", err)
	}
	if payload.Type != "assistant" {
		t.Fatalf("payload type: got %q, want assistant", payload.Type)
	}
}

func TestBuildHookEvents_Bash(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()
	event := &hooks.Event{
		Type:          hooks.ToolStepCompleted,
		SessionID:     "conv-123",
		TranscriptRef: testTranscriptRef,
		Timestamp:     1000,
		TurnID:        "turn-123",
		ToolUseID:     "shell-123",
		ToolName:      "Bash",
		ToolInput: json.RawMessage(`{
			"conversation_id":"conv-123",
			"tool_name":"Shell",
			"tool_use_id":"shell-123",
			"tool_input":{"command":"cat file.txt","cwd":"/repo","timeout":30000},
			"tool_output":"{\"output\":\"ok\\n\",\"exitCode\":0}"
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
	if ev.Summary != "cat file.txt" {
		t.Errorf("summary: got %q, want cat file.txt", ev.Summary)
	}
	if ev.ProvenanceHash == "" {
		t.Fatal("expected provenance hash")
	}
}

func TestBuildHookEvents_SubagentPrompt(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()
	event := &hooks.Event{
		Type:          hooks.SubagentPromptSubmitted,
		SessionID:     "conv-123",
		TranscriptRef: testTranscriptRef,
		Timestamp:     1000,
		TurnID:        "turn-123",
		ToolUseID:     "agent-123",
		ToolName:      "Agent",
		ToolInput: json.RawMessage(`{
			"tool_input":{"prompt":"Review the code"}
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
		t.Fatal("expected prompt payload hash")
	}
}

func TestBuildHookEvents_AgentCompletion(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()
	event := &hooks.Event{
		Type:          hooks.SubagentCompleted,
		SessionID:     "conv-123",
		TranscriptRef: testTranscriptRef,
		Timestamp:     1000,
		TurnID:        "turn-123",
		ToolUseID:     "agent-123",
		ToolName:      "Agent",
		ToolInput: json.RawMessage(`{
			"conversation_id":"conv-123",
			"subagent_id":"agent-123",
			"subagent_type":"general-purpose",
			"status":"completed",
			"duration_ms":9000,
			"message_count":3,
			"tool_call_count":1
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
	if ev.PayloadHash == "" {
		t.Fatal("expected payload hash")
	}
	if ev.ProvenanceHash == "" {
		t.Fatal("expected provenance hash")
	}
	if !strings.Contains(ev.ToolUsesJSON, `"Agent"`) {
		t.Errorf("tool_uses should contain Agent, got %q", ev.ToolUsesJSON)
	}
}
