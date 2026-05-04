package kirocli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/hooks"

	_ "modernc.org/sqlite"
)

func TestInstallHooks_NewConfig(t *testing.T) {
	repoRoot := t.TempDir()
	p := &Provider{}

	count, err := p.InstallHooks(context.Background(), repoRoot, "/usr/local/bin/semantica")
	if err != nil {
		t.Fatal(err)
	}
	if count != 5 {
		t.Errorf("installed %d hooks, want 5", count)
	}

	configPath := filepath.Join(repoRoot, ".kiro", "agents", "semantica.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("config not found: %v", err)
	}

	if !strings.Contains(string(data), semanticaMarker) {
		t.Error("missing semantica marker in command")
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}

	// Top-level fields the agent needs to be usable when selected.
	var name, description string
	_ = json.Unmarshal(raw["name"], &name)
	_ = json.Unmarshal(raw["description"], &description)
	if name != agentName {
		t.Errorf("name = %q, want %q", name, agentName)
	}
	if description == "" {
		t.Error("description is empty")
	}

	var tools []string
	if err := json.Unmarshal(raw["tools"], &tools); err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0] != "*" {
		t.Errorf(`tools = %v, want ["*"]`, tools)
	}

	// Hook surface: agentSpawn, userPromptSubmit, two postToolUse
	// entries with distinct matchers, and stop.
	var hooksMap map[string][]agentConfigHookEntry
	if err := json.Unmarshal(raw["hooks"], &hooksMap); err != nil {
		t.Fatal(err)
	}
	if _, ok := hooksMap["preToolUse"]; ok {
		t.Error("preToolUse should not be registered")
	}
	for _, ev := range []string{"agentSpawn", "userPromptSubmit", "stop"} {
		entries := hooksMap[ev]
		if len(entries) != 1 || entries[0].Matcher != "" {
			t.Errorf("%s entries = %+v, want exactly one unmatched entry", ev, entries)
		}
	}
	postEntries := hooksMap["postToolUse"]
	if len(postEntries) != 2 {
		t.Fatalf("postToolUse entries = %d, want 2", len(postEntries))
	}
	matchers := map[string]bool{}
	for _, e := range postEntries {
		matchers[e.Matcher] = true
	}
	for _, m := range []string{"fs_write", "execute_bash"} {
		if !matchers[m] {
			t.Errorf("postToolUse missing matcher %q", m)
		}
	}
}

