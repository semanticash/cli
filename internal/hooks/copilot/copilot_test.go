package copilot

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/hooks"
)

func TestInstallHooks_CreatesFile(t *testing.T) {
	dir := t.TempDir()

	p := &Provider{}
	count, err := p.InstallHooks(context.Background(), dir, "/usr/local/bin/semantica")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if count != 7 {
		t.Errorf("count: got %d, want 7", count)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".github", "hooks", "semantica.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var cfg copilotHooksFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if cfg.Version != 1 {
		t.Errorf("version: got %d, want 1", cfg.Version)
	}

	// Verify all hook points.
	for _, hp := range []string{"userPromptSubmitted", "preToolUse", "postToolUse", "agentStop", "sessionStart", "sessionEnd", "subagentStop"} {
		defs, ok := cfg.Hooks[hp]
		if !ok {
			t.Errorf("missing hook point %q", hp)
			continue
		}
		if len(defs) != 1 {
			t.Errorf("%s: got %d defs, want 1", hp, len(defs))
			continue
		}
		if defs[0].Type != "command" {
			t.Errorf("%s: type = %q, want %q", hp, defs[0].Type, "command")
		}
		if !strings.Contains(defs[0].Bash, semanticaMarker) {
			t.Errorf("%s: bash doesn't contain marker: %q", hp, defs[0].Bash)
		}
		if !strings.HasPrefix(defs[0].Bash, "/usr/local/bin/semantica") {
			t.Errorf("%s: bash doesn't start with binary path: %q", hp, defs[0].Bash)
		}
	}
}

func TestInstallHooks_Idempotent(t *testing.T) {
	dir := t.TempDir()
	p := &Provider{}

	if _, err := p.InstallHooks(context.Background(), dir, "semantica"); err != nil {
		t.Fatalf("first install: %v", err)
	}
	_, err := p.InstallHooks(context.Background(), dir, "semantica")
	if err != nil {
		t.Fatalf("second install: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".github", "hooks", "semantica.json"))
	if err != nil {
		t.Fatalf("read hooks: %v", err)
	}
	var cfg copilotHooksFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal hooks: %v", err)
	}

	// Each hook point should have exactly 1 entry (not duplicated).
	for hp, defs := range cfg.Hooks {
		if len(defs) != 1 {
			t.Errorf("%s: got %d defs after double install, want 1", hp, len(defs))
		}
	}
}

func TestInstallHooks_PreservesExistingHooks(t *testing.T) {
	dir := t.TempDir()

	// Pre-create a hooks file with an existing entry.
	hooksDir := filepath.Join(dir, ".github", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	existing := `{
  "version": 1,
  "hooks": {
    "userPromptSubmitted": [{"type": "command", "bash": "echo custom hook"}]
  }
}`
	if err := os.WriteFile(filepath.Join(hooksDir, "semantica.json"), []byte(existing), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	p := &Provider{}
	_, err := p.InstallHooks(context.Background(), dir, "semantica")
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(hooksDir, "semantica.json"))
	if err != nil {
		t.Fatalf("read hooks: %v", err)
	}
	var cfg copilotHooksFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal hooks: %v", err)
	}

	// userPromptSubmitted should have both the original and semantica hooks.
	defs := cfg.Hooks["userPromptSubmitted"]
	if len(defs) != 2 {
		t.Fatalf("userPromptSubmitted: got %d defs, want 2", len(defs))
	}
}

func TestUninstallHooks_RemovesSemanticaEntries(t *testing.T) {
	dir := t.TempDir()
	p := &Provider{}

	if _, err := p.InstallHooks(context.Background(), dir, "semantica"); err != nil {
		t.Fatalf("install: %v", err)
	}

	err := p.UninstallHooks(context.Background(), dir)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	// File should be removed since no hooks remain.
	hooksPath := filepath.Join(dir, ".github", "hooks", "semantica.json")
	if _, err := os.Stat(hooksPath); !os.IsNotExist(err) {
		t.Error("hooks file should be removed when empty")
	}
}

func TestUninstallHooks_PreservesOtherEntries(t *testing.T) {
	dir := t.TempDir()
	p := &Provider{}

	// Install semantica hooks first.
	if _, err := p.InstallHooks(context.Background(), dir, "semantica"); err != nil {
		t.Fatalf("install: %v", err)
	}

	// Add a non-semantica hook.
	hooksPath := filepath.Join(dir, ".github", "hooks", "semantica.json")
	data, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("read hooks: %v", err)
	}
	var cfg copilotHooksFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal hooks: %v", err)
	}
	cfg.Hooks["userPromptSubmitted"] = append(cfg.Hooks["userPromptSubmitted"],
		copilotHookDef{Type: "command", Bash: "echo custom"})
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal hooks: %v", err)
	}
	if err := os.WriteFile(hooksPath, out, 0o644); err != nil {
		t.Fatalf("write hooks: %v", err)
	}

	// Uninstall.
	if err := p.UninstallHooks(context.Background(), dir); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	// File should still exist with the custom hook.
	data, err = os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("hooks file should still exist: %v", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal hooks after uninstall: %v", err)
	}
	defs := cfg.Hooks["userPromptSubmitted"]
	if len(defs) != 1 || defs[0].Bash != "echo custom" {
		t.Errorf("custom hook should be preserved, got %v", defs)
	}
}

