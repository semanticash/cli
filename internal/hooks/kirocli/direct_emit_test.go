package kirocli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/hooks"
)

// fakeBlobPutter records Put calls and returns predictable hashes.
// Matches the pattern used by the claude, copilot, cursor, and gemini
// hook tests so the safety net reads the same way across providers.
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

// failingBlobPutter always returns an error, exercising the paths
// where the provider must fall back to empty hash values without
// panicking or propagating the error upward.
type failingBlobPutter struct{}

func (failingBlobPutter) Put(_ context.Context, _ []byte) (string, int64, error) {
	return "", 0, errors.New("blob store unavailable")
}

// --- Prompt ---

func TestBuildHookEvents_Prompt(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	event := &hooks.Event{
		Type:      hooks.PromptSubmitted,
		SessionID: "sess-kiro-1",
		TurnID:    "turn-1",
		CWD:       "/repo",
		Prompt:    "Add a retry handler to payments.go",
		Timestamp: 500,
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
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
		t.Error("expected non-empty payload_hash for prompt")
	}
	if ev.ProvenanceHash != ev.PayloadHash {
		t.Errorf("provenance_hash = %q, want equal to payload_hash %q",
			ev.ProvenanceHash, ev.PayloadHash)
	}
	if ev.Summary != "Add a retry handler to payments.go" {
		t.Errorf("summary = %q, want the prompt text", ev.Summary)
	}
	if ev.SourceProjectPath != "/repo" {
		t.Errorf("source_project_path = %q, want /repo", ev.SourceProjectPath)
	}
}

