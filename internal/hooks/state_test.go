package hooks

import (
	"os"
	"testing"
)

func setupTestCaptureDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SEMANTICA_HOME", dir)
}

func TestSaveAndLoadCaptureState(t *testing.T) {
	setupTestCaptureDir(t)

	state := &CaptureState{
		SessionID:        "session-abc",
		Provider:         "claude-code",
		TranscriptRef:    "/workspace/.claude/projects/test/session.jsonl",
		TranscriptOffset: 42,
		Timestamp:        1000,
	}

	if err := SaveCaptureState(state); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := LoadCaptureState("session-abc")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if loaded.SessionID != state.SessionID {
		t.Errorf("session ID: got %q, want %q", loaded.SessionID, state.SessionID)
	}
	if loaded.Provider != state.Provider {
		t.Errorf("provider: got %q, want %q", loaded.Provider, state.Provider)
	}
	if loaded.TranscriptRef != state.TranscriptRef {
		t.Errorf("transcript ref: got %q, want %q", loaded.TranscriptRef, state.TranscriptRef)
	}
	if loaded.TranscriptOffset != state.TranscriptOffset {
		t.Errorf("offset: got %d, want %d", loaded.TranscriptOffset, state.TranscriptOffset)
	}
	if loaded.Timestamp != state.Timestamp {
		t.Errorf("timestamp: got %d, want %d", loaded.Timestamp, state.Timestamp)
	}
}

func TestLoadCaptureState_NotFound(t *testing.T) {
	setupTestCaptureDir(t)

	_, err := LoadCaptureState("nonexistent")
	if err != ErrNoCaptureState {
		t.Errorf("got %v, want ErrNoCaptureState", err)
	}
}

func TestDeleteCaptureState(t *testing.T) {
	setupTestCaptureDir(t)

	state := &CaptureState{
		SessionID:        "session-del",
		Provider:         "cursor",
		TranscriptRef:    "/tmp/transcript.jsonl",
		TranscriptOffset: 10,
		Timestamp:        2000,
	}

	if err := SaveCaptureState(state); err != nil {
		t.Fatalf("save: %v", err)
	}

	if err := DeleteCaptureState("session-del"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, err := LoadCaptureState("session-del")
	if err != ErrNoCaptureState {
		t.Errorf("after delete: got %v, want ErrNoCaptureState", err)
	}
}

func TestDeleteCaptureState_NotFound(t *testing.T) {
	setupTestCaptureDir(t)

	// Deleting a nonexistent state should not error.
	if err := DeleteCaptureState("nonexistent"); err != nil {
		t.Errorf("delete nonexistent: %v", err)
	}
}

func TestLoadActiveCaptureStates(t *testing.T) {
	setupTestCaptureDir(t)

	states := []*CaptureState{
		{SessionID: "s1", Provider: "claude-code", TranscriptRef: "/t1", TranscriptOffset: 1, Timestamp: 100},
		{SessionID: "s2", Provider: "cursor", TranscriptRef: "/t2", TranscriptOffset: 2, Timestamp: 200},
		{SessionID: "s3", Provider: "gemini", TranscriptRef: "/t3", TranscriptOffset: 3, Timestamp: 300},
	}

	for _, s := range states {
		if err := SaveCaptureState(s); err != nil {
			t.Fatalf("save %s: %v", s.SessionID, err)
		}
	}

	loaded, err := LoadActiveCaptureStates()
	if err != nil {
		t.Fatalf("load all: %v", err)
	}

	if len(loaded) != 3 {
		t.Fatalf("got %d states, want 3", len(loaded))
	}

	// Verify all sessions are present.
	found := make(map[string]bool)
	for _, s := range loaded {
		found[s.SessionID] = true
	}
	for _, s := range states {
		if !found[s.SessionID] {
			t.Errorf("missing session %s", s.SessionID)
		}
	}
}

func TestLoadActiveCaptureStates_EmptyDir(t *testing.T) {
	setupTestCaptureDir(t)

	loaded, err := LoadActiveCaptureStates()
	if err != nil {
		t.Fatalf("load all: %v", err)
	}
	if len(loaded) != 0 {
		t.Errorf("got %d states, want 0", len(loaded))
	}
}

func TestSaveAndLoadCaptureState_OverwriteAdvancesOffset(t *testing.T) {
	setupTestCaptureDir(t)

	state := &CaptureState{
		SessionID:        "session-overwrite",
		Provider:         "claude-code",
		TranscriptRef:    "/transcript.jsonl",
		TranscriptOffset: 10,
		Timestamp:        1000,
	}
	if err := SaveCaptureState(state); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Advance offset.
	state.TranscriptOffset = 50
	state.Timestamp = 2000
	if err := SaveCaptureState(state); err != nil {
		t.Fatalf("save updated: %v", err)
	}

	loaded, err := LoadCaptureState("session-overwrite")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.TranscriptOffset != 50 {
		t.Errorf("offset: got %d, want 50", loaded.TranscriptOffset)
	}
	if loaded.Timestamp != 2000 {
		t.Errorf("timestamp: got %d, want 2000", loaded.Timestamp)
	}
}

func TestSaveCaptureState_EmptySessionID(t *testing.T) {
	setupTestCaptureDir(t)

	state := &CaptureState{SessionID: ""}
	err := SaveCaptureState(state)
	if err == nil {
		t.Error("expected error for empty session ID")
	}
}

func TestSaveCaptureState_AtomicWrite(t *testing.T) {
	setupTestCaptureDir(t)

	state := &CaptureState{
		SessionID:        "session-atomic",
		Provider:         "claude-code",
		TranscriptRef:    "/transcript.jsonl",
		TranscriptOffset: 5,
		Timestamp:        1000,
	}
	if err := SaveCaptureState(state); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Verify no .tmp file remains.
	path, _ := stateFilePath("session-atomic")
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file should not exist after successful save")
	}
}
