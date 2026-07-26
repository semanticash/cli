//go:build !windows

package platform

import "os"

// replaceFile atomically renames src over dst.
func replaceFile(src, dst string) error {
	return os.Rename(src, dst)
}
