package broker

import (
	"context"
	"path/filepath"
	"testing"
)

func tempRegistry(t *testing.T) (*Handle, string) {
	t.Helper()
	dir := t.TempDir()
	registryPath := filepath.Join(dir, "repos.json")
	ctx := context.Background()
	h, err := Open(ctx, registryPath)
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	return h, registryPath
}

func TestRegister_NewRepo(t *testing.T) {
	h, _ := tempRegistry(t)
	ctx := context.Background()

	if err := Register(ctx, h, "/workspace/cli", "/workspace/cli"); err != nil {
		t.Fatalf("register: %v", err)
	}

	found := false
	for _, r := range h.registry.Repos {
		if r.CanonicalPath == "/workspace/cli" {
			found = true
			if !r.Active {
				t.Error("expected active")
			}
			if r.Path != "/workspace/cli" {
				t.Errorf("expected path=/workspace/cli, got %s", r.Path)
			}
		}
	}
	if !found {
		t.Fatal("repo not found after register")
	}
}

func TestRegister_Reactivate(t *testing.T) {
	h, _ := tempRegistry(t)
	ctx := context.Background()

	if err := Register(ctx, h, "/proj", "/proj"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := Deactivate(ctx, h, "/proj"); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	for _, r := range h.registry.Repos {
		if r.CanonicalPath == "/proj" && r.Active {
			t.Fatal("expected inactive after deactivate")
		}
	}

	// Re-register should reactivate.
	if err := Register(ctx, h, "/proj", "/proj"); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	for _, r := range h.registry.Repos {
		if r.CanonicalPath == "/proj" {
			if !r.Active {
				t.Error("expected active after re-register")
			}
			if r.DisabledAt != nil {
				t.Error("expected disabled_at=nil after re-register")
			}
		}
	}
}

func TestDeactivate_MarksInactive(t *testing.T) {
	h, _ := tempRegistry(t)
	ctx := context.Background()

	if err := Register(ctx, h, "/proj", "/proj"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := Deactivate(ctx, h, "/proj"); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	for _, r := range h.registry.Repos {
		if r.CanonicalPath == "/proj" {
			if r.Active {
				t.Error("expected inactive")
			}
			if r.DisabledAt == nil {
				t.Error("expected disabled_at to be set")
			}
		}
	}
}

func TestDeactivate_NonexistentRepo_NoError(t *testing.T) {
	h, _ := tempRegistry(t)
	ctx := context.Background()

	if err := Deactivate(ctx, h, "/nonexistent"); err != nil {
		t.Fatalf("deactivate nonexistent: %v", err)
	}
}

func TestListActiveRepos_FiltersInactive(t *testing.T) {
	h, _ := tempRegistry(t)
	ctx := context.Background()

	if err := Register(ctx, h, "/a", "/a"); err != nil {
		t.Fatalf("register /a: %v", err)
	}
	if err := Register(ctx, h, "/b", "/b"); err != nil {
		t.Fatalf("register /b: %v", err)
	}
	if err := Register(ctx, h, "/c", "/c"); err != nil {
		t.Fatalf("register /c: %v", err)
	}
	if err := Deactivate(ctx, h, "/b"); err != nil {
		t.Fatalf("deactivate /b: %v", err)
	}

	repos, err := ListActiveRepos(ctx, h)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(repos) != 2 {
		t.Fatalf("expected 2 active repos, got %d", len(repos))
	}

	paths := map[string]bool{}
	for _, r := range repos {
		paths[r.CanonicalPath] = true
	}
	if !paths["/a"] || !paths["/c"] {
		t.Errorf("expected /a and /c in active repos, got %v", paths)
	}
}

func TestOpen_Persistence(t *testing.T) {
	dir := t.TempDir()
	registryPath := filepath.Join(dir, "repos.json")
	ctx := context.Background()

	// Write.
	h1, err := Open(ctx, registryPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := Register(ctx, h1, "/proj", "/proj"); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Re-open and verify.
	h2, err := Open(ctx, registryPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	repos, err := ListActiveRepos(ctx, h2)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(repos) != 1 || repos[0].CanonicalPath != "/proj" {
		t.Errorf("expected /proj after reopen, got %v", repos)
	}
}

func TestCanonicalRepoPath(t *testing.T) {
	got := filepath.ToSlash(CanonicalRepoPath("/workspace/./projects/../projects/cli/"))
	want := "/workspace/projects/cli"
	if got != want {
		t.Errorf("CanonicalRepoPath = %q, want %q", got, want)
	}
}
