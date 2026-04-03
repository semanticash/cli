package broker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestGetStatus_NoRegistry(t *testing.T) {
	dir := t.TempDir()
	registryPath := filepath.Join(dir, "nonexistent", "repos.json")

	ctx := context.Background()
	status, err := getStatusFromPath(ctx, registryPath)
	if err != nil {
		t.Fatalf("GetStatus with no registry: %v", err)
	}
	if len(status.Repos) != 0 {
		t.Errorf("expected 0 repos, got %d", len(status.Repos))
	}
	if status.ActiveCount != 0 {
		t.Errorf("expected 0 active, got %d", status.ActiveCount)
	}
}

func TestGetStatus_WithRepos(t *testing.T) {
	dir := t.TempDir()
	registryPath := filepath.Join(dir, "repos.json")
	ctx := context.Background()

	// Create fake repo dirs with .semantica so prune doesn't remove them.
	cliRepo := filepath.Join(dir, "cli")
	apiRepo := filepath.Join(dir, "api")
	if err := os.MkdirAll(filepath.Join(cliRepo, ".semantica"), 0o755); err != nil {
		t.Fatalf("mkdir cli: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(apiRepo, ".semantica"), 0o755); err != nil {
		t.Fatalf("mkdir api: %v", err)
	}

	h, err := Open(ctx, registryPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if err := Register(ctx, h, cliRepo, cliRepo); err != nil {
		t.Fatalf("register cli: %v", err)
	}
	if err := Register(ctx, h, apiRepo, apiRepo); err != nil {
		t.Fatalf("register api: %v", err)
	}
	if err := Deactivate(ctx, h, apiRepo); err != nil {
		t.Fatalf("deactivate api: %v", err)
	}

	// Re-read from disk via getStatusFromPath.
	status, err := getStatusFromPath(ctx, registryPath)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}

	if status.ActiveCount != 1 {
		t.Errorf("expected 1 active, got %d", status.ActiveCount)
	}
	if len(status.Repos) != 2 {
		t.Errorf("expected 2 repos total, got %d", len(status.Repos))
	}

	for _, r := range status.Repos {
		switch r.CanonicalPath {
		case cliRepo:
			if !r.Active {
				t.Error("cli repo should be active")
			}
		case apiRepo:
			if r.Active {
				t.Error("api repo should be inactive")
			}
		default:
			t.Errorf("unexpected repo: %s", r.CanonicalPath)
		}
	}
}

func TestGetStatus_PrunesStaleRepos(t *testing.T) {
	dir := t.TempDir()
	registryPath := filepath.Join(dir, "repos.json")
	ctx := context.Background()

	// Create two repos, but only one has .semantica on disk.
	liveRepo := filepath.Join(dir, "live")
	staleRepo := filepath.Join(dir, "stale")
	if err := os.MkdirAll(filepath.Join(liveRepo, ".semantica"), 0o755); err != nil {
		t.Fatalf("mkdir live: %v", err)
	}
	// staleRepo has no .semantica - simulates manual deletion.

	h, err := Open(ctx, registryPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if err := Register(ctx, h, liveRepo, liveRepo); err != nil {
		t.Fatalf("register live: %v", err)
	}
	if err := Register(ctx, h, staleRepo, staleRepo); err != nil {
		t.Fatalf("register stale: %v", err)
	}

	status, err := getStatusFromPath(ctx, registryPath)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}

	if len(status.Repos) != 1 {
		t.Errorf("expected 1 repo after prune, got %d", len(status.Repos))
	}
	if status.ActiveCount != 1 {
		t.Errorf("expected 1 active, got %d", status.ActiveCount)
	}
	if len(status.Repos) > 0 && status.Repos[0].Path != liveRepo {
		t.Errorf("expected live repo, got %s", status.Repos[0].Path)
	}
}
