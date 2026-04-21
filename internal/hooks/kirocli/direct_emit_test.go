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
// Kiro CLI's fs_write tool_input shape differs from the other providers:
// it uses `path` (not `file_path`) and `file_text` (for create) or
// `old_str`/`new_str` (for str_replace). These tests exercise that
// provider-specific shape.

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
			`{"command":"create","path":"/repo/new.go","file_text":"package main\n"}`,
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
	// Kiro CLI serializes tool_uses through its own helper which uses
	// a synthetic tool name (kiro_file_edit) with the real operation
	// surfaced as file_op. This differs from the other providers and
	// is captured here as the current baseline.
	if !strings.Contains(ev.ToolUsesJSON, `"kiro_file_edit"`) {
		t.Errorf("tool_uses should contain kiro_file_edit, got %q", ev.ToolUsesJSON)
	}
	if !strings.Contains(ev.ToolUsesJSON, `"file_op":"create"`) {
		t.Errorf("tool_uses should record file_op=create, got %q", ev.ToolUsesJSON)
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

	// fs_write with no path is a no-op, matching the sibling providers.
	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		SessionID: "sess-kiro-1",
		ToolName:  "Write",
		ToolInput: json.RawMessage(`{"command":"create","file_text":"x"}`),
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

	// Edit operations use Kiro CLI's str_replace command with old_str
	// and new_str fields instead of old_string/new_string.
	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		SessionID: "sess-kiro-1",
		TurnID:    "turn-1",
		ToolUseID: "kiro-step-2",
		ToolName:  "Edit",
		ToolInput: json.RawMessage(
			`{"command":"str_replace","path":"/repo/main.go","old_str":"foo","new_str":"bar"}`,
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
	// Same synthetic-name pattern as Write (see TestBuildHookEvents_Write).
	if !strings.Contains(ev.ToolUsesJSON, `"kiro_file_edit"`) {
		t.Errorf("tool_uses should contain kiro_file_edit, got %q", ev.ToolUsesJSON)
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

	// Kiro CLI's execute_bash shape uses command plus an optional
	// working_dir field. Unlike Gemini, there is no description field
	// to use as summary, so the redacted command itself becomes the
	// summary.
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
	// BuildToolUsesJSON returns an empty NullString when file_path is
	// absent, so Bash events currently ship with empty ToolUsesJSON.
	// This is Kiro CLI specific behavior and is captured here as the
	// current baseline.
	if ev.ToolUsesJSON != "" {
		t.Errorf("tool_uses for Bash = %q, want empty", ev.ToolUsesJSON)
	}
}

func TestBuildHookEvents_BashWithResponse(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	// execute_bash emits a result array with {exit_status, stdout, stderr}.
	// The stdout and stderr are redacted before they reach the
	// provenance blob.
	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		SessionID: "sess-kiro-1",
		ToolName:  "Bash",
		ToolInput: json.RawMessage(`{"command":"ls"}`),
		ToolResponse: json.RawMessage(
			`{"success":true,"result":[{"exit_status":"0","stdout":"file1\nfile2","stderr":""}]}`,
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

func TestBuildHookEvents_SubagentPromptWithPrompt(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	// Kiro CLI accepts either `prompt` or `task` in the subagent input;
	// `prompt` takes precedence when both are present.
	event := &hooks.Event{
		Type:      hooks.SubagentPromptSubmitted,
		SessionID: "sess-kiro-1",
		TurnID:    "turn-1",
		ToolUseID: "kiro-agent-1",
		ToolName:  "Agent",
		ToolInput: json.RawMessage(
			`{"prompt":"Review this PR","task":"fallback task"}`,
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
	if !strings.Contains(ev.Summary, "Review this PR") {
		t.Errorf("summary should contain the prompt, got %q", ev.Summary)
	}
	if ev.PayloadHash == "" {
		t.Error("expected non-empty payload_hash for subagent prompt")
	}
}

func TestBuildHookEvents_SubagentPromptWithTaskFallback(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	// When `prompt` is absent, `task` is used as the subagent intent.
	event := &hooks.Event{
		Type:      hooks.SubagentPromptSubmitted,
		SessionID: "sess-kiro-1",
		ToolName:  "Agent",
		ToolInput: json.RawMessage(`{"task":"Generate the JSON schema"}`),
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if !strings.Contains(events[0].Summary, "Generate the JSON schema") {
		t.Errorf("summary should fall back to task, got %q", events[0].Summary)
	}
}

func TestBuildHookEvents_SubagentPromptEmpty(t *testing.T) {
	p := &Provider{}
	// Neither prompt nor task yields no events. Mirrors the
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

func TestBuildHookEvents_SubagentCompleted(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	// Kiro CLI's subagent response shape is {success, result: []string};
	// the first result string becomes the event summary.
	event := &hooks.Event{
		Type:      hooks.SubagentCompleted,
		SessionID: "sess-kiro-1",
		TurnID:    "turn-1",
		ToolUseID: "kiro-agent-1",
		ToolName:  "Agent",
		ToolInput: json.RawMessage(`{"prompt":"Review the auth package"}`),
		ToolResponse: json.RawMessage(
			`{"success":true,"result":["Reviewed: found two issues in auth/session.go"]}`,
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
	if ev.Summary != "Reviewed: found two issues in auth/session.go" {
		t.Errorf("summary = %q, want the first result string", ev.Summary)
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
			`{"command":"create","path":"/repo/a.go","file_text":"x"}`,
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
