//go:build !darwin && !linux && !windows

package launcher

// Stubs for the darwin-only path helpers so the package compiles on
// non-darwin targets without those callers needing build tags.
// External callers reference UnitTarget by name; on unsupported
// platforms it returns an empty string and Kickstart returns
// ErrUnsupportedOS before the empty target can reach a real daemon
// manager.

// UnitPath returns ErrUnsupportedOS on platforms without a launcher
// backend.
func UnitPath() (string, error) {
	return "", ErrUnsupportedOS
}

// UserDomain returns "" on platforms without a launcher backend.
func UserDomain() string {
	return ""
}

// UnitTarget returns "" on platforms without a launcher backend.
func UnitTarget() string {
	return ""
}
