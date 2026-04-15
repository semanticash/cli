package platform

import (
	"runtime"
	"testing"
)

func TestLooksAbsolutePath(t *testing.T) {
	tests := []struct {
		path    string
		want    bool
		winOnly bool // true = only expected to pass on Windows
	}{
		// POSIX absolute (works on all platforms)
		{"/workspace/cli/main.go", true, false},
		{"/usr/local/bin/semantica", true, false},
		{"/", true, false},

		// Windows absolute (filepath.IsAbs recognizes these only on Windows)
		{"C:\\Users\\dev\\repo\\main.go", true, true},
		{"D:/projects/cli/main.go", true, true},

		// UNC paths (work on all platforms via prefix check)
		{"\\\\server\\share\\file.go", true, false},
		{"//server/share/file.go", true, false},

		// Relative
		{"main.go", false, false},
		{"./internal/service/worker.go", false, false},
		{"../other/file.go", false, false},

		// Empty
		{"", false, false},
	}
	for _, tt := range tests {
		if tt.winOnly && runtime.GOOS != "windows" {
			continue
		}
		got := LooksAbsolutePath(tt.path)
		if got != tt.want {
			t.Errorf("LooksAbsolutePath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestNormalizePathForCompare_NoOpOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-only: verifies no lowercasing on case-sensitive filesystems")
	}
	got := NormalizePathForCompare("/Users/Dev/Repo/Main.go")
	want := "/Users/Dev/Repo/Main.go"
	if got != want {
		t.Errorf("NormalizePathForCompare = %q, want %q (case preserved on Unix)", got, want)
	}
}

func TestNormalizePathForCompare_LowercasesOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only: verifies case normalization")
	}
	got := NormalizePathForCompare("C:\\Users\\Dev\\Repo\\Main.go")
	want := "c:/users/dev/repo/main.go"
	if got != want {
		t.Errorf("NormalizePathForCompare = %q, want %q", got, want)
	}
}

func TestNormalizePathForCompare_ForwardSlashes(t *testing.T) {
	// On all platforms, backslashes should become forward slashes.
	got := NormalizePathForCompare("/workspace/repo/src/main.go")
	if got != "/workspace/repo/src/main.go" {
		t.Errorf("NormalizePathForCompare = %q, want forward slashes", got)
	}
}
