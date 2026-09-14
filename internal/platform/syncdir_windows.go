//go:build windows

package platform

// SyncDir is unnecessary after ReplaceFile's write-through move on Windows.
func SyncDir(string) error { return nil }
