package platform

import (
	"path/filepath"
	"strings"
)

// LooksAbsolutePath returns true if path looks like an absolute path on
// any OS. Unlike filepath.IsAbs, this recognizes POSIX-style absolute
// paths (/workspace/...) even on Windows, which is needed for file paths
// from agent payloads that may use POSIX conventions regardless of host OS.
func LooksAbsolutePath(path string) bool {
	if path == "" {
		return false
	}
	if filepath.IsAbs(path) {
		return true
	}
	if path[0] == '/' {
		return true
	}
	return strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, `//`)
}
