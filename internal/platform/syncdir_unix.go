//go:build !windows

package platform

import "os"

// SyncDir persists directory entries after an atomic file replacement.
func SyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}
