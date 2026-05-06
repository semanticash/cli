package health

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/util"
)

// hook-errors sidecar reading

func TestCheckHookErrors_NoFile_OK(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	checks := checkHookErrors(context.Background())
	if len(checks) != 1 || checks[0].Status != StatusOK {
		t.Errorf("missing log should be ok, got %+v", checks)
	}
}

func TestCheckHookErrors_UnreadableLog_Warns(t *testing.T) {
	// Skip on Windows because chmod 0 on a directory does not block
	// reads under Windows ACLs the way it does on POSIX, so the
	// "unreadable" precondition cannot be reproduced cheaply.
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only permission test")
	}
	if os.Getuid() == 0 {
		t.Skip("root bypasses file mode bits; skip permission test")
	}

	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	// Create the log, then drop the parent dir's perms so doctor
	// cannot open the file. ReadHookErrorTail must return a non-nil
	// error (not the missing-file shortcut) for this to exercise
	// the warn path.
	configDir := filepath.Join(dir, "semantica")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(configDir, util.HookErrorLogBasename)
	if err := os.WriteFile(logPath, []byte("{}"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(logPath, 0o644) })

	checks := checkHookErrors(context.Background())
	if len(checks) == 0 || checks[0].Status != StatusWarn {
		t.Fatalf("unreadable log must warn, got %+v", checks)
	}
	if checks[0].Remediation == "" {
		t.Error("expected a remediation hint pointing at the log path")
	}
}

func TestCheckHookErrors_OnlyOldEntries_OK(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	writeHookErrorRaw(t, dir, time.Now().Add(-48*time.Hour), "claude-code", "PostToolUse", "old")

	checks := checkHookErrors(context.Background())
	if len(checks) != 1 || checks[0].Status != StatusOK {
		t.Errorf("entries older than 24h should be ok, got %+v", checks)
	}
	if !strings.Contains(checks[0].Message, "no hook errors in the last 24h") {
		t.Errorf("expected 'no hook errors in the last 24h' message, got %q", checks[0].Message)
	}
}

func TestCheckHookErrors_RecentFailures_WarnGroupedByProviderHook(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	now := time.Now()
	writeHookErrorRaw(t, dir, now.Add(-1*time.Hour), "claude-code", "PostToolUse", "boom 1")
	writeHookErrorRaw(t, dir, now.Add(-2*time.Hour), "claude-code", "PostToolUse", "boom 2")
	writeHookErrorRaw(t, dir, now.Add(-30*time.Minute), "kiro-ide", "agentStop", "kapow")

	checks := checkHookErrors(context.Background())
	if checks[0].Status != StatusWarn {
		t.Errorf("recent failures should warn, got %q", checks[0].Status)
	}
	if !strings.Contains(checks[0].Message, "3 hook error") {
		t.Errorf("summary count missing: %q", checks[0].Message)
	}

	var sawClaudeRow, sawKiroRow bool
	for _, c := range checks[1:] {
		if strings.Contains(c.Message, "claude-code/PostToolUse: 2") {
			sawClaudeRow = true
		}
		if strings.Contains(c.Message, "kiro-ide/agentStop: 1") {
			sawKiroRow = true
		}
	}
	if !sawClaudeRow {
		t.Errorf("missing claude-code/PostToolUse: 2 row in checks: %+v", checks)
	}
	if !sawKiroRow {
		t.Errorf("missing kiro-ide/agentStop: 1 row in checks: %+v", checks)
	}
}

func TestHookErrorLabel_Fallbacks(t *testing.T) {
	cases := []struct {
		name string
		in   util.HookErrorEntry
		want string
	}{
		{"provider+hook", util.HookErrorEntry{Provider: "p", Hook: "h"}, "p/h"},
		{"provider only", util.HookErrorEntry{Provider: "p"}, "p"},
		{"message colon-prefix", util.HookErrorEntry{Message: "global blob store: io: closed pipe"}, "global blob store"},
		{"message no colon", util.HookErrorEntry{Message: "boom"}, "boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hookErrorLabel(tc.in); got != tc.want {
				t.Errorf("hookErrorLabel(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Claude tracked-settings check

func TestClaudeTrackedHookCheck_Warns(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := `{"hooks": {"PostToolUse": [{"hooks":[{"command":"semantica capture claude-code PostToolUse"}]}]}}`
	if err := os.WriteFile(filepath.Join(repo, ".claude", "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok := claudeTrackedHookCheck(repo)
	if !ok {
		t.Fatal("expected warn for tracked settings.json with marker")
	}
	if got.Status != StatusWarn {
		t.Errorf("status = %q, want warn", got.Status)
	}
	if got.Remediation == "" {
		t.Error("expected remediation to point at settings.local.json")
	}
}

func TestClaudeTrackedHookCheck_NoFile_Silent(t *testing.T) {
	repo := t.TempDir()
	if _, ok := claudeTrackedHookCheck(repo); ok {
		t.Error("expected no warning when .claude/settings.json absent")
	}
}

func TestClaudeTrackedHookCheck_NoMarker_Silent(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".claude", "settings.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := claudeTrackedHookCheck(repo); ok {
		t.Error("expected no warning when settings.json lacks the Semantica marker")
	}
}

func writeHookErrorRaw(t *testing.T, configDir string, ts time.Time, provider, hook, msg string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(configDir, "semantica"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"ts":"` + ts.UTC().Format(time.RFC3339) + `","provider":"` + provider + `","hook":"` + hook + `","msg":"` + msg + `"}` + "\n"
	path := filepath.Join(configDir, "semantica", util.HookErrorLogBasename)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
}
