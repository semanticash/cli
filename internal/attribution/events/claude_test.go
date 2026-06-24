package events

import "testing"

func TestNormalizePath_RelativeInputReturnedAsRepoRelative(t *testing.T) {
	got := NormalizePath("internal/service/pre-push.go", "/workspace/semantica")
	want := "internal/service/pre-push.go"
	if got != want {
		t.Errorf("NormalizePath(relative) = %q, want %q", got, want)
	}
}

func TestNormalizePath_AbsoluteInputRelativizedAgainstRoot(t *testing.T) {
	got := NormalizePath("/workspace/semantica/internal/service/pre-push.go", "/workspace/semantica")
	want := "internal/service/pre-push.go"
	if got != want {
		t.Errorf("NormalizePath(absolute) = %q, want %q", got, want)
	}
}

func TestExtractDeletedPaths_RelativeRMArgumentsKeepDirectory(t *testing.T) {
	cmd := "rm internal/service/pre-push.go internal/service/pre-push_test.go && ls internal/service/pre-push*"
	got := ExtractDeletedPaths(cmd, "/workspace/semantica")

	want := map[string]bool{
		"internal/service/pre-push.go":      true,
		"internal/service/pre-push_test.go": true,
	}
	for _, p := range got {
		if want[p] {
			delete(want, p)
		}
	}
	if len(want) > 0 {
		t.Errorf("ExtractDeletedPaths missed paths: %v (got %v)", want, got)
	}
}
