package version

import (
	"fmt"
	"runtime"
	"strings"
)

var (
	Version = "dev" // Version is set at build time via ldflags
	Commit  = ""    // Commit is set at build time via ldflags
)

// Short returns the version with optional commit hash on one line.
func Short() string {
	if commit := strings.TrimSpace(Commit); commit != "" {
		return Version + " (" + commit + ")"
	}
	return Version
}

// Display returns the full version info including Go version and OS/Arch.
func Display() string {
	commit := strings.TrimSpace(Commit)
	line1 := "Semantica CLI " + Version
	if commit != "" {
		line1 += " (" + commit + ")"
	}
	return fmt.Sprintf("%s\nGo version: %s\nOS/Arch: %s/%s",
		line1, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// UserAgent returns a User-Agent string for HTTP requests.
func UserAgent() string {
	return "semantica-cli/" + Version
}
