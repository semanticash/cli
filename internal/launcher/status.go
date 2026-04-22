package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
)

// StatusResult describes launcher state as reported by settings, the
// plist on disk, and launchd.
type StatusResult struct {
	// OS is the runtime.GOOS value at the time of the call.
	OS string

	// SettingsEnabled is the launcher.enabled flag from settings.json.
	SettingsEnabled bool

	// InstalledPlistPath is the plist path recorded in settings.
	InstalledPlistPath string

	// InstalledAt is the enable-time Unix millisecond timestamp.
	InstalledAt int64

	// SettingsError is set when the settings file exists but could not
	// be read cleanly.
	SettingsError string

	// ExpectedPlistPath is the canonical plist path for the current user.
	ExpectedPlistPath string

	// PlistOnDisk reports whether the plist file exists at the recorded
	// or expected path.
	PlistOnDisk bool

	// DomainTarget is the launchctl target for the service.
	DomainTarget string

	// LoadedInLaunchd reports whether launchctl print found the service.
	LoadedInLaunchd bool

	// LaunchdState is a short summary of what launchctl reported:
	//   - "loaded"       : service is registered in launchd
	//   - "not loaded"   : service is not registered
	//   - "unsupported"  : host is not macOS
	//   - "error: <msg>" : launchctl call failed for some
	//                      other reason
	LaunchdState string

	// LogPath is the launcher log path.
	LogPath string
}

// Status gathers launcher state from settings, the filesystem, and
// launchd. Non-fatal problems are encoded into StatusResult.
func Status(ctx context.Context) (StatusResult, error) {
	result := StatusResult{
		OS:           runtime.GOOS,
		DomainTarget: DomainTarget(),
	}

	// Surface settings read failures instead of flattening them to
	// "not enabled." Missing files still read as the zero value.
	if s, err := ReadSettings(); err == nil {
		result.SettingsEnabled = s.Launcher.Enabled
		result.InstalledPlistPath = s.Launcher.InstalledPlistPath
		result.InstalledAt = s.Launcher.InstalledAt
	} else {
		result.SettingsError = err.Error()
	}

	// Expected plist path.
	expected, err := PlistPath()
	if err != nil {
		return result, fmt.Errorf("status: resolve plist path: %w", err)
	}
	result.ExpectedPlistPath = expected

	// Check the recorded path first, then the expected path.
	probe := result.InstalledPlistPath
	if probe == "" {
		probe = result.ExpectedPlistPath
	}
	if _, err := os.Stat(probe); err == nil {
		result.PlistOnDisk = true
	}

	// Log path.
	if log, err := WorkerLogPath(); err == nil {
		result.LogPath = log
	}

	// Launchd state.
	switch {
	case runtime.GOOS != "darwin":
		result.LaunchdState = "unsupported"
	default:
		loaded, err := IsLoaded(ctx, result.DomainTarget)
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
	}

	return result, nil
}
