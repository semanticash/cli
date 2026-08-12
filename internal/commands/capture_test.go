package commands

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/broker"
)

// readHookPayload is the bounded stdin reader used by `semantica
// capture`. These tests cover the behaviors capture relies on:
//
//  1. Piped readers return their bytes before the deadline.
//  2. Closed empty readers return an empty payload without timing out.
//  3. Open readers time out instead of blocking indefinitely.

func TestReadHookPayload_PipedReaderReturnsBytes(t *testing.T) {
	const want = `{"session_id":"sess-1","tool_name":"Edit"}`
	payload, timedOut, err := readHookPayload(strings.NewReader(want), 100*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if timedOut {
		t.Errorf("timedOut=true on a closed reader; want false")
	}
	if string(payload) != want {
		t.Errorf("payload = %q, want %q", payload, want)
	}
}

func TestReadHookPayload_EmptyClosedReaderReturnsEmpty(t *testing.T) {
	// io.Pipe with an immediately-closed writer mimics a hook runner
	// that opens stdin but writes nothing before closing it.
	pr, pw := io.Pipe()
	_ = pw.Close()
	payload, timedOut, err := readHookPayload(pr, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if timedOut {
		t.Errorf("timedOut=true on a closed empty reader; want false")
	}
	if len(payload) != 0 {
		t.Errorf("payload = %q, want empty", payload)
	}
}

func TestReadHookPayload_OpenReaderTimesOut(t *testing.T) {
	// pw is intentionally left open to mimic hosts that inherit stdin
	// instead of piping and closing a hook payload.
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	const deadline = 50 * time.Millisecond
	start := time.Now()
	payload, timedOut, err := readHookPayload(pr, deadline)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !timedOut {
		t.Errorf("timedOut=false on an open reader; want true")
	}
	if payload != nil {
		t.Errorf("payload = %q, want nil on timeout", payload)
	}
	// Allow generous slack for slow CI, but fail if the read is
	// effectively unbounded.
	maxElapsed := 10 * deadline
	if elapsed > maxElapsed {
		t.Errorf("readHookPayload took %v on a never-closing reader; want under %v", elapsed, maxElapsed)
	}
}

// Codex pre-tool capture must not emit a permission decision.
func TestCaptureCmd_CodexPreToolUse_ExitsZeroAndNeverDenies(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)

	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	canon, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	registryPath, err := broker.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("registry path: %v", err)
	}
	bh, err := broker.Open(context.Background(), registryPath)
	if err != nil {
		t.Fatalf("open broker: %v", err)
	}
	if err := broker.Register(context.Background(), bh, repo, canon); err != nil {
		t.Fatalf("register repo: %v", err)
	}
	_ = broker.Close(bh)

	payload := `{"session_id":"sess-deny","turn_id":"t","cwd":"` + canon +
		`","tool_name":"Bash","tool_use_id":"call_deny_1","tool_input":{"command":"gofmt -w ."}}`

	stdout, _ := runCaptureCapturingStd(t, payload, "codex", "pre-tool-use")

	if strings.TrimSpace(stdout) != "" {
		t.Errorf("capture wrote to stdout %q; Codex would read it as a decision", stdout)
	}
	if strings.Contains(stdout, "permissionDecision") || strings.Contains(stdout, "deny") {
		t.Errorf("capture emitted a permission decision: %q", stdout)
	}
}

// Capture failures remain non-blocking and write diagnostics only to stderr.
func TestCaptureCmd_CodexParseFailure_ExitsZeroAndSilentStdout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)

	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	canon, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	registryPath, err := broker.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("registry path: %v", err)
	}
	bh, err := broker.Open(context.Background(), registryPath)
	if err != nil {
		t.Fatalf("open broker: %v", err)
	}
	if err := broker.Register(context.Background(), bh, repo, canon); err != nil {
		t.Fatalf("register repo: %v", err)
	}
	_ = broker.Close(bh)

	// The cwd passes preflight, but the malformed session id must fail closed.
	payload := `{"cwd":"` + canon + `","session_id":{},"tool_name":"Bash","tool_use_id":"c1"}`

	stdout, stderr := runCaptureCapturingStd(t, payload, "codex", "pre-tool-use")

	if strings.TrimSpace(stdout) != "" {
		t.Errorf("capture wrote to stdout %q on the failure path; must stay silent", stdout)
	}
	if strings.Contains(stdout, "permissionDecision") || strings.Contains(stdout, "deny") {
		t.Errorf("capture emitted a permission decision on failure: %q", stdout)
	}
	if !strings.Contains(stderr, "parse hook event") {
		t.Errorf("expected the parse-error branch to log to stderr; got %q", stderr)
	}
}

// runCaptureCapturingStd runs capture with process-level standard streams.
func runCaptureCapturingStd(t *testing.T, payload string, args ...string) (stdout, stderr string) {
	t.Helper()
	// Keep capture diagnostics out of the developer's config directory.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	origIn, origOut, origErr := os.Stdin, os.Stdout, os.Stderr
	// Restore process-wide streams before returning.
	defer func() { os.Stdin, os.Stdout, os.Stderr = origIn, origOut, origErr }()
	t.Cleanup(func() { os.Stdin, os.Stdout, os.Stderr = origIn, origOut, origErr })

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	go func() {
		_, _ = inW.WriteString(payload)
		_ = inW.Close()
	}()

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	os.Stdin, os.Stdout, os.Stderr = inR, outW, errW

	outCh, errCh := make(chan string, 1), make(chan string, 1)
	go func() { data, _ := io.ReadAll(outR); outCh <- string(data) }()
	go func() { data, _ := io.ReadAll(errR); errCh <- string(data) }()

	cmd := NewCaptureCmd()
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Errorf("capture returned error %v; hooks must always exit 0", err)
	}

	_ = outW.Close()
	_ = errW.Close()
	stdout, stderr = <-outCh, <-errCh
	_ = outR.Close()
	_ = errR.Close()
	_ = inR.Close()
	return stdout, stderr
}
