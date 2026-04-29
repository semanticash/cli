//go:build linux

package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/semanticash/cli/internal/platform"
)

// linuxManager backs the launcher with a systemd user unit.
type linuxManager struct{}

// newManager returns the linux backend without probing the user
// manager. Install does that check explicitly; Disable and Status
// stay available even in degraded environments so users can still
// inspect or clean up launcher state.
func newManager() (manager, error) {
	return &linuxManager{}, nil
}

// Install renders the systemd user unit, writes it under
// $XDG_CONFIG_HOME/systemd/user/, and tells the user manager to
// pick it up via daemon-reload. The unit is on-demand only - no
// `enable` to autostart at boot - because the post-commit hook is
// the trigger and the unit's job is to be kickable.
//
// Linger (`loginctl enable-linger`) is intentionally out of scope.
// It usually requires sudo, and the launcher only needs the user
// manager while the user is logged in.
//
// Install probes systemd user reachability up front because a
// daemon-reload failure surfaced from deeper in the flow would
// be opaque ("daemon-reload: exit 1: ..."). The pre-flight gives
// users a clear "systemd user instance not reachable" message
// pointing at the actual environment problem.
func (m *linuxManager) Install(ctx context.Context, binaryPath string) (*InstallResult, error) {
	if err := userManagerReachable(ctx); err != nil {
		return nil, fmt.Errorf("launcher: systemd user instance not reachable; ensure XDG_RUNTIME_DIR is set and `systemctl --user` works: %w", err)
	}

	unitPath, err := UnitPath()
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

	body, err := renderWorkerUnit(unitInput{
		BinaryPath: binaryPath,
		LogPath:    logPath,
	})
	if err != nil {
		return nil, err
	}

	// Detect a previously-registered unit before the rewrite so
	// the Reinstalled flag is meaningful. Uses isUnitRegistered
	// (LoadState=loaded), NOT isUnitActive: Type=oneshot units
	// return to inactive between kicks, so an is-active probe
	// would read false for a fully-registered idle unit and every
	// subsequent `semantica launcher enable` would falsely report
	// a fresh install. Probe errors are treated as "not previously
	// registered" rather than blocking the rewrite - a transient
	// systemctl issue should not stop the install path.
	previouslyRegistered, _ := isUnitRegistered(ctx, UnitTarget())

	if err := writeUnitAtomic(unitPath, []byte(body)); err != nil {
		return nil, err
	}

	if err := daemonReload(ctx); err != nil {
		return nil, fmt.Errorf("launcher: daemon-reload: %w", err)
	}

	return &InstallResult{
		UnitPath:    unitPath,
		UnitTarget:  UnitTarget(),
		Reinstalled: previouslyRegistered,
	}, nil
}

// Uninstall stops the unit (best-effort), removes the unit file,
// and reloads the user manager. Stop and reload errors do not fail
// the call because removing the file is the important user-visible
// state; a stale in-memory entry can be cleaned up later.
func (m *linuxManager) Uninstall(ctx context.Context) (*DisableResult, error) {
	settings, err := ReadSettings()
	if err != nil {
		// Fall back to empty settings so disable can still clean
		// up the unit when the file is unreadable.
		settings = UserSettings{}
	}
	res := &DisableResult{WasEnabled: settings.Launcher.Enabled}

	// Stop is best-effort: the unit may not be running, may not
	// exist, or systemctl may be transiently unavailable.
	_ = stopUnit(ctx, UnitTarget())

	unitPath := settings.Launcher.InstalledUnitPath
	if unitPath == "" {
		p, err := UnitPath()
		if err != nil {
			return nil, fmt.Errorf("launcher: resolve unit path: %w", err)
		}
		unitPath = p
	}
	if err := os.Remove(unitPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("launcher: remove unit: %w", err)
		}
	} else {
		res.RemovedUnitPath = unitPath
	}

	// Reload after removal so systemd forgets the unit. A failure
	// here is logged through the wrapped error class but does not
	// fail the disable: the file is gone, which is the only
	// user-visible state we need to land.
	_ = daemonReload(ctx)

	return res, nil
}

// Kick triggers an on-demand run of the worker unit.
func (m *linuxManager) Kick(ctx context.Context) error {
	return startUnit(ctx, UnitTarget())
}

// IsRegistered reports whether the systemd user instance has the
// worker unit loaded. Uses isUnitRegistered (LoadState=loaded)
// rather than isUnitActive: Type=oneshot units return to
// "inactive" between kicks, so is-active would report false for
// a perfectly registered idle unit and Status would render drift
// hints on every status check between worker runs.
func (m *linuxManager) IsRegistered(ctx context.Context) (bool, error) {
	return isUnitRegistered(ctx, UnitTarget())
}

// writeUnitAtomic atomically writes the systemd unit file.
func writeUnitAtomic(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("launcher: create systemd user dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return fmt.Errorf("launcher: write tmp unit: %w", err)
	}
	if err := platform.SafeRename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("launcher: rename unit: %w", err)
	}
	return nil
}
