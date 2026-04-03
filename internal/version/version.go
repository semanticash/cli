package version

import "strings"

var (
	Version = "dev" // Version is set at build time via ldflags
	Commit  = ""    // Commit is set at build time via ldflags
)

// Display returns the CLI version string shown to users.
func Display() string {
	if commit := strings.TrimSpace(Commit); commit != "" {
		return Version + " (" + commit + ")"
	}
	return Version
}

// UserAgent returns a User-Agent string for HTTP requests.
func UserAgent() string {
	return "semantica-cli/" + Version
}
