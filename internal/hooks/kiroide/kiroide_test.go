package kiroide

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentKiro "github.com/semanticash/cli/internal/agents/kiro"
	"github.com/semanticash/cli/internal/hooks"
)

// TestHashEventID_ActionIDDistinguishesSameFileOps checks that same-file edits
// with different action IDs get different event IDs.
func TestHashEventID_ActionIDDistinguishesSameFileOps(t *testing.T) {
	first := hashEventID("exec-1", "replace", "main.go", "tooluse_a")
	second := hashEventID("exec-1", "replace", "main.go", "tooluse_b")
	if first == second {
		t.Errorf("event IDs collide despite distinct action IDs: both = %q", first)
	}
}

// TestHashEventID_StableAcrossRuns checks that event IDs stay deterministic
// for identical inputs.
func TestHashEventID_StableAcrossRuns(t *testing.T) {
	a := hashEventID("exec-1", "create", "main.go", "tooluse_z")
	b := hashEventID("exec-1", "create", "main.go", "tooluse_z")
	if a != b {
		t.Errorf("hash not stable: %q != %q", a, b)
	}
}

// TestBuildEventForOp_CreateUsesCanonicalWriteShape checks that create
// actions use canonical Write tool_uses.
func TestBuildEventForOp_CreateUsesCanonicalWriteShape(t *testing.T) {
	op := agentKiro.FileOperation{
		ActionType: "create",
		ActionID:   "tooluse_create_1",
		FilePath:   "main.go",
		Content:    "package main\n",
	}
	ev, ok := buildEventForOp(context.Background(), op, "exec-1", 0, "sess-1", 0, "transcript", "/repo", nil)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if ev.ToolName != agentKiro.ToolNameWrite {
		t.Errorf("ToolName = %q, want %q", ev.ToolName, agentKiro.ToolNameWrite)
	}
	if !strings.Contains(ev.ToolUsesJSON, `"name":"`+agentKiro.ToolNameWrite+`"`) {
		t.Errorf("ToolUsesJSON missing canonical Write name: %q", ev.ToolUsesJSON)
	}
	if strings.Contains(ev.ToolUsesJSON, agentKiro.ToolNameFileEdit) {
		t.Errorf("ToolUsesJSON unexpectedly contains kiro_file_edit (would short-circuit scoring): %q", ev.ToolUsesJSON)
	}
}

// TestBuildEventForOp_ReplaceUsesCanonicalEditShape checks that replace
// actions use canonical Edit tool_uses.
func TestBuildEventForOp_ReplaceUsesCanonicalEditShape(t *testing.T) {
	op := agentKiro.FileOperation{
		ActionType:      "replace",
		ActionID:        "tooluse_replace_1",
		FilePath:        "main.go",
		Content:         "after",
		OriginalContent: "before",
	}
	ev, ok := buildEventForOp(context.Background(), op, "exec-1", 0, "sess-1", 0, "transcript", "/repo", nil)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if ev.ToolName != agentKiro.ToolNameEdit {
		t.Errorf("ToolName = %q, want %q", ev.ToolName, agentKiro.ToolNameEdit)
	}
	if !strings.Contains(ev.ToolUsesJSON, `"name":"`+agentKiro.ToolNameEdit+`"`) {
		t.Errorf("ToolUsesJSON missing canonical Edit name: %q", ev.ToolUsesJSON)
	}
}

