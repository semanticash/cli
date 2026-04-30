//go:build windows

package launcher

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// schtasksError is a non-zero exit from schtasks.exe. The shape
// mirrors launchctl's *Error and systemctl's *systemctlError so the
// three backends report failures the same way through the audit/log
// surfaces.
type schtasksError struct {
	// Subcommand is the schtasks operation (Create, Delete, Run,
	// Query).
	Subcommand string

	// Args is the full argv passed after `schtasks`.
	Args []string

	// ExitCode is schtasks.exe's exit code.
	ExitCode int

	// Stderr is trimmed stderr.
	Stderr string
}

// Error renders the schtasks failure.
func (e *schtasksError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("schtasks %s: exit %d", e.Subcommand, e.ExitCode)
	}
	return fmt.Sprintf("schtasks %s: exit %d: %s", e.Subcommand, e.ExitCode, e.Stderr)
}

// runSchtasks invokes `schtasks.exe <subcommand> <args...>` and
// wraps non-zero exits as *schtasksError. Subcommand is passed as
// `/<Subcommand>` (e.g. /Create, /Run); the slash prefix is added
// here so callers do not have to spell it.
func runSchtasks(ctx context.Context, subcommand string, args ...string) (string, error) {
	fullArgs := append([]string{"/" + subcommand}, args...)
	cmd := exec.CommandContext(ctx, "schtasks", fullArgs...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return stdout.String(), nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stdout.String(), &schtasksError{
			Subcommand: subcommand,
			Args:       fullArgs,
			ExitCode:   exitErr.ExitCode(),
			Stderr:     strings.TrimRight(stderr.String(), "\r\n\t "),
		}
	}
	return stdout.String(), fmt.Errorf("schtasks %s: %w", subcommand, err)
}

// createTaskFromXML registers a task using a pre-built XML file.
// /F overwrites any existing task with the same name, matching
// launchctl bootstrap's "rebootstrap" semantics.
func createTaskFromXML(ctx context.Context, taskName, xmlPath string) error {
	_, err := runSchtasks(ctx, "Create", "/TN", taskName, "/XML", xmlPath, "/F")
	return err
}

// deleteTask unregisters a task. Treat "task not found" as success
// to keep Disable idempotent.
func deleteTask(ctx context.Context, taskName string) error {
	_, err := runSchtasks(ctx, "Delete", "/TN", taskName, "/F")
	if err == nil {
		return nil
	}
	var se *schtasksError
	if errors.As(err, &se) && isTaskNotFoundError(se) {
		return nil
	}
	return err
}

// runTask triggers the task on demand. schtasks /Run returns
// promptly; the task continues in the background under Task
// Scheduler.
func runTask(ctx context.Context, taskName string) error {
	_, err := runSchtasks(ctx, "Run", "/TN", taskName)
	return err
}

// taskState reports whether the task is registered and, if so, its
// runtime state. Returns:
//
//   - ("Running", nil)  : the task is currently executing.
//   - ("Ready", nil)    : the task is registered but not running.
//   - ("Disabled", nil) : the task is registered but disabled.
//   - ("", nil)         : the task is not registered.
//   - ("", err)         : schtasks failed for some other reason.
//
// Output is parsed from /FO CSV so the column order is stable
// across schtasks versions.
func taskState(ctx context.Context, taskName string) (string, error) {
	out, err := runSchtasks(ctx, "Query", "/TN", taskName, "/FO", "CSV", "/NH")
	if err != nil {
		var se *schtasksError
		if errors.As(err, &se) && isTaskNotFoundError(se) {
			return "", nil
		}
		return "", err
	}
	return parseTaskQueryStatus(out)
}

// isUnitActive reports whether the task is currently running.
// Mirrors the launchctl/systemctl IsActive contract: true when
// running, false otherwise; non-existence collapses to (false, nil).
func isUnitActive(ctx context.Context, taskName string) (bool, error) {
	state, err := taskState(ctx, taskName)
	if err != nil {
		return false, err
	}
	return state == "Running", nil
}

// isTaskRegistered reports whether the task is registered at all
// (Running, Ready, or Disabled). Empty state means not registered.
func isTaskRegistered(ctx context.Context, taskName string) (bool, error) {
	state, err := taskState(ctx, taskName)
	if err != nil {
		return false, err
	}
	return state != "", nil
}

// isTaskNotFoundError matches schtasks' stable not-found wording.
// Exit codes vary across Windows releases, so detection uses
// stderr instead.
//
// The matcher is deliberately narrow: a generic `exit 1 && ERROR:`
// fallback would catch real failures (permission denied, scheduler
// service stopped, malformed query) and silently classify them as
// "task not found", which would make Status report "not loaded"
// for a host that's actually broken and let Disable swallow real
// delete failures. If a locale-translated not-found message slips
// past this list, the operation surfaces as a real schtasks error
// with the original stderr - preferable to misclassification.
func isTaskNotFoundError(err *schtasksError) bool {
	msg := strings.ToLower(err.Stderr)
	return strings.Contains(msg, "the system cannot find the file specified") ||
		strings.Contains(msg, "cannot find the file") ||
		(strings.Contains(msg, "task name") && strings.Contains(msg, "does not exist")) ||
		strings.Contains(msg, "the specified task name")
}

// parseTaskQueryStatus extracts the Status column from a
// `schtasks /Query /FO CSV /NH` output. /NH suppresses the header
// row, so the first record is the data row directly. The Status
// column is at index 2 (TaskName, Next Run Time, Status).
func parseTaskQueryStatus(out string) (string, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return "", nil
	}
	r := csv.NewReader(strings.NewReader(out))
	// Allow variable-length records - locale-translated outputs
	// occasionally pad columns differently.
	r.FieldsPerRecord = -1
	rec, err := r.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		return "", fmt.Errorf("parse schtasks query: %w", err)
	}
	if len(rec) < 3 {
		return "", fmt.Errorf("schtasks query output has %d columns, expected at least 3: %q", len(rec), out)
	}
	return strings.TrimSpace(rec[2]), nil
}

// Kickstart triggers an on-demand activation of the worker task.
// The target argument is the Task Scheduler task name; honors the
// caller-supplied target so callers retain control over which task
// is started.
func Kickstart(ctx context.Context, target string) error {
	return runTask(ctx, target)
}
