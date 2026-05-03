package gemini

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

	data, err := os.ReadFile(filepath.Join(dir, ".gemini", "settings.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// hooksConfig.enabled should be true.
	var hc struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(raw["hooksConfig"], &hc); err != nil {
		t.Fatalf("unmarshal hooksConfig: %v", err)
	}
	if !hc.Enabled {
		t.Error("hooksConfig.enabled should be true")
	}

	var hooksMap map[string][]geminiHookMatcher
	if err := json.Unmarshal(raw["hooks"], &hooksMap); err != nil {
		t.Fatalf("unmarshal hooks: %v", err)
	}

	for _, hp := range []string{"BeforeAgent", "AfterAgent", "SessionStart", "SessionEnd", "PreCompress"} {
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

	// SessionEnd should have two matchers (exit + logout).
	matchers := hooksMap["SessionEnd"]
	semCount := 0
	for _, m := range matchers {
		for _, h := range m.Hooks {
			if strings.Contains(h.Command, semanticaMarker) {
				semCount++
			}
		}
	}
	if semCount != 2 {
		t.Errorf("SessionEnd: got %d semantica hooks, want 2", semCount)
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

	data, err := os.ReadFile(filepath.Join(dir, ".gemini", "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	var hooksMap map[string][]geminiHookMatcher
	if err := json.Unmarshal(raw["hooks"], &hooksMap); err != nil {
		t.Fatalf("unmarshal hooks: %v", err)
	}

	// Count total semantica hooks across all points - should still be 6.
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

	geminiDir := filepath.Join(dir, ".gemini")
	if err := os.MkdirAll(geminiDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	existing := `{
  "hooksConfig": {"enabled": true},
  "hooks": {
    "BeforeAgent": [{"matcher": "", "hooks": [{"name": "custom", "type": "command", "command": "echo hello"}]}]
  },
  "theme": "dark"
}`
	if err := os.WriteFile(filepath.Join(geminiDir, "settings.json"), []byte(existing), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	p := &Provider{}
	_, err := p.InstallHooks(context.Background(), dir, "semantica")
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(geminiDir, "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}

	// The extra "theme" key should be preserved.
	if _, ok := raw["theme"]; !ok {
		t.Error("existing 'theme' key should be preserved")
	}

	// BeforeAgent should have both the custom and semantica hooks.
	var hooksMap map[string][]geminiHookMatcher
	if err := json.Unmarshal(raw["hooks"], &hooksMap); err != nil {
		t.Fatalf("unmarshal hooks: %v", err)
	}
	matchers := hooksMap["BeforeAgent"]
	if len(matchers) < 2 {
		t.Fatalf("BeforeAgent: got %d matchers, want >= 2", len(matchers))
	}
}

// TestInstallHooks_RespectsHandEditedCommand documents the deliberate
// choice to leave manually-edited hook entries alone. If the user (or
// a debugging workflow) put a custom command under our name, enable
// must not silently rewrite it. Resetting is a `disable` + `enable`
// round-trip because `disable` strips entries by marker.
func TestInstallHooks_RespectsHandEditedCommand(t *testing.T) {
	dir := t.TempDir()

	geminiDir := filepath.Join(dir, ".gemini")
	if err := os.MkdirAll(geminiDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	existing := `{
  "hooks": {
    "AfterTool": [{"matcher": "*", "hooks": [{"name": "semantica-after-tool", "type": "command", "command": "/tmp/my-tracer.sh after-tool"}]}]
  }
}`
	if err := os.WriteFile(filepath.Join(geminiDir, "settings.json"), []byte(existing), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	p := &Provider{}
	if _, err := p.InstallHooks(context.Background(), dir, "semantica"); err != nil {
		t.Fatalf("install: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(geminiDir, "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var hooksMap map[string][]geminiHookMatcher
	if err := json.Unmarshal(raw["hooks"], &hooksMap); err != nil {
		t.Fatalf("unmarshal hooks: %v", err)
	}

	// Exactly one AfterTool entry, the user's custom one, untouched.
	matchers := hooksMap["AfterTool"]
	totalAfterTool := 0
	preservedTracer := false
	for _, m := range matchers {
		for _, h := range m.Hooks {
			if h.Name == "semantica-after-tool" {
				totalAfterTool++
				if strings.Contains(h.Command, "my-tracer") {
					preservedTracer = true
				}
			}
		}
	}
	if totalAfterTool != 1 {
		t.Errorf("expected exactly 1 AfterTool semantica entry, got %d", totalAfterTool)
	}
	if !preservedTracer {
		t.Error("hand-edited tracer command must be preserved by enable")
	}
}

// TestInstallHooks_DoesNotEscapeShellMetacharacters protects against
// regressing the unescaped-output behavior across the file. Hook
// commands carry `>` and `&`; the persisted settings should keep
// them literal so humans can read the file.
func TestInstallHooks_DoesNotEscapeShellMetacharacters(t *testing.T) {
	dir := t.TempDir()

	p := &Provider{}
	if _, err := p.InstallHooks(context.Background(), dir, "semantica"); err != nil {
		t.Fatalf("install: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".gemini", "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if strings.Contains(string(data), `\u003e`) || strings.Contains(string(data), `\u0026`) {
		t.Errorf("expected unescaped shell metacharacters, got escaped form:\n%s", data)
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

	data, err := os.ReadFile(filepath.Join(dir, ".gemini", "settings.json"))
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
	settingsPath := filepath.Join(dir, ".gemini", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	var hooksMap map[string][]geminiHookMatcher
	if err := json.Unmarshal(raw["hooks"], &hooksMap); err != nil {
		t.Fatalf("unmarshal hooks: %v", err)
	}
	hooksMap["BeforeAgent"] = append(hooksMap["BeforeAgent"], geminiHookMatcher{
		Matcher: "",
		Hooks:   []geminiHookEntry{{Name: "custom", Type: "command", Command: "echo custom"}},
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
	matchers := hooksMap["BeforeAgent"]
	if len(matchers) != 1 {
		t.Fatalf("BeforeAgent: got %d matchers, want 1", len(matchers))
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

func TestParseHookEvent_BeforeAgent(t *testing.T) {
	p := &Provider{}
	input := `{"session_id":"sess-123","transcript_path":"/path/to/transcript.json","prompt":"create a file"}`

	event, err := p.ParseHookEvent(context.Background(), "before-agent", strings.NewReader(input))
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
	if event.TranscriptRef != "/path/to/transcript.json" {
		t.Errorf("transcript_ref: got %q", event.TranscriptRef)
	}
}

func TestParseHookEvent_AfterAgent(t *testing.T) {
	p := &Provider{}
	input := `{"session_id":"sess-123","transcript_path":"/path/to/transcript.json"}`

	event, err := p.ParseHookEvent(context.Background(), "after-agent", strings.NewReader(input))
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

func TestParseHookEvent_PreCompress(t *testing.T) {
	p := &Provider{}
	input := `{"session_id":"sess-123"}`

	event, err := p.ParseHookEvent(context.Background(), "pre-compress", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event.Type != hooks.ContextCompacted {
		t.Errorf("type: got %v, want ContextCompacted", event.Type)
	}
}

// TestParseHookEvent_AfterTool_WriteFile_040Shape covers file-edit
// payloads that omit tool_name.
func TestParseHookEvent_AfterTool_WriteFile_040Shape(t *testing.T) {
	p := &Provider{}
	input := `{
		"session_id": "sess-040",
		"tool_input": {
			"content": "# hello\n",
			"file_path": "AUDIT.md"
		},
		"tool_response": {
			"llmContent": "Successfully created and wrote to new file: AUDIT.md."
		}
	}`

	event, err := p.ParseHookEvent(context.Background(), "after-tool", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.Type != hooks.ToolStepCompleted {
		t.Errorf("type: got %v, want ToolStepCompleted", event.Type)
	}
	if event.ToolName != "Write" {
		t.Errorf("tool_name: got %q, want Write", event.ToolName)
	}
	if event.ToolUseID == "" {
		t.Error("tool_use_id should be synthesized when tool_name is absent")
	}
}

// TestParseHookEvent_AfterTool_Replace_040Shape covers replace
// payloads that omit tool_name.
func TestParseHookEvent_AfterTool_Replace_040Shape(t *testing.T) {
	p := &Provider{}
	input := `{
		"session_id": "sess-040",
		"tool_input": {
			"old_string": "- alpha",
			"new_string": "- alpha-line",
			"file_path": "AUDIT.md",
			"instruction": "Change '- alpha' to '- alpha-line'."
		},
		"tool_response": {
			"llmContent": "Successfully modified file: AUDIT.md."
		}
	}`

	event, err := p.ParseHookEvent(context.Background(), "after-tool", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.ToolName != "Edit" {
		t.Errorf("tool_name: got %q, want Edit", event.ToolName)
	}
}

// TestParseHookEvent_AfterTool_Bash_BothShapes covers Bash payloads
// with and without explicit tool_name.
func TestParseHookEvent_AfterTool_Bash_BothShapes(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{
			name:  "explicit tool_name",
			input: `{"session_id":"s","tool_name":"run_shell_command","tool_input":{"command":"ls","description":""},"tool_response":{"output":""}}`,
		},
		{
			name:  "inferred from command",
			input: `{"session_id":"s","tool_input":{"command":"ls","description":""},"tool_response":{"output":""}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Provider{}
			event, err := p.ParseHookEvent(context.Background(), "after-tool", strings.NewReader(tc.input))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if event == nil {
				t.Fatal("expected event, got nil")
			}
			if event.ToolName != "Bash" {
				t.Errorf("tool_name: got %q, want Bash", event.ToolName)
			}
		})
	}
}

// TestParseHookEvent_AfterTool_UnknownShape confirms read-only or
// unknown shapes do not produce attribution events.
func TestParseHookEvent_AfterTool_UnknownShape(t *testing.T) {
	p := &Provider{}
	input := `{"session_id":"s","tool_input":{"path":"some/file","limit":10},"tool_response":{"content":"..."}}`

	event, err := p.ParseHookEvent(context.Background(), "after-tool", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event != nil {
		t.Errorf("unknown payload shape should not produce an event, got %+v", event)
	}
}

// TestInferGeminiToolName covers the shape-inference table directly.
func TestInferGeminiToolName(t *testing.T) {
	cases := []struct {
		name      string
		toolName  string
		toolInput string
		want      string
	}{
		{
			name:      "explicit name wins over shape",
			toolName:  "run_shell_command",
			toolInput: `{"command":"ls"}`,
			want:      "run_shell_command",
		},
		{
			name:      "write_file inferred from content + file_path",
			toolName:  "",
			toolInput: `{"content":"hi","file_path":"f.md"}`,
			want:      "write_file",
		},
		{
			name:      "replace inferred from old_string + new_string + file_path",
			toolName:  "",
			toolInput: `{"old_string":"a","new_string":"b","file_path":"f.md"}`,
			want:      "replace",
		},
		{
			name:      "run_shell_command inferred from command alone",
			toolName:  "",
			toolInput: `{"command":"ls"}`,
			want:      "run_shell_command",
		},
		{
			name:      "unknown shape returns empty",
			toolName:  "",
			toolInput: `{"path":"f.md"}`,
			want:      "",
		},
		{
			name:      "empty input returns empty",
			toolName:  "",
			toolInput: ``,
			want:      "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := inferGeminiToolName(tc.toolName, []byte(tc.toolInput))
			if got != tc.want {
				t.Errorf("inferGeminiToolName(%q, %q) = %q, want %q",
					tc.toolName, tc.toolInput, got, tc.want)
			}
		})
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

func TestParseHookEvent_WithTimestamp(t *testing.T) {
	p := &Provider{}
	input := `{"session_id":"sess-123","timestamp":"2026-03-13T10:00:00Z"}`

	event, err := p.ParseHookEvent(context.Background(), "before-agent", strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Verify the timestamp was parsed (not fallback to time.Now).
	// 2026-03-13T10:00:00Z = 1773396000 seconds.
	want := int64(1773396000000)
	if event.Timestamp != want {
		t.Errorf("timestamp: got %d, want %d", event.Timestamp, want)
	}
}

func TestTranscriptOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")

	content := `{"messages":[
		{"type":"user","content":"hello"},
		{"type":"gemini","content":"hi there"},
		{"type":"user","content":"bye"}
	]}`
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
	offset, err := p.TranscriptOffset(context.Background(), "/nonexistent/session.json")
	if err != nil {
		t.Fatalf("should not error for missing file: %v", err)
	}
	if offset != 0 {
		t.Errorf("offset: got %d, want 0", offset)
	}
}

func TestReadFromOffset(t *testing.T) {
	dir := t.TempDir()

	transcript := `{"messages":[
		{"type":"user","content":"create a file"},
		{"type":"gemini","content":"I will create it","toolCalls":[{"id":"tc1","name":"write_file","args":{"path":"/tmp/hello.txt"}}]}
	]}`
	path := filepath.Join(dir, "session.json")
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

// TestReadFromOffset_ResolvesRelativeToolPaths ensures transcript
// replay resolves relative tool-call file paths before routing.
// Uses filepath.Join for the expected value so the assertion holds
// on Windows (where filepath.Join produces backslash-separated paths).
func TestReadFromOffset_ResolvesRelativeToolPaths(t *testing.T) {
	dir := t.TempDir()

	transcript := `{"messages":[
		{"type":"gemini","content":"editing","toolCalls":[{"id":"tc1","name":"write_file","args":{"file_path":"src/a.go","content":"x"}}]}
	]}`
	path := filepath.Join(dir, "session.json")
	if err := os.WriteFile(path, []byte(transcript), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cwd := filepath.FromSlash("/repo")
	ctx := context.WithValue(context.Background(), hooks.CWDKey, cwd)
	p := &Provider{}
	events, _, err := p.ReadFromOffset(ctx, path, 0, nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events: got %d, want 1", len(events))
	}

	wantPath := filepath.Join(cwd, "src", "a.go")
	ev := events[0]
	if len(ev.FilePaths) != 1 || ev.FilePaths[0] != wantPath {
		t.Errorf("file_paths: got %v, want [%s]", ev.FilePaths, wantPath)
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

// TestReadFromOffset_NoCWDPreservesRelative documents that without a
// CWD on the context the existing relative path is kept verbatim. This
// avoids inventing a path the replay cannot justify when transcript
// scans are run outside the hook lifecycle.
func TestReadFromOffset_NoCWDPreservesRelative(t *testing.T) {
	dir := t.TempDir()

	transcript := `{"messages":[
		{"type":"gemini","content":"editing","toolCalls":[{"id":"tc1","name":"write_file","args":{"file_path":"src/a.go","content":"x"}}]}
	]}`
	path := filepath.Join(dir, "session.json")
	if err := os.WriteFile(path, []byte(transcript), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	p := &Provider{}
	events, _, err := p.ReadFromOffset(context.Background(), path, 0, nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events: got %d, want 1", len(events))
	}
	if !strings.Contains(events[0].ToolUsesJSON, `src/a.go`) || strings.Contains(events[0].ToolUsesJSON, `/repo/`) {
		t.Errorf("expected relative path preserved, got %q", events[0].ToolUsesJSON)
	}
}

func TestReadFromOffset_SkipsAlreadyRead(t *testing.T) {
	dir := t.TempDir()

	transcript := `{"messages":[
		{"type":"user","content":"first"},
		{"type":"user","content":"second"},
		{"type":"user","content":"third"}
	]}`
	path := filepath.Join(dir, "session.json")
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
	events, offset, err := p.ReadFromOffset(context.Background(), "/nonexistent/session.json", 0, nil)
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

func TestReadFromOffset_WithTokens(t *testing.T) {
	dir := t.TempDir()

	transcript := `{"messages":[
		{"type":"gemini","content":"hello","tokens":{"input":100,"output":50,"cached":10,"thoughts":5,"tool":0,"total":165}}
	]}`
	path := filepath.Join(dir, "session.json")
	if err := os.WriteFile(path, []byte(transcript), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	p := &Provider{}
	events, _, err := p.ReadFromOffset(context.Background(), path, 0, nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events: got %d, want 1", len(events))
	}
	if events[0].TokensIn != 100 {
		t.Errorf("tokens_in: got %d, want 100", events[0].TokensIn)
	}
	if events[0].TokensOut != 50 {
		t.Errorf("tokens_out: got %d, want 50", events[0].TokensOut)
	}
	if events[0].TokensCacheRead != 10 {
		t.Errorf("tokens_cache_read: got %d, want 10", events[0].TokensCacheRead)
	}
}
