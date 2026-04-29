//go:build !darwin

package launcher

import (
	"context"
	"path/filepath"
	"testing"
)

// TestStatus_UnsupportedOS pins the cross-platform contract: Status
// returns "unsupported" for the daemon-manager state on platforms
// without a launcher backend. Settings and log paths are still
// reported so the dashboard can render configured-but-inactive
// state.
func TestStatus_UnsupportedOS(t *testing.T) {
	base := t.TempDir()
	t.Setenv("SEMANTICA_HOME", base)
	t.Setenv("HOME", base)

	s, err := Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if s.LaunchdState != "unsupported" {
		t.Errorf("LaunchdState = %q, want %q", s.LaunchdState, "unsupported")
	}
	if s.LoadedInLaunchd {
		t.Errorf("LoadedInLaunchd = true on platform without launcher backend")
	}
	if s.LogPath != filepath.Join(base, "worker-launcher.log") {
		t.Errorf("LogPath = %q, want %q", s.LogPath, filepath.Join(base, "worker-launcher.log"))
	}
}
