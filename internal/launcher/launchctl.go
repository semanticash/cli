package launcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// ErrUnsupportedOS is returned by every launchctl wrapper function
// when the host is not macOS. Callers that compose the wrapper into
// user-facing commands should check for this first and surface a
// readable message ("the launcher is only available on macOS")
// rather than propagating the raw error.
var ErrUnsupportedOS = errors.New("launchctl wrapper: unsupported OS (launchd is macOS-only)")

// Error is the typed failure returned by the launchctl wrappers
// when launchctl itself exits non-zero. It carries enough detail
// for higher-level commands to classify specific failure modes
// (for example, bootstrap returning exit 5 when the label is
// already loaded, or bootout returning non-zero when the label
// is not currently loaded) without having to parse the error
// string at every call site.
type Error struct {
	// Subcommand is the launchctl subcommand that failed, for
	// example "bootstrap", "bootout", "kickstart", or "print".
	Subcommand string

	// Args is the full argv passed to launchctl, useful in logs
	// and error messages for reproducing the call by hand.
	Args []string

	// ExitCode is launchctl's exit code. The most actionable
	// values for the semantica launcher are 5 (input/output
	// error, typically "already loaded" from bootstrap) and
	// non-zero codes from bootout when a label is not loaded.
	ExitCode int

	// Stderr is the bytes launchctl wrote to its stderr, trimmed
	// at the call site. Included in the Error string.
	Stderr string
}

// Error renders the failure in a form suitable for worker log
// output and human-readable CLI messages.
func (e *Error) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("launchctl %s: exit %d", e.Subcommand, e.ExitCode)
	}
	return fmt.Sprintf("launchctl %s: exit %d: %s", e.Subcommand, e.ExitCode, e.Stderr)
}

// Bootstrap registers a launchd service from a plist file into the
// user's GUI bootstrap context (gui/$UID). The plist must already
// exist on disk at plistPath; launchd does not watch for later
// changes. If the label inside the plist is already bootstrapped,
// launchd returns exit code 5 with a "Bootstrap failed: 5:
// Input/output error" message; callers that want to treat that as
// a recoverable state should do so by inspecting the returned
// *Error's ExitCode rather than string-matching.
func Bootstrap(ctx context.Context, domain, plistPath string) error {
	if runtime.GOOS != "darwin" {
		return ErrUnsupportedOS
	}
	return run(ctx, "bootstrap", domain, plistPath)
}

// Bootout unloads a launchd service from a bootstrap domain. The
// domain is usually "gui/$UID" followed by the service label,
// for example "gui/501/sh.semantica.worker". Returns a *Error if
// the service is not currently loaded; callers that want idempotent
// disable should ignore that specific failure.
func Bootout(ctx context.Context, domainTarget string) error {
	if runtime.GOOS != "darwin" {
		return ErrUnsupportedOS
	}
	return run(ctx, "bootout", domainTarget)
}

// Kickstart instructs launchd to run the specified service
// immediately. domainTarget is the same shape as Bootout's
// argument: "gui/$UID/<label>". This wrapper does not pass the
// -k flag; a kickstart against a currently-running service is
// therefore a no-op, and the caller's queue-drain loop is
// expected to absorb markers that arrive while the worker is
// running.
func Kickstart(ctx context.Context, domainTarget string) error {
	if runtime.GOOS != "darwin" {
		return ErrUnsupportedOS
	}
	return run(ctx, "kickstart", domainTarget)
}

// IsLoaded reports whether a service label is currently
// bootstrapped in the given domain. Implemented on top of
// "launchctl print", which exits zero when the service exists
// and non-zero with a known "Could not find service" message when
// it does not.
//
// The implementation only flattens the specific not-loaded verdict
// into (false, nil). Other launchctl print failures (malformed
// domain target, permission-denied on a protected domain, generic
// launchctl internal errors) return (false, err) so the caller can
// surface them instead of silently treating them as "no service."
func IsLoaded(ctx context.Context, domainTarget string) (bool, error) {
	if runtime.GOOS != "darwin" {
		return false, ErrUnsupportedOS
	}
	err := run(ctx, "print", domainTarget)
	if err == nil {
		return true, nil
	}
	var le *Error
	if errors.As(err, &le) && isServiceNotLoadedError(le) {
		return false, nil
	}
	return false, err
}

// isServiceNotLoadedError returns true when err matches the
// launchctl-documented "service not found" response to a print
// call. Detection uses a stderr substring match because launchctl's
// exit code for this case has drifted across macOS versions (seen
// 113, 37, and 3 in different releases) while the human-readable
// message has been stable since OS X 10.10. Matching is
// case-insensitive and covers the two wordings Apple has shipped.
func isServiceNotLoadedError(err *Error) bool {
	msg := strings.ToLower(err.Stderr)
	return strings.Contains(msg, "could not find service") ||
		strings.Contains(msg, "service not found")
}

// run invokes launchctl with the given subcommand and args, wraps
// any non-zero exit as *Error, and wraps any exec-level failure
// (binary not found, I/O error) as a plain error. Kept unexported
// so the small set of exported wrappers remains the only public
// surface.
func run(ctx context.Context, subcommand string, args ...string) error {
	fullArgs := append([]string{subcommand}, args...)
	cmd := exec.CommandContext(ctx, "launchctl", fullArgs...)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return &Error{
			Subcommand: subcommand,
			Args:       fullArgs,
			ExitCode:   exitErr.ExitCode(),
			Stderr:     bytesTrimmed(stderr.Bytes()),
		}
	}
	return fmt.Errorf("launchctl %s: %w", subcommand, err)
}

// bytesTrimmed returns the string form of b with trailing newlines
// and spaces removed. Factored out so the Error struct's Stderr
// field is always one-line clean for log output.
func bytesTrimmed(b []byte) string {
	return string(bytes.TrimRight(b, "\r\n\t "))
}
