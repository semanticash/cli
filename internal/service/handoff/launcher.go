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

// providerLaunch is the per-agent recipe for turning a bundle
// path into a launch command. Each entry encodes the binary name
// on PATH and how that agent's CLI accepts a starter prompt that
// lands the user in an interactive REPL afterward. Adding a new
// agent is a one-line table change; the spawn/print-fallback
// plumbing in spawnLaunch is shared.
type providerLaunch struct {
	// binary is the executable name looked up via exec.LookPath.
	binary string

	// argsFor builds the argv (excluding binary) for a bundle
	// path. Keep the starter prompt the last element so manual-
	// launch hints render the human-readable text at the tail.
	argsFor func(bundlePath string) []string
}

// providerLaunches lists every agent we know how to spawn into
// REPL-with-seed mode. kiro-ide is intentionally absent because
// it is the IDE surface, not a CLI. No equivalent spawn path
// exists, so kiro-ide bundles fall through to manualLaunch.
var providerLaunches = map[string]providerLaunch{
	// Claude Code: positional starter prompt. The REPL takes over
	// after the first answer.
	"claude-code": {
		binary:  "claude",
		argsFor: func(p string) []string { return []string{ContinuePromptFor(p)} },
	},
	// Cursor: `cursor-agent "<prompt>"` is positional and stays interactive.
	// `-p` / `--print` is the *non-interactive* mode and would be
	// wrong here.
	"cursor": {
		binary:  "cursor-agent",
		argsFor: func(p string) []string { return []string{ContinuePromptFor(p)} },
	},
	// Gemini CLI: `gemini -i "<prompt>"` is the REPL-with-seed
	// flag (`--prompt-interactive`). `-p` exits after answering.
	"gemini-cli": {
		binary:  "gemini",
		argsFor: func(p string) []string { return []string{"-i", ContinuePromptFor(p)} },
	},
	// GitHub Copilot CLI: `copilot --prompt "<prompt>"` is the
	// documented starter-prompt flag; the REPL stays after the
	// answer. Note: --prompt is a flag, not a positional.
	"copilot": {
		binary:  "copilot",
		argsFor: func(p string) []string { return []string{"--prompt", ContinuePromptFor(p)} },
	},
	// Kiro CLI: `kiro-cli chat "<prompt>"` uses the chat subcommand with
	// positional prompt. Omitting --no-interactive keeps the REPL.
	"kiro-cli": {
		binary:  "kiro-cli",
		argsFor: func(p string) []string { return []string{"chat", ContinuePromptFor(p)} },
	},
}

// BuildLaunchSpec resolves a launch plan for the given provider.
// The returned spec carries enough information for the command
// layer to either exec the agent's binary (Spawn=true) or print a
// manual-launch hint (Spawn=false). printOnly forces the
// print-the-command branch even for providers we know how to
// spawn, which lets users copy the invocation rather than land in
// a new agent shell.
//
// bundlePath must be absolute. The starter prompt and any manual-
// launch hint embed it directly so the resulting commands are
// usable from any directory the user might paste them into.
func BuildLaunchSpec(provider, bundlePath string, printOnly bool) (*LaunchSpec, error) {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return nil, ErrUnknownProvider
	}

	if cfg, ok := providerLaunches[provider]; ok {
		return spawnLaunch(provider, cfg.binary, cfg.argsFor(bundlePath), printOnly), nil
	}

	// kiro-ide is the IDE, not a CLI: no spawn path exists. We
	// still need to handle the provider so the bundle resolves to
	// a useful manual hint instead of "unknown provider".
	if provider == "kiro-ide" {
		return manualLaunch(provider, bundlePath), nil
	}

	return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, provider)
}

// spawnLaunch is the shared spawn-or-print plumbing every entry
// in providerLaunches routes through. Falls back to the print
// path when --print is set, the platform is Windows (Unix-only
// exec mechanic), or the agent's binary is not installed.
func spawnLaunch(provider, bin string, args []string, printOnly bool) *LaunchSpec {
	if printOnly || runtime.GOOS == "windows" || !binaryOnPath(bin) {
		return &LaunchSpec{
			Provider: provider,
			Binary:   bin,
			Args:     args,
			Spawn:    false,
			Message:  formatManualMessage(bin, args),
		}
	}
	return &LaunchSpec{
		Provider: provider,
		Binary:   bin,
		Args:     args,
		Spawn:    true,
		Message:  fmt.Sprintf("Launching %s with the handoff bundle...", bin),
	}
}

// manualLaunch is the print-only path for providers that have no
// CLI we can spawn (currently just kiro-ide). The hint embeds the
// absolute bundle path so the user can copy-paste the file
// reference from any terminal.
func manualLaunch(provider, bundlePath string) *LaunchSpec {
	hint := fmt.Sprintf(
		"Auto-launch for %s is not wired (no CLI for this surface). "+
			"Open the agent yourself and ask it to read %s.",
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
// Embedded single quotes are escaped by closing the quote, writing
// an escaped literal quote, and reopening the quote, so the result
// remains a single shell token.
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
