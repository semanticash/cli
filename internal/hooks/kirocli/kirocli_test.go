package kirocli

import (
	"context"
	"database/sql"
	"encoding/json"
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

func TestReadFromOffset_ReturnsEventsWithTimestamp(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	// Create a temporary SQLite database with one conversation.
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

	// Insert one fs_write tool call into the conversation.
	toolUses := []toolUse{{ID: "tu-test-1", Name: "fs_write", Args: mustMarshal(fsWriteArgs{
		Command: "create", Path: "/ws/test.txt", FileText: "hello",
	})}}
	block := toolUseBlock{ToolUses: toolUses}
	asst := assistantToolUse{ToolUse: &block}
	asstJSON, _ := json.Marshal(asst)

	conv := conversationValue{
		ConversationID: "conv-test",
		History: []historyEntry{
			{User: json.RawMessage(`{}`), Assistant: asstJSON},
		},
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

	// Seed an empty sidecar so all tool calls are returned.
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
	if newOffset != 1 {
		t.Errorf("newOffset = %d, want 1", newOffset)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}

	ev := events[0]
	if ev.Timestamp == 0 {
		t.Fatal("event Timestamp is 0; events with ts=0 are excluded from attribution windows")
	}
	if !strings.Contains(ev.ToolUsesJSON, "kiro_file_edit") {
		t.Errorf("ToolUsesJSON missing kiro_file_edit: %s", ev.ToolUsesJSON)
	}
	if len(ev.FilePaths) == 0 || ev.FilePaths[0] != "/ws/test.txt" {
		t.Errorf("FilePaths = %v, want [/ws/test.txt]", ev.FilePaths)
	}
	if ev.SourceKey != ref {
		t.Errorf("SourceKey = %q, want %q", ev.SourceKey, ref)
	}
	if ev.ProviderSessionID != "conv-test" {
		t.Errorf("ProviderSessionID = %q, want conv-test", ev.ProviderSessionID)
	}
}

func TestReadFromOffset_DoesNotAdvanceSidecar(t *testing.T) {
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

	toolUses := []toolUse{{ID: "tu-no-advance", Name: "fs_write", Args: mustMarshal(fsWriteArgs{
		Command: "create", Path: "/ws/file.txt", FileText: "content",
	})}}
	block := toolUseBlock{ToolUses: toolUses}
	asst := assistantToolUse{ToolUse: &block}
	asstJSON, _ := json.Marshal(asst)

	conv := conversationValue{
		ConversationID: "conv-no-adv",
		History:        []historyEntry{{User: json.RawMessage(`{}`), Assistant: asstJSON}},
	}
	convJSON, _ := json.Marshal(conv)
	if _, err := db.Exec(
		`INSERT INTO conversations_v2 (key, conversation_id, value, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"/ws", "conv-no-adv", string(convJSON), 1700000000000, 1700000000000,
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

	ref := buildTranscriptRef(dbPath, "conv-no-adv")
	p := &Provider{}

	// The first read should return the event.
	events1, _, _ := p.ReadFromOffset(context.Background(), ref, 0, nil)
	if len(events1) != 1 {
		t.Fatalf("first read: got %d events, want 1", len(events1))
	}

	// ReadFromOffset leaves the sidecar unchanged.
	lastSeen, _ := readSidecar(wsKey)
	if lastSeen != "" {
		t.Errorf("sidecar advanced to %q after ReadFromOffset; should remain empty", lastSeen)
	}

	// A second read returns the same event again.
	events2, _, _ := p.ReadFromOffset(context.Background(), ref, 0, nil)
	if len(events2) != 1 {
		t.Errorf("second read: got %d events, want 1 (retry should re-emit)", len(events2))
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
