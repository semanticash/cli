package implementations

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/store/impldb"
	impldbgen "github.com/semanticash/cli/internal/store/impldb/db"
)

func setupAutoSummaryDB(t *testing.T) (*impldb.Handle, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SEMANTICA_HOME", dir)
	ctx := context.Background()
	dbPath := filepath.Join(dir, "implementations.db")
	h, err := impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = impldb.Close(h) })
	return h, dir
}

func insertImplWithRepos(t *testing.T, ctx context.Context, q *impldbgen.Queries, repoCount int) string {
	t.Helper()
	id := uuid.NewString()
	now := time.Now().UnixMilli()
	_ = q.InsertImplementation(ctx, impldbgen.InsertImplementationParams{
		ImplementationID: id, CreatedAt: now, LastActivityAt: now,
	})
	for i := 0; i < repoCount; i++ {
		_ = q.UpsertImplementationRepo(ctx, impldbgen.UpsertImplementationRepoParams{
			ImplementationID: id,
			CanonicalPath:    filepath.Join("/repos", uuid.NewString()[:4]),
			DisplayName:      uuid.NewString()[:4],
			RepoRole:         "related",
			FirstSeenAt:      now,
			LastSeenAt:       now,
		})
	}
	return id
}

func TestShouldAutoSummarize_SkipSingleRepo(t *testing.T) {
	h, _ := setupAutoSummaryDB(t)
	ctx := context.Background()

	id := insertImplWithRepos(t, ctx, h.Queries, 1)

	ok, reason := ShouldAutoSummarize(ctx, h, id, ShouldAutoSummarizeOpts{})
	if ok {
		t.Error("expected skip for single-repo implementation")
	}
	if reason == "" {
		t.Error("expected skip reason")
	}
}

func TestShouldAutoSummarize_SkipManualTitle(t *testing.T) {
	h, _ := setupAutoSummaryDB(t)
	ctx := context.Background()

	id := insertImplWithRepos(t, ctx, h.Queries, 2)

	meta := ImplementationMeta{TitleSource: SourceManual}
	encoded, _ := WriteImplementationMeta(meta)
	_ = h.Queries.UpdateImplementationMetadata(ctx, impldbgen.UpdateImplementationMetadataParams{
		MetadataJson: encoded, ImplementationID: id,
	})

	ok, reason := ShouldAutoSummarize(ctx, h, id, ShouldAutoSummarizeOpts{})
	if ok {
		t.Error("expected skip for manual title")
	}
	if reason != "title or summary was manually set" {
		t.Errorf("reason: got %q", reason)
	}
}

func TestShouldAutoSummarize_SkipManualSummary(t *testing.T) {
	h, _ := setupAutoSummaryDB(t)
	ctx := context.Background()

	id := insertImplWithRepos(t, ctx, h.Queries, 2)

	meta := ImplementationMeta{SummarySource: SourceManual}
	encoded, _ := WriteImplementationMeta(meta)
	_ = h.Queries.UpdateImplementationMetadata(ctx, impldbgen.UpdateImplementationMetadataParams{
		MetadataJson: encoded, ImplementationID: id,
	})

	ok, _ := ShouldAutoSummarize(ctx, h, id, ShouldAutoSummarizeOpts{})
	if ok {
		t.Error("expected skip for manual summary")
	}
}

func TestShouldAutoSummarize_SkipScopeUnchanged(t *testing.T) {
	h, _ := setupAutoSummaryDB(t)
	ctx := context.Background()

	id := insertImplWithRepos(t, ctx, h.Queries, 3)

	meta := ImplementationMeta{
		TitleSource:        SourceAuto,
		SummarySource:      SourceAuto,
		GeneratedRepoCount: 3,
	}
	encoded, _ := WriteImplementationMeta(meta)
	_ = h.Queries.UpdateImplementationMetadata(ctx, impldbgen.UpdateImplementationMetadataParams{
		MetadataJson: encoded, ImplementationID: id,
	})

	ok, reason := ShouldAutoSummarize(ctx, h, id, ShouldAutoSummarizeOpts{})
	if ok {
		t.Error("expected skip when scope unchanged")
	}
	if reason == "" {
		t.Error("expected skip reason")
	}
}

func TestShouldAutoSummarize_SkipInProgress(t *testing.T) {
	h, _ := setupAutoSummaryDB(t)
	ctx := context.Background()

	id := insertImplWithRepos(t, ctx, h.Queries, 2)

	meta := ImplementationMeta{
		GenerationInProgressAt: time.Now().UnixMilli(), // just started
	}
	encoded, _ := WriteImplementationMeta(meta)
	_ = h.Queries.UpdateImplementationMetadata(ctx, impldbgen.UpdateImplementationMetadataParams{
		MetadataJson: encoded, ImplementationID: id,
	})

	ok, reason := ShouldAutoSummarize(ctx, h, id, ShouldAutoSummarizeOpts{})
	if ok {
		t.Error("expected skip when generation in progress")
	}
	if reason != "generation already in progress" {
		t.Errorf("reason: got %q", reason)
	}
}

func TestShouldAutoSummarize_StaleInProgressAllowsGeneration(t *testing.T) {
	h, _ := setupAutoSummaryDB(t)
	ctx := context.Background()

	id := insertImplWithRepos(t, ctx, h.Queries, 2)

	meta := ImplementationMeta{
		GenerationInProgressAt: time.Now().Add(-10 * time.Minute).UnixMilli(), // stale
	}
	encoded, _ := WriteImplementationMeta(meta)
	_ = h.Queries.UpdateImplementationMetadata(ctx, impldbgen.UpdateImplementationMetadataParams{
		MetadataJson: encoded, ImplementationID: id,
	})

	ok, _ := ShouldAutoSummarize(ctx, h, id, ShouldAutoSummarizeOpts{})
	if !ok {
		t.Error("stale in-progress marker should not block generation")
	}
}

