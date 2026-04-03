package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadPRTemplate_GitHubLocation(t *testing.T) {
	dir := t.TempDir()
	ghDir := filepath.Join(dir, ".github")
	if err := os.MkdirAll(ghDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "## Summary\n\n<!-- describe -->\n\n## Test plan\n\n- [ ] tests pass"
	if err := os.WriteFile(filepath.Join(ghDir, "pull_request_template.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got := readPRTemplate(dir)
	if got == "" {
		t.Fatal("expected non-empty template")
	}
	if contains(got, "<!--") {
		t.Error("HTML comments should be stripped")
	}
	if !contains(got, "## Summary") {
		t.Error("heading should be preserved")
	}
	if !contains(got, "- [ ] tests pass") {
		t.Error("checklist should be preserved")
	}
}

func TestReadPRTemplate_NotFound(t *testing.T) {
	got := readPRTemplate(t.TempDir())
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestReadPRTemplate_DocsLocation(t *testing.T) {
	dir := t.TempDir()
	docsDir := filepath.Join(dir, "docs")
	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docsDir, "pull_request_template.md"), []byte("## Docs template"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := readPRTemplate(dir)
	if !contains(got, "Docs template") {
		t.Errorf("expected docs/ template, got %q", got)
	}
}

func TestReadPRTemplate_Subdirectory(t *testing.T) {
	dir := t.TempDir()
	tmplDir := filepath.Join(dir, ".github", "PULL_REQUEST_TEMPLATE")
	if err := os.MkdirAll(tmplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmplDir, "default.md"), []byte("## Subdir template"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := readPRTemplate(dir)
	if !contains(got, "Subdir template") {
		t.Errorf("expected subdirectory template, got %q", got)
	}
}

func TestReadPRTemplate_FallbackLocations(t *testing.T) {
	dir := t.TempDir()
	content := "## Root template"
	if err := os.WriteFile(filepath.Join(dir, "PULL_REQUEST_TEMPLATE.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got := readPRTemplate(dir)
	if !contains(got, "Root template") {
		t.Errorf("expected root-level template, got %q", got)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
