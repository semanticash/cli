//go:build e2e

package e2e_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestCaptureSmoke(t *testing.T) {
	dir, env := initGitRepo(t)
	enableRepo(t, env, dir)

	// Write transcript in two stages: user-prompt-submit saves the offset at
	// the current end of the file, then stop reads from that offset forward.
	// So the assistant content must be written AFTER user-prompt-submit.
	transcriptPath := filepath.Join(dir, "transcript.jsonl")
	targetFile := filepath.Join(dir, "test.go")

	initial := `{"type":"system","message":{"content":[]},"timestamp":"2026-03-12T00:00:00Z"}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(initial), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	// user-prompt-submit saves the capture offset at the end of the current file.
	submitStdin := fmt.Sprintf(
		`{"session_id":"sess-e2e","transcript_path":"%s","prompt":"test","model":"claude-sonnet-4-6"}`,
		transcriptPath)
	runSemRaw(t, env, dir, []byte(submitStdin),
		"capture", "claude-code", "user-prompt-submit")

	// Append assistant output after the offset is saved so stop can capture it.
	f, err := os.OpenFile(transcriptPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open transcript for append: %v", err)
	}
	assistantLine := fmt.Sprintf(
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Write","input":{"file_path":"%s","content":"package main"}}]},"timestamp":"2026-03-12T00:00:01Z"}`,
		targetFile)
	resultLine := `{"type":"result","result":{"type":"success"},"timestamp":"2026-03-12T00:00:02Z"}`
	if _, err := fmt.Fprintf(f, "%s\n%s\n", assistantLine, resultLine); err != nil {
		f.Close()
		t.Fatalf("append transcript: %v", err)
	}
	f.Close()

	// stop reads from the saved offset and routes new events to the repo.
	stopStdin := fmt.Sprintf(
		`{"session_id":"sess-e2e","transcript_path":"%s"}`,
		transcriptPath)
	runSemRaw(t, env, dir, []byte(stopStdin),
		"capture", "claude-code", "stop")

	sessOut := runSem(t, env, dir, "sessions", "--all", "--json")
	var sess sessionsOutput
	if err := json.Unmarshal([]byte(sessOut), &sess); err != nil {
		t.Fatalf("parse sessions output: %v\n%s", err, sessOut)
	}

	found := false
	for _, s := range sess.Sessions {
		if s.Provider == "claude_code" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no session with provider=claude_code found; sessions: %+v", sess.Sessions)
	}
}
