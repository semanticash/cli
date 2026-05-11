package commands

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

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
