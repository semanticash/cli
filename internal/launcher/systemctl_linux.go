//go:build linux

package launcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// systemctlError is a non-zero exit from `systemctl --user`. The
// shape mirrors launchctl's *Error so the two backends report
// failures the same way through the audit/log surfaces.
type systemctlError struct {
	// Subcommand is the failed systemctl subcommand
	// (daemon-reload, start, stop, is-active, show-environment).
	Subcommand string

	// Args is the full argv passed after `systemctl`.
	Args []string

	// ExitCode is systemctl's exit code.
	ExitCode int

	// Stderr is trimmed stderr.
	Stderr string
}

// Error renders the systemctl failure.
func (e *systemctlError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("systemctl --user %s: exit %d", e.Subcommand, e.ExitCode)
	}
	return fmt.Sprintf("systemctl --user %s: exit %d: %s", e.Subcommand, e.ExitCode, e.Stderr)
}

// runSystemctl invokes `systemctl --user <subcommand> <args...>`
// and wraps non-zero exits as *systemctlError.
func runSystemctl(ctx context.Context, subcommand string, args ...string) error {
	fullArgs := append([]string{"--user", subcommand}, args...)
	cmd := exec.CommandContext(ctx, "systemctl", fullArgs...)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return &systemctlError{
			Subcommand: subcommand,
			Args:       fullArgs,
			ExitCode:   exitErr.ExitCode(),
			Stderr:     strings.TrimRight(stderr.String(), "\r\n\t "),
		}
	}
	return fmt.Errorf("systemctl --user %s: %w", subcommand, err)
}

// daemonReload tells the user-instance systemd to re-read its unit
// files. Required after writing or deleting a unit definition.
func daemonReload(ctx context.Context) error {
	return runSystemctl(ctx, "daemon-reload")
}

// startUnit triggers an on-demand activation of the unit. Uses
// --no-block so the call returns immediately, matching launchd
// kickstart's fire-and-forget semantics for Type=oneshot units.
func startUnit(ctx context.Context, unit string) error {
	return runSystemctl(ctx, "start", "--no-block", unit)
}

// stopUnit stops a unit. Best-effort at the call site - stopping
// an inactive Type=oneshot unit is allowed by systemd and exits 0.
func stopUnit(ctx context.Context, unit string) error {
	return runSystemctl(ctx, "stop", unit)
}

// isUnitActive reports whether the unit is currently active.
// systemctl --user is-active exits 0 when active and 3 when not.
// Both flatten to (bool, nil); other exit codes propagate.
func isUnitActive(ctx context.Context, unit string) (bool, error) {
	err := runSystemctl(ctx, "is-active", "--quiet", unit)
	if err == nil {
		return true, nil
	}
	var se *systemctlError
	if errors.As(err, &se) && se.ExitCode == 3 {
		return false, nil
	}
	return false, err
}

// userManagerReachable checks whether `systemctl --user` can talk
// to the user's systemd instance. `show-environment` is a better
// probe than `is-system-running` here because it succeeds on
// usable managers even when the overall state is `degraded` or
// `starting`.
func userManagerReachable(ctx context.Context) error {
	return runSystemctl(ctx, "show-environment")
}

// Kickstart triggers an on-demand activation of the worker unit.
// The target argument is the systemd unit name; honors the
// caller-supplied target rather than deriving its own so callers
// retain control over which unit is started.
func Kickstart(ctx context.Context, target string) error {
	return startUnit(ctx, target)
}
