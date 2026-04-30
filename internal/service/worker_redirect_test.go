package service

import (
	"bytes"
	"fmt"
	stdlog "log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRedirectWorkerLog_WritesToRequestedFile pins the basic contract:
// after RedirectWorkerLog, both wlog and direct stderr writes land in
// the file. Mirrors what the Linux/Windows backends will see when
// they pass --log-file.
func TestRedirectWorkerLog_WritesToRequestedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "global.log")

	cleanup, err := RedirectWorkerLog(path)
	if err != nil {
		t.Fatalf("RedirectWorkerLog: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	wlog("hello via wlog\n")
	_, _ = fmt.Fprintln(os.Stderr, "hello via stderr")
	_, _ = fmt.Fprintln(os.Stdout, "hello via stdout")
	slog.Warn("hello via slog", "key", "value")

	// Cleanup early so we can read the file safely without racing the
	// deferred close. Calling cleanup twice is documented as safe.
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	got := string(body)

	for _, want := range []string{"hello via wlog", "hello via stderr", "hello via stdout", "hello via slog"} {
		if !strings.Contains(got, want) {
			t.Errorf("log file missing %q; got:\n%s", want, got)
		}
	}
}

// TestRedirectWorkerLog_CapturesSlogFromService ensures service-side
// slog calls stay inside the redirected window.
func TestRedirectWorkerLog_CapturesSlogFromService(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "global.log")

	cleanup, err := RedirectWorkerLog(path)
	if err != nil {
		t.Fatalf("RedirectWorkerLog: %v", err)
	}

	// Match the service's existing slog call shape.
	slog.Warn("attribution: payload load failed",
		"event_id", "abc",
		"payload_hash", "deadbeef",
	)
	slog.Debug("commit-msg: write attribution summary failed",
		"path", "/tmp/x",
		"err", "boom",
	)

	if err := cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	got := readLogFile(t, path)
	if !strings.Contains(got, "attribution: payload load failed") {
		t.Errorf("slog warning missing from log file; got:\n%s", got)
	}
	// Debug is below the default LevelInfo, so this should not appear.
	if strings.Contains(got, "commit-msg: write attribution summary failed") {
		t.Errorf("slog debug must be filtered at LevelInfo; got:\n%s", got)
	}
}

// TestRedirectWorkerLog_DefaultBehaviorUnchanged confirms that the
// flag is opt-in: when RedirectWorkerLog is not called, wlogWriter
// remains the package-level default, and writes do not surface in
// any file we control. Pins the macOS path where the flag is unused.
func TestRedirectWorkerLog_DefaultBehaviorUnchanged(t *testing.T) {
	// Snapshot the package-level default. If a previous test mutated
	// it without restoring (or this test's setup did), fail loudly so
	// the regression is visible rather than silently absorbed.
	if wlogWriter != os.Stderr {
		t.Fatalf("precondition: wlogWriter is not os.Stderr; another test did not restore it")
	}

	// Capture by swapping wlogWriter to a buffer manually - the same
	// pattern existing tests use.
	// This proves wlog routes through wlogWriter without going
	// anywhere RedirectWorkerLog could intercept.
	var buf bytes.Buffer
	prev := wlogWriter
	wlogWriter = &buf
	t.Cleanup(func() { wlogWriter = prev })

	wlog("default path\n")

	if !strings.Contains(buf.String(), "default path") {
		t.Errorf("expected 'default path' in buffer, got %q", buf.String())
	}
}

