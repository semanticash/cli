//go:build windows

package platform

// SyncDir is a no-op on Windows, where ReplaceFile supplies write-through semantics.
func SyncDir(string) error { return nil }
