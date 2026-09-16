//go:build !windows

package toolsnap

func storeGitLocation(dir string) (cwd, gitDir string, err error) {
	return dir, ".", nil
}
