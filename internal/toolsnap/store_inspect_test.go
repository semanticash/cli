package toolsnap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// storeInspectionPaths initializes a valid store and returns paths used by
// inspection tests.
func storeInspectionPaths(t *testing.T) (rc RepoContext, semDir, configPath, altPath string) {
	t.Helper()
	ctx := context.Background()
	root := testRepo(t)
	_ = openTestStore(t, root) // initialize a valid store
	rc, err := ResolveRepoContext(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	semDir = filepath.Join(root, ".semantica")
	dir := filepath.Join(semDir, storeDirName)
	return rc, semDir, filepath.Join(dir, "config"), filepath.Join(dir, "objects", "info", "alternates")
}

func TestOpenStoreForInspectionAcceptsValid(t *testing.T) {
	rc, semDir, _, _ := storeInspectionPaths(t)
	if _, err := OpenStoreForInspection(context.Background(), rc, semDir); err != nil {
		t.Fatalf("valid store rejected: %v", err)
	}
}

func TestOpenStoreForInspectionRejectsUnsafeConfig(t *testing.T) {
	rc, semDir, configPath, _ := storeInspectionPaths(t)
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	fetch := string(raw) + "\n[remote \"origin\"]\n\turl = https://example.com/x.git\n"
	if err := os.WriteFile(configPath, []byte(fetch), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStoreForInspection(context.Background(), rc, semDir); !errors.Is(err, ErrStoreIncompatible) {
		t.Fatalf("err = %v, want ErrStoreIncompatible", err)
	}
}

func TestOpenStoreForInspectionRejectsInvalidAlternate(t *testing.T) {
	rc, semDir, _, altPath := storeInspectionPaths(t)
	if err := os.WriteFile(altPath, []byte("/some/other/objects\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStoreForInspection(context.Background(), rc, semDir); !errors.Is(err, ErrStoreIncompatible) {
		t.Fatalf("err = %v, want ErrStoreIncompatible", err)
	}
}

func TestOpenStoreForInspectionRejectsGenericBareConfig(t *testing.T) {
	rc, semDir, configPath, _ := storeInspectionPaths(t)
	// A generic bare repository lacks the required snapshot-store settings.
	generic := t.TempDir()
	run(t, generic, "git", "init", "--bare", "-q", ".")
	cfg, err := os.ReadFile(filepath.Join(generic, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStoreForInspection(context.Background(), rc, semDir); !errors.Is(err, ErrStoreIncompatible) {
		t.Fatalf("err = %v, want ErrStoreIncompatible", err)
	}
}

func TestOpenStoreForInspectionRejectsSymlink(t *testing.T) {
	ctx := context.Background()
	root := testRepo(t)
	openTestStore(t, root)
	rc, err := ResolveRepoContext(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	semDir := filepath.Join(root, ".semantica")
	realStore := filepath.Join(semDir, storeDirName)

	// Redirect the expected path to an otherwise valid store.
	redirect := filepath.Join(root, "redirected-store.git")
	if err := os.Rename(realStore, redirect); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(redirect, realStore); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStoreForInspection(ctx, rc, semDir); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err = %v, want symlink rejection", err)
	}
}

func TestOpenRegistryForInspectionRejectsSymlink(t *testing.T) {
	root := testRepo(t)
	semDir := filepath.Join(root, ".semantica")
	target := filepath.Join(root, "elsewhere-tool-windows")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(semDir, "tool-windows")); err != nil {
		t.Fatal(err)
	}
	_, err := OpenRegistryForInspection(semDir)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err = %v, want symlink rejection", err)
	}
}
