//go:build darwin

package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/semanticash/cli/internal/platform"
)

// darwinManager backs the launcher with a launchd LaunchAgent.
type darwinManager struct{}

// newManager returns the darwin launchd backend.
func newManager() (manager, error) {
	return &darwinManager{}, nil
}

// Install renders the worker plist, writes it under
// ~/Library/LaunchAgents, and bootstraps it into the user's launchd
// domain. Existing services are bootouted first so install is safe
// to re-run.
func (m *darwinManager) Install(ctx context.Context, binaryPath string) (*InstallResult, error) {
	plistPath, err := PlistPath()
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

	plistBody, err := RenderWorkerPlist(PlistInput{
		BinaryPath: binaryPath,
		LogPath:    logPath,
	})
	if err != nil {
		return nil, err
	}

	previouslyLoaded, err := bootoutIgnoringNotLoaded(ctx, DomainTarget())
	if err != nil {
		return nil, fmt.Errorf("launcher: bootout previous service: %w", err)
	}

	if err := writePlistAtomic(plistPath, []byte(plistBody)); err != nil {
		return nil, err
	}

	if err := Bootstrap(ctx, UserDomain(), plistPath); err != nil {
		return nil, fmt.Errorf("launcher: bootstrap: %w", err)
	}

	return &InstallResult{
		PlistPath:    plistPath,
		DomainTarget: DomainTarget(),
		Reinstalled:  previouslyLoaded,
	}, nil
}

// Uninstall bootouts the service and removes the plist file. Both
// missing services and missing files are treated as success.
func (m *darwinManager) Uninstall(ctx context.Context) (*DisableResult, error) {
	settings, err := ReadSettings()
	if err != nil {
		// Fall back to empty settings so disable can still clean up
		// launchd state when the file is unreadable.
		settings = UserSettings{}
	}

	res := &DisableResult{WasEnabled: settings.Launcher.Enabled}

	if _, err := bootoutIgnoringNotLoaded(ctx, DomainTarget()); err != nil {
		return nil, fmt.Errorf("launcher: bootout: %w", err)
	}

	plistPath := settings.Launcher.InstalledPlistPath
	if plistPath == "" {
		p, err := PlistPath()
		if err != nil {
			return nil, fmt.Errorf("launcher: resolve plist path: %w", err)
		}
		plistPath = p
	}
	if err := os.Remove(plistPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("launcher: remove plist: %w", err)
		}
	} else {
		res.RemovedPlistPath = plistPath
	}

	return res, nil
}

// Kick triggers a launchctl kickstart against the worker target.
func (m *darwinManager) Kick(ctx context.Context) error {
	return run(ctx, "kickstart", DomainTarget())
}

// IsActive reports whether the launchd service is loaded.
func (m *darwinManager) IsActive(ctx context.Context) (bool, error) {
	return IsLoaded(ctx, DomainTarget())
}

// UnitPath returns the installed plist path.
func (m *darwinManager) UnitPath() (string, error) {
	return PlistPath()
}

// UnitTarget returns the launchctl gui/<uid>/<label> tuple.
func (m *darwinManager) UnitTarget() string {
	return DomainTarget()
}

// bootoutIgnoringNotLoaded treats the known "not loaded" result as
// success and reports whether anything was unloaded.
func bootoutIgnoringNotLoaded(ctx context.Context, target string) (bool, error) {
	err := Bootout(ctx, target)
	if err == nil {
		return true, nil
	}
	var le *Error
	if errors.As(err, &le) && isServiceNotLoadedError(le) {
		return false, nil
	}
	return false, err
}

// writePlistAtomic atomically writes the plist file.
func writePlistAtomic(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("launcher: create LaunchAgents dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return fmt.Errorf("launcher: write tmp plist: %w", err)
	}
	if err := platform.SafeRename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("launcher: rename plist: %w", err)
	}
	return nil
}
