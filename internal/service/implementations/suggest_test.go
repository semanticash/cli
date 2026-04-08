package implementations

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/llm"
	"github.com/semanticash/cli/internal/store/impldb"
	impldbgen "github.com/semanticash/cli/internal/store/impldb/db"
)

func TestApplyTitle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_HOME", dir)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	dbPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	id := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: id, CreatedAt: now, LastActivityAt: now,
	})
	_ = impldb.Close(h)

	if err := ApplyTitle(ctx, id, "Migrate auth to OAuth2"); err != nil {
		t.Fatalf("apply title: %v", err)
	}

	h, _ = impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	defer func() { _ = impldb.Close(h) }()

	impl, _ := h.Queries.GetImplementation(ctx, id)
	if !impl.Title.Valid || impl.Title.String != "Migrate auth to OAuth2" {
		t.Errorf("title: got %v", impl.Title)
	}
}

func TestApplyTitle_ShortID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_HOME", dir)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	dbPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	id := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: id, CreatedAt: now, LastActivityAt: now,
	})
	_ = impldb.Close(h)

	if err := ApplyTitle(ctx, id[:8], "Short ID title"); err != nil {
		t.Fatalf("apply title by short id: %v", err)
	}

	h, _ = impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	defer func() { _ = impldb.Close(h) }()

	impl, _ := h.Queries.GetImplementation(ctx, id)
	if !impl.Title.Valid || impl.Title.String != "Short ID title" {
		t.Errorf("title: got %v", impl.Title)
	}
}

func TestSuggestForImplementation_WithMockLLM(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_HOME", dir)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	dbPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	id := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: id, CreatedAt: now, LastActivityAt: now,
	})
	_ = h.Queries.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
		ImplementationID: id, CanonicalPath: "/repos/api", DisplayName: "api",
		RepoRole: "origin", FirstSeenAt: now, LastSeenAt: now,
	})
	_ = impldb.Close(h)

	// Mock LLM that returns a fixed JSON response.
	mockLLM := func(_ context.Context, prompt string) (*llm.GenerateTextResult, error) {
		return &llm.GenerateTextResult{
			Text:     `{"title": "Add OAuth2 middleware", "summary": "Added OAuth2 support.", "review_priority": []}`,
			Provider: "mock",
			Model:    "test",
		}, nil
	}

	svc := &SuggestService{GenerateText: mockLLM}
	res, err := svc.SuggestForImplementation(ctx, id)
	if err != nil {
		t.Fatalf("suggest: %v", err)
	}
	if res.Title != "Add OAuth2 middleware" {
		t.Errorf("title: got %q", res.Title)
	}
	if res.Provider != "mock" {
		t.Errorf("provider: got %q", res.Provider)
	}
}

func TestSuggestBatch_WithMockLLM(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_HOME", dir)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	dbPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())

	// Create two implementations, one untitled.
	id1 := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: id1, CreatedAt: now, LastActivityAt: now,
	})
	id2 := uuid.NewString()
	_ = h.Queries.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: id2, CreatedAt: now, LastActivityAt: now,
	})
	_ = h.Queries.UpdateImplementationTitle(ctx, impldbgen.UpdateImplementationTitleParams{
		Title: impldb.NullStr("Already titled"), ImplementationID: id2,
	})
	_ = impldb.Close(h)

	mockLLM := func(_ context.Context, prompt string) (*llm.GenerateTextResult, error) {
		return &llm.GenerateTextResult{
			Text: `{"titles": [{"implementation_id": "` + id1[:8] + `", "title": "Fix rate limiting"}], "merges": []}`,
		}, nil
	}

	svc := &SuggestService{GenerateText: mockLLM}
	res, err := svc.SuggestBatch(ctx)
	if err != nil {
		t.Fatalf("suggest batch: %v", err)
	}
	if len(res.Titles) != 1 {
		t.Errorf("titles: got %d", len(res.Titles))
	}
}

func TestSuggestBatch_Empty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_HOME", dir)
	ctx := context.Background()

	// Create empty DB — no implementations.
	dbPath := filepath.Join(dir, "implementations.db")
	h, _ := impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	_ = impldb.Close(h)

	mockLLM := func(_ context.Context, prompt string) (*llm.GenerateTextResult, error) {
		t.Fatal("LLM should not be called with no implementations")
		return nil, nil
	}

	svc := &SuggestService{GenerateText: mockLLM}
	res, err := svc.SuggestBatch(ctx)
	if err != nil {
		t.Fatalf("suggest batch: %v", err)
	}
	if len(res.Titles) != 0 || len(res.Merges) != 0 {
		t.Errorf("expected empty result, got titles=%d merges=%d", len(res.Titles), len(res.Merges))
	}
}

func init() {
	_ = os.Setenv("SEMANTICA_HOME", "/dev/null/nonexistent")
}
