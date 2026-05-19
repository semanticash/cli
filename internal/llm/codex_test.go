package llm

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain intercepts the magic GO_TEST_HELPER_CODEX env var so the
// test binary can be re-execed as a fake codex CLI. The pattern is
// the standard library's TestHelperProcess approach (see os/exec
// tests): every child process spawned with this env var set acts as
// codex; otherwise the test binary runs normally. This avoids
// shipping a separate fake-codex binary or shell scripts that
// wouldn't work cross-platform.
func TestMain(m *testing.M) {
	if os.Getenv("GO_TEST_HELPER_CODEX") == "1" {
		runFakeCodex()
		return
	}
	os.Exit(m.Run())
}

// runFakeCodex is the re-exec entry point. It parses os.Args
// looking for the --output-last-message flag, reads stdin if
// requested, optionally logs argv + stdin to test-supplied paths,
// then writes a canned narrative (or simulates failure) based on
// FAKE_CODEX_MODE.
func runFakeCodex() {
	var outputPath string
	for i, a := range os.Args {
		if a == "--output-last-message" && i+1 < len(os.Args) {
			outputPath = os.Args[i+1]
		}
	}

	if logPath := os.Getenv("FAKE_CODEX_ARGV_LOG"); logPath != "" {
		// Args[1:] excludes the binary path; tests assert on the
		// argv codex received, not on the test binary's location.
		_ = os.WriteFile(logPath, []byte(strings.Join(os.Args[1:], "\n")), 0o644)
	}
	if logPath := os.Getenv("FAKE_CODEX_STDIN_LOG"); logPath != "" {
		data, _ := io.ReadAll(os.Stdin)
		_ = os.WriteFile(logPath, data, 0o644)
	}
	if logPath := os.Getenv("FAKE_CODEX_OUTPUT_PATH_LOG"); logPath != "" {
		// Record the tempfile path codex received via -o so the
		// test can stat it after Generate returns to verify
		// cleanup (the runner's defer os.Remove).
		_ = os.WriteFile(logPath, []byte(outputPath), 0o644)
	}

	switch os.Getenv("FAKE_CODEX_MODE") {
	case "fail":
		_, _ = os.Stderr.WriteString("simulated codex failure on stderr\n")
		os.Exit(7)
	case "partial-then-fail":
		// Write partial narrative to the tempfile, then exit
		// non-zero. The runner contract requires NOT reading the
		// tempfile on failure; this case proves it.
		if outputPath != "" {
			_ = os.WriteFile(outputPath, []byte("STALE-PARTIAL-OUTPUT"), 0o644)
		}
		_, _ = os.Stderr.WriteString("partial then fail\n")
		os.Exit(3)
	case "no-output":
		// Success exit but no file written. Runner should surface
		// a read error rather than crash.
		os.Exit(0)
	default:
		// Happy path: write canned narrative to outputPath, exit 0.
		if outputPath == "" {
			_, _ = os.Stderr.WriteString("fake-codex: no --output-last-message flag\n")
			os.Exit(2)
		}
		_, _ = os.Stderr.WriteString("fake-codex: progress noise should not appear in narrative\n")
		_ = os.WriteFile(outputPath, []byte("FAKE_NARRATIVE_OK\n"), 0o644)
		os.Exit(0)
	}
}

// setupFakeCodex puts the test binary path + the helper env var
// onto t for the duration of the test. Returns the path callers
// pass as the binPath argument to codexWriter.Generate.
func setupFakeCodex(t *testing.T) string {
	t.Helper()
	t.Setenv("GO_TEST_HELPER_CODEX", "1")
	t.Setenv("FAKE_CODEX_MODE", "")
	return os.Args[0]
}

// TestCodex_Generate_HappyPath covers the success path: argv shape,
// stdin prompt routing, --output-last-message tempfile read.
func TestCodex_Generate_HappyPath(t *testing.T) {
	fake := setupFakeCodex(t)
	tmp := t.TempDir()
	argvLog := filepath.Join(tmp, "argv")
	stdinLog := filepath.Join(tmp, "stdin")
	t.Setenv("FAKE_CODEX_ARGV_LOG", argvLog)
	t.Setenv("FAKE_CODEX_STDIN_LOG", stdinLog)

	out, err := (codexWriter{}).Generate(context.Background(), fake, "test prompt content")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if out != "FAKE_NARRATIVE_OK" {
		t.Errorf("Generate returned %q, want %q", out, "FAKE_NARRATIVE_OK")
	}

	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	argvStr := string(argv)
	for _, want := range []string{
		"exec",
		"--skip-git-repo-check",
		"--output-last-message",
		"-",
	} {
		if !strings.Contains(argvStr, want) {
			t.Errorf("codex argv missing %q; got:\n%s", want, argvStr)
		}
	}

	stdin, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatalf("read stdin log: %v", err)
	}
	if string(stdin) != "test prompt content" {
		t.Errorf("codex stdin = %q, want %q", string(stdin), "test prompt content")
	}
}

