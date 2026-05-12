package commands

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/service/handoff"
	"github.com/spf13/cobra"
)

// --- readContinueAnswer ---
//
// The post-write prompt asks "Continue in a new session now? [Y/n]"
// and chains into `semantica handoff continue` on accept. The
// accept-rules need to be tight: anything other than empty / y /
// yes (case-insensitive, whitespace-tolerant) must fall through to
// the manual hint so the user does not get an unexpected agent
// spawn.

func TestReadContinueAnswer_AcceptCases(t *testing.T) {
	accepts := []string{
		"\n", // bare Enter: default is Y
		"y\n",
		"Y\n",
		"yes\n",
		"YES\n",
		"  y\n",     // leading whitespace
		"y  \n",     // trailing whitespace
		"  YeS  \n", // mixed casing + whitespace
		"y",         // typed "y" then Ctrl-D (no newline): explicit accept
	}
	for _, in := range accepts {
		if !readContinueAnswer(strings.NewReader(in)) {
			t.Errorf("input %q should be treated as accept", in)
		}
	}
}

func TestReadContinueAnswer_DeclineCases(t *testing.T) {
	declines := []string{
		"n\n",
		"N\n",
		"no\n",
		"NO\n",
		"nope\n",
		"maybe\n",
		"yeah sure\n", // multi-token: not the exact "yes" sentinel
		"yy\n",        // only exact "y" or "yes" count
		"continue\n",
	}
	for _, in := range declines {
		if readContinueAnswer(strings.NewReader(in)) {
			t.Errorf("input %q should be treated as decline", in)
		}
	}
}

// TestReadContinueAnswer_CtrlDOnEmptyIsDecline guards against
// silently launching the follow-on agent when the user pressed
// Ctrl-D at the prompt without typing anything. The
// distinction matters: bare Enter ("\n") IS the [Y/n] default and
// must accept, while EOF on an empty line is the universal cancel
// and must decline.
func TestReadContinueAnswer_CtrlDOnEmptyIsDecline(t *testing.T) {
	if readContinueAnswer(strings.NewReader("")) {
		t.Error("empty input + EOF must decline; Ctrl-D is cancel, not default-accept")
	}
}

// errReader simulates a stream that errors mid-read (not io.EOF).
// confirmContinueNow must treat that as decline rather than
// chaining into a spawn the user did not actually authorize.
type errReader struct{}

