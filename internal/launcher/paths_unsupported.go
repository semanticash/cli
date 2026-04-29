//go:build !darwin

package launcher

// Stubs for the darwin-only path helpers so the package compiles on
// non-darwin targets without those callers needing build tags.
// External callers (commands/launcher.go, service/post-commit.go)
// reference DomainTarget by name; on unsupported platforms it
// returns an empty string and Kickstart returns ErrUnsupportedOS
// before the empty target can reach launchctl.

// PlistPath returns ErrUnsupportedOS on platforms without a
// launcher backend.
func PlistPath() (string, error) {
	return "", ErrUnsupportedOS
}

// UserDomain returns "" on platforms without a launcher backend.
func UserDomain() string {
	return ""
}

// DomainTarget returns "" on platforms without a launcher backend.
func DomainTarget() string {
	return ""
}