// TestCodex_Generate_TempfileCleanedUpOnSuccess verifies the runner
// removes the --output-last-message tempfile on a successful exit.
// The fake codex records the path it received so the test can stat
// it after Generate returns.
func TestCodex_Generate_TempfileCleanedUpOnSuccess(t *testing.T) {
	fake := setupFakeCodex(t)
	tmp := t.TempDir()
	pathLog := filepath.Join(tmp, "tmppath")
	t.Setenv("FAKE_CODEX_OUTPUT_PATH_LOG", pathLog)

	if _, err := (codexWriter{}).Generate(context.Background(), fake, "prompt"); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	tempPathBytes, err := os.ReadFile(pathLog)
	if err != nil {
		t.Fatalf("read path log: %v", err)
	}
	tempPath := strings.TrimSpace(string(tempPathBytes))
	if tempPath == "" {
		t.Fatal("fake codex did not record a tempfile path")
	}
	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Errorf("tempfile %q still exists after Generate; want removed (defer os.Remove failed)", tempPath)
	}
}

// TestCodex_Generate_FailureSurfacesStderr verifies that a non-zero
// exit produces an error that contains the captured stderr text.
// Without this, callers up the stack would have no signal for why
// codex failed.
func TestCodex_Generate_FailureSurfacesStderr(t *testing.T) {
	fake := setupFakeCodex(t)
	t.Setenv("FAKE_CODEX_MODE", "fail")

	_, err := (codexWriter{}).Generate(context.Background(), fake, "prompt")
	if err == nil {
		t.Fatal("expected error from failing codex; got nil")
	}
	if !strings.Contains(err.Error(), "simulated codex failure on stderr") {
		t.Errorf("error does not surface stderr text: %v", err)
	}
}

// TestCodex_Generate_DoesNotReadPartialTempfileOnFailure guards the
// "read only on successful exit" contract. The fake codex writes a
// partial narrative to the tempfile and then exits non-zero; the
// runner must surface the exec error, never the stale content.
func TestCodex_Generate_DoesNotReadPartialTempfileOnFailure(t *testing.T) {
	fake := setupFakeCodex(t)
	t.Setenv("FAKE_CODEX_MODE", "partial-then-fail")

	out, err := (codexWriter{}).Generate(context.Background(), fake, "prompt")
	if err == nil {
		t.Fatal("expected error when codex exits non-zero; got nil")
	}
	if out != "" {
		t.Errorf("Generate returned text %q on failure; should be empty", out)
	}
	if strings.Contains(err.Error(), "STALE-PARTIAL-OUTPUT") {
		t.Errorf("error contains stale tempfile content; runner should not have read it: %v", err)
	}
}

// TestCodex_Generate_TempfileCleanedUpOnFailure verifies the runner's
// defer os.Remove fires even when codex exits non-zero, so a stream
// of failed runs doesn't leak tempfiles into the system temp dir.
func TestCodex_Generate_TempfileCleanedUpOnFailure(t *testing.T) {
	fake := setupFakeCodex(t)
	tmp := t.TempDir()
	pathLog := filepath.Join(tmp, "tmppath")
	t.Setenv("FAKE_CODEX_MODE", "partial-then-fail")
	t.Setenv("FAKE_CODEX_OUTPUT_PATH_LOG", pathLog)

	_, _ = (codexWriter{}).Generate(context.Background(), fake, "prompt")

	tempPathBytes, err := os.ReadFile(pathLog)
	if err != nil {
		t.Fatalf("read path log: %v", err)
	}
	tempPath := strings.TrimSpace(string(tempPathBytes))
	if tempPath == "" {
		t.Fatal("fake codex did not record a tempfile path")
	}
	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Errorf("tempfile %q still exists after failed Generate; defer cleanup must run on every exit path", tempPath)
	}
}

// codexWriter.Find() is a one-line wrapper around
// util.ResolveExecutable; the actual lookup behavior (PATH search,
// fallback to /opt/homebrew/bin and other well-known install dirs,
// .exe handling on Windows) lives in internal/util and is covered
// by tests there. No dedicated Find() test in this file - it would
// only re-test util.ResolveExecutable.
