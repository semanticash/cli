package implementations

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/store/impldb"
	impldbgen "github.com/semanticash/cli/internal/store/impldb/db"
)

func setupListDB(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SEMANTICA_HOME", dir)

	ctx := context.Background()
	dbPath := filepath.Join(dir, "implementations.db")
	h, err := impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UnixMilli()

	// Active multi-repo implementation.
	implA := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: implA, CreatedAt: now, LastActivityAt: now,
	})
	_ = h.Queries.UpdateImplementationTitle(ctx, impldbgen.UpdateImplementationTitleParams{
		Title: impldb.NullStr("Migrate auth to OAuth2"), ImplementationID: implA,
	})
	_ = h.Queries.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
		ImplementationID: implA, CanonicalPath: "/repos/api", DisplayName: "api",
		RepoRole: "origin", FirstSeenAt: now, LastSeenAt: now,
	})
	_ = h.Queries.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
		ImplementationID: implA, CanonicalPath: "/repos/sdk", DisplayName: "sdk",
		RepoRole: "downstream", FirstSeenAt: now, LastSeenAt: now,
	})

	// Closed multi-repo implementation.
	implD := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: implD, CreatedAt: now - 3600_000, LastActivityAt: now - 3600_000,
	})
	_ = h.Queries.UpdateImplementationState(ctx, impldbgen.UpdateImplementationStateParams{
		State: "closed", ImplementationID: implD,
	})
	_ = h.Queries.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
		ImplementationID: implD, CanonicalPath: "/repos/web", DisplayName: "web",
		RepoRole: "origin", FirstSeenAt: now - 3600_000, LastSeenAt: now - 3600_000,
	})
	_ = h.Queries.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
		ImplementationID: implD, CanonicalPath: "/repos/docs", DisplayName: "docs",
		RepoRole: "downstream", FirstSeenAt: now - 3600_000, LastSeenAt: now - 3600_000,
	})

	// Dormant single-repo implementation.
	implB := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: implB, CreatedAt: now - 7200_000, LastActivityAt: now - 7200_000,
	})
	_ = h.Queries.UpdateImplementationState(ctx, impldbgen.UpdateImplementationStateParams{
		State: "dormant", ImplementationID: implB,
	})
	_ = h.Queries.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
		ImplementationID: implB, CanonicalPath: "/repos/cli", DisplayName: "cli",
		RepoRole: "origin", FirstSeenAt: now - 7200_000, LastSeenAt: now - 7200_000,
	})

	// Active single-repo implementation.
	implC := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: implC, CreatedAt: now - 100, LastActivityAt: now - 100,
	})
	_ = h.Queries.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
		ImplementationID: implC, CanonicalPath: "/repos/docs", DisplayName: "docs",
		RepoRole: "origin", FirstSeenAt: now - 100, LastSeenAt: now - 100,
	})

	_ = impldb.Close(h)
}

func TestList_Default_CrossRepoFocus(t *testing.T) {
	setupListDB(t)
	ctx := context.Background()

	result, err := List(ctx, ListInput{Limit: 20})
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	// Default: cross-repo only across all states.
	// implA (multi-repo active) and implD (multi-repo closed) should appear.
	// implB (single-repo dormant) and implC (single-repo active) should NOT appear.
	if result.Total != 2 {
		t.Errorf("got %d items, want 2 (cross-repo only across all states)", result.Total)
		for _, item := range result.Items {
			t.Logf("  %s state=%s repos=%d", item.Title, item.State, item.RepoCount)
		}
	}
}

func TestList_IncludeSingle(t *testing.T) {
	setupListDB(t)
	ctx := context.Background()

	result, err := List(ctx, ListInput{Limit: 20, IncludeSingle: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	// IncludeSingle with active+dormant: active+dormant across repo counts, not closed.
	if result.Total != 3 {
		t.Errorf("got %d items, want 3", result.Total)
	}
}

func TestList_All(t *testing.T) {
	setupListDB(t)
	ctx := context.Background()

	result, err := List(ctx, ListInput{Limit: 20, All: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	// All: all 4.
	if result.Total != 4 {
		t.Errorf("got %d items, want 4", result.Total)
	}
}

func TestList_OriginRepoFirst(t *testing.T) {
	setupListDB(t)
	ctx := context.Background()

	result, _ := List(ctx, ListInput{Limit: 20})

	// Find the multi-repo implementation.
	for _, item := range result.Items {
		if item.RepoCount > 1 {
			if len(item.Repos) != 2 {
				t.Fatalf("expected 2 repos, got %d", len(item.Repos))
			}
			if item.Repos[0].Role != "origin" {
				t.Errorf("expected origin repo first, got %q", item.Repos[0].Role)
			}
			return
		}
	}
	t.Error("multi-repo implementation not found in results")
}

func TestList_EmptyDB(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_HOME", dir)
	// No DB file at all.

	ctx := context.Background()
	result, err := List(ctx, ListInput{Limit: 20})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if result.Total != 0 {
		t.Errorf("got %d items, want 0", result.Total)
	}
}

func TestList_NoSEMANTICA_HOME(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", "/nonexistent")

	ctx := context.Background()
	result, err := List(ctx, ListInput{Limit: 20})
	if err != nil {
		t.Fatalf("expected empty result, got error: %v", err)
	}
	if result.Total != 0 {
		t.Errorf("got %d items, want 0", result.Total)
	}
}

func init() {
	// Ensure tests don't accidentally touch real SEMANTICA_HOME.
	// Each test sets its own via t.Setenv.
	_ = os.Setenv("SEMANTICA_HOME", "/dev/null/nonexistent")
}
