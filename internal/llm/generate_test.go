package llm

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Timeout errors should report elapsed wall time, not the configured cap.
func TestFormatShellError_TimeoutReportsActualElapsed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()

	stderr := &bytes.Buffer{}
	start := time.Now().Add(-1200 * time.Millisecond)

	err := formatShellError(ctx, errors.New("signal: killed"), stderr, start)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "timed out after ") {
		t.Errorf("expected 'timed out after ...' prefix, got %q", msg)
	}
	if !strings.Contains(msg, "1s") {
		t.Errorf("expected elapsed 1s in message, got %q", msg)
	}
	if strings.Contains(msg, llmShellTimeout.String()) {
		t.Errorf("message must not hardcode llmShellTimeout, got %q", msg)
	}
}

// Non-timeout failures should keep the existing exec error formatting.
func TestFormatShellError_NonTimeoutDelegatesToFmtExecErr(t *testing.T) {
	ctx := context.Background()
	stderr := &bytes.Buffer{}
	stderr.WriteString("boom")
	start := time.Now()

	err := formatShellError(ctx, errors.New("generic failure"), stderr, start)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	if strings.Contains(msg, "timed out") {
		t.Errorf("non-deadline error should not report timeout, got %q", msg)
	}
	if !strings.Contains(msg, "generic failure") {
		t.Errorf("expected wrapped original error, got %q", msg)
	}
}

// Explicit cancellation is not a timeout.
func TestFormatShellError_CancelledContextIsNotTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stderr := &bytes.Buffer{}
	start := time.Now()

	err := formatShellError(ctx, errors.New("signal: killed"), stderr, start)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Errorf("cancellation must not be reported as timeout, got %q", err.Error())
	}
}

func TestExtractJSONFromMarkdown_PlainJSON(t *testing.T) {
	input := `{"intent":"fix bug","outcome":"fixed","learnings":[],"friction":[],"open_items":[]}`
	got := extractJSONFromMarkdown(input)
	if got != input {
		t.Errorf("expected unchanged JSON, got %q", got)
	}
}

func TestExtractJSONFromMarkdown_CodeBlockJSON(t *testing.T) {
	input := "```json\n{\"intent\":\"fix\"}\n```"
	want := `{"intent":"fix"}`
	got := extractJSONFromMarkdown(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractJSONFromMarkdown_CodeBlockNoLang(t *testing.T) {
	input := "```\n{\"intent\":\"fix\"}\n```"
	want := `{"intent":"fix"}`
	got := extractJSONFromMarkdown(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractJSONFromMarkdown_WhitespaceWrapped(t *testing.T) {
	input := "  \n```json\n{\"intent\":\"fix\"}\n```\n  "
	want := `{"intent":"fix"}`
	got := extractJSONFromMarkdown(input)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseNarrativeJSON_Valid(t *testing.T) {
	input := `{"intent":"fix auth","outcome":"fixed login","learnings":["use bcrypt"],"friction":["slow CI"],"open_items":["add tests"]}`
	result, err := parseNarrativeJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Intent != "fix auth" {
		t.Errorf("intent = %q, want %q", result.Intent, "fix auth")
	}
	if result.Outcome != "fixed login" {
		t.Errorf("outcome = %q, want %q", result.Outcome, "fixed login")
	}
	if len(result.Learnings) != 1 || result.Learnings[0] != "use bcrypt" {
		t.Errorf("learnings = %v, want [use bcrypt]", result.Learnings)
	}
}

func TestParseNarrativeJSON_MarkdownWrapped(t *testing.T) {
	input := "```json\n{\"intent\":\"refactor\",\"outcome\":\"cleaner\",\"learnings\":[],\"friction\":[],\"open_items\":[]}\n```"
	result, err := parseNarrativeJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Intent != "refactor" {
		t.Errorf("intent = %q, want %q", result.Intent, "refactor")
	}
}

func TestParseNarrativeJSON_Invalid(t *testing.T) {
	_, err := parseNarrativeJSON("not json at all")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestCleanEnv_FiltersGitAndClaudeCode(t *testing.T) {
	env := []string{
		"HOME=/workspace/home",
		"GIT_DIR=/repo/.git",
		"GIT_WORK_TREE=/repo",
		"CLAUDECODE=1",
		"PATH=/usr/bin",
	}
	got := cleanEnv(env)
	want := map[string]bool{"HOME=/workspace/home": true, "PATH=/usr/bin": true}
	if len(got) != len(want) {
		t.Fatalf("got %d vars, want %d: %v", len(got), len(want), got)
	}
	for _, v := range got {
		if !want[v] {
			t.Errorf("unexpected env var: %s", v)
		}
	}
}

func TestFindClaude_ReturnsEmptyWhenNotInstalled(t *testing.T) {
	// With a clean PATH that doesn't contain claude, findClaude should
	// check fallback paths. On CI / test machines it likely returns "".
	// We just verify it doesn't panic.
	_ = findClaude()
}

func TestFindCursorAgent_ReturnsEmptyWhenNotInstalled(t *testing.T) {
	// Same - verify it doesn't panic.
	_ = findCursorAgent()
}

func TestVSCodeClaudeBinaries_FindsBundledBinary(t *testing.T) {
	home := t.TempDir()

	bin := "claude"
	if runtime.GOOS == "windows" {
		bin = "claude.exe"
	}

	// Create two fake extension versions with native binaries.
	for _, ver := range []string{"1.0.0-darwin-arm64", "2.0.0-darwin-arm64"} {
		dir := filepath.Join(home, ".vscode", "extensions", "anthropic.claude-code-"+ver, "resources", "native-binary")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, bin), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	got := vsCodeClaudeBinaries(home)
	if len(got) != 2 {
		t.Fatalf("expected 2 candidates, got %d: %v", len(got), got)
	}
	// Newest version should be first.
	if filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(got[0])))) != "anthropic.claude-code-2.0.0-darwin-arm64" {
		t.Errorf("expected newest version first, got %s", got[0])
	}
}

func TestVSCodeClaudeBinaries_EmptyWhenNoExtension(t *testing.T) {
	home := t.TempDir()
	got := vsCodeClaudeBinaries(home)
	if len(got) != 0 {
		t.Errorf("expected 0 candidates, got %d: %v", len(got), got)
	}
}
