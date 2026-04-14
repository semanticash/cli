package platform

import (
	"path/filepath"
	"runtime"
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
	if strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, `//`) {
		return true
	}
	// On Windows, a root-relative path (\workspace\...) is drive-rooted and
	// behaves as absolute even though filepath.IsAbs returns false for it.
	return runtime.GOOS == "windows" && path[0] == '\\'
}

// NormalizePathForCompare returns a path suitable for case-aware prefix
// matching. On Windows, paths are lowercased because the filesystem is
// case-insensitive (C:\Users and c:\Users are the same path). On
// macOS/Linux, the path is returned unchanged.
func NormalizePathForCompare(p string) string {
	s := filepath.ToSlash(filepath.Clean(p))
	if runtime.GOOS == "windows" {
		s = strings.ToLower(s)
	}
	return s
}
