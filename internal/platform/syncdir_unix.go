//go:build unix

package platform

import "os"

// SyncDir persists directory entry changes before their dependencies are released.
func SyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}
