package hooks

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

// isExpectedWindowsConcurrencyError reports whether err is a Windows
// filesystem error that can still surface under heavy concurrent
// rename/open contention:
//
//   - ERROR_ACCESS_DENIED (5): two writers race the Remove/Rename
//     sequence and one keeps losing.
//   - ERROR_SHARING_VIOLATION (32): a reader's os.Open lands in the
//     brief window where a writer's rename is in flight.
//
// On non-Windows the helper always returns false. The concurrent tests
// still assert that at least one save succeeded, the final file is
// valid, and no .tmp file is stranded.
func isExpectedWindowsConcurrencyError(err error) bool {
	if runtime.GOOS != "windows" || err == nil {
		return false
	}
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		return errno == 5 || errno == 32
	}
	return false
}

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

	// SaveCaptureState uses random temp names, so scan the capture
	// directory instead of checking one fixed temp path.
	dir, err := captureDir()
	if err != nil {
		t.Fatalf("captureDir: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("stranded temp file after successful save: %s", e.Name())
		}
	}
}

// TestSaveCaptureState_ConcurrentSameSession verifies that concurrent
// writers for one session leave a valid state file and no temp files.
func TestSaveCaptureState_ConcurrentSameSession(t *testing.T) {
	setupTestCaptureDir(t)

	const sessionID = "session-concurrent"
	const writers = 32

	// Each writer persists a distinguishable CaptureState for the same
	// session.
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			state := &CaptureState{
				SessionID:        sessionID,
				Provider:         "cursor",
				TranscriptRef:    "/transcript.jsonl",
				TranscriptOffset: i,
				Timestamp:        int64(1000 + i),
			}
			if err := SaveCaptureState(state); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	saveFailures := 0
	for err := range errs {
		if isExpectedWindowsConcurrencyError(err) {
			saveFailures++
			continue
		}
		t.Errorf("save: %v", err)
	}
	if saveFailures == writers {
		t.Fatalf("every save returned a Windows concurrency error; no writer ever succeeded")
	}

	// The final file must contain one complete CaptureState.
	loaded, err := LoadCaptureState(sessionID)
	if err != nil {
		t.Fatalf("load after concurrent writes: %v", err)
	}
	if loaded.SessionID != sessionID {
		t.Errorf("final session ID = %q, want %q", loaded.SessionID, sessionID)
	}
	if loaded.TranscriptOffset < 0 || loaded.TranscriptOffset >= writers {
		t.Errorf("final offset = %d, must be one of the writers' values [0..%d)", loaded.TranscriptOffset, writers)
	}

	// Successful writes should not leave temp files behind.
	dir, err := captureDir()
	if err != nil {
		t.Fatalf("captureDir: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("stranded temp file after concurrent writes: %s", e.Name())
		}
	}

	// In-flight temp files must stay out of active-state enumeration.
	bogusTmp := filepath.Join(dir, "capture-bogus.json.tmp")
	if err := os.WriteFile(bogusTmp, []byte(`{"session_id":"bogus"}`), 0o644); err != nil {
		t.Fatalf("write bogus tmp: %v", err)
	}
	defer func() { _ = os.Remove(bogusTmp) }()
	states, err := LoadActiveCaptureStates()
	if err != nil {
		t.Fatalf("LoadActiveCaptureStates: %v", err)
	}
	for _, s := range states {
		if s.SessionID == "bogus" {
			t.Error("LoadActiveCaptureStates included a .tmp file")
		}
	}
}

// TestSaveCaptureState_ConcurrentReadersAndWriters verifies that
// readers never observe partially written capture-state JSON.
func TestSaveCaptureState_ConcurrentReadersAndWriters(t *testing.T) {
	setupTestCaptureDir(t)

	const sessionID = "session-readwrite"
	const writers = 16
	const readers = 16

	// Seed the file so readers can start before the first concurrent
	// writer completes.
	if err := SaveCaptureState(&CaptureState{SessionID: sessionID, TranscriptOffset: -1, Timestamp: 1}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, writers+readers)
	// Track writer successes separately so the test can assert at
	// least one concurrent writer actually committed. The seed save
	// above means LoadCaptureState would otherwise succeed even if
	// every concurrent writer failed.
	var writerSuccesses atomic.Int32

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := SaveCaptureState(&CaptureState{
				SessionID:        sessionID,
				TranscriptOffset: i,
				Timestamp:        int64(1000 + i),
			}); err != nil {
				errs <- err
				return
			}
			writerSuccesses.Add(1)
		}(i)
	}
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := LoadCaptureState(sessionID)
			if err != nil {
				// On Windows, SafeRename replaces the destination with
				// a remove-then-rename sequence. A reader can briefly
				// see ErrNoCaptureState in that window. Other errors,
				// including JSON parse failures, still fail the test.
				if runtime.GOOS == "windows" && errors.Is(err, ErrNoCaptureState) {
					return
				}
				errs <- err
				return
			}
			// Re-encode the loaded state to assert it is a valid
			// CaptureState value after unmarshalling.
			if _, err := json.Marshal(s); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if isExpectedWindowsConcurrencyError(err) {
			continue
		}
		t.Errorf("concurrent op: %v", err)
	}

	// At least one concurrent writer must succeed so the seed file
	// cannot hide write failures.
	if writerSuccesses.Load() == 0 {
		t.Fatalf("no concurrent writer succeeded under contention (all %d returned Windows errors)", writers)
	}

	// Final state must be a valid CaptureState. The file may carry any
	// writer's offset, but it must unmarshal cleanly.
	loaded, err := LoadCaptureState(sessionID)
	if err != nil {
		t.Fatalf("load after concurrent ops: %v", err)
	}
	if loaded.SessionID != sessionID {
		t.Errorf("final session ID = %q, want %q", loaded.SessionID, sessionID)
	}

	// No .tmp must be stranded after the contention settles.
	dir, err := captureDir()
	if err != nil {
		t.Fatalf("captureDir: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("stranded temp file after concurrent ops: %s", e.Name())
		}
	}
}