func TestInstallHooks_RefreshesStaleFieldsAndPreservesUserContent(t *testing.T) {
	repoRoot := t.TempDir()
	agentsDir := filepath.Join(repoRoot, ".kiro", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Pre-existing config: stale tools (empty) and missing
	// description, plus a user-added prompt that must survive.
	existing := `{
  "name": "old-name",
  "tools": [],
  "prompt": "You are a security-focused assistant.",
  "hooks": {}
}`
	if err := os.WriteFile(filepath.Join(agentsDir, "semantica.json"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Provider{}
	if _, err := p.InstallHooks(context.Background(), repoRoot, "/usr/local/bin/semantica"); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(filepath.Join(agentsDir, "semantica.json"))
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}

	// Semantica-owned fields are refreshed.
	var name, description string
	var tools []string
	_ = json.Unmarshal(raw["name"], &name)
	_ = json.Unmarshal(raw["description"], &description)
	_ = json.Unmarshal(raw["tools"], &tools)
	if name != agentName {
		t.Errorf("name not refreshed: got %q", name)
	}
	if description == "" {
		t.Error("description not refreshed")
	}
	if len(tools) != 1 || tools[0] != "*" {
		t.Errorf("tools not refreshed: got %v", tools)
	}

	// User-added top-level field survives.
	var prompt string
	_ = json.Unmarshal(raw["prompt"], &prompt)
	if prompt != "You are a security-focused assistant." {
		t.Errorf("user prompt not preserved: got %q", prompt)
	}
}

// TestInstallHooks_RemovesStaleSemanticaEntriesOnUpgrade simulates
// a user upgrading from an earlier Kiro CLI integration that wrote
// unmatched postToolUse and preToolUse rows. The current install
// must drop those stale rows before adding the canonical matcher-
// scoped entries, otherwise the unmatched rows fire for every tool
// and duplicate capture.
func TestInstallHooks_RemovesStaleSemanticaEntriesOnUpgrade(t *testing.T) {
	repoRoot := t.TempDir()
	agentsDir := filepath.Join(repoRoot, ".kiro", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	staleCmd := "if command -v semantica >/dev/null 2>&1; then semantica capture kiro-cli post-tool-use || true; fi"
	stalePreCmd := "if command -v semantica >/dev/null 2>&1; then semantica capture kiro-cli pre-tool-use || true; fi"
	existing := `{
  "hooks": {
    "preToolUse":  [{"command": "` + stalePreCmd + `"}],
    "postToolUse": [{"command": "` + staleCmd + `"}]
  }
}`
	if err := os.WriteFile(filepath.Join(agentsDir, "semantica.json"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Provider{}
	if _, err := p.InstallHooks(context.Background(), repoRoot, "semantica"); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(filepath.Join(agentsDir, "semantica.json"))
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(data, &raw)
	var hooksMap map[string][]agentConfigHookEntry
	_ = json.Unmarshal(raw["hooks"], &hooksMap)

	if _, ok := hooksMap["preToolUse"]; ok {
		t.Errorf("stale preToolUse entry survived upgrade: %+v", hooksMap["preToolUse"])
	}
	postEntries := hooksMap["postToolUse"]
	if len(postEntries) != 2 {
		t.Fatalf("postToolUse entries = %d, want 2 (unmatched stale entry must be removed)", len(postEntries))
	}
	for _, e := range postEntries {
		if e.Matcher == "" {
			t.Errorf("unmatched postToolUse entry survived: %+v", e)
		}
	}
}

func TestInstallHooks_DedupesByEventMatcherCommand(t *testing.T) {
	repoRoot := t.TempDir()
	agentsDir := filepath.Join(repoRoot, ".kiro", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Pre-seed only one of the two postToolUse entries. A
	// marker-only de-dupe would skip adding the execute_bash row
	// because it sees the marker on the fs_write row and assumes
	// the event is already covered. The (event, matcher, command)
	// rule must still add the missing matcher.
	existing := `{
  "hooks": {
    "postToolUse": [
      {"matcher": "fs_write", "command": "if command -v semantica >/dev/null 2>&1; then semantica capture kiro-cli post-tool-use || true; fi"}
    ]
  }
}`
	if err := os.WriteFile(filepath.Join(agentsDir, "semantica.json"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Provider{}
	if _, err := p.InstallHooks(context.Background(), repoRoot, "semantica"); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(filepath.Join(agentsDir, "semantica.json"))
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(data, &raw)
	var hooksMap map[string][]agentConfigHookEntry
	_ = json.Unmarshal(raw["hooks"], &hooksMap)

	postEntries := hooksMap["postToolUse"]
	if len(postEntries) != 2 {
		t.Fatalf("postToolUse entries = %d, want 2 after refresh", len(postEntries))
	}
	matchers := map[string]int{}
	for _, e := range postEntries {
		matchers[e.Matcher]++
	}
	if matchers["fs_write"] != 1 {
		t.Errorf("fs_write entries = %d, want 1", matchers["fs_write"])
	}
	if matchers["execute_bash"] != 1 {
		t.Errorf("execute_bash entries = %d, want 1", matchers["execute_bash"])
	}
}

func TestInstallHooks_Idempotent(t *testing.T) {
	repoRoot := t.TempDir()
	p := &Provider{}

	_, _ = p.InstallHooks(context.Background(), repoRoot, "/usr/local/bin/semantica")
	_, err := p.InstallHooks(context.Background(), repoRoot, "/usr/local/bin/semantica")
	if err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(filepath.Join(repoRoot, ".kiro", "agents", "semantica.json"))
	count := strings.Count(string(data), semanticaMarker)
	if count != 5 {
		t.Errorf("marker appears %d times, want 5 (one per hook)", count)
	}
}

func TestUninstallHooks_RemovesOnlySemanticaHooks(t *testing.T) {
	repoRoot := t.TempDir()
	p := &Provider{}

	_, _ = p.InstallHooks(context.Background(), repoRoot, "/usr/local/bin/semantica")
	if !p.AreHooksInstalled(context.Background(), repoRoot) {
		t.Fatal("hooks not detected after install")
	}

	configPath := filepath.Join(repoRoot, ".kiro", "agents", "semantica.json")
	data, _ := os.ReadFile(configPath)
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(data, &raw)
	var hooksMap map[string][]agentConfigHookEntry
	_ = json.Unmarshal(raw["hooks"], &hooksMap)
	hooksMap["stop"] = append(hooksMap["stop"], agentConfigHookEntry{Command: "my-custom-hook"})
	hooksJSON, _ := json.Marshal(hooksMap)
	raw["hooks"] = hooksJSON
	out, _ := json.MarshalIndent(raw, "", "  ")
	_ = os.WriteFile(configPath, out, 0o644)

	if err := p.UninstallHooks(context.Background(), repoRoot); err != nil {
		t.Fatal(err)
	}
	if p.AreHooksInstalled(context.Background(), repoRoot) {
		t.Error("semantica hooks still detected after uninstall")
	}

	afterData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal("config file was deleted instead of cleaned")
	}
	if !strings.Contains(string(afterData), "my-custom-hook") {
		t.Error("user hook was removed")
	}
	if strings.Contains(string(afterData), semanticaMarker) {
		t.Error("semantica hooks not removed")
	}
}

func TestParseHookEvent_UserPromptSubmit(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	p := &Provider{
		resolveConversation: func(workspacePath string) (string, string, error) {
			return "/mock/db.sqlite3", "conv-123", nil
		},
		loadConv: func(dbPath, convID string) (*conversationValue, error) {
			return &conversationValue{ConversationID: convID, History: nil}, nil
		},
	}

	stdin := strings.NewReader(`{"hook_event_name":"userPromptSubmit","cwd":"/workspace","prompt":"do something"}`)
	event, err := p.ParseHookEvent(context.Background(), "user-prompt-submit", stdin)
	if err != nil {
		t.Fatal(err)
	}
	if event.Type != hooks.PromptSubmitted {
		t.Errorf("type = %d, want PromptSubmitted", event.Type)
	}
	if !strings.HasPrefix(event.SessionID, "kirocli:") {
		t.Errorf("session ID = %q, want kirocli: prefix", event.SessionID)
	}
	if event.TranscriptRef != "/mock/db.sqlite3#conv-123" {
		t.Errorf("transcript ref = %q", event.TranscriptRef)
	}
	if event.Prompt != "do something" {
		t.Errorf("prompt = %q", event.Prompt)
	}
}

// Resolver failures must not block prompt capture state.
func TestParseHookEvent_UserPromptSubmit_ResolverFailureIsBestEffort(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	p := &Provider{
		resolveConversation: func(workspacePath string) (string, string, error) {
			return "", "", fmt.Errorf("boom: db missing")
		},
	}

	stdin := strings.NewReader(`{"hook_event_name":"userPromptSubmit","cwd":"/workspace","prompt":"hello"}`)
	event, err := p.ParseHookEvent(context.Background(), "user-prompt-submit", stdin)
	if err != nil {
		t.Fatalf("expected no error from resolver failure, got %v", err)
	}
	if event == nil {
		t.Fatal("expected event to be returned even when resolver fails")
	}
	if event.Type != hooks.PromptSubmitted {
		t.Errorf("type = %d, want PromptSubmitted", event.Type)
	}
	if event.TranscriptRef != "" {
		t.Errorf("transcript ref = %q, want empty when resolver fails", event.TranscriptRef)
	}
}

// Conversation load failures skip offset writes but keep the prompt event.
func TestParseHookEvent_UserPromptSubmit_LoadConvFailureIsBestEffort(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	p := &Provider{
		resolveConversation: func(workspacePath string) (string, string, error) {
			return "/nonexistent/db.sqlite3", "conv-x", nil
		},
		loadConv: func(dbPath, convID string) (*conversationValue, error) {
			return nil, fmt.Errorf("boom: schema mismatch")
		},
	}

	stdin := strings.NewReader(`{"hook_event_name":"userPromptSubmit","cwd":"/workspace","prompt":"hello"}`)
	event, err := p.ParseHookEvent(context.Background(), "user-prompt-submit", stdin)
	if err != nil {
		t.Fatalf("expected no error from loadConv failure, got %v", err)
	}
	if event == nil || event.Type != hooks.PromptSubmitted {
		t.Fatalf("expected PromptSubmitted event, got %+v", event)
	}
}

func TestTranscriptOffset_ToleratesEmptyAndUnreadable(t *testing.T) {
	p := &Provider{}
	cases := []string{
		"",
		"no-hash-separator",
		"/nonexistent/db.sqlite3#conv-x",
	}
	for _, ref := range cases {
		t.Run(ref, func(t *testing.T) {
			offset, err := p.TranscriptOffset(context.Background(), ref)
			if err != nil {
				t.Errorf("ref %q: unexpected error %v", ref, err)
			}
			if offset != 0 {
				t.Errorf("ref %q: offset = %d, want 0", ref, offset)
			}
		})
	}
}

func TestParseHookEvent_StopReusesPinned(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)

	wsKey := workspaceKey("/workspace")
	if err := hooks.SaveCaptureState(&hooks.CaptureState{
		SessionID:     wsKey,
		Provider:      providerName,
		TranscriptRef: "/pinned/db.sqlite3#pinned-conv",
		Timestamp:     1000,
	}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hooks.DeleteCaptureState(wsKey) }()

	resolverCalled := false
	p := &Provider{
		resolveConversation: func(workspacePath string) (string, string, error) {
			resolverCalled = true
			return "/new/db.sqlite3", "new-conv", nil
		},
	}

	stdin := strings.NewReader(`{"hook_event_name":"stop","cwd":"/workspace"}`)
	event, err := p.ParseHookEvent(context.Background(), "stop", stdin)
	if err != nil {
		t.Fatal(err)
	}

	if resolverCalled {
		t.Error("resolver should not be called when capture state exists")
	}
	if event.TranscriptRef != "/pinned/db.sqlite3#pinned-conv" {
		t.Errorf("transcript ref = %q, want pinned value", event.TranscriptRef)
	}
}

// Raw postToolUse payloads are normalized at the hook boundary.
func TestParseHookEvent_PostToolUse_Write_Create(t *testing.T) {
	p := &Provider{}
	stdin := strings.NewReader(`{
		"hook_event_name":"postToolUse",
		"cwd":"/workspace",
		"tool_name":"write",
		"tool_input":{"command":"create","path":"new.go","content":"package main\n"},
		"tool_response":{"items":[{"Text":"Successfully created /workspace/new.go (1 lines)."}]}
	}`)
	event, err := p.ParseHookEvent(context.Background(), "post-tool-use", stdin)
	if err != nil {
		t.Fatal(err)
	}
	if event == nil {
		t.Fatal("expected non-nil event for write/create")
	}
	if event.Type != hooks.ToolStepCompleted {
		t.Errorf("type = %d, want ToolStepCompleted", event.Type)
	}
	if event.ToolName != "Write" {
		t.Errorf("tool_name = %q, want Write", event.ToolName)
	}
}

func TestParseHookEvent_PostToolUse_Write_StrReplace(t *testing.T) {
	p := &Provider{}
	stdin := strings.NewReader(`{
		"hook_event_name":"postToolUse",
		"cwd":"/workspace",
		"tool_name":"write",
		"tool_input":{"command":"strReplace","path":"main.go","oldStr":"foo","newStr":"bar"},
		"tool_response":{"items":[{"Text":"Successfully replaced 1 occurrence(s)."}]}
	}`)
	event, err := p.ParseHookEvent(context.Background(), "post-tool-use", stdin)
	if err != nil {
		t.Fatal(err)
	}
	if event == nil || event.ToolName != "Edit" {
		t.Fatalf("expected Edit, got %+v", event)
	}
}

func TestParseHookEvent_PostToolUse_Write_Insert(t *testing.T) {
	p := &Provider{}
	stdin := strings.NewReader(`{
		"hook_event_name":"postToolUse",
		"cwd":"/workspace",
		"tool_name":"write",
		"tool_input":{"command":"insert","path":"main.go","content":"\nappended"},
		"tool_response":{"items":[{"Text":"Successfully inserted 1 line(s)."}]}
	}`)
	event, err := p.ParseHookEvent(context.Background(), "post-tool-use", stdin)
	if err != nil {
		t.Fatal(err)
	}
	if event == nil || event.ToolName != "Edit" {
		t.Fatalf("expected Edit (insert maps to Edit), got %+v", event)
	}
}

func TestParseHookEvent_PostToolUse_Shell(t *testing.T) {
	p := &Provider{}
	stdin := strings.NewReader(`{
		"hook_event_name":"postToolUse",
		"cwd":"/workspace",
		"tool_name":"shell",
		"tool_input":{"command":"ls","working_dir":"/workspace","__tool_use_purpose":"List files"},
		"tool_response":{"items":[{"Json":{"exit_status":"exit status: 0","stdout":"","stderr":""}}]}
	}`)
	event, err := p.ParseHookEvent(context.Background(), "post-tool-use", stdin)
	if err != nil {
		t.Fatal(err)
	}
	if event == nil || event.ToolName != "Bash" {
		t.Fatalf("expected Bash, got %+v", event)
	}
}

// Unknown write sub-commands are ignored instead of guessed.
func TestParseHookEvent_PostToolUse_Write_UnknownSubcommand(t *testing.T) {
	p := &Provider{}
	stdin := strings.NewReader(`{
		"hook_event_name":"postToolUse",
		"cwd":"/workspace",
		"tool_name":"write",
		"tool_input":{"command":"futureOp","path":"main.go"}
	}`)
	event, err := p.ParseHookEvent(context.Background(), "post-tool-use", stdin)
	if err != nil {
		t.Fatal(err)
	}
	if event != nil {
		t.Errorf("expected nil event for unknown write sub-command, got %+v", event)
	}
}

func TestParseHookEvent_UnknownHook(t *testing.T) {
	p := &Provider{}
	stdin := strings.NewReader(`{"hook_event_name":"unknown","cwd":"/workspace"}`)
	ev, err := p.ParseHookEvent(context.Background(), "unknown-hook", stdin)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev != nil {
		t.Fatal("expected nil event for unknown hook")
	}
}

func TestParseHookEvent_FallsBackOnEmptyCwd(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	p := &Provider{
		resolveConversation: func(workspacePath string) (string, string, error) {
			if workspacePath == "" {
				t.Error("empty workspace path passed to resolver")
			}
			return "/mock/db.sqlite3", "conv-fallback", nil
		},
		loadConv: func(dbPath, convID string) (*conversationValue, error) {
			return &conversationValue{ConversationID: convID, History: nil}, nil
		},
	}

	stdin := strings.NewReader(`{"hook_event_name":"userPromptSubmit","cwd":""}`)
	event, err := p.ParseHookEvent(context.Background(), "user-prompt-submit", stdin)
	if err != nil {
		t.Fatal(err)
	}
	if event.TranscriptRef == "" {
		t.Error("expected non-empty transcript ref")
	}
}

func TestParseTranscriptRef(t *testing.T) {
	dbPath, convID, err := parseTranscriptRef("/path/to/data.sqlite3#conv-abc-123")
	if err != nil {
		t.Fatal(err)
	}
	if dbPath != "/path/to/data.sqlite3" {
		t.Errorf("dbPath = %q", dbPath)
	}
	if convID != "conv-abc-123" {
		t.Errorf("convID = %q", convID)
	}
}

func TestParseTranscriptRef_Invalid(t *testing.T) {
	_, _, err := parseTranscriptRef("no-hash-separator")
	if err == nil {
		t.Fatal("expected error for invalid ref")
	}
}

func TestExtractToolCalls(t *testing.T) {
	toolUses := []toolUse{
		{ID: "tu-1", Name: "fs_write", Args: mustMarshal(fsWriteArgs{Command: "create", Path: "/ws/file.txt", FileText: "hello"})},
		{ID: "tu-2", Name: "fs_read", Args: mustMarshal(map[string]string{"path": "/ws/other.txt"})},
	}
	block := toolUseBlock{ToolUses: toolUses}
	asst := assistantToolUse{ToolUse: &block}
	asstJSON, _ := json.Marshal(asst)

	conv := &conversationValue{
		History: []historyEntry{
			{Assistant: asstJSON},
		},
	}

	calls := extractToolCalls(conv)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1 (only fs_write)", len(calls))
	}
	if calls[0].ID != "tu-1" || calls[0].FileText != "hello" {
		t.Errorf("call = %+v", calls[0])
	}
}

// Stored conversations may contain either legacy or current write shapes.
func TestExtractToolCalls_AcceptsCurrentShape(t *testing.T) {
	toolUses := []toolUse{
		{ID: "tu-w-create", Name: "write", Args: mustMarshal(fsWriteArgs{Command: "create", Path: "/ws/a.txt", Content: "alpha"})},
		{ID: "tu-w-replace", Name: "write", Args: mustMarshal(fsWriteArgs{Command: "strReplace", Path: "/ws/b.txt"})},
		{ID: "tu-w-insert", Name: "write", Args: mustMarshal(fsWriteArgs{Command: "insert", Path: "/ws/c.txt", Content: "appended"})},
		{ID: "tu-shell", Name: "shell", Args: mustMarshal(map[string]string{"command": "ls"})},
	}
	block := toolUseBlock{ToolUses: toolUses}
	asst := assistantToolUse{ToolUse: &block}
	asstJSON, _ := json.Marshal(asst)
	conv := &conversationValue{History: []historyEntry{{Assistant: asstJSON}}}

	calls := extractToolCalls(conv)
	if len(calls) != 3 {
		t.Fatalf("got %d calls, want 3 (three write ops, shell ignored)", len(calls))
	}

	byID := map[string]toolCallInfo{}
	for _, c := range calls {
		byID[c.ID] = c
	}

	if c := byID["tu-w-create"]; c.Command != "create" || c.FileText != "alpha" || c.FilePath != "/ws/a.txt" {
		t.Errorf("create call = %+v", c)
	}
	// strReplace and insert preserve the raw command name; the
	// replay path does not normalize commands today. Asserting the
	// raw value documents that explicitly so any future
	// normalization shows up as a deliberate test change.
	if c := byID["tu-w-replace"]; c.Command != "strReplace" {
		t.Errorf("strReplace call command = %q, want raw value", c.Command)
	}
	if c := byID["tu-w-insert"]; c.Command != "insert" || c.FileText != "appended" {
		t.Errorf("insert call = %+v", c)
	}
}

func TestToolCallsToEvents_TimestampNonZero(t *testing.T) {
	calls := []toolCallInfo{
		{ID: "tu-1", FilePath: "/ws/file.txt", Command: "create"},
	}
	events := toolCallsToEvents(calls, "ref", "/ws", "conv-1", 1700000000000)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Timestamp == 0 {
		t.Fatal("event Timestamp is 0; events with ts=0 are excluded from attribution windows")
	}
	if events[0].Timestamp != 1700000000000 {
		t.Errorf("Timestamp = %d, want 1700000000000", events[0].Timestamp)
	}
}

func TestNewToolCallsSince(t *testing.T) {
	calls := []toolCallInfo{
		{ID: "a"}, {ID: "b"}, {ID: "c"},
	}

	got := newToolCallsSince(calls, "a")
	if len(got) != 2 || got[0].ID != "b" || got[1].ID != "c" {
		t.Errorf("after a: got %v", ids(got))
	}

	got = newToolCallsSince(calls, "")
	if len(got) != 3 {
		t.Errorf("empty lastSeen: got %d, want 3", len(got))
	}

	// Unknown lastSeen falls back to the full set.
	got = newToolCallsSince(calls, "unknown")
	if len(got) != 3 {
		t.Errorf("unknown lastSeen: got %d, want 3", len(got))
	}
}

func TestSidecar_WriteAndRead(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	wsKey := "kirocli-test-key"
	if err := writeSidecar(wsKey, "tooluse_abc123"); err != nil {
		t.Fatal(err)
	}

	got, err := readSidecar(wsKey)
	if err != nil {
		t.Fatal(err)
	}
	if got != "tooluse_abc123" {
		t.Errorf("sidecar = %q, want tooluse_abc123", got)
	}
}

func TestSidecar_OverwrittenOnNextPromptSubmit(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	wsKey := "kirocli-test-overwrite"
	if err := writeSidecar(wsKey, "old-id"); err != nil {
		t.Fatal(err)
	}
	if err := writeSidecar(wsKey, "new-id"); err != nil {
		t.Fatal(err)
	}

	got, _ := readSidecar(wsKey)
	if got != "new-id" {
		t.Errorf("sidecar = %q after overwrite, want new-id", got)
	}
}

// Replay stays silent while direct postToolUse hooks own capture.
func TestReadFromOffset_DisabledReplay(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "data.sqlite3")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE conversations_v2 (
		key TEXT NOT NULL, conversation_id TEXT NOT NULL,
		value TEXT NOT NULL, created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL, PRIMARY KEY (key, conversation_id)
	)`); err != nil {
		t.Fatal(err)
	}

	// Seed both legacy and current shapes to prove the disabled
	// replay applies to both.
	toolUses := []toolUse{
		{ID: "tu-legacy", Name: "fs_write", Args: mustMarshal(fsWriteArgs{Command: "create", Path: "/ws/legacy.txt", FileText: "hello"})},
		{ID: "tu-current", Name: "write", Args: mustMarshal(fsWriteArgs{Command: "create", Path: "/ws/current.txt", Content: "hi"})},
	}
	block := toolUseBlock{ToolUses: toolUses}
	asst := assistantToolUse{ToolUse: &block}
	asstJSON, _ := json.Marshal(asst)
	conv := conversationValue{
		ConversationID: "conv-test",
		History:        []historyEntry{{User: json.RawMessage(`{}`), Assistant: asstJSON}},
	}
	convJSON, _ := json.Marshal(conv)
	if _, err := db.Exec(
		`INSERT INTO conversations_v2 (key, conversation_id, value, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"/ws", "conv-test", string(convJSON), 1700000000000, 1700000000000,
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	wsKey := workspaceKey("/ws")
	if err := writeSidecar(wsKey, ""); err != nil {
		t.Fatal(err)
	}

	ref := buildTranscriptRef(dbPath, "conv-test")
	p := &Provider{}
	events, newOffset, err := p.ReadFromOffset(context.Background(), ref, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("got %d events, want 0 (replay disabled)", len(events))
	}
	if newOffset != 2 {
		t.Errorf("newOffset = %d, want 2 (offset must still advance)", newOffset)
	}
}

func TestMissedPromptSubmit_StopProducesNoEvents(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	// Stop without a prior prompt-submit still produces a valid event.
	p := &Provider{
		resolveConversation: func(workspacePath string) (string, string, error) {
			return "/mock/db.sqlite3", "conv-orphan", nil
		},
	}

	stdin := strings.NewReader(`{"hook_event_name":"stop","cwd":"/workspace"}`)
	event, err := p.ParseHookEvent(context.Background(), "stop", stdin)
	if err != nil {
		t.Fatal(err)
	}
	// The lifecycle handles the missing capture state.
	if event.Type != hooks.AgentCompleted {
		t.Errorf("type = %d, want AgentCompleted", event.Type)
	}
	// This test verifies ParseHookEvent does not fail on the stop hook alone.
}

// helpers

func mustMarshal(v interface{}) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

func ids(calls []toolCallInfo) []string {
	var result []string
	for _, c := range calls {
		result = append(result, c.ID)
	}
	return result
}
