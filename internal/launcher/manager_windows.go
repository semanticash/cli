//go:build windows

package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/platform"
)

// windowsManager backs the launcher with a Task Scheduler task in
// the per-user context.
type windowsManager struct{}

// newManager returns the windows Task Scheduler backend without
// probing schtasks.exe. Following the same precedent as the linux
// backend: probing in newManager would block Disable and Status in
// degraded environments where users still need to clean up. Each
// operation handles its own schtasks errors via its normal error
// channel.
func newManager() (manager, error) {
	return &windowsManager{}, nil
}

// Install renders the Task Scheduler XML, writes it under
// broker.GlobalBase, and registers it via `schtasks /Create /XML`.
// /F is passed so re-installs cleanly overwrite the previous task
// definition.
//
// UAC: the task runs as the current interactive user with
// LeastPrivilege. We deliberately do NOT use /RU SYSTEM or
// /RL HIGHEST - both trigger UAC prompts and pin the wrong
// security context for post-commit hooks, which need to run as
// the user who owns the repository.
func (m *windowsManager) Install(ctx context.Context, binaryPath string) (*InstallResult, error) {
	xmlPath, err := UnitPath()
	if err != nil {
		return nil, err
	}
	logPath, err := WorkerLogPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, fmt.Errorf("launcher: create log dir: %w", err)
	}

	globalBase, err := broker.GlobalBase()
	if err != nil {
		return nil, err
	}

	body, err := renderWorkerTask(taskInput{
		BinaryPath:       binaryPath,
		LogPath:          logPath,
		WorkingDirectory: resolveWorkingDirectory(binaryPath, globalBase),
	})
	if err != nil {
		return nil, err
	}

	// Detect "previously registered" before overwriting so the
	// Reinstalled flag is meaningful. Probe errors are folded into
	// "not previously registered" rather than failing install: a
	// transient schtasks issue should not block the rewrite.
	previouslyRegistered, _ := isTaskRegistered(ctx, UnitTarget())

	if err := writeTaskXMLAtomic(xmlPath, encodeUTF16LE(body)); err != nil {
		return nil, err
	}

	if err := createTaskFromXML(ctx, UnitTarget(), xmlPath); err != nil {
		return nil, fmt.Errorf("launcher: schtasks create: %w", err)
	}

	return &InstallResult{
		UnitPath:    xmlPath,
		UnitTarget:  UnitTarget(),
		Reinstalled: previouslyRegistered,
	}, nil
}

// Uninstall removes the registered task and deletes the XML file.
// Both schtasks Delete on a missing task and os.Remove on a missing
// file are treated as success so cleanup completes in degraded
// environments.
func (m *windowsManager) Uninstall(ctx context.Context) (*DisableResult, error) {
	settings, err := ReadSettings()
	if err != nil {
		// Fall back to empty settings so disable can still clean
		// up the task when the file is unreadable.
		settings = UserSettings{}
	}
	res := &DisableResult{WasEnabled: settings.Launcher.Enabled}

	// Delete the registered task. deleteTask folds "task not
	// found" into success; other schtasks failures are logged
	// through but do not fail the disable so XML cleanup still
	// runs.
	_ = deleteTask(ctx, UnitTarget())

	xmlPath := settings.Launcher.InstalledUnitPath
	if xmlPath == "" {
		p, err := UnitPath()
		if err != nil {
			return nil, fmt.Errorf("launcher: resolve unit path: %w", err)
		}
		xmlPath = p
	}
	if err := os.Remove(xmlPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("launcher: remove task XML: %w", err)
		}
	} else {
		res.RemovedUnitPath = xmlPath
	}

	return res, nil
}

// Kick triggers an on-demand run of the worker task.
func (m *windowsManager) Kick(ctx context.Context) error {
	return runTask(ctx, UnitTarget())
}

// IsRegistered reports whether Task Scheduler has the task
// registered. Ready, Running, and Disabled all map to true (the
// task is known to the scheduler); only a missing task returns
// false. This matches the semantic Status displays under
// LoadedInDaemon: a registered idle task is "loaded", not
// "not loaded". Using isUnitActive (the running-state probe) here
// would render drift hints on every status check between task
// runs - Task Scheduler tasks sit in Ready between executions,
// which is the steady state, not a problem.
func (m *windowsManager) IsRegistered(ctx context.Context) (bool, error) {
	return isTaskRegistered(ctx, UnitTarget())
}

// writeTaskXMLAtomic atomically writes the Task Scheduler XML file.
// schtasks /XML expects Unicode-encoded files; encodeUTF16LE has
// already converted the body before this is called.
func writeTaskXMLAtomic(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("launcher: create state dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return fmt.Errorf("launcher: write tmp xml: %w", err)
	}
	if err := platform.SafeRename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("launcher: rename xml: %w", err)
	}
	return nil
}
