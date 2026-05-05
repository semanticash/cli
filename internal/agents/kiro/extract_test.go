package kiro

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEncodeWorkspacePath(t *testing.T) {
	got := EncodeWorkspacePath("/tmp/proxy")
	want := "L3RtcC9wcm94eQ__"
	if got != want {
		t.Errorf("EncodeWorkspacePath = %q, want %q", got, want)
	}
}

func TestCountExecutionEntries(t *testing.T) {
	h := &SessionHistory{
		History: []HistoryEntry{
			{Message: HistoryMessage{Role: "user"}},
			{Message: HistoryMessage{Role: "assistant"}, ExecutionID: "exec-1"},
			{Message: HistoryMessage{Role: "user"}},
			{Message: HistoryMessage{Role: "assistant"}, ExecutionID: "exec-2"},
		},
	}
	got := CountExecutionEntries(h)
	if got != 2 {
		t.Errorf("CountExecutionEntries = %d, want 2", got)
	}
}

func TestNewExecutionIDs(t *testing.T) {
	h := &SessionHistory{
		History: []HistoryEntry{
			{ExecutionID: "exec-1"},
			{ExecutionID: "exec-2"},
			{ExecutionID: "exec-3"},
		},
	}

	ids := NewExecutionIDs(h, 1)
	if len(ids) != 2 {
		t.Fatalf("NewExecutionIDs(offset=1) returned %d ids, want 2", len(ids))
	}
	if ids[0] != "exec-2" || ids[1] != "exec-3" {
		t.Errorf("ids = %v, want [exec-2 exec-3]", ids)
	}

	ids = NewExecutionIDs(h, 3)
	if len(ids) != 0 {
		t.Errorf("NewExecutionIDs(offset=3) returned %d ids, want 0", len(ids))
	}
}

func TestExtractFileOps_Create(t *testing.T) {
	input, _ := json.Marshal(CreateInput{File: "name.txt", ModifiedContent: "John Doe\n"})
	trace := &ExecutionTrace{
		Actions: []ExecutionAction{
			{ActionType: "create", ActionID: "tooluse_create_1", Input: input, EmittedAt: 1234},
		},
	}
	ops := ExtractFileOps(trace)
	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	got := ops[0]
	if got.ActionType != "create" || got.FilePath != "name.txt" || got.Content != "John Doe\n" {
		t.Errorf("op = %+v", got)
	}
	if got.ActionID != "tooluse_create_1" {
		t.Errorf("ActionID = %q, want tooluse_create_1", got.ActionID)
	}
	if got.OriginalContent != "" {
		t.Errorf("OriginalContent = %q, want empty for create", got.OriginalContent)
	}
}

// TestExtractFileOps_Replace covers the current in-place edit shape.
func TestExtractFileOps_Replace(t *testing.T) {
	input, _ := json.Marshal(AppendInput{
		File:            "name.txt",
		OriginalContent: "John Doe\n",
		ModifiedContent: "John Smith\n",
	})
	trace := &ExecutionTrace{
		Actions: []ExecutionAction{
			{ActionType: "replace", ActionID: "tooluse_replace_1", Input: input, EmittedAt: 1234},
		},
	}
	ops := ExtractFileOps(trace)
	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	got := ops[0]
	if got.ActionType != "replace" {
		t.Errorf("ActionType = %q, want replace (preserved verbatim)", got.ActionType)
	}
	if got.ActionID != "tooluse_replace_1" {
		t.Errorf("ActionID = %q", got.ActionID)
	}
	if got.OriginalContent != "John Doe\n" || got.Content != "John Smith\n" {
		t.Errorf("op = %+v", got)
	}
}

// TestExtractFileOps_Append covers the legacy in-place edit action retained
// in older traces.
func TestExtractFileOps_Append(t *testing.T) {
	input, _ := json.Marshal(AppendInput{
		File:            "name.txt",
		OriginalContent: "John Doe\n",
		ModifiedContent: "John Doe\nJane Doe\n",
	})
	trace := &ExecutionTrace{
		Actions: []ExecutionAction{
			{ActionType: "append", ActionID: "tooluse_append_1", Input: input, EmittedAt: 1234},
		},
	}
	ops := ExtractFileOps(trace)
	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	got := ops[0]
	if got.ActionType != "append" {
		t.Errorf("ActionType = %q, want append", got.ActionType)
	}
	if got.OriginalContent != "John Doe\n" || got.Content != "John Doe\nJane Doe\n" {
		t.Errorf("op = %+v", got)
	}
}

