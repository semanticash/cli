package cursor

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
	if count != 9 {
		t.Errorf("count: got %d, want 9", count)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".cursor", "hooks.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var cfg cursorHooksConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if cfg.Version != 1 {
		t.Errorf("version: got %d, want 1", cfg.Version)
	}

	for _, hp := range []string{
		"sessionStart",
		"sessionEnd",
		"beforeSubmitPrompt",
		"preToolUse",
		"postToolUse",
		"afterFileEdit",
		"stop",
		"subagentStop",
		"preCompact",
	} {
		defs, ok := cfg.Hooks[hp]
		if !ok {
			t.Errorf("missing hook point %q", hp)
			continue
		}
		if len(defs) != 1 {
			t.Errorf("%s: got %d defs, want 1", hp, len(defs))
			continue
		}
		if !strings.Contains(defs[0].Command, semanticaMarker) {
			t.Errorf("%s: command doesn't contain marker: %q", hp, defs[0].Command)
		}
		if !strings.HasPrefix(defs[0].Command, "/usr/local/bin/semantica") {
			t.Errorf("%s: command doesn't start with binary path: %q", hp, defs[0].Command)
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

	data, err := os.ReadFile(filepath.Join(dir, ".cursor", "hooks.json"))
	if err != nil {
		t.Fatalf("read hooks: %v", err)
	}
	var cfg cursorHooksConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal hooks: %v", err)
	}

	for hp, defs := range cfg.Hooks {
		if len(defs) != 1 {
			t.Errorf("%s: got %d defs after double install, want 1", hp, len(defs))
		}
	}
}

func TestInstallHooks_PreservesExistingHooks(t *testing.T) {
	dir := t.TempDir()

	hooksDir := filepath.Join(dir, ".cursor")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	existing := `{
  "version": 1,
  "hooks": {
    "stop": [{"command": "echo custom hook"}]
  }
}`
	if err := os.WriteFile(filepath.Join(hooksDir, "hooks.json"), []byte(existing), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	p := &Provider{}
	_, err := p.InstallHooks(context.Background(), dir, "semantica")
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(hooksDir, "hooks.json"))
	if err != nil {
		t.Fatalf("read hooks: %v", err)
	}
	var cfg cursorHooksConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal hooks: %v", err)
	}

	defs := cfg.Hooks["stop"]
	if len(defs) != 2 {
		t.Fatalf("stop: got %d defs, want 2", len(defs))
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

	data, err := os.ReadFile(filepath.Join(dir, ".cursor", "hooks.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var cfg cursorHooksConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal hooks: %v", err)
	}

	for hp, defs := range cfg.Hooks {
		for _, h := range defs {
			if strings.Contains(h.Command, semanticaMarker) {
				t.Errorf("%s: semantica hook still present: %q", hp, h.Command)
			}
		}
	}
}

func TestUninstallHooks_PreservesOtherEntries(t *testing.T) {
	dir := t.TempDir()
	p := &Provider{}

	if _, err := p.InstallHooks(context.Background(), dir, "semantica"); err != nil {
		t.Fatalf("install: %v", err)
	}

	hooksPath := filepath.Join(dir, ".cursor", "hooks.json")
	data, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("read hooks: %v", err)
	}
	var cfg cursorHooksConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal hooks: %v", err)
	}
	cfg.Hooks["stop"] = append(cfg.Hooks["stop"],
		cursorHookDef{Command: "echo custom"})
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal hooks: %v", err)
	}
	if err := os.WriteFile(hooksPath, out, 0o644); err != nil {
		t.Fatalf("write hooks: %v", err)
	}

	if err := p.UninstallHooks(context.Background(), dir); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	data, err = os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("hooks file should still exist: %v", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal hooks after uninstall: %v", err)
	}
	defs := cfg.Hooks["stop"]
	if len(defs) != 1 || defs[0].Command != "echo custom" {
		t.Errorf("custom hook should be preserved, got %v", defs)
	}
}

