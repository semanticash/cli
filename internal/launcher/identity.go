package launcher

import (
	"context"
	"fmt"
	"os"
)

// BinaryIdentity is a cheap fingerprint of the binary registered with
// the launcher. Size plus modification time changes on normal upgrades
// and local installs, which is enough to detect when the service should
// be re-registered before the next kick.
type BinaryIdentity struct {
	Path  string
	Size  int64
	ModMS int64 // modification time, Unix milliseconds
}

// StatBinaryIdentity reads the current identity of the binary at path.
func StatBinaryIdentity(path string) (BinaryIdentity, error) {
	info, err := os.Stat(path)
	if err != nil {
		return BinaryIdentity{}, fmt.Errorf("launcher: stat binary: %w", err)
	}
	return BinaryIdentity{
		Path:  path,
		Size:  info.Size(),
		ModMS: info.ModTime().UnixMilli(),
	}, nil
}

// RecordedIdentity returns the identity captured at enable time, and
// whether one was recorded. Settings written by older versions carry no
// identity; callers treat that as stale so one refresh migrates them.
func (s LauncherSettings) RecordedIdentity() (BinaryIdentity, bool) {
	if s.InstalledBinaryPath == "" {
		return BinaryIdentity{}, false
	}
	return BinaryIdentity{
		Path:  s.InstalledBinaryPath,
		Size:  s.InstalledBinarySize,
		ModMS: s.InstalledBinaryModMS,
	}, true
}

// identityStale reports whether the registered binary no longer matches
// the identity recorded at enable time, with a human-readable reason.
// A missing record (legacy settings) is stale by definition.
func identityStale(s LauncherSettings) (bool, string) {
	recorded, ok := s.RecordedIdentity()
	if !ok {
		return true, "no binary identity recorded (enabled by an older version)"
	}
	current, err := StatBinaryIdentity(recorded.Path)
	if err != nil {
		return true, fmt.Sprintf("registered binary not statable: %v", err)
	}
	if current.Size != recorded.Size || current.ModMS != recorded.ModMS {
		return true, fmt.Sprintf(
			"binary at %s changed since enable (size %d -> %d, mtime %d -> %d)",
			recorded.Path, recorded.Size, current.Size, recorded.ModMS, current.ModMS,
		)
	}
	return false, ""
}

// RefreshResult reports what Refresh did.
type RefreshResult struct {
	// Enabled is false when the launcher is not enabled; Refresh is a
	// no-op in that case.
	Enabled bool

	// BinaryPath is the binary the launcher was re-bound to.
	BinaryPath string

	// Install carries the re-install result when Enabled.
	Install *InstallResult
}

// Refresh re-binds the OS daemon manager to the currently executing
// binary. It is a no-op when the launcher is disabled, and a full
// re-enable when it is enabled.
//
// The target is always os.Executable(), never the path recorded in
// settings: installer hooks invoke the freshly installed binary, so
// preferring the recorded path would re-bind launchd to a stale install
// location after a user migrates (Homebrew to curl, custom INSTALL_DIR),
// and then stamp a fresh identity on the old binary, making status look
// healthy while the launcher runs the wrong build.
//
// Refresh deliberately does not kickstart; callers decide whether to
// kick (the CLI command does, to drain queued markers).
func Refresh(ctx context.Context) (*RefreshResult, error) {
	settings, err := ReadSettings()
	if err != nil {
		return nil, fmt.Errorf("launcher: read settings: %w", err)
	}
	if !settings.Launcher.Enabled {
		return &RefreshResult{Enabled: false}, nil
	}

	binaryPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("launcher: resolve current binary: %w", err)
	}

	install, err := Enable(ctx, binaryPath)
	if err != nil {
		return nil, err
	}
	return &RefreshResult{Enabled: true, BinaryPath: binaryPath, Install: install}, nil
}

// EnsureFreshBinary checks the registered binary's identity and refreshes
// the launcher when it is stale. Returns whether a refresh ran. The
// dispatch path calls this so a binary replacement self-heals on the next
// commit.
func EnsureFreshBinary(ctx context.Context) (bool, error) {
	settings, err := ReadSettings()
	if err != nil {
		return false, fmt.Errorf("launcher: read settings: %w", err)
	}
	if !settings.Launcher.Enabled {
		return false, nil
	}
	stale, reason := identityStale(settings.Launcher)
	if !stale {
		return false, nil
	}
	if _, err := Refresh(ctx); err != nil {
		return false, fmt.Errorf("launcher: refresh stale binary (%s): %w", reason, err)
	}
	return true, nil
}
