package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/hooks"
)

// fakeBlobPutter records Put calls and returns predictable hashes.
// Matches the pattern used by the claude, copilot, and cursor hook
// tests so the safety net reads the same way across providers.
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

const geminiTranscriptRef = "/workspace/.gemini/sessions/sess-gemini-1/transcript.jsonl"

// --- Prompt ---

func TestBuildHookEvents_Prompt(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	event := &hooks.Event{
		Type:          hooks.PromptSubmitted,
		SessionID:     "sess-gemini-1",
		TurnID:        "turn-1",
		Prompt:        "Write a hello world program",
		TranscriptRef: geminiTranscriptRef,
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
	if ev.ProvenanceHash != ev.PayloadHash {
		t.Errorf("provenance_hash = %q, want equal to payload_hash %q",
			ev.ProvenanceHash, ev.PayloadHash)
	}
	if ev.Summary != "Write a hello world program" {
		t.Errorf("summary = %q, want the prompt text", ev.Summary)
	}
}

func TestBuildHookEvents_PromptTruncation(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	long := strings.Repeat("a", 250)
	event := &hooks.Event{
		Type:          hooks.PromptSubmitted,
		SessionID:     "sess-gemini-1",
		TurnID:        "turn-1",
		Prompt:        long,
		TranscriptRef: geminiTranscriptRef,
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	// Summary is truncated at 200 characters with a trailing "..."
	// to match the behavior of the other providers.
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
		SessionID: "sess-gemini-1",
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

func TestBuildHookEvents_Write(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	event := &hooks.Event{
		Type:          hooks.ToolStepCompleted,
		SessionID:     "sess-gemini-1",
		TurnID:        "turn-1",
		ToolUseID:     "gemini-step-1",
		ToolName:      "Write",
		ToolInput:     json.RawMessage(`{"file_path":"/repo/new.go","content":"package main\n"}`),
		TranscriptRef: geminiTranscriptRef,
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
	if ev.Role != "assistant" || ev.Kind != "assistant" {
		t.Errorf("role/kind = %q/%q, want assistant/assistant", ev.Role, ev.Kind)
	}
	if ev.ToolName != "Write" {
		t.Errorf("tool_name = %q, want Write", ev.ToolName)
	}
	if ev.PayloadHash == "" {
		t.Error("expected non-empty payload_hash")
	}
	if ev.ProvenanceHash == "" {
		t.Error("expected non-empty provenance_hash")
	}
	if len(ev.FilePaths) != 1 || ev.FilePaths[0] != "/repo/new.go" {
		t.Errorf("file_paths = %v, want [/repo/new.go]", ev.FilePaths)
	}
	if !strings.Contains(ev.ToolUsesJSON, `"Write"`) {
		t.Errorf("tool_uses should contain Write, got %q", ev.ToolUsesJSON)
	}

	// Verify the payload blob is the synthesized assistant shape.
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

	// tool_input without file_path is treated as a no-op rather than
	// an error, matching the existing provider behavior.
	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		SessionID: "sess-gemini-1",
		ToolName:  "Write",
		ToolInput: json.RawMessage(`{"content":"package main\n"}`),
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events when file_path is missing, got %d", len(events))
	}
}

// TestBuildHookEvents_Write_RelativePathResolved covers relative
// file_path values from Gemini hook payloads. Expected paths are
// computed via filepath.Join so the assertions match OS-native
// separators on Windows as well as POSIX hosts.
func TestBuildHookEvents_Write_RelativePathResolved(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	cwd := filepath.FromSlash("/repo")
	event := &hooks.Event{
		Type:          hooks.ToolStepCompleted,
		SessionID:     "sess-gemini-1",
		ToolName:      "Write",
		CWD:           cwd,
		ToolInput:     json.RawMessage(`{"file_path":"src/a.go","content":"package main\n"}`),
		TranscriptRef: geminiTranscriptRef,
		Timestamp:     1000,
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	wantPath := filepath.Join(cwd, "src", "a.go")
	ev := events[0]
	if len(ev.FilePaths) != 1 || ev.FilePaths[0] != wantPath {
		t.Errorf("file_paths = %v, want [%s]", ev.FilePaths, wantPath)
	}

	// Parse tool_uses structurally so the assertion is independent of
	// JSON-escaped path separator differences across platforms.
	var tu struct {
		Tools []struct {
			FilePath string `json:"file_path"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(ev.ToolUsesJSON), &tu); err != nil {
		t.Fatalf("unmarshal tool_uses: %v", err)
	}
	if len(tu.Tools) != 1 || tu.Tools[0].FilePath != wantPath {
		t.Errorf("tool_uses file_path = %+v, want %s", tu.Tools, wantPath)
	}

	// The scorer payload should use the same resolved path as routing.
	var payload struct {
		Message struct {
			Content []struct {
				Input struct {
					FilePath string `json:"file_path"`
				} `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(bs.stored[ev.PayloadHash], &payload); err != nil {
		t.Fatalf("unmarshal payload blob: %v", err)
	}
	if len(payload.Message.Content) != 1 || payload.Message.Content[0].Input.FilePath != wantPath {
		t.Errorf("synthesized payload file_path = %+v, want %s", payload.Message.Content, wantPath)
	}
}

// TestBuildHookEvents_Write_AbsolutePathPreserved verifies absolute
// inputs flow through unchanged.
func TestBuildHookEvents_Write_AbsolutePathPreserved(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		SessionID: "sess-gemini-1",
		ToolName:  "Write",
		CWD:       "/repo",
		ToolInput: json.RawMessage(`{"file_path":"/other/abs.go","content":"x"}`),
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].FilePaths[0] != "/other/abs.go" {
		t.Errorf("file_paths = %v, want [/other/abs.go]", events[0].FilePaths)
	}
}

// TestBuildHookEvents_Write_NoCWDPreservesRelative ensures we never
// invent a path: when CWD is missing, the relative input is kept as-is
// rather than guessed against process state.
func TestBuildHookEvents_Write_NoCWDPreservesRelative(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		SessionID: "sess-gemini-1",
		ToolName:  "Write",
		ToolInput: json.RawMessage(`{"file_path":"src/a.go","content":"x"}`),
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].FilePaths[0] != "src/a.go" {
		t.Errorf("file_paths = %v, want [src/a.go]", events[0].FilePaths)
	}
}

// TestBuildHookEvents_Edit_RelativePathResolved covers the same
// resolution behavior for replace/edit_file payloads.
func TestBuildHookEvents_Edit_RelativePathResolved(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	cwd := filepath.FromSlash("/repo")
	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		SessionID: "sess-gemini-1",
		ToolName:  "Edit",
		CWD:       cwd,
		ToolInput: json.RawMessage(`{"file_path":"pkg/x/y.go","old_string":"foo","new_string":"bar"}`),
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	wantPath := filepath.Join(cwd, "pkg", "x", "y.go")
	ev := events[0]
	if ev.FilePaths[0] != wantPath {
		t.Errorf("file_paths = %v, want [%s]", ev.FilePaths, wantPath)
	}
	var tu struct {
		Tools []struct {
			FilePath string `json:"file_path"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(ev.ToolUsesJSON), &tu); err != nil {
		t.Fatalf("unmarshal tool_uses: %v", err)
	}
	if len(tu.Tools) != 1 || tu.Tools[0].FilePath != wantPath {
		t.Errorf("tool_uses file_path = %+v, want %s", tu.Tools, wantPath)
	}
}

func TestBuildHookEvents_Edit(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		SessionID: "sess-gemini-1",
		TurnID:    "turn-1",
		ToolUseID: "gemini-step-2",
		ToolName:  "Edit",
		ToolInput: json.RawMessage(
			`{"file_path":"/repo/main.go","old_string":"foo","new_string":"bar"}`,
		),
		TranscriptRef: geminiTranscriptRef,
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
	if !strings.Contains(ev.ToolUsesJSON, `"Edit"`) {
		t.Errorf("tool_uses should contain Edit, got %q", ev.ToolUsesJSON)
	}
	if ev.PayloadHash == "" || ev.ProvenanceHash == "" {
		t.Error("expected both payload_hash and provenance_hash to be set")
	}
}

func TestBuildHookEvents_Bash(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		SessionID: "sess-gemini-1",
		TurnID:    "turn-1",
		ToolUseID: "gemini-step-3",
		ToolName:  "Bash",
		ToolInput: json.RawMessage(
			`{"command":"go test ./...","description":"Run all tests"}`,
		),
		TranscriptRef: geminiTranscriptRef,
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
	// When description is non-empty, it is used as the summary in
	// preference to the command itself.
	if ev.Summary != "Run all tests" {
		t.Errorf("summary = %q, want 'Run all tests'", ev.Summary)
	}
	// Bash events carry a provenance blob even when no file paths are
	// attached so the command output can be inspected later.
	if ev.ProvenanceHash == "" {
		t.Error("expected non-empty provenance_hash for Bash")
	}
	if len(ev.FilePaths) != 0 {
		t.Errorf("file_paths = %v, want empty for Bash", ev.FilePaths)
	}
}

func TestBuildHookEvents_BashFallsBackToCommand(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	// When description is empty, the redacted command doubles as the
	// summary (truncated at 200 characters plus ellipsis).
	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		SessionID: "sess-gemini-1",
		ToolName:  "Bash",
		ToolInput: json.RawMessage(`{"command":"ls -la","description":""}`),
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Summary != "ls -la" {
		t.Errorf("summary = %q, want 'ls -la'", events[0].Summary)
	}
}

func TestBuildHookEvents_BashRedaction(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	// A GitHub token inside the command must be redacted before it
	// lands in the summary or in the stored blobs.
	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		SessionID: "sess-gemini-1",
		ToolName:  "Bash",
		ToolInput: json.RawMessage(
			`{"command":"curl -H 'Authorization: Bearer ghp_1234567890abcdef1234567890abcdef12345678' https://api.github.com/user","description":""}`,
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
		t.Errorf("summary should retain the command shell structure, got %q", ev.Summary)
	}
	// The provenance blob must also have the redacted command, not
	// the raw token.
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
		SessionID: "sess-gemini-1",
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

// TestBuildHookEvents_SubagentPrompt covers the Gemini 0.40+
// invoke_agent before-tool payload.
func TestBuildHookEvents_SubagentPrompt(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	event := &hooks.Event{
		Type:          hooks.SubagentPromptSubmitted,
		SessionID:     "sess-gemini-1",
		TurnID:        "turn-1",
		ToolUseID:     "gemini-agent-1",
		ToolName:      "Agent",
		ToolInput:     json.RawMessage(`{"prompt":"Review the auth package","agent_name":"codebase_investigator"}`),
		TranscriptRef: geminiTranscriptRef,
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
	if ev.Summary != "Review the auth package" {
		t.Errorf("summary = %q, want the prompt text", ev.Summary)
	}
	if ev.PayloadHash == "" {
		t.Error("expected non-empty payload_hash for subagent prompt")
	}
	if ev.ProvenanceHash == "" {
		t.Error("expected non-empty provenance_hash for subagent prompt")
	}
}

// TestBuildHookEvents_SubagentPromptMissingPrompt requires prompt.
func TestBuildHookEvents_SubagentPromptMissingPrompt(t *testing.T) {
	p := &Provider{}

	cases := []struct {
		name  string
		input string
	}{
		{"empty object", `{}`},
		{"only agent_name", `{"agent_name":"codebase_investigator"}`},
		{"empty prompt", `{"prompt":"","agent_name":"x"}`},
		// Earlier Gemini subagent shapes are not accepted here.
		{"legacy request field", `{"request":"Do something"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event := &hooks.Event{
				Type:      hooks.SubagentPromptSubmitted,
				SessionID: "sess-gemini-1",
				ToolName:  "Agent",
				ToolInput: json.RawMessage(tc.input),
			}
			events, err := p.BuildHookEvents(context.Background(), event, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(events) != 0 {
				t.Errorf("expected 0 events, got %d", len(events))
			}
		})
	}
}

// TestBuildHookEvents_SubagentCompletedSuccess covers the structured
// returnDisplay state used by invoke_agent completions.
func TestBuildHookEvents_SubagentCompletedSuccess(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	event := &hooks.Event{
		Type:      hooks.SubagentCompleted,
		SessionID: "sess-gemini-1",
		TurnID:    "turn-1",
		ToolUseID: "gemini-agent-1",
		ToolName:  "Agent",
		ToolInput: json.RawMessage(`{"prompt":"Review the auth package","agent_name":"codebase_investigator"}`),
		ToolResponse: json.RawMessage(
			`{"llmContent":"Reviewed: found two issues in auth/session.go","returnDisplay":{"isSubagentProgress":true,"agentName":"codebase_investigator","state":"success"}}`,
		),
		TranscriptRef: geminiTranscriptRef,
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
	if ev.Summary != "codebase_investigator subagent success" {
		t.Errorf("summary = %q, want '<agent> subagent <state>' phrasing", ev.Summary)
	}
	if !strings.Contains(ev.ToolUsesJSON, `"Agent"`) {
		t.Errorf("tool_uses should contain Agent, got %q", ev.ToolUsesJSON)
	}

	// Synthesized payload keeps the renderable subagent fields together.
	var payload struct {
		Message struct {
			Content []struct {
				Input struct {
					Prompt    string `json:"prompt"`
					AgentName string `json:"agent_name"`
					State     string `json:"state"`
				} `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(bs.stored[ev.PayloadHash], &payload); err != nil {
		t.Fatalf("unmarshal payload blob: %v", err)
	}
	if len(payload.Message.Content) != 1 {
		t.Fatalf("payload content length = %d, want 1", len(payload.Message.Content))
	}
	got := payload.Message.Content[0].Input
	if got.Prompt != "Review the auth package" || got.AgentName != "codebase_investigator" || got.State != "success" {
		t.Errorf("synthesized payload input = %+v, want prompt/agent_name/state populated", got)
	}
}

// TestBuildHookEvents_SubagentCompletedError covers an errored
// invoke_agent completion.
func TestBuildHookEvents_SubagentCompletedError(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	event := &hooks.Event{
		Type:      hooks.SubagentCompleted,
		SessionID: "sess-gemini-1",
		ToolName:  "Agent",
		ToolInput: json.RawMessage(`{"prompt":"Analyze payments/","agent_name":"codebase_investigator"}`),
		ToolResponse: json.RawMessage(
			`{"llmContent":"Subagent 'codebase_investigator' failed.","returnDisplay":{"isSubagentProgress":true,"agentName":"codebase_investigator","state":"error"}}`,
		),
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Summary != "codebase_investigator subagent error" {
		t.Errorf("summary = %q, want error-state phrasing", events[0].Summary)
	}
}

// TestBuildHookEvents_SubagentCompletedNoResponse keeps empty
// completions renderable.
func TestBuildHookEvents_SubagentCompletedNoResponse(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	event := &hooks.Event{
		Type:      hooks.SubagentCompleted,
		SessionID: "sess-gemini-1",
		ToolName:  "Agent",
		ToolInput: json.RawMessage(`{"prompt":"Do something","agent_name":"codebase_investigator"}`),
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Summary != "Gemini subagent completed" {
		t.Errorf("summary = %q, want neutral placeholder", events[0].Summary)
	}
}

// TestBuildHookEvents_SubagentCompletedLLMContentFallback uses
// llmContent when structured state is absent.
func TestBuildHookEvents_SubagentCompletedLLMContentFallback(t *testing.T) {
	p := &Provider{}
	bs := newFakeBlobPutter()

	event := &hooks.Event{
		Type:         hooks.SubagentCompleted,
		SessionID:    "sess-gemini-1",
		ToolName:     "Agent",
		ToolInput:    json.RawMessage(`{"prompt":"Do something"}`),
		ToolResponse: json.RawMessage(`{"llmContent":"Analyzed payments: one bug in retry logic"}`),
	}

	events, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if events[0].Summary != "Analyzed payments: one bug in retry logic" {
		t.Errorf("summary = %q, want llmContent text", events[0].Summary)
	}
}

// --- Miscellaneous ---

func TestBuildHookEvents_UnknownType(t *testing.T) {
	p := &Provider{}
	event := &hooks.Event{
		Type:      hooks.AgentCompleted,
		SessionID: "sess-gemini-1",
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
	// When the blob store fails, the provider should still emit an
	// event with empty hashes rather than returning an error. This
	// preserves the caller's ability to degrade gracefully when the
	// local blob store is unavailable.
	p := &Provider{}

	event := &hooks.Event{
		Type:      hooks.ToolStepCompleted,
		SessionID: "sess-gemini-1",
		ToolName:  "Write",
		ToolInput: json.RawMessage(`{"file_path":"/repo/a.go","content":"x"}`),
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
	// The rest of the event shape must still be correct so downstream
	// consumers receive a well-formed row.
	if ev.ToolName != "Write" || ev.FilePaths[0] != "/repo/a.go" {
		t.Errorf("event shape degraded unexpectedly: %+v", ev)
	}
}

func TestBuildHookEvents_NilBlobPutter(t *testing.T) {
	// Calling the provider without a blob store is valid (dry runs,
	// tests, etc.). Events are emitted with empty hashes.
	p := &Provider{}

	event := &hooks.Event{
		Type:      hooks.PromptSubmitted,
		SessionID: "sess-gemini-1",
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