func TestUninstallHooks_NoFile(t *testing.T) {
	dir := t.TempDir()
	p := &Provider{}

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

func TestParseHookEvent_BeforeSubmitPrompt(t *testing.T) {
	p := &Provider{}
	input := `{"conversation_id":"conv-123","transcript_path":"/path/to/transcript.jsonl","prompt":"create a file"}`

	event, err := p.ParseHookEvent(context.Background(), "before-submit-prompt", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.Type != hooks.PromptSubmitted {
		t.Errorf("type: got %v, want PromptSubmitted", event.Type)
	}
	if event.SessionID != "conv-123" {
		t.Errorf("session_id: got %q", event.SessionID)
	}
	if event.Prompt != "create a file" {
		t.Errorf("prompt: got %q", event.Prompt)
	}
	if event.TranscriptRef != "/path/to/transcript.jsonl" {
		t.Errorf("transcript_ref: got %q", event.TranscriptRef)
	}
}

func TestParseHookEvent_BeforeSubmitPrompt_DerivesTranscriptPath(t *testing.T) {
	p := &Provider{}
	input := `{"conversation_id":"conv-123","prompt":"create a file","workspace_roots":["/tmp/demo-project"]}`

	event, err := p.ParseHookEvent(context.Background(), "before-submit-prompt", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.TranscriptRef == "" {
		t.Fatal("expected transcript path fallback")
	}
	if !strings.Contains(event.TranscriptRef, "/.cursor/projects/tmp-demo-project/agent-transcripts/conv-123/conv-123.jsonl") {
		t.Errorf("unexpected transcript path: %q", event.TranscriptRef)
	}
	if event.CWD != "/tmp/demo-project" {
		t.Errorf("cwd: got %q, want /tmp/demo-project", event.CWD)
	}
}

func TestParseHookEvent_Stop(t *testing.T) {
	p := &Provider{}
	input := `{"conversation_id":"conv-123","transcript_path":"/path/to/transcript.jsonl"}`

	event, err := p.ParseHookEvent(context.Background(), "stop", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.Type != hooks.AgentCompleted {
		t.Errorf("type: got %v, want AgentCompleted", event.Type)
	}
}

func TestParseHookEvent_SubagentStop(t *testing.T) {
	p := &Provider{}
	input := `{"conversation_id":"conv-123","transcript_path":"/path/parent.jsonl","subagent_id":"call_123\nctx_456","subagent_type":"general-purpose","status":"completed"}`

	event, err := p.ParseHookEvent(context.Background(), "subagent-stop", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.Type != hooks.SubagentCompleted {
		t.Errorf("type: got %v, want SubagentCompleted", event.Type)
	}
	if event.ToolName != "Agent" {
		t.Errorf("tool_name: got %q, want Agent", event.ToolName)
	}
	if event.ToolUseID != "call_123 ctx_456" {
		t.Errorf("tool_use_id: got %q", event.ToolUseID)
	}
	if string(event.ToolInput) == "" {
		t.Fatal("expected raw subagent payload in tool_input")
	}
}

func TestParseHookEvent_SessionStart(t *testing.T) {
	p := &Provider{}
	input := `{"conversation_id":"conv-123"}`

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
	input := `{"conversation_id":"conv-123"}`

	event, err := p.ParseHookEvent(context.Background(), "session-end", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.Type != hooks.SessionClosed {
		t.Errorf("type: got %v, want SessionClosed", event.Type)
	}
}

func TestParseHookEvent_PreCompact(t *testing.T) {
	p := &Provider{}
	input := `{"conversation_id":"conv-123"}`

	event, err := p.ParseHookEvent(context.Background(), "pre-compact", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.Type != hooks.ContextCompacted {
		t.Errorf("type: got %v, want ContextCompacted", event.Type)
	}
}

func TestParseHookEvent_Unknown(t *testing.T) {
	p := &Provider{}
	input := `{"conversation_id":"conv-123","tool_name":"Read"}`

	event, err := p.ParseHookEvent(context.Background(), "pre-tool-use", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event != nil {
		t.Error("unknown hook should return nil event")
	}
}

func TestParseHookEvent_PostToolUseShell(t *testing.T) {
	p := &Provider{}
	input := `{
		"conversation_id":"conv-123",
		"transcript_path":"/path/to/transcript.jsonl",
		"tool_name":"Shell",
		"tool_use_id":"call_123\nctx_456",
		"tool_input":{"command":"cat file.txt","cwd":"/repo","timeout":30000},
		"tool_output":"{\"output\":\"ok\\n\",\"exitCode\":0}",
		"cwd":"/repo"
	}`

	event, err := p.ParseHookEvent(context.Background(), "post-tool-use", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.Type != hooks.ToolStepCompleted {
		t.Fatalf("type: got %v, want ToolStepCompleted", event.Type)
	}
	if event.ToolName != "Bash" {
		t.Errorf("tool_name: got %q, want Bash", event.ToolName)
	}
	if event.ToolUseID != "call_123 ctx_456" {
		t.Errorf("tool_use_id: got %q", event.ToolUseID)
	}
	if string(event.ToolInput) == "" {
		t.Fatal("expected raw hook payload in tool_input")
	}
	if event.CWD != "/repo" {
		t.Errorf("cwd: got %q, want /repo", event.CWD)
	}
}

func TestParseHookEvent_PreToolUseAgent(t *testing.T) {
	p := &Provider{}
	input := `{
		"conversation_id":"conv-123",
		"transcript_path":"/path/to/transcript.jsonl",
		"tool_name":"Agent",
		"tool_use_id":"agent_123\nctx_456",
		"tool_input":{"prompt":"Review the code"},
		"workspace_roots":["/repo"]
	}`

	event, err := p.ParseHookEvent(context.Background(), "pre-tool-use", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.Type != hooks.SubagentPromptSubmitted {
		t.Fatalf("type: got %v, want SubagentPromptSubmitted", event.Type)
	}
	if event.ToolName != "Agent" {
		t.Errorf("tool_name: got %q, want Agent", event.ToolName)
	}
	if event.ToolUseID != "agent_123 ctx_456" {
		t.Errorf("tool_use_id: got %q", event.ToolUseID)
	}
}

func TestParseHookEvent_AfterFileEditWrite(t *testing.T) {
	p := &Provider{}
	input := `{
		"conversation_id":"conv-123",
		"generation_id":"gen-1",
		"file_path":"/repo/new.txt",
		"edits":[{"old_string":"","new_string":"hello\n"}],
		"workspace_roots":["/repo"]
	}`

	event, err := p.ParseHookEvent(context.Background(), "after-file-edit", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.Type != hooks.ToolStepCompleted {
		t.Fatalf("type: got %v, want ToolStepCompleted", event.Type)
	}
	if event.ToolName != "Write" {
		t.Errorf("tool_name: got %q, want Write", event.ToolName)
	}
	if event.ToolUseID == "" {
		t.Fatal("expected synthetic tool_use_id")
	}
	if event.CWD != "/repo" {
		t.Errorf("cwd: got %q, want /repo", event.CWD)
	}
}

func TestParseHookEvent_AfterFileEditEdit(t *testing.T) {
	p := &Provider{}
	input := `{
		"conversation_id":"conv-123",
		"generation_id":"gen-1",
		"file_path":"/repo/main.go",
		"edits":[{"old_string":"foo","new_string":"bar"}],
		"workspace_roots":["/repo"]
	}`

	event, err := p.ParseHookEvent(context.Background(), "after-file-edit", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.ToolName != "Edit" {
		t.Errorf("tool_name: got %q, want Edit", event.ToolName)
	}
}

func TestTranscriptOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")

	content := `{"role":"user","message":{"content":[{"type":"text","text":"hello"}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}
{"role":"user","message":{"content":[{"type":"text","text":"bye"}]}}
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
	offset, err := p.TranscriptOffset(context.Background(), "/nonexistent/transcript.jsonl")
	if err != nil {
		t.Fatalf("should not error for missing file: %v", err)
	}
	if offset != 0 {
		t.Errorf("offset: got %d, want 0", offset)
	}
}

func TestReadFromOffset(t *testing.T) {
	dir := t.TempDir()

	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"create a file"}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"I will create it"},{"type":"tool_use","name":"Edit","input":{"file_path":"/projects/myrepo/hello.txt"}}]}}
`
	path := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(path, []byte(transcript), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	p := &Provider{}
	events, newOffset, err := p.ReadFromOffset(context.Background(), path, 0, nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if newOffset != 2 {
		t.Errorf("new offset: got %d, want 2", newOffset)
	}
	if len(events) != 2 {
		t.Fatalf("events: got %d, want 2", len(events))
	}

	if events[0].Role != "user" {
		t.Errorf("event[0] role: got %q, want %q", events[0].Role, "user")
	}
	if events[1].Role != "assistant" {
		t.Errorf("event[1] role: got %q, want %q", events[1].Role, "assistant")
	}
	if events[1].Summary != "I will create it" {
		t.Errorf("event[1] summary: got %q", events[1].Summary)
	}
}

func TestReadFromOffset_SkipsAlreadyRead(t *testing.T) {
	dir := t.TempDir()

	transcript := `{"role":"user","message":{"content":[{"type":"text","text":"first"}]}}
{"role":"user","message":{"content":[{"type":"text","text":"second"}]}}
{"role":"user","message":{"content":[{"type":"text","text":"third"}]}}
`
	path := filepath.Join(dir, "transcript.jsonl")
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
		t.Fatalf("events: got %d, want 1", len(events))
	}
	if events[0].Summary != "third" {
		t.Errorf("summary: got %q, want %q", events[0].Summary, "third")
	}
}

func TestReadFromOffset_NotExist(t *testing.T) {
	p := &Provider{}
	events, offset, err := p.ReadFromOffset(context.Background(), "/nonexistent/transcript.jsonl", 0, nil)
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

func TestExtractConversationID(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/workspace/.cursor/agent-transcripts/abc-123.jsonl", "abc-123"},
		{"/tmp/test/conv-456.jsonl", "conv-456"},
		{"simple.jsonl", "simple"},
	}
	for _, tt := range tests {
		got := extractConversationID(tt.path)
		if got != tt.want {
			t.Errorf("extractConversationID(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestExtractParentConversationID(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/workspace/.cursor/projects/repo/agent-transcripts/parent-123/subagents/child-456.jsonl", "parent-123"},
		{"/tmp/test/subagents/child.jsonl", "test"},
		{"/tmp/test/standalone.jsonl", ""},
	}
	for _, tt := range tests {
		got := extractParentConversationID(tt.path)
		if got != tt.want {
			t.Errorf("extractParentConversationID(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestDiscoverSubagentTranscripts(t *testing.T) {
	dir := t.TempDir()

	parentDir := filepath.Join(dir, "agent-transcripts", "parent-uuid")
	parentPath := filepath.Join(parentDir, "parent-uuid.jsonl")
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(parentPath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write parent: %v", err)
	}

	subDir := filepath.Join(parentDir, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("mkdir subagents: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "child-a.jsonl"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write child: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "child-b.jsonl"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write child: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "ignore.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	p := &Provider{}
	paths, err := p.DiscoverSubagentTranscripts(context.Background(), parentPath)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("paths: got %d, want 2", len(paths))
	}
}

func TestDiscoverSubagentTranscripts_NoSubagents(t *testing.T) {
	dir := t.TempDir()
	parentDir := filepath.Join(dir, "agent-transcripts", "parent-uuid")
	parentPath := filepath.Join(parentDir, "parent-uuid.jsonl")
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(parentPath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write parent: %v", err)
	}

	p := &Provider{}
	paths, err := p.DiscoverSubagentTranscripts(context.Background(), parentPath)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if paths != nil {
		t.Errorf("expected nil paths, got %v", paths)
	}
}

func TestSubagentStateKey(t *testing.T) {
	p := &Provider{}
	tests := []struct {
		path string
		want string
	}{
		{"/path/to/subagents/child-abc.jsonl", "child-abc"},
		{"/tmp/test/sub.jsonl", "sub"},
		{"simple.jsonl", "simple"},
	}
	for _, tt := range tests {
		got := p.SubagentStateKey(tt.path)
		if got != tt.want {
			t.Errorf("SubagentStateKey(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}
