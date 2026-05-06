package util

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendHookError_WritesJSONLine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	AppendHookError("claude-code", "PostToolUse", "dispatch failed: kaboom")

	data, err := os.ReadFile(filepath.Join(dir, "semantica", HookErrorLogBasename))
	if err != nil {
		t.Fatalf("expected log file to exist: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d: %q", len(lines), data)
	}
	var entry HookErrorEntry
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, lines[0])
	}
	if entry.Provider != "claude-code" || entry.Hook != "PostToolUse" {
		t.Errorf("provider/hook lost in encoding: %+v", entry)
	}
	if !strings.Contains(entry.Message, "kaboom") {
		t.Errorf("message lost: %q", entry.Message)
	}
	if entry.Timestamp == "" {
		t.Error("expected timestamp to be set")
	}
}

func TestAppendHookError_AppendsAcrossInvocations(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	AppendHookError("claude-code", "PostToolUse", "first")
	AppendHookError("kiro-ide", "agentStop", "second")

	entries, err := ReadHookErrorTail(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d: %+v", len(entries), entries)
	}
	if entries[0].Provider != "claude-code" || entries[1].Provider != "kiro-ide" {
		t.Errorf("entries out of order: %+v", entries)
	}
}

func TestReadHookErrorTail_NoFileReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	entries, err := ReadHookErrorTail(10)
	if err != nil {
		t.Errorf("missing file should not be an error, got %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty slice, got %d entries", len(entries))
	}
}

func TestReadHookErrorTail_LimitsToMaxLines(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	for i := 0; i < 5; i++ {
		AppendHookError("p", "h", "msg")
	}
	entries, err := ReadHookErrorTail(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Errorf("expected 3 entries, got %d", len(entries))
	}
}

func TestReadHookErrorTail_SkipsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	// Write a mix of valid + garbage manually so the function has
	// to skip the bad lines.
	if err := os.MkdirAll(filepath.Join(dir, "semantica"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"ts":"2026-05-06T12:00:00Z","provider":"a","hook":"h","msg":"ok"}
not-json garbage
{"ts":"2026-05-06T12:01:00Z","provider":"b","hook":"h","msg":"also ok"}
`)
	if err := os.WriteFile(filepath.Join(dir, "semantica", HookErrorLogBasename), body, 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := ReadHookErrorTail(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 valid entries (garbage skipped), got %d", len(entries))
	}
}
