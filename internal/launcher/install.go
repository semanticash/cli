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

// InstallResult describes the disk state produced by Enable or
// by a successful re-install. The fields are surfaced by the
// cobra command so the user sees exactly where the plist landed.
type InstallResult struct {
	// PlistPath is the absolute path of the plist file that was
	// written or replaced.
	PlistPath string

	// DomainTarget is the launchctl service target the agent
	// was bootstrapped as (gui/<uid>/sh.semantica.worker). The
	// caller can paste this into `launchctl print` for
	// diagnostics.
	DomainTarget string

	// Reinstalled is true when Enable ran against a system that
	// already had the launcher installed. The caller may
	// surface this differently ("re-installed" vs "installed")
	// but the end state is identical.
	Reinstalled bool
}

// Enable installs the worker plist, bootstraps it into the
// current user's launchd domain, and records the state in the
// user-level settings file. Idempotent: running Enable when
// already enabled re-renders the plist against the current
// binary path and re-bootstraps it, leaving the system in a
// known-good state even if the user has tampered with the
// plist or the binary has moved since the last Enable.
//
// binaryPath is the absolute path launchd should invoke. The
// caller typically passes os.Executable(). If the host is not
// macOS, ErrUnsupportedOS is returned.
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

	// Make sure the directory for the log file exists so launchd
	// can open it on the first kickstart. Creating here rather
	// than inside the drain command keeps the failure mode local
	// to install (where the user sees it) instead of deferred to
	// the first commit.
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

	// Always attempt to bootout the canonical target, regardless
	// of what settings claim. Keying off settings alone (IsEnabled)
	// creates a wedge: if a previous install got as far as
	// bootstrap but failed before WriteSettings, or if the settings
	// file was later deleted or corrupted, settings say "not
	// enabled" while launchd still has the service loaded. A
	// retry of Enable would then bootstrap against an already-
	// loaded label and fail with "already loaded." Using launchd
	// as the source of truth removes that failure mode entirely.
	previouslyLoaded, err := bootoutIgnoringNotLoaded(ctx, DomainTarget())
	if err != nil {
		return nil, fmt.Errorf("launcher: bootout previous service: %w", err)
	}

	if err := writePlistAtomic(plistPath, []byte(plistBody)); err != nil {
		return nil, err
	}

	if err := Bootstrap(ctx, UserDomain(), plistPath); err != nil {
		// Bootstrap failure after the plist is on disk: leave
		// the plist in place so the user can inspect it, but
		// do not record enabled=true because the service is
		// not actually loaded.
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

// DisableResult describes what Disable changed. Fields are
// populated even on a partial no-op so the caller can emit an
// accurate summary ("launcher was not installed; nothing to do").
type DisableResult struct {
	// WasEnabled is true when the settings file previously
	// recorded an enabled launcher. False means Disable
	// encountered an empty state and did nothing substantive.
	WasEnabled bool

	// RemovedPlistPath is the plist file that was removed.
	// Empty when the file did not exist at teardown time
	// (for example because the user deleted it manually).
	RemovedPlistPath string
}

// Disable unloads the worker agent, removes the plist file, and
// clears the user-level settings flag. Idempotent: running
// Disable when already disabled is a silent no-op that returns
// WasEnabled=false.
//
// If the launchctl bootout call reports "not loaded", Disable
// treats it as success. If the plist file is already absent at
// the recorded path, Disable treats it as success. The intent
// is that a user can run Disable any number of times from any
// starting state and end up in a clean disabled state.
func Disable(ctx context.Context) (*DisableResult, error) {
	if runtime.GOOS != "darwin" {
		return nil, ErrUnsupportedOS
	}

	settings, err := ReadSettings()
	if err != nil {
		// A malformed settings file should not prevent
		// disable. Log-equivalent behavior would be to surface
		// the error, but the caller has no obvious remedy.
		// Proceed as if settings were empty so disable can
		// still clean up the plist and domain.
		settings = UserSettings{}
	}

	res := &DisableResult{WasEnabled: settings.Launcher.Enabled}

	// Bootout by the canonical DomainTarget even if settings
	// are empty: a previous install may have written a plist
	// and bootstrapped it, then settings got lost. The helper
	// returns nil when the service was loaded and unloaded
	// cleanly, also nil when launchd reports the service is
	// not loaded, and a wrapped error for any other failure.
	if _, err := bootoutIgnoringNotLoaded(ctx, DomainTarget()); err != nil {
		return nil, fmt.Errorf("launcher: bootout: %w", err)
	}

	// Prefer the settings-recorded plist path because the user
	// may have installed to a different location on an older
	// semantica version. Fall back to the default for systems
	// where the settings file was lost or never existed.
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

	// Clear the launcher section so future reads return
	// Enabled=false. Keep any other top-level sections
	// untouched by round-tripping the current UserSettings
	// with the Launcher field zeroed.
	settings.Launcher = LauncherSettings{}
	if err := WriteSettings(settings); err != nil {
		return nil, fmt.Errorf("launcher: clear settings: %w", err)
	}

	return res, nil
}

// bootoutIgnoringNotLoaded calls launchctl bootout against the
// given domain target and classifies the outcome into three
// cases:
//
//   - Bootout succeeded: returns (true, nil). The caller knows
//     something was previously loaded.
//   - Launchctl reported the service is not loaded: returns
//     (false, nil). The caller knows there was nothing to
//     bootout, which is treated as soft success for idempotency.
//   - Any other launchctl or OS failure: returns (false, err)
//     with the wrapped error so the caller can surface it.
//
// Used by both Enable (as a pre-install clean-slate) and
// Disable (as the teardown step). Centralizing the classifier
// here means both entry points have identical semantics for
// "service was not loaded," and neither relies on the settings
// file to decide whether to attempt the bootout.
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

// writePlistAtomic writes the plist bytes to path via a tmp +
// rename so a crashed install cannot leave a half-written file
// that launchd might pick up on its next refresh. Creates the
// parent directory if missing.
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
