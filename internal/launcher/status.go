package launcher

import (
	"context"
	"errors"
	"os"
	"runtime"
)

// StatusResult describes launcher state as reported by settings, the
// unit/plist/task file on disk, and the OS daemon manager.
type StatusResult struct {
	// OS is the runtime.GOOS value at the time of the call.
	OS string

	// SettingsEnabled is the launcher.enabled flag from settings.json.
	SettingsEnabled bool

	// InstalledPlistPath is the unit/plist/task path recorded in
	// settings.
	InstalledPlistPath string

	// InstalledAt is the enable-time Unix millisecond timestamp.
	InstalledAt int64

	// SettingsError is set when the settings file exists but could
	// not be read cleanly.
	SettingsError string

	// ExpectedPlistPath is the canonical unit/plist/task path for
	// the current user.
	ExpectedPlistPath string

	// PlistOnDisk reports whether the unit/plist/task file exists
	// at the recorded or expected path.
	PlistOnDisk bool

	// DomainTarget is the OS-specific service identifier.
	DomainTarget string

	// LoadedInLaunchd reports whether the OS daemon manager has the
	// service registered.
	LoadedInLaunchd bool

	// LaunchdState is a short summary of what the OS daemon manager
	// reported:
	//   - "loaded"       : service is registered
	//   - "not loaded"   : service is not registered
	//   - "unsupported"  : no launcher backend on this OS
	//   - "error: <msg>" : daemon-manager call failed
	LaunchdState string

	// LogPath is the launcher worker log path.
	LogPath string
}

// Status gathers launcher state from settings, the filesystem, and
// the OS daemon manager. Non-fatal problems are encoded into
// StatusResult so the caller can render a coherent view even when
// one source disagrees with another.
func Status(ctx context.Context) (StatusResult, error) {
	result := StatusResult{OS: runtime.GOOS}

	if s, err := ReadSettings(); err == nil {
		result.SettingsEnabled = s.Launcher.Enabled
		result.InstalledPlistPath = s.Launcher.InstalledPlistPath
		result.InstalledAt = s.Launcher.InstalledAt
	} else {
		result.SettingsError = err.Error()
	}

	if log, err := WorkerLogPath(); err == nil {
		result.LogPath = log
	}

	m, mErr := newManager()
	if mErr != nil {
		// Unsupported platform. Settings and log path are still
		// reported so the dashboard can show what the user has
		// configured even on a host that cannot run the launcher.
		result.LaunchdState = "unsupported"
		return result, nil
	}

	expected, err := m.UnitPath()
	if err != nil {
		return result, err
	}
	result.ExpectedPlistPath = expected
	result.DomainTarget = m.UnitTarget()

	probe := result.InstalledPlistPath
	if probe == "" {
		probe = result.ExpectedPlistPath
	}
	if _, err := os.Stat(probe); err == nil {
		result.PlistOnDisk = true
	}

	loaded, err := m.IsActive(ctx)
	switch {
	case err == nil && loaded:
		result.LoadedInLaunchd = true
		result.LaunchdState = "loaded"
	case err == nil:
		result.LaunchdState = "not loaded"
	case errors.Is(err, ErrUnsupportedOS):
		result.LaunchdState = "unsupported"
	default:
		result.LaunchdState = "error: " + err.Error()
	}

	return result, nil
}