func TestUninstallHooks_NoFile(t *testing.T) {
	dir := t.TempDir()
	p := &Provider{}

	// Should not error when file doesn't exist.
	if err := p.UninstallHooks(context.Background(), dir); err != nil {
		t.Errorf("uninstall with no file: %v", err)
	}
}

func TestAreHooksInstalled(t *testing.T) {
	dir := t.TempDir()
	p := &Provider{}

	if p.AreHooksInstalled(context.Background(), dir) {
		t.Error("should be false before install")
	}

	if _, err := p.InstallHooks(context.Background(), dir, "semantica"); err != nil {
		t.Fatalf("install: %v", err)
	}

	if !p.AreHooksInstalled(context.Background(), dir) {
		t.Error("should be true after install")
	}
}

func TestHookBinary(t *testing.T) {
	dir := t.TempDir()
	p := &Provider{}

	if _, err := p.InstallHooks(context.Background(), dir, "/opt/homebrew/bin/semantica"); err != nil {
		t.Fatalf("install: %v", err)
	}

	bin, err := p.HookBinary(context.Background(), dir)
	if err != nil {
		t.Fatalf("hook binary: %v", err)
	}
	if bin != "/opt/homebrew/bin/semantica" {
		t.Errorf("binary: got %q, want %q", bin, "/opt/homebrew/bin/semantica")
	}
}