// TestExtractFileOps_TwoEditsSameFile checks that repeated edits to the same
// file keep distinct action IDs.
func TestExtractFileOps_TwoEditsSameFile(t *testing.T) {
	first, _ := json.Marshal(AppendInput{
		File:            "name.txt",
		OriginalContent: "v1",
		ModifiedContent: "v2",
	})
	second, _ := json.Marshal(AppendInput{
		File:            "name.txt",
		OriginalContent: "v2",
		ModifiedContent: "v3",
	})
	trace := &ExecutionTrace{
		Actions: []ExecutionAction{
			{ActionType: "append", ActionID: "tooluse_a", Input: first, EmittedAt: 1000},
			{ActionType: "append", ActionID: "tooluse_b", Input: second, EmittedAt: 1001},
		},
	}
	ops := ExtractFileOps(trace)
	if len(ops) != 2 {
		t.Fatalf("got %d ops, want 2", len(ops))
	}
	if ops[0].ActionID == ops[1].ActionID {
		t.Errorf("ActionIDs collide: %q == %q", ops[0].ActionID, ops[1].ActionID)
	}
	if ops[0].ActionID != "tooluse_a" || ops[1].ActionID != "tooluse_b" {
		t.Errorf("ActionIDs = [%q, %q], want [tooluse_a, tooluse_b]", ops[0].ActionID, ops[1].ActionID)
	}
}

func TestExtractFileOps_Relocate(t *testing.T) {
	input, _ := json.Marshal(RelocateInput{SourcePath: "old.txt", DestinationPath: "new.txt"})
	trace := &ExecutionTrace{
		Actions: []ExecutionAction{
			{ActionType: "smartRelocate", ActionID: "tooluse_relocate_1", Input: input},
		},
	}
	ops := ExtractFileOps(trace)
	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	got := ops[0]
	if got.SourcePath != "old.txt" || got.DestPath != "new.txt" {
		t.Errorf("op = %+v", got)
	}
	if got.ActionID != "tooluse_relocate_1" {
		t.Errorf("ActionID = %q", got.ActionID)
	}
	if got.OriginalContent != "" {
		t.Errorf("OriginalContent should be empty for smartRelocate, got %q", got.OriginalContent)
	}
}

// TestExtractFileOps_SkipsMalformedInput checks that malformed action input is
// skipped without aborting the rest of the trace.
func TestExtractFileOps_SkipsMalformedInput(t *testing.T) {
	good, _ := json.Marshal(CreateInput{File: "ok.txt", ModifiedContent: "x"})
	trace := &ExecutionTrace{
		Actions: []ExecutionAction{
			{ActionType: "create", ActionID: "good", Input: good},
			{ActionType: "create", ActionID: "broken", Input: json.RawMessage(`{not valid`)},
			{ActionType: "append", ActionID: "missing-file", Input: json.RawMessage(`{"file":""}`)},
		},
	}
	ops := ExtractFileOps(trace)
	if len(ops) != 1 || ops[0].ActionID != "good" {
		t.Errorf("ops = %+v, want only the good create", ops)
	}
}

func TestExtractFileOps_SkipsNonFileActions(t *testing.T) {
	trace := &ExecutionTrace{
		Actions: []ExecutionAction{
			{ActionType: "intentClassification"},
			{ActionType: "model"},
			{ActionType: "say"},
		},
	}
	ops := ExtractFileOps(trace)
	if len(ops) != 0 {
		t.Errorf("expected 0 ops for non-file actions, got %d", len(ops))
	}
}

func TestResolveLatestSessionIn_PicksLastEntry(t *testing.T) {
	sessDir := t.TempDir()

	sessions := []SessionIndex{
		{SessionID: "old-session", Title: "Old", DateCreated: "1000"},
		{SessionID: "new-session", Title: "New", DateCreated: "2000"},
	}
	data, _ := json.Marshal(sessions)
	if err := os.WriteFile(filepath.Join(sessDir, "sessions.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	history := SessionHistory{SessionID: "new-session", History: []HistoryEntry{}}
	histData, _ := json.Marshal(history)
	if err := os.WriteFile(filepath.Join(sessDir, "new-session.json"), histData, 0o644); err != nil {
		t.Fatal(err)
	}

	sessionID, histPath, err := ResolveLatestSessionIn(sessDir)
	if err != nil {
		t.Fatal(err)
	}
	if sessionID != "new-session" {
		t.Errorf("sessionID = %q, want new-session", sessionID)
	}
	if histPath != filepath.Join(sessDir, "new-session.json") {
		t.Errorf("histPath = %q, want %q", histPath, filepath.Join(sessDir, "new-session.json"))
	}
}

func TestResolveLatestSessionIn_EmptySessions(t *testing.T) {
	sessDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sessDir, "sessions.json"), []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := ResolveLatestSessionIn(sessDir)
	if err == nil {
		t.Fatal("expected error for empty sessions")
	}
}

func TestResolveLatestSessionIn_MissingHistoryFile(t *testing.T) {
	sessDir := t.TempDir()

	sessions := []SessionIndex{
		{SessionID: "orphan-session", Title: "Orphan", DateCreated: "1000"},
	}
	data, _ := json.Marshal(sessions)
	if err := os.WriteFile(filepath.Join(sessDir, "sessions.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := ResolveLatestSessionIn(sessDir)
	if err == nil {
		t.Fatal("expected error when history file is missing")
	}
}