func (errReader) Read(_ []byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestReadContinueAnswer_ReadErrorIsDecline(t *testing.T) {
	if readContinueAnswer(errReader{}) {
		t.Error("a non-EOF read error must not be interpreted as accept")
	}
}

// --- confirmContinueNow ---
//
// confirmContinueNow is the TTY-gated wrapper around the answer
// parser. The TTY checks are the only reason this isn't just
// readContinueAnswer: a piped stdin would block ReadString forever,
// and a non-TTY stdout means the user has no way to see the prompt
// text. Both gates must short-circuit to false WITHOUT writing the
// prompt to stdout, or skill-body and CI runs would emit an unexpected
// "Continue in a new session now? [Y/n] " line into structured
// output.

// forceWriterGateTTY stubs the stdout TTY check to return true so
// the rest of confirmContinueNow's gates and prompt logic actually
// run. Without this seam the stdout gate trips first under tests
// (bytes.Buffer is never a TTY) and tests that claim to exercise
// the stdin gate would silently exit before reaching it.
func forceWriterGateTTY(t *testing.T) {
	t.Helper()
	orig := isTerminalWriterFn
	isTerminalWriterFn = func(_ io.Writer) bool { return true }
	t.Cleanup(func() { isTerminalWriterFn = orig })
}

func TestConfirmContinueNow_NonTerminalWriterShortCircuits(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	// Stdin left as default. The real (non-stubbed) writer check
	// should fail first because bytes.Buffer is not a TTY.

	if confirmContinueNow(cmd) {
		t.Error("non-TTY stdout must return false")
	}
	if out.Len() != 0 {
		t.Errorf("must not emit prompt text on non-TTY stdout; got %q", out.String())
	}
}

// TestConfirmContinueNow_NonOSFileStdinShortCircuits pins that a
// reader that isn't an *os.File (e.g. bytes.Buffer, the default
// under runRoot) declines without reading. Stdout is forced past
// the writer gate so the stdin type-assertion is what actually
// gets exercised.
func TestConfirmContinueNow_NonOSFileStdinShortCircuits(t *testing.T) {
	forceWriterGateTTY(t)

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	// A bytes.Buffer with "y\n" would say accept if we leaked
	// through the gate, so the input here is deliberately a
	// would-be accept, to catch a gate-bypass regression.
	cmd.SetIn(bytes.NewBufferString("y\n"))

	if confirmContinueNow(cmd) {
		t.Error("bytes.Buffer stdin must decline regardless of contents")
	}
	if out.Len() != 0 {
		t.Errorf("must not emit prompt text when stdin gate fails; got %q", out.String())
	}
}

// --- Ambiguous-active-session handling ---
//
// The handoff write command resolves ambiguity differently by
// caller mode. Interactive terminals show a picker and re-route the
// bundle source through --from <picked>. Non-interactive callers
// get the active provider list and a --from hint.

// initRepoWithActiveStates creates a fresh git repo, redirects
// SEMANTICA_HOME at a temp dir, and writes one capture-state
// JSON per (sessionID, provider) entry. Returns the canonical
// repo path so callers can use it for assertions.
//
// Uses json.Marshal rather than concatenating strings so paths
// with backslashes (Windows temp dirs), quotes, or other JSON-
// significant characters do not produce invalid fixtures.
func initRepoWithActiveStates(t *testing.T, entries []struct{ sessionID, provider string }) string {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	cmd := exec.Command("git", "init", dir)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	semHome := filepath.Join(t.TempDir(), "semantica-home")
	t.Setenv("SEMANTICA_HOME", semHome)
	capDir := filepath.Join(semHome, "capture")
	if err := os.MkdirAll(capDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	for _, e := range entries {
		// Marshal an anonymous struct rather than building JSON
		// by string concat. The CWD field can contain
		// backslashes on Windows or other JSON-significant
		// characters; the marshaler escapes them correctly.
		body, err := json.Marshal(struct {
			SessionID        string `json:"session_id"`
			StateKey         string `json:"state_key"`
			Provider         string `json:"provider"`
			TranscriptRef    string `json:"transcript_ref"`
			TranscriptOffset int    `json:"transcript_offset"`
			Timestamp        int64  `json:"timestamp"`
			CWD              string `json:"cwd"`
		}{
			SessionID:     e.sessionID,
			StateKey:      e.sessionID,
			Provider:      e.provider,
			TranscriptRef: "x",
			Timestamp:     now,
			CWD:           dir,
		})
		if err != nil {
			t.Fatalf("marshal capture state: %v", err)
		}
		path := filepath.Join(capDir, "capture-"+e.sessionID+".json")
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestInitRepoWithActiveStates_EmitsValidJSON pins the fixture's
// safety contract: the capture-state files it writes must be
// valid JSON regardless of what characters the OS-supplied temp
// path contains. A previous string-concat implementation would
// produce malformed JSON when the temp dir contained backslashes
// (Windows) or quote-like characters; reverting to that approach
// must fail this test, not silently break downstream tests.
func TestInitRepoWithActiveStates_EmitsValidJSON(t *testing.T) {
	dir := initRepoWithActiveStates(t, []struct{ sessionID, provider string }{
		{"valid-json-check", "claude-code"},
	})

	semHome := os.Getenv("SEMANTICA_HOME")
	if semHome == "" {
		t.Fatal("SEMANTICA_HOME should have been set by the fixture")
	}
	path := filepath.Join(semHome, "capture", "capture-valid-json-check.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	// Decode strictly. Malformed JSON (e.g. an unescaped
	// backslash in the cwd field) would fail here.
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("fixture should emit valid JSON; got %v\nbody: %s", err, body)
	}
	if got["cwd"] != dir {
		t.Errorf("cwd round-trip mismatch: got %v, want %q", got["cwd"], dir)
	}
	if got["provider"] != "claude-code" {
		t.Errorf("provider round-trip mismatch: got %v, want claude-code", got["provider"])
	}
}

// TestHandoffWrite_NonInteractive_AmbiguousEnumeratesProviders pins
// the skill/CI path: multiple distinct active providers must
// produce an error that names them so the user can rerun with
// --from. Stripping the list (or returning the old generic
// "close all but one" message) would be a regression.
func TestHandoffWrite_NonInteractive_AmbiguousEnumeratesProviders(t *testing.T) {
	dir := initRepoWithActiveStates(t, []struct{ sessionID, provider string }{
		{"claude-1", "claude-code"},
		{"gemini-1", "gemini-cli"},
		{"cursor-1", "cursor"},
	})

	// runRoot wires bytes.Buffer stdio, so the non-interactive
	// branch fires.
	out, err := runRoot(t, []string{"handoff", "--repo", dir, "--write"})
	if err == nil {
		t.Fatalf("expected ambiguity error; stdout=%q", out)
	}
	msg := err.Error()
	for _, p := range []string{"claude-code", "gemini-cli", "cursor"} {
		if !strings.Contains(msg, p) {
			t.Errorf("error message should enumerate active provider %q; got %q", p, msg)
		}
	}
	if !strings.Contains(msg, "--from") {
		t.Errorf("error should suggest --from rerun; got %q", msg)
	}
}

// TestHandoffWrite_Interactive_PickerRoutesThroughFromFlag pins
// the TTY path: when the picker stub returns a provider, the
// write command must re-invoke itself with that provider as
// --from. We verify the wiring by capturing the candidate list
// the picker received AND asserting the eventual error names
// the picked provider in the picker-re-entry shape (which only
// surfaces on the picker recursion path).
func TestHandoffWrite_Interactive_PickerRoutesThroughFromFlag(t *testing.T) {
	dir := initRepoWithActiveStates(t, []struct{ sessionID, provider string }{
		{"claude-1", "claude-code"},
		{"gemini-1", "gemini-cli"},
	})

	// Force the interactive gate; the real check would fail under
	// bytes.Buffer stdio. The picker itself is stubbed below so
	// no huh form runs against the test process.
	origGate := isInteractiveCmdFn
	isInteractiveCmdFn = func(_ *cobra.Command) bool { return true }
	t.Cleanup(func() { isInteractiveCmdFn = origGate })

	var pickedCandidates []handoff.ActiveProvider
	origPicker := pickActiveProviderFn
	pickActiveProviderFn = func(_ *cobra.Command, providers []handoff.ActiveProvider) (string, error) {
		pickedCandidates = providers
		return "claude-code", nil
	}
	t.Cleanup(func() { pickActiveProviderFn = origPicker })

	_, err := runRoot(t, []string{"handoff", "--repo", dir, "--write"})

	// Picker received the dedup'd candidate list.
	if len(pickedCandidates) != 2 {
		t.Fatalf("picker should have received 2 candidates; got %d (%+v)",
			len(pickedCandidates), pickedCandidates)
	}
	gotProviders := map[string]bool{}
	for _, p := range pickedCandidates {
		gotProviders[p.Provider] = true
	}
	if !gotProviders["claude-code"] || !gotProviders["gemini-cli"] {
		t.Errorf("picker candidates should include claude-code and gemini-cli; got %+v", pickedCandidates)
	}

	// The recursion calls Write with From="claude-code". With no
	// lineage.db, the resolver returns ErrNoFromMatch, which the
	// picker-re-entry branch reshapes to "could not resolve
	// selected provider claude-code; ...". That shape is unique
	// to the picker path, so seeing it proves the recursion
	// happened with the picker's choice.
	if err == nil {
		t.Fatal("expected an error on recursion (no lineage row to pick); got nil")
	}
	if !strings.Contains(err.Error(), "selected provider claude-code") {
		t.Errorf("error should be the picker-re-entry shape naming the picked provider; got %v", err)
	}
}

// TestHandoffWrite_AutoCollapse_FailureOmitsDropFromHint verifies
// that auto-selected providers do not surface --from advice. The
// command should tell the user to retry after capture finishes.
//
// Setup: two claude-code capture states active, no lineage.db.
// Service routes through auto-collapse, hits the lineage-missing
// guard, returns ErrAutoSelectFailed; command layer must shape
// the message accordingly.
func TestHandoffWrite_AutoCollapse_FailureOmitsDropFromHint(t *testing.T) {
	dir := initRepoWithActiveStates(t, []struct{ sessionID, provider string }{
		{"claude-1", "claude-code"},
		{"claude-2", "claude-code"},
	})

	_, err := runRoot(t, []string{"handoff", "--repo", dir, "--write"})
	if err == nil {
		t.Fatal("expected an error when auto-collapse target has no lineage; got nil")
	}
	msg := err.Error()

	// The user did not type --from, so this advice would be wrong.
	if strings.Contains(msg, "drop --from") {
		t.Errorf("auto-collapse failure must not advise dropping --from "+
			"(user never typed it); got %q", msg)
	}
	if !strings.Contains(msg, "auto-selected provider") {
		t.Errorf("error should signal that the provider was auto-selected; got %q", msg)
	}
	if !strings.Contains(msg, "retry after capture finishes") {
		t.Errorf("error should suggest waiting for capture; got %q", msg)
	}
}

// TestHandoffWrite_Interactive_PickerReEntryErrorOmitsFromHint
// verifies that picker re-entry errors do not mention --from. The
// user picked from a prompt, so the message should name the picked
// provider and offer picker-specific next steps.
func TestHandoffWrite_Interactive_PickerReEntryErrorOmitsFromHint(t *testing.T) {
	dir := initRepoWithActiveStates(t, []struct{ sessionID, provider string }{
		{"claude-1", "claude-code"},
		{"gemini-1", "gemini-cli"},
	})

	origGate := isInteractiveCmdFn
	isInteractiveCmdFn = func(_ *cobra.Command) bool { return true }
	t.Cleanup(func() { isInteractiveCmdFn = origGate })

	origPicker := pickActiveProviderFn
	pickActiveProviderFn = func(_ *cobra.Command, _ []handoff.ActiveProvider) (string, error) {
		return "gemini-cli", nil
	}
	t.Cleanup(func() { pickActiveProviderFn = origPicker })

	_, err := runRoot(t, []string{"handoff", "--repo", dir, "--write"})
	if err == nil {
		t.Fatal("expected the picker-re-entry error; got nil")
	}
	msg := err.Error()

	// The user did not type --from, so this advice would be wrong.
	if strings.Contains(msg, "drop --from") {
		t.Errorf("picker-re-entry error must not advise dropping --from "+
			"(user never typed it); got %q", msg)
	}
	if !strings.Contains(msg, "selected provider gemini-cli") {
		t.Errorf("picker-re-entry error should name the picked provider; got %q", msg)
	}
	if !strings.Contains(msg, "choose another provider") &&
		!strings.Contains(msg, "retry after capture finishes") {
		t.Errorf("picker-re-entry error should suggest picker-specific next steps; got %q", msg)
	}
}

// TestConfirmContinueNow_PipeStdinIsNotInteractive pins the real
// stdin gate: an os.Pipe reader IS an *os.File so the type check
// passes, but isatty.IsTerminal returns false for it. Without the
// writer-gate stub this test would silently exit before reaching
// the isInteractiveTerminal call.
func TestConfirmContinueNow_PipeStdinIsNotInteractive(t *testing.T) {
	forceWriterGateTTY(t)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	// Preload an accept-looking payload so a gate-bypass would
	// produce a true result. The test passes only because the
	// pipe-not-a-TTY check kicks in first.
	if _, err := w.WriteString("y\n"); err != nil {
		t.Fatalf("write to pipe: %v", err)
	}

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetIn(r)

	if confirmContinueNow(cmd) {
		t.Error("pipe stdin must decline (not a TTY) regardless of contents")
	}
	if out.Len() != 0 {
		t.Errorf("must not emit prompt text when stdin gate fails; got %q", out.String())
	}
}
