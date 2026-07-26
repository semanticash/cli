//go:build windows

package platform

import "golang.org/x/sys/windows"

// replaceFile replaces dst in one MoveFileEx call without removing it
// first. The write-through flag completes the move before returning.
func replaceFile(src, dst string) error {
	s, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	d, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(s, d, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
