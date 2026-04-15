package version

import (
	"runtime"
	"strings"
	"testing"
)

func TestDisplay_WithCommit(t *testing.T) {
	origVersion := Version
	origCommit := Commit
	t.Cleanup(func() {
		Version = origVersion
		Commit = origCommit
	})

	Version = "v0.1.0"
	Commit = "abc1234"
	got := Display()

	if !strings.Contains(got, "Semantica CLI v0.1.0 (abc1234)") {
		t.Errorf("Display() missing version line, got:\n%s", got)
	}
	if !strings.Contains(got, "Go version: "+runtime.Version()) {
		t.Errorf("Display() missing Go version, got:\n%s", got)
	}
	if !strings.Contains(got, "OS/Arch: "+runtime.GOOS+"/"+runtime.GOARCH) {
		t.Errorf("Display() missing OS/Arch, got:\n%s", got)
	}
}

func TestDisplay_WithoutCommit(t *testing.T) {
	origVersion := Version
	origCommit := Commit
	t.Cleanup(func() {
		Version = origVersion
		Commit = origCommit
	})

	Version = "v0.1.0"
	Commit = ""
	got := Display()

	if !strings.Contains(got, "Semantica CLI v0.1.0") {
		t.Errorf("Display() missing version, got:\n%s", got)
	}
	if strings.Contains(got, "(") {
		t.Errorf("Display() should not contain commit hash when empty, got:\n%s", got)
	}
}

func TestShort_WithCommit(t *testing.T) {
	origVersion := Version
	origCommit := Commit
	t.Cleanup(func() {
		Version = origVersion
		Commit = origCommit
	})

	Version = "v0.1.0"
	Commit = "abc1234"
	if got := Short(); got != "v0.1.0 (abc1234)" {
		t.Errorf("Short() = %q, want %q", got, "v0.1.0 (abc1234)")
	}
}

func TestShort_WithoutCommit(t *testing.T) {
	origVersion := Version
	origCommit := Commit
	t.Cleanup(func() {
		Version = origVersion
		Commit = origCommit
	})

	Version = "v0.1.0"
	Commit = ""
	if got := Short(); got != "v0.1.0" {
		t.Errorf("Short() = %q, want %q", got, "v0.1.0")
	}
}
