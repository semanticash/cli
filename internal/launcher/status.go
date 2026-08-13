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

	// InstalledUnitPath is the unit/plist/task path recorded in
	// settings.
	InstalledUnitPath string

	// InstalledAt is the enable-time Unix millisecond timestamp.
	InstalledAt int64

	// SettingsError is set when the settings file exists but could
	// not be read cleanly.
	SettingsError string

	// ExpectedUnitPath is the canonical unit/plist/task path for
	// the current user.
	ExpectedUnitPath string

	// UnitOnDisk reports whether the unit/plist/task file exists
	// at the recorded or expected path.
	UnitOnDisk bool

	// UnitTarget is the OS-specific service identifier.
	UnitTarget string

	// LoadedInDaemon reports whether the OS daemon manager has the
	// service registered.
	LoadedInDaemon bool

	// ServiceState is a short summary of what the OS daemon manager
	// reported:
	//   - "loaded"       : service is registered
	//   - "not loaded"   : service is not registered
	//   - "unsupported"  : no launcher backend on this OS
	//   - "error: <msg>" : daemon-manager call failed
	ServiceState string

	// LogPath is the launcher worker log path.
	LogPath string

	// Health carries OS-specific daemon spawn-health signals.
	Health ServiceHealth

	// BinaryStale reports that the registered binary no longer matches
	// the identity recorded at enable time; BinaryStaleReason explains.
	// Remediation for both this and Health.SpawnRefused is
	// `semantica launcher refresh`.
	BinaryStale       bool
	BinaryStaleReason string
}

// ServiceHealth is the OS daemon manager's spawn-health view of the
// worker service.
type ServiceHealth struct {
	// LastExitReason is the daemon's recorded last exit reason, when
	// exposed (e.g. launchd's OS_REASON_CODESIGNING).
	LastExitReason string

	// NeedsLWCRUpdate reports launchd's "needs LWCR update" property:
	// the pinned code requirement no longer matches the binary.
	NeedsLWCRUpdate bool

	// SpawnRefused reports that the service is registered but the OS
	// daemon manager is refusing to run it.
	SpawnRefused bool
}

// Status gathers launcher state from settings, the filesystem, and
// the OS daemon manager. Non-fatal problems are encoded into
// StatusResult so the caller can render a coherent view even when
// one source disagrees with another.
func Status(ctx context.Context) (StatusResult, error) {
	result := StatusResult{OS: runtime.GOOS}

	if s, err := ReadSettings(); err == nil {
		result.SettingsEnabled = s.Launcher.Enabled
		result.InstalledUnitPath = s.Launcher.InstalledUnitPath
		result.InstalledAt = s.Launcher.InstalledAt
		if s.Launcher.Enabled {
			result.BinaryStale, result.BinaryStaleReason = identityStale(s.Launcher)
		}
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
		result.ServiceState = "unsupported"
		return result, nil
	}

	expected, err := UnitPath()
	if err != nil {
		return result, err
	}
	result.ExpectedUnitPath = expected
	result.UnitTarget = UnitTarget()

	probe := result.InstalledUnitPath
	if probe == "" {
		probe = result.ExpectedUnitPath
	}
	if _, err := os.Stat(probe); err == nil {
		result.UnitOnDisk = true
	}

	loaded, err := m.IsRegistered(ctx)
	switch {
	case err == nil && loaded:
		result.LoadedInDaemon = true
		result.ServiceState = "loaded"
		result.Health = daemonServiceHealth(ctx)
	case err == nil:
		result.ServiceState = "not loaded"
	case errors.Is(err, ErrUnsupportedOS):
		result.ServiceState = "unsupported"
	default:
		result.ServiceState = "error: " + err.Error()
	}

	return result, nil
}
