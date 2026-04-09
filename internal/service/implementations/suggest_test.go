package implementations

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/llm"
	"github.com/semanticash/cli/internal/store/impldb"
	impldbgen "github.com/semanticash/cli/internal/store/impldb/db"
)

func TestApplySuggestion(t *testing.T) {
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

	if err := ApplySuggestion(ctx, id, "Migrate auth to OAuth2", "Moves API and SDK auth to OAuth2."); err != nil {
		t.Fatalf("apply suggestion: %v", err)
	}

	h, _ = impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	defer func() { _ = impldb.Close(h) }()

	impl, _ := h.Queries.GetImplementation(ctx, id)
	if !impl.Title.Valid || impl.Title.String != "Migrate auth to OAuth2" {
		t.Errorf("title: got %v", impl.Title)
	}
	if !impl.MetadataJson.Valid || !strings.Contains(impl.MetadataJson.String, `"summary":"Moves API and SDK auth to OAuth2."`) {
		t.Errorf("metadata_json: got %v", impl.MetadataJson)
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

func TestImplementationMetadataWithSummary_PreservesExistingFields(t *testing.T) {
	got, err := implementationMetadataWithSummary(
		impldb.NullStr(`{"tag":"cross-repo","summary":"old"}`),
		"New summary",
	)
	if err != nil {
		t.Fatalf("metadata with summary: %v", err)
	}
	if !got.Valid {
		t.Fatal("expected metadata json to stay valid")
	}
	if !strings.Contains(got.String, `"tag":"cross-repo"`) {
		t.Fatalf("expected existing metadata to be preserved: %s", got.String)
	}
	if !strings.Contains(got.String, `"summary":"New summary"`) {
		t.Fatalf("expected summary to be updated: %s", got.String)
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
			Text:     `{"title": "Add OAuth2 middleware", "summary": "Added OAuth2 support."}`,
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

func TestSuggestTopFileChanges_FiltersInternalPaths(t *testing.T) {
	detail := &ImplementationDetail{
		Timeline: []TimelineEntry{
			{RepoName: "pulse-api", FilePath: ".claude/settings.json", FileOp: "edited"},
			{RepoName: "pulse-api", FilePath: "src/roadmap.ts", FileOp: "edited"},
			{RepoName: "pulse-web", FilePath: "README.md", FileOp: "edited"},
		},
	}

	got := suggestTopFileChanges(detail, 10)
	if len(got) != 2 {
		t.Fatalf("top file changes: got %d want 2", len(got))
	}
	if got[0] != "pulse-web README.md (edited)" {
		t.Fatalf("unexpected first file change: %q", got[0])
	}
	if got[1] != "pulse-api src/roadmap.ts (edited)" {
		t.Fatalf("unexpected second file change: %q", got[1])
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

	// Create an empty DB with no implementations.
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
