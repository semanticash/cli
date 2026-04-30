package launcher

import (
	"path/filepath"
	"testing"
)

func TestWorkerLogPath_HonorsSemanticaHome(t *testing.T) {
	base := t.TempDir()
	t.Setenv("SEMANTICA_HOME", base)

	got, err := WorkerLogPath()
	if err != nil {
		t.Fatalf("WorkerLogPath: %v", err)
	}
	want := filepath.Join(base, "worker-launcher.log")
	if got != want {
		t.Errorf("WorkerLogPath = %q, want %q", got, want)
	}
}
