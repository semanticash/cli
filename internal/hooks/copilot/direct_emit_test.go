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
