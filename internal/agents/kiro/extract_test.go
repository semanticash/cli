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
			{ActionType: "create", Input: input},
		},
	}
	ops := ExtractFileOps(trace)
	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	if ops[0].ActionType != "create" || ops[0].FilePath != "name.txt" || ops[0].Content != "John Doe\n" {
		t.Errorf("op = %+v", ops[0])
	}
}

func TestExtractFileOps_Append(t *testing.T) {
	input, _ := json.Marshal(AppendInput{File: "name.txt", ModifiedContent: "John Doe\nJane Doe\n"})
	trace := &ExecutionTrace{
		Actions: []ExecutionAction{
			{ActionType: "append", Input: input},
		},
	}
	ops := ExtractFileOps(trace)
	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	if ops[0].ActionType != "append" || ops[0].Content != "John Doe\nJane Doe\n" {
		t.Errorf("op = %+v", ops[0])
	}
}

func TestExtractFileOps_Relocate(t *testing.T) {
	input, _ := json.Marshal(RelocateInput{SourcePath: "old.txt", DestinationPath: "new.txt"})
	trace := &ExecutionTrace{
		Actions: []ExecutionAction{
			{ActionType: "smartRelocate", Input: input},
		},
	}
	ops := ExtractFileOps(trace)
	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	if ops[0].SourcePath != "old.txt" || ops[0].DestPath != "new.txt" {
		t.Errorf("op = %+v", ops[0])
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
