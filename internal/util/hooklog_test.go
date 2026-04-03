package util

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendActivityLog_CreatesFileAndWritesEntry(t *testing.T) {
	dir := t.TempDir()

	AppendActivityLog(dir, "test message %d", 42)

	data, err := os.ReadFile(filepath.Join(dir, "activity.log"))
	if err != nil {
		t.Fatalf("expected activity.log to exist: %v", err)
	}

	line := strings.TrimSpace(string(data))
	if !strings.HasSuffix(line, "  test message 42") {
		t.Fatalf("unexpected log line: %q", line)
	}

	// Timestamp prefix should be RFC3339-ish (contains T and a timezone)
	parts := strings.SplitN(line, "  ", 2)
	if len(parts) != 2 {
		t.Fatalf("expected timestamp + message separated by two spaces, got: %q", line)
	}
	ts := parts[0]
	if !strings.Contains(ts, "T") {
		t.Fatalf("timestamp doesn't look like RFC3339: %q", ts)
	}
}

func TestAppendActivityLog_AppendsMultipleEntries(t *testing.T) {
	dir := t.TempDir()

	AppendActivityLog(dir, "first")
	AppendActivityLog(dir, "second")
	AppendActivityLog(dir, "third")

	data, err := os.ReadFile(filepath.Join(dir, "activity.log"))
	if err != nil {
		t.Fatalf("expected activity.log to exist: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %q", len(lines), string(data))
	}

	for i, want := range []string{"first", "second", "third"} {
		if !strings.HasSuffix(lines[i], "  "+want) {
			t.Errorf("line %d: expected suffix %q, got %q", i, want, lines[i])
		}
	}
}

func TestAppendActivityLog_CreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "semdir")

	AppendActivityLog(dir, "hello")

	data, err := os.ReadFile(filepath.Join(dir, "activity.log"))
	if err != nil {
		t.Fatalf("expected activity.log to be created in nested dir: %v", err)
	}
	if !strings.Contains(string(data), "hello") {
		t.Fatalf("expected 'hello' in log, got: %q", string(data))
	}
}

func TestAppendActivityLog_FormatArgs(t *testing.T) {
	dir := t.TempDir()

	AppendActivityLog(dir, "error: %s failed with code %d", "open", 2)

	data, err := os.ReadFile(filepath.Join(dir, "activity.log"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(data), "error: open failed with code 2") {
		t.Fatalf("format args not applied: %q", string(data))
	}
}