// TestBuildEventForOp_SmartRelocateSetsFileTouchEvidence checks that rename
// actions use file-touch tool_uses on the destination path.
func TestBuildEventForOp_SmartRelocateSetsFileTouchEvidence(t *testing.T) {
	op := agentKiro.FileOperation{
		ActionType: "smartRelocate",
		ActionID:   "tooluse_relocate_1",
		SourcePath: "old/name.go",
		DestPath:   "new/name.go",
	}
	ev, ok := buildEventForOp(context.Background(), op, "exec-1", 0, "sess-1", 0, "transcript", "/repo", nil)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if ev.ToolUsesJSON == "" {
		t.Fatal("ToolUsesJSON empty: rename has no file-touch evidence downstream")
	}
	if !strings.Contains(ev.ToolUsesJSON, agentKiro.ToolNameFileEdit) {
		t.Errorf("ToolUsesJSON missing kiro_file_edit (file-touch fast path), got %q", ev.ToolUsesJSON)
	}
	if !strings.Contains(ev.ToolUsesJSON, "new/name.go") {
		t.Errorf("ToolUsesJSON should reference destination path, got %q", ev.ToolUsesJSON)
	}
	// filepath.Join uses platform-native separators; build the suffix
	// the same way so the assertion holds on POSIX and Windows.
	wantSuffix := filepath.Join("new", "name.go")
	if len(ev.FilePaths) != 1 || !strings.HasSuffix(ev.FilePaths[0], wantSuffix) {
		t.Errorf("FilePaths = %v, want destination resolved against workspace (suffix %q)", ev.FilePaths, wantSuffix)
	}
}

// TestBuildEventForOp_UnknownActionTypeDropped checks that unknown action
// types are dropped instead of emitted as partial rows.
func TestBuildEventForOp_UnknownActionTypeDropped(t *testing.T) {
	op := agentKiro.FileOperation{
		ActionType: "futureKindWeDoNotKnow",
		ActionID:   "tooluse_x",
		FilePath:   "main.go",
	}
	_, ok := buildEventForOp(context.Background(), op, "exec-1", 0, "sess-1", 0, "transcript", "/repo", nil)
	if ok {
		t.Error("expected ok=false for unrecognized action type")
	}
}

func TestInstallHooks_CreatesCorrectFiles(t *testing.T) {
	repoRoot := t.TempDir()
	p := &Provider{}

	count, err := p.InstallHooks(context.Background(), repoRoot, "/usr/local/bin/semantica")
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Errorf("installed %d hooks, want 3", count)
	}

	for _, name := range []string{"semantica-prompt-submit.kiro.hook", "semantica-file-edited.kiro.hook", "semantica-agent-stop.kiro.hook"} {
		path := filepath.Join(repoRoot, ".kiro", "hooks", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("hook file %s not found: %v", name, err)
		}

		var hook kiroHook
		if err := json.Unmarshal(data, &hook); err != nil {
			t.Fatalf("invalid JSON in %s: %v", name, err)
		}
		if !hook.Enabled {
			t.Errorf("%s: enabled = false, want true", name)
		}
		if hook.Then.Type != "runCommand" {
			t.Errorf("%s: then.type = %q, want runCommand", name, hook.Then.Type)
		}
		if !strings.Contains(hook.Then.Command, "semantica capture kiro-ide") {
			t.Errorf("%s: command missing capture marker: %q", name, hook.Then.Command)
		}
		if !strings.Contains(hook.Then.Command, "/usr/local/bin/semantica") {
			t.Errorf("%s: command missing binary path: %q", name, hook.Then.Command)
		}
	}
}

func TestInstallHooks_Idempotent(t *testing.T) {
	repoRoot := t.TempDir()
	p := &Provider{}

	_, _ = p.InstallHooks(context.Background(), repoRoot, "/usr/local/bin/semantica")
	count, err := p.InstallHooks(context.Background(), repoRoot, "/usr/local/bin/semantica")
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Errorf("second install returned %d, want 3 (idempotent)", count)
	}
}

