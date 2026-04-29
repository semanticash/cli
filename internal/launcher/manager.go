package launcher

import (
	"context"
	"errors"
	"strings"
)

// ErrUnsupportedOS reports that the launcher has no backend on the
// current OS.
var ErrUnsupportedOS = errors.New("launcher: unsupported OS")

// LabelWorker is the worker service label, used as the launchd
// plist label, the systemd unit base name, and the Task Scheduler
// task name. Same identifier across all backends so diagnostics
// stay consistent.
const LabelWorker = "sh.semantica.worker"

// InstallResult describes a successful Enable or reinstall.
type InstallResult struct {
	// UnitPath is the installed unit/plist/task path.
	UnitPath string

	// UnitTarget is the OS-specific service identifier.
	UnitTarget string

	// Reinstalled reports whether a previous service was already
	// loaded at the time of install.
	Reinstalled bool
}

// DisableResult describes the outcome of a Disable call.
type DisableResult struct {
	// WasEnabled reflects the settings flag prior to disable.
	WasEnabled bool

	// RemovedUnitPath is the unit/plist/task path that was
	// removed, if any. Empty when no file was on disk.
	RemovedUnitPath string
}

// manager is the OS-specific backend the public API delegates to.
// Build-tagged newManager constructors select the implementation.
type manager interface {
	// Install renders the unit/plist/task definition, registers it
	// with the OS daemon manager, and returns the resulting paths
	// and identifiers.
	Install(ctx context.Context, binaryPath string) (*InstallResult, error)

	// Uninstall removes the unit/plist/task and unregisters it from
	// the OS daemon manager. Idempotent: missing files and missing
	// registrations are treated as success.
	Uninstall(ctx context.Context) (*DisableResult, error)

	// Kick triggers an on-demand run of the worker drain. Returns
	// promptly; the actual drain runs asynchronously under the OS
	// daemon manager.
	Kick(ctx context.Context) error

	// IsActive reports whether the service is currently registered
	// with the OS daemon manager.
	IsActive(ctx context.Context) (bool, error)
}

// isPOSIXAbsolute reports whether p starts with "/". Used by the
// darwin install path; lives here so cross-platform callers can
// reference it without dragging in a darwin-tagged file.
func isPOSIXAbsolute(p string) bool {
	return strings.HasPrefix(p, "/")
}
