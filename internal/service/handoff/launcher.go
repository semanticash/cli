package handoff

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
)

// ContinuePromptFor returns the starter prompt the launcher passes
// to the spawned agent for a given bundle path. The path is
// embedded verbatim so the spawned agent's cwd doesn't matter
// and the print-path command stays copy-pasteable from any
// terminal regardless of the user's current working directory.
// Callers always pass an absolute path; relative paths break the
// "run this from a fresh terminal" promise.
func ContinuePromptFor(bundlePath string) string {
	return fmt.Sprintf(
		"Read %s and continue from where the previous session left off.",
		bundlePath,
	)
}

// LaunchSpec describes how the continue command should hand off to
// the next agent. The command layer turns this into either an
// actual exec (when Spawn is true) or a printed instruction the
// user runs themselves (when Spawn is false).
type LaunchSpec struct {
	// Provider is the canonical name of the agent (matches the
	// capture-state and SKILL.md naming, e.g. "claude-code").
	Provider string

	// Binary is the executable name on PATH (e.g. "claude" for
	// claude-code). Empty when Spawn is false and the user must
	// run the agent manually.
	Binary string

	// Args are the positional + flag arguments to pass to Binary.
	// The starter prompt is included here in whatever shape the
	// target agent's CLI accepts.
	Args []string

	// Spawn reports whether this launch can be exec'd directly.
	// When false, callers print Message and exit; the user is
	// expected to invoke the agent themselves.
	Spawn bool

	// Message is the human-readable text the command layer prints
	// before either spawning (informational) or surrendering to
	// the manual flow (the explanation of why we can't spawn).
	Message string
}

// ErrBundleMissing indicates `.semantica/handoff.md` does not
// exist for the current repo. The continue command surfaces this
// by pointing the user at `semantica handoff --write` first.
var ErrBundleMissing = errors.New("handoff bundle not found")

// ErrUnknownProvider indicates --agent named a provider we don't
// have any launch knowledge for. Distinct from "no spawn for
// this provider" (which is the LaunchSpec.Spawn=false path):
// unknown provider means we don't even have a print-the-command
// fallback because we don't know which binary to suggest.
var ErrUnknownProvider = errors.New("unknown agent provider")

// providerLineRegex captures the bundle's "Original session: ID
// (<provider>)" line so the continue command can default --agent
// to the agent that produced the bundle.
var providerLineRegex = regexp.MustCompile(`Original session:\s+\S+\s+\(([a-z][a-z0-9-]*)\)`)

// ProviderFromBundle extracts the provider name from the
// "Original session" line emitted by renderBundle. Returns an
// empty string when the line is absent or malformed; callers
// surface that as "couldn't determine which agent; pass --agent."
func ProviderFromBundle(body []byte) string {
	m := providerLineRegex.FindSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return string(m[1])
}

// BuildLaunchSpec resolves a launch plan for the given provider.
// The returned spec carries enough information for the command
// layer to either exec the agent's binary (Spawn=true) or print a
// manual-launch hint (Spawn=false). printOnly forces the
// print-the-command branch even for providers we know how to
// spawn, which lets users copy
// the invocation rather than land in a new agent shell.
//
// bundlePath must be absolute. The starter prompt and any manual-
// launch hint embed it directly so the resulting commands are
// usable from any directory the user might paste them into.
func BuildLaunchSpec(provider, bundlePath string, printOnly bool) (*LaunchSpec, error) {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return nil, ErrUnknownProvider
	}

	switch provider {
	case "claude-code":
		// Claude Code accepts a positional initial prompt. The
		// REPL takes over after it answers, so exec is the right
		// hand-off mechanic on Unix; on Windows we fall back to
		// the print path (see below).
		return claudeCodeLaunch(bundlePath, printOnly), nil

	case "cursor", "copilot", "gemini-cli", "kiro-cli", "kiro-ide":
		// These agents either run as GUI editors or do not yet
		// have verified initial-prompt launch arguments. Return a
		// manual hint rather than guessing at flags that might open
		// the wrong UI or route the prompt incorrectly.
		return manualLaunch(provider, bundlePath), nil

	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, provider)
	}
}

// claudeCodeLaunch returns the spawn spec for Claude Code, or a
// print-only spec when printOnly is true / the binary is missing
// from PATH / the platform is one we haven't validated for exec.
func claudeCodeLaunch(bundlePath string, printOnly bool) *LaunchSpec {
	const bin = "claude"
	args := []string{ContinuePromptFor(bundlePath)}

	if printOnly || runtime.GOOS == "windows" || !binaryOnPath(bin) {
		return &LaunchSpec{
			Provider: "claude-code",
			Binary:   bin,
			Args:     args,
			Spawn:    false,
			Message:  formatManualMessage(bin, args),
		}
	}
	return &LaunchSpec{
		Provider: "claude-code",
		Binary:   bin,
		Args:     args,
		Spawn:    true,
		Message:  fmt.Sprintf("Launching %s with the handoff bundle...", bin),
	}
}

// manualLaunch describes the print-only path for providers we
// haven't verified spawn arguments for yet. The hint embeds the
// absolute bundle path so the user can copy-paste the file
// reference from any terminal.
func manualLaunch(provider, bundlePath string) *LaunchSpec {
	hint := fmt.Sprintf(
		"Auto-launch for %s is not wired yet. Open the agent yourself and ask it to read %s.",
		provider, bundlePath,
	)
	return &LaunchSpec{
		Provider: provider,
		Spawn:    false,
		Message:  hint,
	}
}

// binaryOnPath wraps exec.LookPath. Mocked in tests via the
// package-level lookPath variable.
func binaryOnPath(name string) bool {
	_, err := lookPath(name)
	return err == nil
}

// lookPath is the test seam for binaryOnPath. Production code
// uses exec.LookPath; tests swap in a stub that reports a
// controlled set of binaries as available.
var lookPath = exec.LookPath

// formatManualMessage builds the copy-pasteable "Run this in a
// fresh terminal" hint for the print-only path.
func formatManualMessage(bin string, args []string) string {
	parts := append([]string{bin}, quoteForShell(args)...)
	return fmt.Sprintf(
		"Run this in a fresh terminal to continue the handed-off session:\n\n    %s\n",
		strings.Join(parts, " "),
	)
}

// quoteForShell wraps each arg containing whitespace or shell-
// special characters in single quotes so a copy-pasted command
// runs the way the user expects. Single quotes are deliberate:
// they prevent every form of shell expansion the printed command
// might otherwise trigger when the path or prompt contains a
// dollar sign, backtick, command substitution, or backslash.
//
// Embedded single quotes are escaped using the standard POSIX
// pattern `'\”` (close the quote, write a literal quote with a
// backslash, reopen the quote) so the result remains a single
// shell token.
func quoteForShell(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if shellSafe(a) {
			out[i] = a
			continue
		}
		escaped := strings.ReplaceAll(a, `'`, `'\''`)
		out[i] = `'` + escaped + `'`
	}
	return out
}

func shellSafe(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == '/':
		default:
			return false
		}
	}
	return true
}
