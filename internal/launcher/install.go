package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/semanticash/cli/internal/platform"
)

// InstallResult describes a successful enable or reinstall.
type InstallResult struct {
	// PlistPath is the installed plist path.
	PlistPath string

	// DomainTarget is the bootstrapped launchctl target.
	DomainTarget string

	// Reinstalled reports whether a service was already loaded.
	Reinstalled bool
}

// Enable installs the worker plist, bootstraps it, and records the
// enabled state.
func Enable(ctx context.Context, binaryPath string) (*InstallResult, error) {
	if runtime.GOOS != "darwin" {
		return nil, ErrUnsupportedOS
	}
	if !isPOSIXAbsolute(binaryPath) {
		return nil, fmt.Errorf("launcher: binary path must be absolute, got %q", binaryPath)
	}
	if _, err := os.Stat(binaryPath); err != nil {
		return nil, fmt.Errorf("launcher: binary path not usable: %w", err)
	}

	plistPath, err := PlistPath()
	if err != nil {
		return nil, err
	}
	logPath, err := WorkerLogPath()
	if err != nil {
		return nil, err
	}

	// Create the log directory up front so install fails early if it
	// is not writable.
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

	// Treat launchd, not settings, as the source of truth.
	previouslyLoaded, err := bootoutIgnoringNotLoaded(ctx, DomainTarget())
	if err != nil {
		return nil, fmt.Errorf("launcher: bootout previous service: %w", err)
	}

	if err := writePlistAtomic(plistPath, []byte(plistBody)); err != nil {
		return nil, err
	}

	if err := Bootstrap(ctx, UserDomain(), plistPath); err != nil {
		// Leave the plist behind for inspection, but do not record an
		// enabled state.
		return nil, fmt.Errorf("launcher: bootstrap: %w", err)
	}

	settings := UserSettings{
		Launcher: LauncherSettings{
			Enabled:            true,
			InstalledPlistPath: plistPath,
			InstalledAt:        time.Now().UnixMilli(),
		},
	}
	if err := WriteSettings(settings); err != nil {
		return nil, fmt.Errorf("launcher: write settings: %w", err)
	}

	return &InstallResult{
		PlistPath:    plistPath,
		DomainTarget: DomainTarget(),
		Reinstalled:  previouslyLoaded,
	}, nil
}

// DisableResult describes a disable operation.
type DisableResult struct {
	// WasEnabled is the previous settings value.
	WasEnabled bool

	// RemovedPlistPath is the removed plist, if any.
	RemovedPlistPath string
}

// Disable unloads the worker, removes the plist, and clears the
// launcher settings. Missing services and plist files are treated as
// success.
func Disable(ctx context.Context) (*DisableResult, error) {
	if runtime.GOOS != "darwin" {
		return nil, ErrUnsupportedOS
	}

	settings, err := ReadSettings()
	if err != nil {
		// Fall back to empty settings so disable can still clean up the
		// launchd state and plist.
		settings = UserSettings{}
	}

	res := &DisableResult{WasEnabled: settings.Launcher.Enabled}

	// Always address the canonical launchd target.
	if _, err := bootoutIgnoringNotLoaded(ctx, DomainTarget()); err != nil {
		return nil, fmt.Errorf("launcher: bootout: %w", err)
	}

	// Prefer the recorded plist path, then fall back to the default.
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

	// Preserve unrelated top-level settings.
	settings.Launcher = LauncherSettings{}
	if err := WriteSettings(settings); err != nil {
		return nil, fmt.Errorf("launcher: clear settings: %w", err)
	}

	return res, nil
}

// bootoutIgnoringNotLoaded treats "service not found" as success
// and reports whether anything was unloaded.
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