func TestParseHookEvent_UserPromptSubmitted(t *testing.T) {
	p := &Provider{}
	input := `{"sessionId":"abc-123","prompt":"create a file","cwd":"/projects/test"}`

	event, err := p.ParseHookEvent(context.Background(), "user-prompt-submitted", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.Type != hooks.PromptSubmitted {
		t.Errorf("type: got %v, want PromptSubmitted", event.Type)
	}
	if event.SessionID != "abc-123" {
		t.Errorf("session_id: got %q", event.SessionID)
	}
	if event.Prompt != "create a file" {
		t.Errorf("prompt: got %q", event.Prompt)
	}
	// TranscriptRef should be derived from sessionId.
	if event.TranscriptRef == "" {
		t.Error("transcript_ref should be derived from sessionId")
	}
	if !strings.Contains(event.TranscriptRef, "abc-123") {
		t.Errorf("transcript_ref should contain sessionId: %q", event.TranscriptRef)
	}
}

func TestParseHookEvent_AgentStop(t *testing.T) {
	p := &Provider{}
	input := `{"sessionId":"abc-123","transcriptPath":"/tmp/copilot/session-state/abc-123/events.jsonl","cwd":"/projects/test","timestamp":12345}`

	event, err := p.ParseHookEvent(context.Background(), "agent-stop", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.Type != hooks.AgentCompleted {
		t.Errorf("type: got %v, want AgentCompleted", event.Type)
	}
	// TranscriptRef should come from stdin payload, not derived.
	if event.TranscriptRef != "/tmp/copilot/session-state/abc-123/events.jsonl" {
		t.Errorf("transcript_ref: got %q", event.TranscriptRef)
	}
	if event.Timestamp != 12345 {
		t.Errorf("timestamp: got %d, want 12345", event.Timestamp)
	}
}

func TestParseHookEvent_PreToolUseTask(t *testing.T) {
	p := &Provider{}
	input := `{
		"sessionId":"abc-123",
		"timestamp":111,
		"cwd":"/projects/test",
		"toolCalls":[
			{
				"id":"call_intent_1",
				"name":"report_intent",
				"args":"{\"intent\":\"Creating JSON via subagent\"}"
			},
			{
				"id":"call_task_123",
				"name":"task",
				"args":"{\"description\":\"Create JSON\",\"prompt\":\"Do the work\",\"agent_type\":\"general-purpose\",\"name\":\"json-creator\"}"
			}
		]
	}`

	event, err := p.ParseHookEvent(context.Background(), "pre-tool-use", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.Type != hooks.SubagentPromptSubmitted {
		t.Fatalf("type: got %v, want SubagentPromptSubmitted", event.Type)
	}
	if event.ToolName != "Agent" {
		t.Fatalf("tool_name: got %q, want Agent", event.ToolName)
	}
	if event.ToolUseID != "call_task_123" {
		t.Fatalf("tool_use_id: got %q, want call_task_123", event.ToolUseID)
	}
	if string(event.ToolInput) != `{"description":"Create JSON","prompt":"Do the work","agent_type":"general-purpose","name":"json-creator"}` {
		t.Fatalf("tool_input: got %s", event.ToolInput)
	}
}

func TestParseHookEvent_PostToolUseEdit(t *testing.T) {
	p := &Provider{}
	input := `{
		"sessionId":"abc-123",
		"timestamp":222,
		"cwd":"/projects/test",
		"toolName":"edit",
		"toolArgs":{"path":"/projects/test/file.txt","old_str":"a","new_str":"b"},
		"toolResult":{"resultType":"success","textResultForLlm":"updated"}
	}`

	event, err := p.ParseHookEvent(context.Background(), "post-tool-use", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.Type != hooks.ToolStepCompleted {
		t.Fatalf("type: got %v, want ToolStepCompleted", event.Type)
	}
	if event.ToolName != "Edit" {
		t.Fatalf("tool_name: got %q, want Edit", event.ToolName)
	}
	if event.ToolUseID == "" {
		t.Fatal("expected synthetic tool_use_id")
	}
	if !strings.Contains(string(event.ToolResponse), `"updated"`) {
		t.Fatalf("tool_response: got %s", event.ToolResponse)
	}
}

func TestParseHookEvent_PostToolUseTask(t *testing.T) {
	p := &Provider{}
	input := `{
		"sessionId":"abc-123",
		"timestamp":333,
		"cwd":"/projects/test",
		"toolName":"task",
		"toolArgs":{"description":"Create JSON","prompt":"Do the work","agent_type":"general-purpose","name":"json-creator"},
		"toolResult":{"resultType":"success","textResultForLlm":"Created file"}
	}`

	event, err := p.ParseHookEvent(context.Background(), "post-tool-use", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.Type != hooks.SubagentCompleted {
		t.Fatalf("type: got %v, want SubagentCompleted", event.Type)
	}
	if event.ToolName != "Agent" {
		t.Fatalf("tool_name: got %q, want Agent", event.ToolName)
	}
}

func TestParseHookEvent_SubagentStop(t *testing.T) {
	p := &Provider{}
	input := `{"sessionId":"child-456","cwd":"/projects/test"}`

	event, err := p.ParseHookEvent(context.Background(), "subagent-stop", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.Type != hooks.SubagentCompleted {
		t.Errorf("type: got %v, want SubagentCompleted", event.Type)
	}
	if event.TranscriptRef == "" {
		t.Error("transcript_ref should be derived from sessionId for subagent-stop")
	}
}

func TestParseHookEvent_SessionStart(t *testing.T) {
	p := &Provider{}
	input := `{"sessionId":"abc-123","cwd":"/projects/test"}`

	event, err := p.ParseHookEvent(context.Background(), "session-start", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.Type != hooks.SessionOpened {
		t.Errorf("type: got %v, want SessionOpened", event.Type)
	}
}

func TestParseHookEvent_SessionEnd(t *testing.T) {
	p := &Provider{}
	input := `{"sessionId":"abc-123","cwd":"/projects/test"}`

	event, err := p.ParseHookEvent(context.Background(), "session-end", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.Type != hooks.SessionClosed {
		t.Errorf("type: got %v, want SessionClosed", event.Type)
	}
	if event.TranscriptRef == "" {
		t.Error("transcript_ref should be derived from sessionId for session-end")
	}
}

func TestParseHookEvent_Unknown(t *testing.T) {
	p := &Provider{}
	input := `{"sessionId":"abc-123"}`

	event, err := p.ParseHookEvent(context.Background(), "pre-tool-use", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event != nil {
		t.Error("unknown hook should return nil event")
	}
}

func TestTranscriptOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	// Write 3 lines.
	content := `{"type":"user.message","data":{"content":"hello"}}
{"type":"assistant.message","data":{"content":"hi"}}
{"type":"tool.execution_complete","data":{"toolTelemetry":{"properties":{}}}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	p := &Provider{}
	offset, err := p.TranscriptOffset(context.Background(), path)
	if err != nil {
		t.Fatalf("offset: %v", err)
	}
	if offset != 3 {
		t.Errorf("offset: got %d, want 3", offset)
	}
}

func TestTranscriptOffset_NotExist(t *testing.T) {
	p := &Provider{}
	offset, err := p.TranscriptOffset(context.Background(), "/nonexistent/events.jsonl")
	if err != nil {
		t.Fatalf("should not error for missing file: %v", err)
	}
	if offset != 0 {
		t.Errorf("offset: got %d, want 0", offset)
	}
}

func TestReadFromOffset(t *testing.T) {
	dir := t.TempDir()

	// Create session directory structure.
	sessionDir := filepath.Join(dir, "session-state", "test-uuid-1234-5678-abcd-efgh12345678")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write workspace.yaml.
	if err := os.WriteFile(filepath.Join(sessionDir, "workspace.yaml"),
		[]byte("id: test-uuid\ncwd: /projects/myrepo\n"), 0o644); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	// Write transcript with 3 events.
	transcript := `{"type":"user.message","data":{"content":"create a file"}}
{"type":"assistant.message","data":{"content":"I will create it","inputTokens":9,"outputTokens":15,"toolRequests":[{"toolCallId":"call_intent","name":"report_intent","arguments":{"intent":"Creating file"}},{"toolCallId":"call_create","name":"create","arguments":{"path":"/projects/myrepo/hello.txt","file_text":"hello"}}]}}
{"type":"tool.execution_complete","data":{"toolCallId":"call_create","toolTelemetry":{"properties":{"filePaths":"[\"/projects/myrepo/hello.txt\"]"},"metrics":{"linesAdded":1}}}}
`
	transcriptPath := filepath.Join(sessionDir, "events.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	p := &Provider{}
	events, newOffset, err := p.ReadFromOffset(context.Background(), transcriptPath, 0, nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if newOffset != 3 {
		t.Errorf("new offset: got %d, want 3", newOffset)
	}
	if len(events) != 3 {
		t.Fatalf("events: got %d, want 3", len(events))
	}

	// Verify user message.
	if events[0].Role != "user" {
		t.Errorf("event[0] role: got %q, want %q", events[0].Role, "user")
	}

	// Verify assistant message with tokens.
	if events[1].Role != "assistant" {
		t.Errorf("event[1] role: got %q, want %q", events[1].Role, "assistant")
	}
	if events[1].TokensOut != 15 {
		t.Errorf("event[1] tokens_out: got %d, want 15", events[1].TokensOut)
	}
	if events[1].TokensIn != 9 {
		t.Errorf("event[1] tokens_in: got %d, want 9", events[1].TokensIn)
	}
	if events[1].ToolName != "create" {
		t.Errorf("event[1] tool_name: got %q, want %q", events[1].ToolName, "create")
	}
	if events[1].ToolUseID != "call_create" {
		t.Errorf("event[1] tool_use_id: got %q, want %q", events[1].ToolUseID, "call_create")
	}

	// Verify tool result with file paths.
	if events[2].Role != "tool" {
		t.Errorf("event[2] role: got %q, want %q", events[2].Role, "tool")
	}
	if len(events[2].FilePaths) != 1 || filepath.ToSlash(events[2].FilePaths[0]) != "/projects/myrepo/hello.txt" {
		t.Errorf("event[2] file_paths: got %v", events[2].FilePaths)
	}
	if events[2].ToolName != "copilot_file_edit" {
		t.Errorf("event[2] tool_name: got %q, want %q", events[2].ToolName, "copilot_file_edit")
	}
	if events[2].ToolUseID != "call_create" {
		t.Errorf("event[2] tool_use_id: got %q, want %q", events[2].ToolUseID, "call_create")
	}

	// Verify source project path propagated.
	if events[0].SourceProjectPath != "/projects/myrepo" {
		t.Errorf("source_project_path: got %q, want %q", events[0].SourceProjectPath, "/projects/myrepo")
	}
}

func TestReadFromOffset_SkipsAlreadyRead(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "session-state", "test-uuid-1234-5678-abcd-efgh12345678")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	transcript := `{"type":"user.message","data":{"content":"first"}}
{"type":"user.message","data":{"content":"second"}}
{"type":"user.message","data":{"content":"third"}}
`
	path := filepath.Join(sessionDir, "events.jsonl")
	if err := os.WriteFile(path, []byte(transcript), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	p := &Provider{}
	events, newOffset, err := p.ReadFromOffset(context.Background(), path, 2, nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if newOffset != 3 {
		t.Errorf("new offset: got %d, want 3", newOffset)
	}
	if len(events) != 1 {
		t.Fatalf("events: got %d, want 1 (only third line)", len(events))
	}
	if events[0].Summary != "third" {
		t.Errorf("summary: got %q, want %q", events[0].Summary, "third")
	}
}

func TestReadFromOffset_NotExist(t *testing.T) {
	p := &Provider{}
	events, offset, err := p.ReadFromOffset(context.Background(), "/nonexistent/events.jsonl", 0, nil)
	if err != nil {
		t.Fatalf("should not error for missing file: %v", err)
	}
	if events != nil {
		t.Error("expected nil events")
	}
	if offset != 0 {
		t.Errorf("offset: got %d, want 0", offset)
	}
}