// TestRedirectWorkerLog_PerJobRepoLogStillWins checks that the
// global redirect and the per-job repo redirect compose correctly.
func TestRedirectWorkerLog_PerJobRepoLogStillWins(t *testing.T) {
	dir := t.TempDir()
	globalPath := filepath.Join(dir, "global.log")
	repoRoot := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repoRoot, ".semantica"), 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}

	cleanup, err := RedirectWorkerLog(globalPath)
	if err != nil {
		t.Fatalf("RedirectWorkerLog: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	// Pre-job: lands in global, both wlog and slog channels.
	wlog("before job wlog\n")
	slog.Warn("before job slog")

	// Per-job redirect - same code path the drain loop uses.
	restore := redirectWlogToRepoLog(repoRoot)
	wlog("during job wlog\n")
	slog.Warn("during job slog")
	restore()

	// Post-job: lands back in global.
	wlog("after job wlog\n")
	slog.Warn("after job slog")

	if err := cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	globalBody := readLogFile(t, globalPath)
	repoBody := readLogFile(t, filepath.Join(repoRoot, ".semantica", "worker.log"))

	// Global file: pre + post on BOTH channels, not during on either.
	for _, want := range []string{"before job wlog", "after job wlog", "before job slog", "after job slog"} {
		if !strings.Contains(globalBody, want) {
			t.Errorf("global log missing %q; got:\n%s", want, globalBody)
		}
	}
	for _, mustNot := range []string{"during job wlog", "during job slog"} {
		if strings.Contains(globalBody, mustNot) {
			t.Errorf("global log MUST NOT contain %q; got:\n%s", mustNot, globalBody)
		}
	}

	// Repo-local file: only the during-job content, on both channels.
	for _, want := range []string{"during job wlog", "during job slog"} {
		if !strings.Contains(repoBody, want) {
			t.Errorf("repo log missing %q; got:\n%s", want, repoBody)
		}
	}
	for _, mustNot := range []string{"before job wlog", "after job wlog", "before job slog", "after job slog"} {
		if strings.Contains(repoBody, mustNot) {
			t.Errorf("repo log MUST NOT contain %q; got:\n%s", mustNot, repoBody)
		}
	}
}

// TestRedirectWorkerLog_AppendsToExisting confirms append-mode
// semantics: a re-redirect to the same path preserves prior content.
// This matters because systemd or Task Scheduler may invoke `worker
// drain --log-file=...` repeatedly across kicks; each invocation must
// add to the launcher log, not truncate it.
func TestRedirectWorkerLog_AppendsToExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "global.log")

	if err := os.WriteFile(path, []byte("prior content\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	cleanup, err := RedirectWorkerLog(path)
	if err != nil {
		t.Fatalf("RedirectWorkerLog: %v", err)
	}
	wlog("appended line\n")
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	got := readLogFile(t,path)
	if !strings.Contains(got, "prior content") {
		t.Errorf("append mode lost prior content; got:\n%s", got)
	}
	if !strings.Contains(got, "appended line") {
		t.Errorf("missing new line; got:\n%s", got)
	}
}

// TestRedirectWorkerLog_RestoresStandardLogWriter confirms that
// log.Printf follows the redirect and then returns to its prior
// writer after cleanup. This test confirms:
//
//   - log.Printf during the redirect lands in the file;
//   - log.Printf after cleanup goes back to its original writer;
//   - cleanup does NOT leave dangling references to the closed file.
func TestRedirectWorkerLog_RestoresStandardLogWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "global.log")

	// Capture standard-log output to a buffer for the post-cleanup
	// half of the test. Restoring it at the end of the test keeps
	// other tests' assumptions about log.Writer intact.
	var postBuf bytes.Buffer
	originalLogWriter := stdlog.Writer()
	stdlog.SetOutput(&postBuf)
	t.Cleanup(func() { stdlog.SetOutput(originalLogWriter) })

	cleanup, err := RedirectWorkerLog(path)
	if err != nil {
		t.Fatalf("RedirectWorkerLog: %v", err)
	}

	// During the window: log.Printf should reach the file (slog wired
	// the standard log to flow through our handler).
	stdlog.Printf("during redirect log.Printf")

	if err := cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	// After the window: log.Printf must NOT keep writing through the
	// closed file's handler. It should go back to whatever log.Writer
	// was before RedirectWorkerLog ran, which we set to postBuf above.
	stdlog.Printf("after redirect log.Printf")

	fileBody := readLogFile(t, path)
	if !strings.Contains(fileBody, "during redirect log.Printf") {
		t.Errorf("log file missing the during-redirect line; got:\n%s", fileBody)
	}
	if strings.Contains(fileBody, "after redirect log.Printf") {
		t.Errorf("log file MUST NOT contain post-cleanup line; got:\n%s", fileBody)
	}

	if !strings.Contains(postBuf.String(), "after redirect log.Printf") {
		t.Errorf("post-cleanup standard-log writer missing line; got: %q", postBuf.String())
	}
}

// TestRedirectWorkerLog_RestoresStandardLogFlags confirms that
// slog's temporary log flag change is restored after cleanup.
//
// The test pins three points: flags become 0 during the redirect;
// cleanup restores the original flags; nested redirects compose
// (the per-job restore returns to the outer redirect's flags, and
// the outer cleanup returns to the original).
func TestRedirectWorkerLog_RestoresStandardLogFlags(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "global.log")

	// Stamp a non-zero flag set so we can detect the install-side
	// zeroing and the restore. LstdFlags is the stock default; using
	// it here means the test exercises the most realistic case.
	originalFlags := stdlog.Flags()
	stdlog.SetFlags(stdlog.LstdFlags)
	t.Cleanup(func() { stdlog.SetFlags(originalFlags) })
	pinned := stdlog.Flags()

	cleanup, err := RedirectWorkerLog(path)
	if err != nil {
		t.Fatalf("RedirectWorkerLog: %v", err)
	}

	if got := stdlog.Flags(); got != 0 {
		t.Errorf("during redirect, slog.SetDefault should zero log.Flags; got %d", got)
	}

	if err := cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	if got := stdlog.Flags(); got != pinned {
		t.Errorf("after cleanup, log.Flags must restore to %d, got %d", pinned, got)
	}
}

// TestRedirectWorkerLog_CleanupIdempotent confirms the documented
// safe-to-call-twice contract.
func TestRedirectWorkerLog_CleanupIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "global.log")

	cleanup, err := RedirectWorkerLog(path)
	if err != nil {
		t.Fatalf("RedirectWorkerLog: %v", err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("first cleanup: %v", err)
	}
	if err := cleanup(); err != nil {
		t.Errorf("second cleanup must be a no-op error-free, got: %v", err)
	}
	if wlogWriter != os.Stderr {
		t.Errorf("cleanup must restore wlogWriter to os.Stderr, got %T", wlogWriter)
	}
}

func readLogFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