// TestInstallHooks_RefreshesStaleSemanticaOwnedHook checks that an existing
// Semantica-owned hook file is refreshed when the rendered definition changes.
func TestInstallHooks_RefreshesStaleSemanticaOwnedHook(t *testing.T) {
	repoRoot := t.TempDir()
	hooksDir := filepath.Join(repoRoot, ".kiro", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Older fileEdited hooks may be missing patterns even though the command
	// marker shows they are Semantica-owned.
	stale := `{"id":"semantica-file-edited","enabled":true,"name":"Old","version":"1","when":{"type":"fileEdited"},"then":{"type":"runCommand","command":"semantica capture kiro-ide file-edited","timeout":5}}`
	stalePath := filepath.Join(hooksDir, "semantica-file-edited.kiro.hook")
	if err := os.WriteFile(stalePath, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Provider{}
	if _, err := p.InstallHooks(context.Background(), repoRoot, "/usr/local/bin/semantica"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(stalePath)
	if err != nil {
		t.Fatal(err)
	}
	var hook kiroHook
	if err := json.Unmarshal(data, &hook); err != nil {
		t.Fatalf("invalid JSON after refresh: %v", err)
	}
	if len(hook.When.Patterns) == 0 {
		t.Fatal("install did not refresh stale hook; patterns still empty after install")
	}
	if hook.Then.Timeout != 30 {
		t.Errorf("timeout = %d, want 30 (stale hook was 5; refresh should overwrite)", hook.Then.Timeout)
	}
}

// TestInstallHooks_FileEditedPatternsRequired checks that fileEdited hooks
// include a non-empty patterns list. Kiro IDE requires patterns before the
// hook can match file edits.
func TestInstallHooks_FileEditedPatternsRequired(t *testing.T) {
	repoRoot := t.TempDir()
	p := &Provider{}

	if _, err := p.InstallHooks(context.Background(), repoRoot, "/usr/local/bin/semantica"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(repoRoot, ".kiro", "hooks", "semantica-file-edited.kiro.hook"))
	if err != nil {
		t.Fatal(err)
	}
	var hook kiroHook
	if err := json.Unmarshal(data, &hook); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if hook.When.Type != "fileEdited" {
		t.Errorf("when.type = %q, want fileEdited", hook.When.Type)
	}
	if len(hook.When.Patterns) == 0 {
		t.Errorf("when.patterns is empty; Kiro IDE drops fileEdited hooks without patterns")
	}
	if !strings.Contains(hook.Then.Command, "capture kiro-ide file-edited") {
		t.Errorf("command missing file-edited subcommand: %q", hook.Then.Command)
	}
}

func TestUninstallHooks_RemovesSemanticaOnly(t *testing.T) {
	repoRoot := t.TempDir()
	hooksDir := filepath.Join(repoRoot, ".kiro", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}

	p := &Provider{}
	_, _ = p.InstallHooks(context.Background(), repoRoot, "/usr/local/bin/semantica")

	otherHook := `{"id":"user-hook","name":"My Hook","when":{"type":"fileSave"},"then":{"type":"alert","message":"saved"}}`
	if err := os.WriteFile(filepath.Join(hooksDir, "user-hook.kiro.hook"), []byte(otherHook), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := p.UninstallHooks(context.Background(), repoRoot); err != nil {
		t.Fatal(err)
	}

	if p.AreHooksInstalled(context.Background(), repoRoot) {
		t.Error("semantica hooks still installed after uninstall")
	}

	if _, err := os.Stat(filepath.Join(hooksDir, "user-hook.kiro.hook")); err != nil {
		t.Error("non-semantica hook was removed")
	}
}

func TestAreHooksInstalled(t *testing.T) {
	repoRoot := t.TempDir()
	p := &Provider{}

	if p.AreHooksInstalled(context.Background(), repoRoot) {
		t.Error("hooks detected before install")
	}

	_, _ = p.InstallHooks(context.Background(), repoRoot, "/usr/local/bin/semantica")

	if !p.AreHooksInstalled(context.Background(), repoRoot) {
		t.Error("hooks not detected after install")
	}
}

func TestHookBinary_ExtractsBinaryPath(t *testing.T) {
	repoRoot := t.TempDir()
	p := &Provider{}

	_, _ = p.InstallHooks(context.Background(), repoRoot, "/opt/bin/semantica")

	bin, err := p.HookBinary(context.Background(), repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if bin != "/opt/bin/semantica" {
		t.Errorf("binary = %q, want /opt/bin/semantica", bin)
	}
}

// TestParseHookEvent_FileEditedReusesPinnedRef checks that file-edited hooks
// reuse the transcript ref pinned by the matching prompt-submit hook.
func TestParseHookEvent_FileEditedReusesPinnedRef(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	histPath := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(histPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Provider{
		resolveSession: func(workspacePath string) (string, string, error) {
			return "session-1", histPath, nil
		},
	}

	submit, err := p.ParseHookEvent(context.Background(), "prompt-submit", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := hooks.SaveCaptureState(&hooks.CaptureState{
		SessionID:     submit.SessionID,
		Provider:      providerName,
		TranscriptRef: submit.TranscriptRef,
		Timestamp:     submit.Timestamp,
	}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hooks.DeleteCaptureState(submit.SessionID) }()

	edited, err := p.ParseHookEvent(context.Background(), "file-edited", nil)
	if err != nil {
		t.Fatal(err)
	}
	if edited == nil {
		t.Fatal("file-edited returned nil event despite capture state being present")
	}
	if edited.Type != hooks.IncrementalCapture {
		t.Errorf("Type = %v, want IncrementalCapture", edited.Type)
	}
	if edited.SessionID != submit.SessionID {
		t.Errorf("SessionID = %q, want %q (workspace-keyed)", edited.SessionID, submit.SessionID)
	}
	if edited.TranscriptRef != histPath {
		t.Errorf("TranscriptRef = %q, want pinned %q", edited.TranscriptRef, histPath)
	}
}

// TestParseHookEvent_FileEditedNoStateNoOp checks that file-edited hooks
// without capture state are ignored because there is no pinned session to scan.
func TestParseHookEvent_FileEditedNoStateNoOp(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	p := &Provider{
		resolveSession: func(workspacePath string) (string, string, error) {
			t.Error("resolveSession should not be called for file-edited without capture state")
			return "", "", fmt.Errorf("unused")
		},
	}

	ev, err := p.ParseHookEvent(context.Background(), "file-edited", nil)
	if err != nil {
		t.Fatalf("file-edited without state should not error, got: %v", err)
	}
	if ev != nil {
		t.Errorf("file-edited without state should return nil event, got: %+v", ev)
	}
}

func TestParseHookEvent_PromptSubmitAndStopUseSameWorkspaceKey(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	histPathA := filepath.Join(t.TempDir(), "session-a.json")
	histPathB := filepath.Join(t.TempDir(), "session-b.json")

	if err := os.WriteFile(histPathA, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(histPathB, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	callCount := 0
	p := &Provider{
		resolveSession: func(workspacePath string) (string, string, error) {
			callCount++
			if callCount == 1 {
				return "session-a", histPathA, nil
			}
			return "session-b", histPathB, nil
		},
	}

	submitEvent, err := p.ParseHookEvent(context.Background(), "prompt-submit", nil)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(submitEvent.SessionID, "kiroide:") {
		t.Errorf("session ID = %q, want kiroide: prefix", submitEvent.SessionID)
	}
	if submitEvent.TranscriptRef != histPathA {
		t.Errorf("transcript ref = %q, want %q", submitEvent.TranscriptRef, histPathA)
	}

	if err := hooks.SaveCaptureState(&hooks.CaptureState{
		SessionID:        submitEvent.SessionID,
		Provider:         providerName,
		TranscriptRef:    submitEvent.TranscriptRef,
		TranscriptOffset: 0,
		Timestamp:        submitEvent.Timestamp,
	}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hooks.DeleteCaptureState(submitEvent.SessionID) }()

	stopEvent, err := p.ParseHookEvent(context.Background(), "stop", nil)
	if err != nil {
		t.Fatal(err)
	}

	if stopEvent.SessionID != submitEvent.SessionID {
		t.Errorf("stop session ID = %q, want %q (same workspace key)", stopEvent.SessionID, submitEvent.SessionID)
	}
	if stopEvent.TranscriptRef != histPathA {
		t.Errorf("stop transcript ref = %q, want %q (pinned from prompt-submit)", stopEvent.TranscriptRef, histPathA)
	}
}

func TestParseHookEvent_StopFallsBackWhenNoCaptureState(t *testing.T) {
	histPath := filepath.Join(t.TempDir(), "fallback.json")
	if err := os.WriteFile(histPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Provider{
		resolveSession: func(workspacePath string) (string, string, error) {
			return "fallback-session", histPath, nil
		},
	}

	event, err := p.ParseHookEvent(context.Background(), "stop", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(event.SessionID, "kiroide:") {
		t.Errorf("session ID = %q, want kiroide: prefix", event.SessionID)
	}
	if event.TranscriptRef != histPath {
		t.Errorf("transcript ref = %q, want %q", event.TranscriptRef, histPath)
	}
}

func TestParseHookEvent_UnknownHook(t *testing.T) {
	p := &Provider{}
	_, err := p.ParseHookEvent(context.Background(), "unknown-hook", nil)
	if err == nil {
		t.Fatal("expected error for unknown hook name")
	}
	if !strings.Contains(err.Error(), "unknown kiro-ide hook") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestReadFromOffset_NewExecutions(t *testing.T) {
	sessDir := t.TempDir()
	histPath := filepath.Join(sessDir, "session.json")

	sessions := []agentKiro.SessionIndex{
		{SessionID: "read-session", DateCreated: "1700000000000", WorkspaceDirectory: "/mock/workspace"},
	}
	sData, _ := json.Marshal(sessions)
	if err := os.WriteFile(filepath.Join(sessDir, "sessions.json"), sData, 0o644); err != nil {
		t.Fatal(err)
	}

	history := agentKiro.SessionHistory{
		SessionID:          "read-session",
		WorkspaceDirectory: "/mock/workspace",
		History: []agentKiro.HistoryEntry{
			{Message: agentKiro.HistoryMessage{Role: "user"}},
			{Message: agentKiro.HistoryMessage{Role: "assistant"}, ExecutionID: "exec-old"},
			{Message: agentKiro.HistoryMessage{Role: "user"}},
			{Message: agentKiro.HistoryMessage{Role: "assistant"}, ExecutionID: "exec-new"},
		},
	}
	hData, _ := json.Marshal(history)
	if err := os.WriteFile(histPath, hData, 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Provider{}
	events, newOffset, err := p.ReadFromOffset(context.Background(), histPath, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Logf("got %d events (trace files found unexpectedly)", len(events))
	}
	if newOffset != 2 {
		t.Errorf("newOffset = %d, want 2", newOffset)
	}
}

func TestReadFromOffset_NoNewExecutions(t *testing.T) {
	sessDir := t.TempDir()
	histPath := filepath.Join(sessDir, "session.json")

	history := agentKiro.SessionHistory{
		SessionID:          "static-session",
		WorkspaceDirectory: "/mock/workspace",
		History: []agentKiro.HistoryEntry{
			{Message: agentKiro.HistoryMessage{Role: "assistant"}, ExecutionID: "exec-1"},
		},
	}
	hData, _ := json.Marshal(history)
	if err := os.WriteFile(histPath, hData, 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Provider{}
	events, newOffset, err := p.ReadFromOffset(context.Background(), histPath, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
	if newOffset != 1 {
		t.Errorf("newOffset = %d, want 1 (unchanged)", newOffset)
	}
}

func TestReadFromOffset_MissingTranscript(t *testing.T) {
	p := &Provider{}
	events, offset, err := p.ReadFromOffset(context.Background(), "/nonexistent/path.json", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 || offset != 0 {
		t.Errorf("expected empty result for missing transcript, got %d events offset %d", len(events), offset)
	}
}

func TestReadFromOffset_UsesRealKiroSessionID(t *testing.T) {
	sessDir := t.TempDir()
	histPath := filepath.Join(sessDir, "session.json")

	sessions := []agentKiro.SessionIndex{
		{SessionID: "real-kiro-id-abc123", DateCreated: "1700000000000", WorkspaceDirectory: "/mock/ws"},
	}
	sData, _ := json.Marshal(sessions)
	if err := os.WriteFile(filepath.Join(sessDir, "sessions.json"), sData, 0o644); err != nil {
		t.Fatal(err)
	}

	history := agentKiro.SessionHistory{
		SessionID:          "real-kiro-id-abc123",
		WorkspaceDirectory: "/mock/ws",
		History: []agentKiro.HistoryEntry{
			{Message: agentKiro.HistoryMessage{Role: "assistant"}, ExecutionID: "exec-1"},
		},
	}
	hData, _ := json.Marshal(history)
	if err := os.WriteFile(histPath, hData, 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Provider{}
	_, newOffset, err := p.ReadFromOffset(context.Background(), histPath, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if newOffset != 1 {
		t.Errorf("newOffset = %d, want 1", newOffset)
	}
}

func TestReadFromOffset_PopulatesSourceKey(t *testing.T) {
	sessDir := t.TempDir()
	histPath := filepath.Join(sessDir, "session.json")

	sessions := []agentKiro.SessionIndex{
		{SessionID: "source-session", DateCreated: "1700000000000", WorkspaceDirectory: "/mock/ws"},
	}
	sData, _ := json.Marshal(sessions)
	if err := os.WriteFile(filepath.Join(sessDir, "sessions.json"), sData, 0o644); err != nil {
		t.Fatal(err)
	}

	history := agentKiro.SessionHistory{
		SessionID:          "source-session",
		WorkspaceDirectory: "/mock/ws",
		History: []agentKiro.HistoryEntry{
			{Message: agentKiro.HistoryMessage{Role: "assistant"}, ExecutionID: "exec-src"},
		},
	}
	hData, _ := json.Marshal(history)
	if err := os.WriteFile(histPath, hData, 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Provider{}
	_, newOffset, err := p.ReadFromOffset(context.Background(), histPath, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if newOffset != 1 {
		t.Errorf("newOffset = %d, want 1", newOffset)
	}
}

func TestExecsAfterID(t *testing.T) {
	execs := []execMeta{
		{ExecutionID: "a"}, {ExecutionID: "b"}, {ExecutionID: "c"},
	}

	got := execsAfterID(execs, "a")
	if len(got) != 2 || got[0].ExecutionID != "b" {
		t.Errorf("after a: got %d execs", len(got))
	}

	got = execsAfterID(execs, "")
	if len(got) != 3 {
		t.Errorf("empty lastID: got %d, want 3", len(got))
	}

	got = execsAfterID(execs, "unknown")
	if len(got) != 3 {
		t.Errorf("unknown lastID: got %d, want 3", len(got))
	}

	got = execsAfterID(execs, "c")
	if len(got) != 0 {
		t.Errorf("after last: got %d, want 0", len(got))
	}
}

func TestTranscriptOffset(t *testing.T) {
	dir := t.TempDir()
	histPath := filepath.Join(dir, "session.json")

	history := agentKiro.SessionHistory{
		SessionID: "test",
		History: []agentKiro.HistoryEntry{
			{Message: agentKiro.HistoryMessage{Role: "user"}},
			{Message: agentKiro.HistoryMessage{Role: "assistant"}, ExecutionID: "exec-1"},
			{Message: agentKiro.HistoryMessage{Role: "user"}},
			{Message: agentKiro.HistoryMessage{Role: "assistant"}, ExecutionID: "exec-2"},
		},
	}
	data, _ := json.Marshal(history)
	if err := os.WriteFile(histPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Provider{}
	offset, err := p.TranscriptOffset(context.Background(), histPath)
	if err != nil {
		t.Fatal(err)
	}
	if offset != 2 {
		t.Errorf("offset = %d, want 2 (two execution entries)", offset)
	}
}
