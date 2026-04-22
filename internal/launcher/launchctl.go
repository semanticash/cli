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

// ErrUnsupportedOS reports that launchctl is unavailable on this OS.
var ErrUnsupportedOS = errors.New("launchctl wrapper: unsupported OS (launchd is macOS-only)")

// Error is a non-zero launchctl exit.
type Error struct {
	// Subcommand is the failed launchctl subcommand.
	Subcommand string

	// Args is the full argv passed to launchctl.
	Args []string

	// ExitCode is launchctl's exit code.
	ExitCode int

	// Stderr is trimmed stderr.
	Stderr string
}

// Error renders the launchctl failure.
func (e *Error) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("launchctl %s: exit %d", e.Subcommand, e.ExitCode)
	}
	return fmt.Sprintf("launchctl %s: exit %d: %s", e.Subcommand, e.ExitCode, e.Stderr)
}

// Bootstrap loads a plist into the user's launchd domain.
func Bootstrap(ctx context.Context, domain, plistPath string) error {
	if runtime.GOOS != "darwin" {
		return ErrUnsupportedOS
	}
	return run(ctx, "bootstrap", domain, plistPath)
}

// Bootout unloads a service from a launchd domain.
func Bootout(ctx context.Context, domainTarget string) error {
	if runtime.GOOS != "darwin" {
		return ErrUnsupportedOS
	}
	return run(ctx, "bootout", domainTarget)
}

// Kickstart asks launchd to run the service. It never passes -k.
func Kickstart(ctx context.Context, domainTarget string) error {
	if runtime.GOOS != "darwin" {
		return ErrUnsupportedOS
	}
	return run(ctx, "kickstart", domainTarget)
}

// IsLoaded reports whether a service is present in the given domain.
// Only the known "not loaded" result is flattened to (false, nil).
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

// isServiceNotLoadedError matches launchctl's stable "not loaded"
// wording. Exit codes vary across macOS releases, so detection uses
// stderr instead.
func isServiceNotLoadedError(err *Error) bool {
	msg := strings.ToLower(err.Stderr)
	return strings.Contains(msg, "could not find service") ||
		strings.Contains(msg, "service not found") ||
		strings.Contains(msg, "no such process")
}

// run invokes launchctl and wraps non-zero exits as *Error.
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

// bytesTrimmed removes trailing whitespace.
func bytesTrimmed(b []byte) string {
	return string(bytes.TrimRight(b, "\r\n\t "))
}