func TestShouldAutoSummarize_GenerateNewMultiRepo(t *testing.T) {
	h, _ := setupAutoSummaryDB(t)
	ctx := context.Background()

	id := insertImplWithRepos(t, ctx, h.Queries, 2)

	ok, _ := ShouldAutoSummarize(ctx, h, id, ShouldAutoSummarizeOpts{})
	if !ok {
		t.Error("expected generation for new multi-repo implementation")
	}
}

func TestShouldAutoSummarize_GenerateOnNewRepo(t *testing.T) {
	h, _ := setupAutoSummaryDB(t)
	ctx := context.Background()

	id := insertImplWithRepos(t, ctx, h.Queries, 3)

	// Previously generated with 2 repos.
	meta := ImplementationMeta{
		TitleSource:        SourceAuto,
		SummarySource:      SourceAuto,
		GeneratedRepoCount: 2,
	}
	encoded, _ := WriteImplementationMeta(meta)
	_ = h.Queries.UpdateImplementationMetadata(ctx, impldbgen.UpdateImplementationMetadataParams{
		MetadataJson: encoded, ImplementationID: id,
	})

	ok, _ := ShouldAutoSummarize(ctx, h, id, ShouldAutoSummarizeOpts{})
	if !ok {
		t.Error("expected generation when a new repo joined")
	}
}

func TestMarkAndClearGenerationInProgress(t *testing.T) {
	h, _ := setupAutoSummaryDB(t)
	ctx := context.Background()

	id := insertImplWithRepos(t, ctx, h.Queries, 2)

	// Mark.
	if err := MarkGenerationInProgress(ctx, h, id); err != nil {
		t.Fatalf("mark: %v", err)
	}

	impl, _ := h.Queries.GetImplementation(ctx, id)
	meta := ReadImplementationMeta(impl.MetadataJson)
	if meta.GenerationInProgressAt == 0 {
		t.Error("expected in-progress marker to be set")
	}

	// Clear.
	ClearGenerationInProgress(ctx, h, id)

	impl, _ = h.Queries.GetImplementation(ctx, id)
	meta = ReadImplementationMeta(impl.MetadataJson)
	if meta.GenerationInProgressAt != 0 {
		t.Error("expected in-progress marker to be cleared")
	}
}

func TestApplySuggestion_AutoSourceTracksMetadata(t *testing.T) {
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

	err := ApplySuggestion(ctx, ApplySuggestionInput{
		ImplementationID: id,
		Title:            "Auto title",
		Summary:          "Auto summary.",
		Source:           SourceAuto,
		RepoCount:        3,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	h, _ = impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	defer func() { _ = impldb.Close(h) }()

	impl, _ := h.Queries.GetImplementation(ctx, id)
	meta := ReadImplementationMeta(impl.MetadataJson)

	if meta.TitleSource != SourceAuto {
		t.Errorf("title_source: got %q, want auto", meta.TitleSource)
	}
	if meta.SummarySource != SourceAuto {
		t.Errorf("summary_source: got %q, want auto", meta.SummarySource)
	}
	if meta.GeneratedRepoCount != 3 {
		t.Errorf("generated_repo_count: got %d, want 3", meta.GeneratedRepoCount)
	}
	if meta.GeneratedAt == 0 {
		t.Error("expected generated_at to be set")
	}
	if meta.GenerationInProgressAt != 0 {
		t.Error("expected in-progress marker to be cleared")
	}
}

func TestApplySuggestion_ManualSourceSetsFlags(t *testing.T) {
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

	err := ApplySuggestion(ctx, ApplySuggestionInput{
		ImplementationID: id,
		Title:            "Manual title",
		Summary:          "Manual summary.",
		Source:           SourceManual,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	h, _ = impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	defer func() { _ = impldb.Close(h) }()

	impl, _ := h.Queries.GetImplementation(ctx, id)
	meta := ReadImplementationMeta(impl.MetadataJson)

	if meta.TitleSource != SourceManual {
		t.Errorf("title_source: got %q, want manual", meta.TitleSource)
	}
	if meta.SummarySource != SourceManual {
		t.Errorf("summary_source: got %q, want manual", meta.SummarySource)
	}
}

func TestMetadata_PreservesUnknownKeys(t *testing.T) {
	raw := impldb.NullStr(`{"summary":"old","tags":["cross-repo"],"custom_field":42}`)

	meta := ReadImplementationMeta(raw)
	meta.Summary = "new summary"
	meta.TitleSource = SourceAuto

	result, err := WriteImplementationMeta(meta)
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	// Unknown keys should survive.
	if !result.Valid {
		t.Fatal("expected valid metadata")
	}

	// Re-read to verify round-trip.
	var check map[string]any
	if err := json.Unmarshal([]byte(result.String), &check); err != nil {
		t.Fatal(err)
	}

	if check["summary"] != "new summary" {
		t.Errorf("summary: got %v", check["summary"])
	}
	if check["title_source"] != "auto" {
		t.Errorf("title_source: got %v", check["title_source"])
	}
	if check["tags"] == nil {
		t.Error("expected 'tags' to be preserved")
	}
	if check["custom_field"] == nil {
		t.Error("expected 'custom_field' to be preserved")
	}
}

func init() {
	_ = os.Setenv("SEMANTICA_HOME", "/dev/null/nonexistent")
}
