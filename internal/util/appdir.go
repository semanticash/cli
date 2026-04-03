package util

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// AppConfigDir returns the global Semantica config directory.
func AppConfigDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "semantica"), nil
	}

	if runtime.GOOS == "windows" {
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return "", fmt.Errorf("APPDATA not set")
		}
		return filepath.Join(appData, "semantica"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".config", "semantica"), nil
}
