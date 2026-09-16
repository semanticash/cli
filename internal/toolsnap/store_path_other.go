//go:build !windows

package toolsnap

func storeLocationEnv(_ string, extra []string) []string {
	return storeGitEnv(extra)
}

func storeGitLocation(dir string) (cwd, gitDir string, err error) {
	return dir, ".", nil
}
