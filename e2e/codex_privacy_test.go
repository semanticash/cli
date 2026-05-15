//go:build e2e

package e2e_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCaptureCodexOffRepoIsSilent drives `semantica capture codex
// post-tool-use` with a hook payload whose cwd resolves outside any
// registered repo. The Codex provider's ShouldCapture must reject the
// invocation before any side effect runs, so nothing should land in
// the broker registry, agent_events, the global blob store, or the
// hook-errors log.
//
// This is the integration counterpart to the unit test in
// internal/hooks/codex that covers ShouldCapture itself. Running the
// real binary catches mis-orderings between the capture entrypoint's
// gates: the existing broker-wide "any active repo" check fires
// first, but the codex-specific cwd preflight must run *before*
// ParseHookEvent, blob-store open, broker write, and any hook-error
// log append.
func TestCaptureCodexOffRepoIsSilent(t *testing.T) {
	dir, env := initGitRepo(t)
	enableRepo(t, env, dir)

	// Capture the hook-errors log byte count before the off-repo
	// invocation. enableRepo can append benign entries (e.g. hook
	// install warnings on some platforms) so a non-zero starting
	// size is normal. We assert no growth instead.
	hookErrLog := hookErrorsLogPath(t, env, dir)
	startSize := fileSize(t, hookErrLog)

	// A second directory outside the registered repo, NOT
	// initialized as a git repo. ShouldCapture's git.FindRoot fails
	// here, so the gate rejects regardless of broker state.
	offRepo, err := os.MkdirTemp("", "e2e-offrepo-*")
	if err != nil {
		t.Fatalf("mkdir off-repo: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(offRepo) })
	if resolved, err := filepath.EvalSymlinks(offRepo); err == nil {
		offRepo = resolved
	}

	// A realistic Codex PostToolUse[apply_patch] payload, except
	// cwd points outside the registered repo. If the gate fails
	// open, the apply_patch envelope would normally produce a
	// line-level Write event for forbidden.go.
	//
	// Marshal via encoding/json so paths containing JSON-active
	// characters, including Windows backslashes, are escaped
	// correctly. The test must exercise a valid payload whose cwd is
	// outside the enabled repo, not a malformed-payload denial.
	payload, err := json.Marshal(map[string]any{
		"session_id":       "off-repo-session",
		"turn_id":          "off-repo-turn",
		"transcript_path":  filepath.Join(offRepo, "transcript.jsonl"),
		"cwd":              offRepo,
		"hook_event_name":  "PostToolUse",
		"model":            "gpt-5.4",
		"permission_mode":  "default",
		"tool_name":        "apply_patch",
		"tool_input": map[string]string{
			"command": "*** Begin Patch\n*** Add File: forbidden.go\n+package secret\n*** End Patch",
		},
		"tool_response": "{}",
		"tool_use_id":   "call_off_repo",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	// Sanity check: the payload round-trips as valid JSON before
	// we send it. If it does not parse here, neither would the
	// capture command, and the test would assert the right
	// outcome via the wrong code path.
	var roundTrip map[string]any
	if err := json.Unmarshal(payload, &roundTrip); err != nil {
		t.Fatalf("payload is not valid JSON: %v\n%s", err, payload)
	}
	if got, _ := roundTrip["cwd"].(string); got != offRepo {
		t.Fatalf("payload cwd round-trip mismatch: got %q, want %q", got, offRepo)
	}

	// Exit must be zero - hooks are non-blocking by contract.
	runSemRaw(t, env, dir, payload,
		"capture", "codex", "post-tool-use")

	// No session should have been recorded for codex.
	sessOut := runSem(t, env, dir, "sessions", "--all", "--json")
	var sess sessionsOutput
	if err := json.Unmarshal([]byte(sessOut), &sess); err != nil {
		t.Fatalf("parse sessions output: %v\n%s", err, sessOut)
	}
	for _, s := range sess.Sessions {
		if s.Provider == "codex" {
			t.Errorf("off-repo Codex hook produced a session: %+v", s)
		}
	}

	// hook-errors.log must not have grown. A parse-error or other
	// log line during this invocation would itself leak that a hook
	// fired in a directory we should not be observing.
	if endSize := fileSize(t, hookErrLog); endSize != startSize {
		t.Errorf("hook-errors.log grew during off-repo invocation: %d -> %d bytes\ntail: %s",
			startSize, endSize, tailFile(t, hookErrLog, 4096))
	}

	// No global blob should mention the forbidden.go fixture path
	// or the codex session id. We scan the objects tree directly
	// because the public CLI surfaces no listing for global blobs.
	globalBlobs := filepath.Join(dir, ".semantica-global", "objects")
	if hits := grepFiles(t, globalBlobs, "off-repo-session"); len(hits) > 0 {
		t.Errorf("session id leaked into global blob store: %v", hits)
	}
	if hits := grepFiles(t, globalBlobs, "forbidden.go"); len(hits) > 0 {
		t.Errorf("apply_patch fixture path leaked into global blob store: %v", hits)
	}
}

// hookErrorsLogPath returns the location the capture command would
// append to. AppConfigDir resolves to <HOME>/Library/Application
// Support/semantica on macOS and <HOME>/.config/semantica on Linux,
// so we honor whichever the test env produced.
func hookErrorsLogPath(t *testing.T, env []string, dir string) string {
	t.Helper()
	// Prefer the path the env's HOME implies. We probe both
	// candidates rather than reimplementing AppConfigDir.
	for _, candidate := range []string{
		filepath.Join(dir, ".config", "semantica", "hook-errors.log"),
		filepath.Join(dir, "Library", "Application Support", "semantica", "hook-errors.log"),
	} {
		if _, err := os.Stat(filepath.Dir(candidate)); err == nil {
			return candidate
		}
	}
	// Default to the Linux-shaped path; doctor will create the
	// directory on first write if the test exercises that branch.
	return filepath.Join(dir, ".config", "semantica", "hook-errors.log")
}

// fileSize returns 0 for missing files; the caller compares the
// before/after size delta to assert silence.
func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// tailFile reads the last n bytes of a file for diagnostic output on
// test failure. Missing files yield an empty string.
func tailFile(t *testing.T, path string, n int) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(data) > n {
		return string(data[len(data)-n:])
	}
	return string(data)
}

// grepFiles walks root and returns paths whose contents contain the
// given substring. Used to confirm sensitive payload tokens never
// land in the global blob store.
func grepFiles(t *testing.T, root, needle string) []string {
	t.Helper()
	var hits []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if strings.Contains(string(data), needle) {
			hits = append(hits, path)
		}
		return nil
	})
	return hits
}
