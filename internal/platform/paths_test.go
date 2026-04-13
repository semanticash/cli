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
