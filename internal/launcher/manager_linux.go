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

// Install writes the systemd user service and timer, reloads the user
// manager, and enables the timer. Commits start the service on demand;
// the timer provides periodic retry and crash recovery.
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

	// A oneshot service is normally inactive between runs, so
	// registration is determined from LoadState rather than activity.
	// Probe failures do not prevent the service from being repaired.
	previouslyRegistered, _ := isUnitRegistered(ctx, UnitTarget())

	if err := writeUnitAtomic(unitPath, []byte(body)); err != nil {
		return nil, err
	}
	timerPath, err := TimerPath()
	if err != nil {
		return nil, err
	}
	if err := writeUnitAtomic(timerPath, []byte(renderWorkerTimer())); err != nil {
		return nil, err
	}

	if err := daemonReload(ctx); err != nil {
		return nil, fmt.Errorf("launcher: daemon-reload: %w", err)
	}
	if err := enableNowUnit(ctx, TimerTarget()); err != nil {
		return nil, fmt.Errorf("launcher: enable drain timer: %w", err)
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

	// Service stop is best-effort. Timer cleanup failures are reported
	// because an enabled timer would continue starting drains.
	_ = stopUnit(ctx, UnitTarget())
	if err := disableNowUnit(ctx, TimerTarget()); err != nil {
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("drain timer %s could not be disabled: %v; run `systemctl --user disable --now %s`",
				TimerTarget(), err, TimerTarget()))
	}
	if timerPath, err := TimerPath(); err == nil {
		if rmErr := os.Remove(timerPath); rmErr != nil && !os.IsNotExist(rmErr) {
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("drain timer unit file was not removed: %v", rmErr))
		}
	}

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

	// Reload after removal so systemd forgets the units. A failure
	// does not fail the disable — the files are gone — but it is
	// reported: systemd may keep stale in-memory entries until the
	// next reload.
	if err := daemonReload(ctx); err != nil {
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("daemon-reload failed after unit removal: %v; run `systemctl --user daemon-reload`", err))
	}

	return res, nil
}

// Kick triggers an on-demand run of the worker unit.
func (m *linuxManager) Kick(ctx context.Context) error {
	return startUnit(ctx, UnitTarget())
}

// IsRegistered reports whether the service is loaded and its timer is
// persistently enabled and active. The oneshot service may be inactive
// between runs; the timer must remain active while waiting.
func (m *linuxManager) IsRegistered(ctx context.Context) (bool, error) {
	service, err := isUnitRegistered(ctx, UnitTarget())
	if err != nil || !service {
		return false, err
	}
	enabled, err := isUnitEnabled(ctx, TimerTarget())
	if err != nil || !enabled {
		return false, err
	}
	return isUnitActive(ctx, TimerTarget())
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
