package toolsnap

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func storeLocationEnv(gitDir string, extra []string) []string {
	// Git expands GIT_DIR during init before long-path configuration is loaded.
	// Keep config and object paths on the same short alias throughout setup.
	return append(storeGitEnv(extra), "GIT_COMMON_DIR="+gitDir)
}

// storeGitLocation keeps both the process directory and Git directory short.
func storeGitLocation(dir string) (cwd, gitDir string, err error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}
	if len(abs) < 220 {
		return filepath.VolumeName(abs) + string(filepath.Separator), abs, nil
	}
	path := abs
	if !strings.HasPrefix(path, `\\?\`) {
		if strings.HasPrefix(path, `\\`) {
			path = `\\?\UNC\` + strings.TrimPrefix(path, `\\`)
		} else {
			path = `\\?\` + path
		}
	}
	wide, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", "", err
	}
	buf := make([]uint16, 32768)
	n, err := windows.GetShortPathName(wide, &buf[0], uint32(len(buf)))
	if err != nil {
		return "", "", fmt.Errorf("toolsnap: resolve short store path: %w", err)
	}
	if n == 0 || n >= uint32(len(buf)) {
		return "", "", fmt.Errorf("toolsnap: invalid short store path length")
	}
	short := windows.UTF16ToString(buf[:n])
	if strings.HasPrefix(short, `\\?\UNC\`) {
		short = `\\` + strings.TrimPrefix(short, `\\?\UNC\`)
	} else {
		short = strings.TrimPrefix(short, `\\?\`)
	}
	// Volumes without short names may return the original path.
	if len(short) >= 220 {
		return "", "", fmt.Errorf("toolsnap: store path exceeds Git's Windows limit and has no usable short name")
	}
	return filepath.VolumeName(short) + string(filepath.Separator), short, nil
}
