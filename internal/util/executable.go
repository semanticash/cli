package util

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// ResolveExecutable returns the first matching executable path for any of the
// provided binary names. It checks PATH first, then common user and package
// manager install locations used on macOS and Linux.
func ResolveExecutable(names []string, extraCandidates ...string) string {
	return resolveExecutable(names, extraCandidates, exec.LookPath, os.UserHomeDir, os.Getenv)
}

func resolveExecutable(
	names []string,
	extraCandidates []string,
	lookPath func(string) (string, error),
	userHomeDir func() (string, error),
	getenv func(string) string,
) string {
	for _, name := range names {
		if path, err := lookPath(name); err == nil {
			return path
		}
	}

	candidates := make([]string, 0, len(extraCandidates)+(len(names)*8))
	candidates = append(candidates, extraCandidates...)

	seenDirs := make(map[string]struct{})
	for _, dir := range executableSearchDirs(userHomeDir, getenv) {
		if dir == "" {
			continue
		}
		if _, ok := seenDirs[dir]; ok {
			continue
		}
		seenDirs[dir] = struct{}{}
		for _, name := range names {
			candidates = append(candidates, filepath.Join(dir, name))
		}
	}

	seenPaths := make(map[string]struct{})
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, ok := seenPaths[candidate]; ok {
			continue
		}
		seenPaths[candidate] = struct{}{}
		if isExecutableFile(candidate) {
			return candidate
		}
	}

	return ""
}

func executableSearchDirs(
	userHomeDir func() (string, error),
	getenv func(string) string,
) []string {
	dirs := make([]string, 0, 8)

	if prefix := getenv("NPM_CONFIG_PREFIX"); prefix != "" {
		dirs = append(dirs, filepath.Join(prefix, "bin"))
	}
	if pnpmHome := getenv("PNPM_HOME"); pnpmHome != "" {
		dirs = append(dirs, pnpmHome)
	}
	if bunInstall := getenv("BUN_INSTALL"); bunInstall != "" {
		dirs = append(dirs, filepath.Join(bunInstall, "bin"))
	}

	if home, err := userHomeDir(); err == nil && home != "" {
		dirs = append(dirs,
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, ".npm", "bin"),
			filepath.Join(home, "bin"),
		)
	}

	dirs = append(dirs,
		"/opt/homebrew/bin",
		"/usr/local/bin",
		"/snap/bin",
	)

	return dirs
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode()&0o111 != 0
}
