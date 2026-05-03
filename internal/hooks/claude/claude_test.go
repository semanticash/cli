package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/hooks"
)

func writeProgressLine(path string, ts time.Time, hookEvent, hookName, command string) error {
	line := `{"type":"progress","data":{"type":"hook_progress","hookEvent":"` + hookEvent + `","hookName":"` + hookName + `","command":"` + command + `"},"timestamp":"` + ts.Format(time.RFC3339Nano) + `"}`
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(line + "\n"); err != nil {
		return err
	}
	return nil
}

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

	data, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	var hooksMap map[string][]hookMatcher
	if err := json.Unmarshal(raw["hooks"], &hooksMap); err != nil {
		t.Fatalf("unmarshal hooks: %v", err)
	}

	for _, hp := range []string{"UserPromptSubmit", "Stop", "PostToolUse", "PreToolUse", "SessionStart", "SessionEnd"} {
		matchers, ok := hooksMap[hp]
		if !ok {
			t.Errorf("missing hook point %q", hp)
			continue
		}
		found := false
		for _, m := range matchers {
			for _, h := range m.Hooks {
				if strings.Contains(h.Command, semanticaMarker) {
					found = true
					if !strings.Contains(h.Command, "/usr/local/bin/semantica") {
						t.Errorf("%s: command doesn't contain binary path: %q", hp, h.Command)
					}
				}
			}
		}
		if !found {
			t.Errorf("%s: no semantica hook found", hp)
		}
	}

	// PostToolUse should have matchers for Agent, Write, Edit, Bash.
	expectedMatchers := map[string]bool{"Agent": false, "Write": false, "Edit": false, "Bash": false}
	for _, m := range hooksMap["PostToolUse"] {
		for _, h := range m.Hooks {
			if strings.Contains(h.Command, semanticaMarker) {
				if _, ok := expectedMatchers[m.Matcher]; ok {
					expectedMatchers[m.Matcher] = true
				} else {
					t.Errorf("PostToolUse: unexpected matcher %q", m.Matcher)
				}
			}
		}
	}
	for matcher, found := range expectedMatchers {
		if !found {
			t.Errorf("PostToolUse: missing matcher %q", matcher)
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

	data, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	var hooksMap map[string][]hookMatcher
	if err := json.Unmarshal(raw["hooks"], &hooksMap); err != nil {
		t.Fatalf("unmarshal hooks: %v", err)
	}

	// Count total semantica hooks - should still be 10.
	total := 0
	for _, matchers := range hooksMap {
		for _, m := range matchers {
			for _, h := range m.Hooks {
				if strings.Contains(h.Command, semanticaMarker) {
					total++
				}
			}
		}
	}
	if total != 9 {
		t.Errorf("total semantica hooks after double install: got %d, want 9", total)
	}
}

func TestInstallHooks_PreservesExistingSettings(t *testing.T) {
	dir := t.TempDir()

	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	existing := `{
  "hooks": {
    "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "echo custom"}]}]
  },
  "allowedTools": ["Edit"]
}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.local.json"), []byte(existing), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	p := &Provider{}
	_, err := p.InstallHooks(context.Background(), dir, "semantica")
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(claudeDir, "settings.local.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}

	// The extra "allowedTools" key should be preserved.
	if _, ok := raw["allowedTools"]; !ok {
		t.Error("existing 'allowedTools' key should be preserved")
	}

	// Stop should have both the custom and semantica hooks.
	var hooksMap map[string][]hookMatcher
	if err := json.Unmarshal(raw["hooks"], &hooksMap); err != nil {
		t.Fatalf("unmarshal hooks: %v", err)
	}
	matchers := hooksMap["Stop"]
	if len(matchers) < 2 {
		t.Fatalf("Stop: got %d matchers, want >= 2", len(matchers))
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

	data, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if strings.Contains(string(data), semanticaMarker) {
		t.Error("semantica marker still present after uninstall")
	}
}

func TestUninstallHooks_PreservesOtherEntries(t *testing.T) {
	dir := t.TempDir()
	p := &Provider{}

	if _, err := p.InstallHooks(context.Background(), dir, "semantica"); err != nil {
		t.Fatalf("install: %v", err)
	}

	// Add a custom hook alongside the semantica one.
	settingsPath := filepath.Join(dir, ".claude", "settings.local.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	var hooksMap map[string][]hookMatcher
	if err := json.Unmarshal(raw["hooks"], &hooksMap); err != nil {
		t.Fatalf("unmarshal hooks: %v", err)
	}
	hooksMap["Stop"] = append(hooksMap["Stop"], hookMatcher{
		Matcher: "",
		Hooks:   []hookEntry{{Type: "command", Command: "echo custom"}},
	})
	hooksJSON, err := json.Marshal(hooksMap)
	if err != nil {
		t.Fatalf("marshal hooks: %v", err)
	}
	raw["hooks"] = hooksJSON
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	if err := os.WriteFile(settingsPath, out, 0o644); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	if err := p.UninstallHooks(context.Background(), dir); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	data, err = os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("settings file should still exist: %v", err)
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal settings after uninstall: %v", err)
	}
	if err := json.Unmarshal(raw["hooks"], &hooksMap); err != nil {
		t.Fatalf("unmarshal hooks after uninstall: %v", err)
	}
	matchers := hooksMap["Stop"]
	if len(matchers) != 1 {
		t.Fatalf("Stop: got %d matchers, want 1", len(matchers))
	}
	if matchers[0].Hooks[0].Command != "echo custom" {
		t.Errorf("custom hook should be preserved, got %v", matchers[0].Hooks[0])
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

func TestParseHookEvent_UserPromptSubmit(t *testing.T) {
	p := &Provider{}
	input := `{"session_id":"sess-123","transcript_path":"/path/to/transcript.jsonl","prompt":"create a file","model":"claude-opus-4-20250514"}`

	event, err := p.ParseHookEvent(context.Background(), "user-prompt-submit", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.Type != hooks.PromptSubmitted {
		t.Errorf("type: got %v, want PromptSubmitted", event.Type)
	}
	if event.SessionID != "sess-123" {
		t.Errorf("session_id: got %q", event.SessionID)
	}
	if event.Prompt != "create a file" {
		t.Errorf("prompt: got %q", event.Prompt)
	}
	if event.TranscriptRef != "/path/to/transcript.jsonl" {
		t.Errorf("transcript_ref: got %q", event.TranscriptRef)
	}
	if event.Model != "claude-opus-4-20250514" {
		t.Errorf("model: got %q", event.Model)
	}
}

func TestParseHookEvent_Stop(t *testing.T) {
	p := &Provider{}
	input := `{"session_id":"sess-123","transcript_path":"/path/to/transcript.jsonl"}`

	event, err := p.ParseHookEvent(context.Background(), "stop", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.Type != hooks.AgentCompleted {
		t.Errorf("type: got %v, want AgentCompleted", event.Type)
	}
}

func TestParseHookEvent_SessionStart(t *testing.T) {
	p := &Provider{}
	input := `{"session_id":"sess-123"}`

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
	input := `{"session_id":"sess-123"}`

	event, err := p.ParseHookEvent(context.Background(), "session-end", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.Type != hooks.SessionClosed {
		t.Errorf("type: got %v, want SessionClosed", event.Type)
	}
}

func TestParseHookEvent_PostWrite(t *testing.T) {
	p := &Provider{}
	input := `{
		"session_id":"sess-123",
		"transcript_path":"/path/to/transcript.jsonl",
		"cwd":"/workspace/project",
		"tool_name":"Write",
		"tool_input":{"file_path":"/workspace/project/main.go","content":"package main\n"},
		"tool_response":{"type":"create","filePath":"/workspace/project/main.go"},
		"tool_use_id":"toolu_abc"
	}`

	event, err := p.ParseHookEvent(context.Background(), "post-write", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.Type != hooks.ToolStepCompleted {
		t.Errorf("type: got %v, want ToolStepCompleted", event.Type)
	}
	if event.ToolName != "Write" {
		t.Errorf("tool_name: got %q, want Write", event.ToolName)
	}
	if event.ToolUseID != "toolu_abc" {
		t.Errorf("tool_use_id: got %q", event.ToolUseID)
	}
	if event.CWD != "/workspace/project" {
		t.Errorf("cwd: got %q", event.CWD)
	}
	if event.ToolInput == nil {
		t.Error("tool_input should not be nil")
	}
	if event.ToolResponse == nil {
		t.Error("tool_response should not be nil")
	}
}

func TestParseHookEvent_PostEdit(t *testing.T) {
	p := &Provider{}
	input := `{
		"session_id":"sess-123",
		"transcript_path":"/path/to/transcript.jsonl",
		"tool_name":"Edit",
		"tool_input":{"file_path":"/repo/main.go","old_string":"foo","new_string":"bar"},
		"tool_use_id":"toolu_def"
	}`

	event, err := p.ParseHookEvent(context.Background(), "post-edit", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.Type != hooks.ToolStepCompleted {
		t.Errorf("type: got %v, want ToolStepCompleted", event.Type)
	}
	if event.ToolName != "Edit" {
		t.Errorf("tool_name: got %q, want Edit", event.ToolName)
	}
}

func TestParseHookEvent_PostBash(t *testing.T) {
	p := &Provider{}
	input := `{
		"session_id":"sess-123",
		"transcript_path":"/path/to/transcript.jsonl",
		"tool_name":"Bash",
		"tool_input":{"command":"go test ./...","description":"Run tests"},
		"tool_use_id":"toolu_ghi"
	}`

	event, err := p.ParseHookEvent(context.Background(), "post-bash", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.Type != hooks.ToolStepCompleted {
		t.Errorf("type: got %v, want ToolStepCompleted", event.Type)
	}
	if event.ToolName != "Bash" {
		t.Errorf("tool_name: got %q, want Bash", event.ToolName)
	}
}

func TestParseHookEvent_PreAgent(t *testing.T) {
	p := &Provider{}
	input := `{
		"session_id":"sess-123",
		"transcript_path":"/path/to/transcript.jsonl",
		"tool_name":"Agent",
		"tool_input":{"prompt":"Review this code"},
		"tool_use_id":"toolu_jkl"
	}`

	event, err := p.ParseHookEvent(context.Background(), "pre-agent", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.Type != hooks.SubagentPromptSubmitted {
		t.Errorf("type: got %v, want SubagentPromptSubmitted", event.Type)
	}
	if event.ToolUseID != "toolu_jkl" {
		t.Errorf("tool_use_id: got %q", event.ToolUseID)
	}
}

func TestParseHookEvent_ToolResultFallback(t *testing.T) {
	p := &Provider{}
	// tool_result instead of tool_response - should be accepted as an alias.
	input := `{
		"session_id":"sess-123",
		"transcript_path":"/path/to/transcript.jsonl",
		"tool_name":"Write",
		"tool_input":{"file_path":"/repo/x.go","content":"x"},
		"tool_result":{"type":"create"},
		"tool_use_id":"toolu_mno"
	}`

	event, err := p.ParseHookEvent(context.Background(), "post-write", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.ToolResponse == nil {
		t.Error("tool_response should be populated from tool_result alias")
	}
}

func TestParseHookEvent_Unknown(t *testing.T) {
	p := &Provider{}
	input := `{"session_id":"sess-123"}`

	event, err := p.ParseHookEvent(context.Background(), "unknown-hook", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event != nil {
		t.Error("unknown hook should return nil event")
	}
}

func TestTranscriptOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")

	content := `{"type":"system","message":{"content":[]},"timestamp":"2026-03-12T00:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]},"timestamp":"2026-03-12T00:00:01Z"}
{"type":"result","result":{"type":"success"},"timestamp":"2026-03-12T00:00:02Z"}
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

func TestReadFromOffset_StaleOffsetResetsToEOF(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "transcript.jsonl")

	lines := `{"type":"system","message":{"content":[]},"timestamp":"2026-03-12T00:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]},"timestamp":"2026-03-12T00:00:01Z"}
{"type":"result","result":{"type":"success"},"timestamp":"2026-03-12T00:00:02Z"}
`
	if err := os.WriteFile(transcript, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Provider{}
	ctx := context.Background()

	// Saved offset (100) far exceeds the 3-line file - simulates compaction.
	events, newOffset, err := p.ReadFromOffset(ctx, transcript, 100, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events after stale offset reset, got %d", len(events))
	}
	if newOffset != 3 {
		t.Errorf("expected offset reset to 3 (total lines), got %d", newOffset)
	}
}

func TestReadFromOffset_ValidOffsetReadsNormally(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "transcript.jsonl")

	lines := `{"type":"system","message":{"content":[]},"timestamp":"2026-03-12T00:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]},"timestamp":"2026-03-12T00:00:01Z"}
{"type":"result","result":{"type":"success"},"timestamp":"2026-03-12T00:00:02Z"}
`
	if err := os.WriteFile(transcript, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Provider{}
	ctx := context.Background()

	// Read from offset 1 - should get lines 1 and 2 (assistant + result).
	events, newOffset, err := p.ReadFromOffset(ctx, transcript, 1, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if newOffset != 3 {
		t.Errorf("expected offset 3, got %d", newOffset)
	}
	// At least the assistant line should produce an event (result may be skipped).
	if len(events) == 0 {
		t.Error("expected at least 1 event from valid offset read")
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

func TestDiscoverSubagentTranscripts(t *testing.T) {
	dir := t.TempDir()

	// Create parent transcript and subagent dir.
	parentPath := filepath.Join(dir, "parent-uuid.jsonl")
	if err := os.WriteFile(parentPath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write parent: %v", err)
	}

	subDir := filepath.Join(dir, "parent-uuid", "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("mkdir subagents: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "agent-abc.jsonl"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "agent-def.jsonl"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	// Compact files should be excluded.
	if err := os.WriteFile(filepath.Join(subDir, "agent-acompact-123.jsonl"), []byte("{}"), 0o644); err != nil {
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

	// Verify compact file is excluded.
	for _, p := range paths {
		if strings.Contains(p, "acompact") {
			t.Errorf("compact file should be excluded: %s", p)
		}
	}
}

func TestDiscoverSubagentTranscripts_NoSubagents(t *testing.T) {
	dir := t.TempDir()
	parentPath := filepath.Join(dir, "parent-uuid.jsonl")
	if err := os.WriteFile(parentPath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
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
		{"/path/to/parent-uuid/subagents/agent-abc.jsonl", "agent-abc"},
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

func TestReadFromOffset_ReadEventsStayLightweight(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "sess.jsonl")
	lines := []string{
		`{"type":"assistant","message":{"model":"claude-opus-4-6","id":"msg_read","type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_read","name":"Read","input":{"file_path":"/tmp/step1.txt"}}],"usage":{"input_tokens":3,"output_tokens":77}},"timestamp":"2026-03-23T14:12:33.338Z","sessionId":"sess-1"}`,
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_read","type":"tool_result","content":"     1->alpha\n     2->beta"}]},"uuid":"user_read","timestamp":"2026-03-23T14:12:33.339Z","toolUseResult":{"type":"text","file":{"filePath":"/tmp/step1.txt","content":"alpha\nbeta\n","numLines":2,"startLine":1,"totalLines":2}},"sessionId":"sess-1"}`,
		`{"type":"assistant","message":{"model":"claude-opus-4-6","id":"msg_final","type":"message","role":"assistant","content":[{"type":"text","text":"Done reading."}],"usage":{"input_tokens":1,"output_tokens":5}},"timestamp":"2026-03-23T14:12:34.000Z","sessionId":"sess-1"}`,
	}
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	p := &Provider{}
	bs := newFakeBlobPutter()
	events, _, err := p.ReadFromOffset(context.Background(), transcript, 0, bs)
	if err != nil {
		t.Fatalf("ReadFromOffset: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("events: got %d, want 3", len(events))
	}

	if events[0].PayloadHash != "" {
		t.Fatalf("read tool_use payload_hash = %q, want empty", events[0].PayloadHash)
	}
	if events[1].PayloadHash != "" {
		t.Fatalf("read tool_result payload_hash = %q, want empty", events[1].PayloadHash)
	}
	if got, want := events[1].Summary, "Read(/tmp/step1.txt) -> 2 lines"; got != want {
		t.Fatalf("read tool_result summary = %q, want %q", got, want)
	}
	if events[2].PayloadHash == "" {
		t.Fatal("final assistant payload_hash should be preserved")
	}
}

func TestCheckStopSentinel_MatchesCurrentStopHook(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "transcript.jsonl")
	hookStart := time.Now().UTC().Round(0)

	if err := writeProgressLine(transcript, hookStart.Add(-500*time.Millisecond), "PostToolUse", "PostToolUse:Write", "callback"); err != nil {
		t.Fatalf("write transcript line: %v", err)
	}
	if err := writeProgressLine(transcript, hookStart, "Stop", "Stop", "/usr/local/bin/semantica capture claude-code stop"); err != nil {
		t.Fatalf("write transcript line: %v", err)
	}

	if !checkStopSentinel(transcript, 4096, hookStart, 2*time.Second) {
		t.Fatal("expected current stop sentinel to match")
	}
}

func TestCheckStopSentinel_RejectsStaleStopHook(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "transcript.jsonl")
	hookStart := time.Now().UTC().Round(0)

	if err := writeProgressLine(transcript, hookStart.Add(-5*time.Second), "Stop", "Stop", "/usr/local/bin/semantica capture claude-code stop"); err != nil {
		t.Fatalf("write transcript line: %v", err)
	}

	if checkStopSentinel(transcript, 4096, hookStart, 2*time.Second) {
		t.Fatal("expected stale stop sentinel to be rejected")
	}
}

func TestPrepareTranscript_StopWaitsForCurrentStopSentinel(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "transcript.jsonl")
	hookStart := time.Now().UTC().Round(0)

	if err := writeProgressLine(transcript, hookStart.Add(-500*time.Millisecond), "PostToolUse", "PostToolUse:Write", "callback"); err != nil {
		t.Fatalf("write transcript line: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		time.Sleep(120 * time.Millisecond)
		errCh <- writeProgressLine(transcript, hookStart, "Stop", "Stop", "/usr/local/bin/semantica capture claude-code stop")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, hooks.HookEventTypeKey, hooks.AgentCompleted)
	ctx = context.WithValue(ctx, hooks.HookTimestampKey, hookStart.UnixMilli())

	p := &Provider{}
	start := time.Now()
	if err := p.PrepareTranscript(ctx, transcript); err != nil {
		t.Fatalf("PrepareTranscript: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("append stop sentinel: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond {
		t.Fatalf("PrepareTranscript returned too early (%s); likely accepted generic hook_progress", elapsed)
	}
	if elapsed > 900*time.Millisecond {
		t.Fatalf("PrepareTranscript returned too late (%s); likely timed out instead of matching stop sentinel", elapsed)
	}
}
