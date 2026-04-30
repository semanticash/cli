package launcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Enable installs the worker, registers it with the OS daemon
// manager, and records the enabled state in user settings. The
// OS-specific work runs through the active manager backend; on
// platforms without one, this returns ErrUnsupportedOS.
//
// The backend check runs before binary-path validation so the
// "unsupported OS" contract holds for callers that use
// errors.Is(err, ErrUnsupportedOS): a non-darwin host returns
// ErrUnsupportedOS regardless of whether the supplied path is
// absolute or exists.
func Enable(ctx context.Context, binaryPath string) (*InstallResult, error) {
	m, err := newManager()
	if err != nil {
		return nil, err
	}

	// filepath.IsAbs is GOOS-aware: it accepts /... on Unix and
	// drive-letter or UNC paths (`C:\...`, `\\server\share\...`)
	// on Windows. The previous isPOSIXAbsolute check would have
	// rejected every os.Executable() return value on Windows
	// because those start with a drive letter, not /.
	if !filepath.IsAbs(binaryPath) {
		return nil, fmt.Errorf("launcher: binary path must be absolute, got %q", binaryPath)
	}
	if _, err := os.Stat(binaryPath); err != nil {
		return nil, fmt.Errorf("launcher: binary path not usable: %w", err)
	}

	result, err := m.Install(ctx, binaryPath)
	if err != nil {
		return nil, err
	}

	settings := UserSettings{
		Launcher: LauncherSettings{
			Enabled:           true,
			InstalledUnitPath: result.UnitPath,
			InstalledAt:       time.Now().UnixMilli(),
		},
	}
	if err := WriteSettings(settings); err != nil {
		return nil, fmt.Errorf("launcher: write settings: %w", err)
	}

	return result, nil
}

// Disable unregisters the worker, removes the unit/plist/task file,
// and clears the launcher settings. Idempotent. Returns
// ErrUnsupportedOS on platforms without a launcher backend.
func Disable(ctx context.Context) (*DisableResult, error) {
	m, err := newManager()
	if err != nil {
		return nil, err
	}

	result, err := m.Uninstall(ctx)
	if err != nil {
		return nil, err
	}

	// Preserve unrelated top-level settings; only clear the launcher
	// section.
	settings, err := ReadSettings()
	if err != nil {
		settings = UserSettings{}
	}
	settings.Launcher = LauncherSettings{}
	if err := WriteSettings(settings); err != nil {
		return nil, fmt.Errorf("launcher: clear settings: %w", err)
	}

	return result, nil
}