func TestBuildHookEvents_PromptTruncation(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	long := strings.Repeat("x", 250)
	event := &hooks.Event{
		Type:      hooks.PromptSubmitted,
		SessionID: "sess-kiro-1",
		Prompt:    long,
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	wantLen := 200 + len("...")
	if len(events[0].Summary) != wantLen {
		t.Errorf("summary length = %d, want %d", len(events[0].Summary), wantLen)
	}
	if !strings.HasSuffix(events[0].Summary, "...") {
		t.Errorf("summary should end with '...', got %q", events[0].Summary)
	}
}

func TestBuildHookEvents_EmptyPrompt(t *testing.T) {
	p := &Provider{}
	event := &hooks.Event{
		Type:      hooks.PromptSubmitted,
		SessionID: "sess-kiro-1",
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

// --- Tool steps ---
//
// Kiro CLI's write tool_input uses provider-specific field names
// (`path`, `content`, `oldStr`, and `newStr`). These tests pin the
// normalized events Semantica emits from that shape.

func TestBuildHookEvents_Write(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		SessionID: "sess-kiro-1",
		TurnID:    "turn-1",
		ToolUseID: "kiro-step-1",
		ToolName:  "Write",
		CWD:       "/repo",
		ToolInput: json.RawMessage(
			`{"command":"create","path":"/repo/new.go","content":"package main\n"}`,
		),
		Timestamp: 1000,
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	ev := events[0]
	if ev.Role != "assistant" || ev.Kind != "assistant" {
		t.Errorf("role/kind = %q/%q, want assistant/assistant", ev.Role, ev.Kind)
	}
	if ev.ToolName != "Write" {
		t.Errorf("tool_name = %q, want Write", ev.ToolName)
	}
	if ev.PayloadHash == "" || ev.ProvenanceHash == "" {
		t.Error("expected both payload_hash and provenance_hash")
	}
	if len(ev.FilePaths) != 1 || ev.FilePaths[0] != "/repo/new.go" {
		t.Errorf("file_paths = %v, want [/repo/new.go]", ev.FilePaths)
	}
	// Write events use canonical tool_uses so the scorer reads the
	// synthesized payload blob for line-level attribution.
	if !strings.Contains(ev.ToolUsesJSON, `"name":"Write"`) {
		t.Errorf("tool_uses should contain canonical Write name, got %q", ev.ToolUsesJSON)
	}
	if strings.Contains(ev.ToolUsesJSON, `kiro_file_edit`) {
		t.Errorf("tool_uses must not contain kiro_file_edit (would short-circuit scoring), got %q", ev.ToolUsesJSON)
	}
	if !strings.Contains(ev.ToolUsesJSON, `"file_op":"write"`) {
		t.Errorf("tool_uses should record file_op=write, got %q", ev.ToolUsesJSON)
	}
	if !strings.Contains(ev.ToolUsesJSON, `"file_path":"/repo/new.go"`) {
		t.Errorf("tool_uses should record file_path, got %q", ev.ToolUsesJSON)
	}

	// The synthesized payload blob is the shared assistant shape.
	var payload struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(bs.stored[ev.PayloadHash], &payload); err != nil {
		t.Fatalf("unmarshal payload blob: %v", err)
	}
	if payload.Type != "assistant" {
		t.Errorf("payload type = %q, want assistant", payload.Type)
	}
	if len(payload.Message.Content) != 1 || payload.Message.Content[0].Name != "Write" {
		t.Errorf("payload content = %+v, want one tool_use named Write", payload.Message.Content)
	}
}

func TestBuildHookEvents_WriteMissingPath(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	// A write event with no path is a no-op, matching sibling providers.
	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		SessionID: "sess-kiro-1",
		ToolName:  "Write",
		ToolInput: json.RawMessage(`{"command":"create","content":"x"}`),
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events when path is missing, got %d", len(events))
	}
}

func TestBuildHookEvents_Edit(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	// strReplace uses Kiro's oldStr/newStr fields and is normalized
	// to old_string/new_string in the stored payload.
	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		SessionID: "sess-kiro-1",
		TurnID:    "turn-1",
		ToolUseID: "kiro-step-2",
		ToolName:  "Edit",
		ToolInput: json.RawMessage(
			`{"command":"strReplace","path":"/repo/main.go","oldStr":"foo","newStr":"bar"}`,
		),
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
	if len(ev.FilePaths) != 1 || ev.FilePaths[0] != "/repo/main.go" {
		t.Errorf("file_paths = %v, want [/repo/main.go]", ev.FilePaths)
	}
	// Edit events use the same canonical tool_uses pattern as Write.
	if !strings.Contains(ev.ToolUsesJSON, `"name":"Edit"`) {
		t.Errorf("tool_uses should contain canonical Edit name, got %q", ev.ToolUsesJSON)
	}
	if strings.Contains(ev.ToolUsesJSON, `kiro_file_edit`) {
		t.Errorf("tool_uses must not contain kiro_file_edit (would short-circuit scoring), got %q", ev.ToolUsesJSON)
	}
	if !strings.Contains(ev.ToolUsesJSON, `"file_op":"edit"`) {
		t.Errorf("tool_uses should record file_op=edit, got %q", ev.ToolUsesJSON)
	}
	if !strings.Contains(ev.ToolUsesJSON, `"file_path":"/repo/main.go"`) {
		t.Errorf("tool_uses should record file_path, got %q", ev.ToolUsesJSON)
	}
	if ev.PayloadHash == "" || ev.ProvenanceHash == "" {
		t.Error("expected payload_hash and provenance_hash to be set")
	}
}

func TestBuildHookEvents_Bash(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	// Without a purpose hint, shell events use the redacted command
	// as their summary.
	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		SessionID: "sess-kiro-1",
		TurnID:    "turn-1",
		ToolUseID: "kiro-step-3",
		ToolName:  "Bash",
		ToolInput: json.RawMessage(
			`{"command":"go test ./...","working_dir":"/repo"}`,
		),
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
	if ev.Summary != "go test ./..." {
		t.Errorf("summary = %q, want 'go test ./...'", ev.Summary)
	}
	if ev.ProvenanceHash == "" {
		t.Error("expected non-empty provenance_hash for Bash")
	}
	if len(ev.FilePaths) != 0 {
		t.Errorf("file_paths = %v, want empty for Bash", ev.FilePaths)
	}
	// Bash events have no file path, so Kiro's helper emits no
	// ToolUsesJSON.
	if ev.ToolUsesJSON != "" {
		t.Errorf("tool_uses for Bash = %q, want empty", ev.ToolUsesJSON)
	}
}

func TestBuildHookEvents_BashWithResponse(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	// Shell tool_response is an items array: each entry is either a
	// Json variant (with exit_status, stdout, stderr) or a Text
	// variant. The provenance blob redacts stdout/stderr from the
	// first Json entry it finds.
	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		SessionID: "sess-kiro-1",
		ToolName:  "Bash",
		ToolInput: json.RawMessage(`{"command":"ls"}`),
		ToolResponse: json.RawMessage(
			`{"items":[{"Json":{"exit_status":"exit status: 0","stdout":"file1\nfile2","stderr":""}}]}`,
		),
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	// Confirm the provenance blob contains a stdout field (redacted or not).
	blob, ok := bs.stored[events[0].ProvenanceHash]
	if !ok {
		t.Fatal("provenance blob was not stored")
	}
	var decoded struct {
		ToolResponse struct {
			Stdout string `json:"stdout"`
		} `json:"tool_response"`
	}
	if err := json.Unmarshal(blob, &decoded); err != nil {
		t.Fatalf("unmarshal provenance blob: %v", err)
	}
	if decoded.ToolResponse.Stdout != "file1\nfile2" {
		t.Errorf("provenance stdout = %q, want 'file1\\nfile2'", decoded.ToolResponse.Stdout)
	}
}

func TestBuildHookEvents_BashRedaction(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	// Secrets in the command must be redacted before landing in the
	// summary or in the stored blobs.
	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		SessionID: "sess-kiro-1",
		ToolName:  "Bash",
		ToolInput: json.RawMessage(
			`{"command":"curl -H 'Authorization: Bearer ghp_1234567890abcdef1234567890abcdef12345678' https://api.github.com/user"}`,
		),
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	ev := events[0]
	if strings.Contains(ev.Summary, "ghp_1234567890abcdef") {
		t.Errorf("summary leaked the GitHub token: %q", ev.Summary)
	}
	if !strings.Contains(ev.Summary, "curl") {
		t.Errorf("summary should retain the shell structure, got %q", ev.Summary)
	}
	blob, ok := bs.stored[ev.ProvenanceHash]
	if !ok {
		t.Fatal("provenance blob was not stored")
	}
	if strings.Contains(string(blob), "ghp_1234567890abcdef") {
		t.Errorf("provenance blob leaked the GitHub token: %s", string(blob))
	}
}

func TestBuildHookEvents_UnknownTool(t *testing.T) {
	p := &Provider{}
	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		SessionID: "sess-kiro-1",
		ToolName:  "SomeFutureTool",
		ToolInput: json.RawMessage(`{}`),
	}

	events, err := p.BuildHookEvents(context.Background(), event, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for an unknown tool, got %d", len(events))
	}
}

// --- Subagent prompts and completions ---

// Purpose is the highest-priority subagent summary source.
func TestBuildHookEvents_SubagentPromptWithPurpose(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	event := &hooks.Event{
		Type:      hooks.SubagentPromptSubmitted,
		SessionID: "sess-kiro-1",
		TurnID:    "turn-1",
		ToolUseID: "kiro-subagent-1",
		ToolName:  "Agent",
		ToolInput: json.RawMessage(
			`{"task":"top level fallback","__tool_use_purpose":"Dispatch three repo investigations","stages":[{"name":"a"},{"name":"b"},{"name":"c"}]}`,
		),
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
	if ev.Summary != "Dispatch three repo investigations" {
		t.Errorf("summary = %q, want __tool_use_purpose to win", ev.Summary)
	}
	if ev.PayloadHash == "" {
		t.Error("expected non-empty payload_hash for subagent prompt")
	}
}

// Task is the summary fallback when purpose is absent.
func TestBuildHookEvents_SubagentPromptWithTask(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	event := &hooks.Event{
		Type:      hooks.SubagentPromptSubmitted,
		SessionID: "sess-kiro-1",
		ToolName:  "Agent",
		ToolInput: json.RawMessage(`{"task":"Generate the JSON schema","stages":[{"name":"a"}]}`),
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Summary != "Generate the JSON schema" {
		t.Errorf("summary = %q, want top-level task", events[0].Summary)
	}
}

// Stage count is the fallback when no purpose or task is present.
func TestBuildHookEvents_SubagentPromptStageCountFallback(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	event := &hooks.Event{
		Type:      hooks.SubagentPromptSubmitted,
		SessionID: "sess-kiro-1",
		ToolName:  "Agent",
		ToolInput: json.RawMessage(
			`{"stages":[{"name":"a","prompt_template":"do A"},{"name":"b","prompt_template":"do B"},{"name":"c","prompt_template":"do C"}]}`,
		),
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Summary != "Kiro subagent: 3 stages" {
		t.Errorf("summary = %q, want stage-count placeholder", events[0].Summary)
	}
	if strings.Contains(events[0].Summary, "do A") {
		t.Error("summary must not surface the first stage prompt")
	}
}

func TestBuildHookEvents_SubagentPromptEmpty(t *testing.T) {
	p := &Provider{}
	// No purpose, no task, no stages: drop. Mirrors the
	// empty-prompt rule above.
	event := &hooks.Event{
		Type:      hooks.SubagentPromptSubmitted,
		SessionID: "sess-kiro-1",
		ToolName:  "Agent",
		ToolInput: json.RawMessage(`{}`),
	}

	events, err := p.BuildHookEvents(context.Background(), event, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for empty subagent prompt, got %d", len(events))
	}
}

// The first AgentCrew text response becomes the completion summary.
func TestBuildHookEvents_SubagentCompleted(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	event := &hooks.Event{
		Type:      hooks.SubagentCompleted,
		SessionID: "sess-kiro-1",
		TurnID:    "turn-1",
		ToolUseID: "kiro-subagent-1",
		ToolName:  "Agent",
		ToolInput: json.RawMessage(`{"task":"Review the auth package","stages":[{"name":"a"}]}`),
		ToolResponse: json.RawMessage(
			`{"items":[{"Text":"Pipeline completed: 1 stages finished.\n\n## a\n\nReviewed: found two issues in auth/session.go"}]}`,
		),
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
	if !strings.Contains(ev.Summary, "Reviewed: found two issues") {
		t.Errorf("summary = %q, want first items[].Text", ev.Summary)
	}
	if ev.ProvenanceHash == "" {
		t.Error("expected non-empty provenance_hash")
	}
}

func TestBuildHookEvents_SubagentCompletedNoResponse(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	// With no tool_response, the provider falls back to a neutral
	// placeholder summary rather than leaving the field empty.
	event := &hooks.Event{
		Type:      hooks.SubagentCompleted,
		SessionID: "sess-kiro-1",
		ToolName:  "Agent",
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Summary == "" {
		t.Error("expected a non-empty fallback summary")
	}
}

// --- Miscellaneous ---

func TestBuildHookEvents_UnknownType(t *testing.T) {
	p := &Provider{}
	event := &hooks.Event{
		Type:      hooks.AgentCompleted,
		SessionID: "sess-kiro-1",
	}

	events, err := p.BuildHookEvents(context.Background(), event, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events for AgentCompleted, got %d", len(events))
	}
}

func TestBuildHookEvents_BlobPutFailureDegradesCleanly(t *testing.T) {
	p := &Provider{}

	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		SessionID: "sess-kiro-1",
		ToolName:  "Write",
		ToolInput: json.RawMessage(
			`{"command":"create","path":"/repo/a.go","content":"x"}`,
		),
	}

	events, err := p.BuildHookEvents(context.Background(), event, failingBlobPutter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.PayloadHash != "" {
		t.Errorf("payload_hash = %q, want empty when blob store fails", ev.PayloadHash)
	}
	if ev.ProvenanceHash != "" {
		t.Errorf("provenance_hash = %q, want empty when blob store fails", ev.ProvenanceHash)
	}
	if ev.ToolName != "Write" || ev.FilePaths[0] != "/repo/a.go" {
		t.Errorf("event shape degraded unexpectedly: %+v", ev)
	}
}

func TestBuildHookEvents_NilBlobPutter(t *testing.T) {
	p := &Provider{}

	event := &hooks.Event{
		Type:      hooks.PromptSubmitted,
		SessionID: "sess-kiro-1",
		Prompt:    "hello",
	}

	events, err := p.BuildHookEvents(context.Background(), event, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].PayloadHash != "" {
		t.Errorf("payload_hash = %q, want empty with nil blob putter", events[0].PayloadHash)
	}
}
