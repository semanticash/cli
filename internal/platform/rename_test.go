package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSafeRename_NewFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")

	if err := os.WriteFile(src, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SafeRename(src, dst); err != nil {
		t.Fatalf("SafeRename: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "content" {
		t.Errorf("content = %q, want content", got)
	}
}

func TestSafeRename_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")

	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SafeRename(src, dst); err != nil {
		t.Fatalf("SafeRename: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "new" {
		t.Errorf("content = %q, want new", got)
	}
}

func TestSafeRename_IntegrationRetriesOnTransient(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only retry test")
	}
	if testing.Short() {
		t.Skip("integration: retry under contention test")
	}
	t.Skip("TODO: implement retry-under-contention test in Phase 2")
}
