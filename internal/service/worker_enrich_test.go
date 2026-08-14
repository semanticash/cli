package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// setupEnrichRepo creates a worker context backed by a Git repository.
func setupEnrichRepo(t *testing.T) (*workerContext, string) {
	t.Helper()
	ctx := context.Background()
	dir := initGitRepo(t)
	semDir := filepath.Join(dir, ".semantica")
	objectsDir := filepath.Join(semDir, "objects")
	if err := os.MkdirAll(objectsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(semDir, "lineage.db")
	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatal(err)
	}
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlstore.Close(h) })

	repoID := "repo-enrich"
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: dir, CreatedAt: 1, EnabledAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: "ck-enrich", RepositoryID: repoID, CreatedAt: 1,
		Kind: "auto", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	cp, err := h.Queries.GetCheckpointByID(ctx, "ck-enrich")
	if err != nil {
		t.Fatal(err)
	}

	repo, err := git.OpenRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	bs, err := blobs.NewStore(objectsDir)
	if err != nil {
		t.Fatal(err)
	}
	return &workerContext{h: h, blobStore: bs, repo: repo, cp: cp, semDir: semDir}, dir
}

func gitCommit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	rev := exec.Command("git", "rev-parse", "HEAD")
	rev.Dir = dir
	out, err := rev.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func enrichStats(t *testing.T, h *sqlstore.Handle) sqldb.CheckpointStat {
	t.Helper()
	stats, err := h.Queries.GetCheckpointStats(context.Background(), "ck-enrich")
	if err != nil {
		t.Fatal(err)
	}
	return stats
}

// An empty diff records a completed result with no attribution.
func TestComputeEnrichmentAttribution_EmptyDiffRecordsVersion(t *testing.T) {
	wctx, dir := setupEnrichRepo(t)
	hash := gitCommit(t, dir, "commit", "--allow-empty", "-m", "empty")

	computeEnrichmentAttribution(context.Background(), wctx, WorkerInput{
		CheckpointID: "ck-enrich", CommitHash: hash, RepoRoot: dir,
	}, workerWindows{})

	stats := enrichStats(t, wctx.h)
	if stats.AiPercentage != -1 {
		t.Errorf("ai_percentage = %v, want -1 sentinel", stats.AiPercentage)
	}
	if !stats.AttributionComputedAt.Valid {
		t.Error("attribution_computed_at not recorded")
	}
	if !stats.AttributionVersion.Valid || stats.AttributionVersion.String != "v1" {
		t.Errorf("attribution_version = %+v, want v1", stats.AttributionVersion)
	}
}

// A commit without agent evidence records a completed empty result.
func TestComputeEnrichmentAttribution_NoEventsRecordsVersion(t *testing.T) {
	wctx, dir := setupEnrichRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("human change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommit(t, dir, "add", "file.txt")
	hash := gitCommit(t, dir, "commit", "-m", "change")

	computeEnrichmentAttribution(context.Background(), wctx, WorkerInput{
		CheckpointID: "ck-enrich", CommitHash: hash, RepoRoot: dir,
	}, workerWindows{})

	stats := enrichStats(t, wctx.h)
	if stats.AiPercentage != -1 {
		t.Errorf("ai_percentage = %v, want -1 sentinel", stats.AiPercentage)
	}
	if !stats.AttributionComputedAt.Valid {
		t.Error("attribution_computed_at not recorded")
	}
	if !stats.AttributionVersion.Valid || stats.AttributionVersion.String != "v1" {
		t.Errorf("attribution_version = %+v, want v1", stats.AttributionVersion)
	}
}

// Empty results retain the configured scoring version.
func TestComputeEnrichmentAttribution_V2ConfiguredRecordsV2(t *testing.T) {
	wctx, dir := setupEnrichRepo(t)
	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "1")
	hash := gitCommit(t, dir, "commit", "--allow-empty", "-m", "empty")

	computeEnrichmentAttribution(context.Background(), wctx, WorkerInput{
		CheckpointID: "ck-enrich", CommitHash: hash, RepoRoot: dir,
	}, workerWindows{})

	stats := enrichStats(t, wctx.h)
	if !stats.AttributionVersion.Valid || stats.AttributionVersion.String != "v2" {
		t.Errorf("attribution_version = %+v, want v2", stats.AttributionVersion)
	}
}

// deltaWorkerContext opens the tool-delta fixture for worker attribution.
func deltaWorkerContext(t *testing.T, dir, commitHash string) (*workerContext, string) {
	t.Helper()
	ctx := context.Background()
	semDir := filepath.Join(dir, ".semantica")
	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlstore.Close(h) })
	link, err := h.Queries.GetCommitLinkByCommitHash(ctx, commitHash)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := h.Queries.GetCheckpointByID(ctx, link.CheckpointID)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := git.OpenRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	bs, err := blobs.NewStore(filepath.Join(semDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	return &workerContext{h: h, blobStore: bs, repo: repo, cp: cp, semDir: semDir}, link.CheckpointID
}

// The configured version controls scoring as well as the stored label.
func TestComputeEnrichmentAttribution_V2ScoresDeltaEvidence(t *testing.T) {
	dir, commitHash := setupDeltaRepo(t)
	wctx, cpID := deltaWorkerContext(t, dir, commitHash)
	window := workerWindows{attrWindow: tsWindow(100_000, 300_000)}
	in := WorkerInput{CheckpointID: cpID, CommitHash: commitHash, RepoRoot: dir}
	ctx := context.Background()

	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "0")
	computeEnrichmentAttribution(ctx, wctx, in, window)
	stats, err := wctx.h.Queries.GetCheckpointStats(ctx, cpID)
	if err != nil {
		t.Fatal(err)
	}
	if stats.AiPercentage != 0 {
		t.Errorf("v1 ai_percentage = %v, want 0 (delta evidence unscored)", stats.AiPercentage)
	}
	if !stats.AttributionVersion.Valid || stats.AttributionVersion.String != "v1" {
		t.Errorf("attribution_version = %+v, want v1", stats.AttributionVersion)
	}

	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "1")
	computeEnrichmentAttribution(ctx, wctx, in, window)
	stats, err = wctx.h.Queries.GetCheckpointStats(ctx, cpID)
	if err != nil {
		t.Fatal(err)
	}
	if stats.AiPercentage <= 0 {
		t.Errorf("v2 ai_percentage = %v, want > 0 from delta-backed lines", stats.AiPercentage)
	}
	if !stats.AttributionVersion.Valid || stats.AttributionVersion.String != "v2" {
		t.Errorf("attribution_version = %+v, want v2", stats.AttributionVersion)
	}
}

// The upsert stores attribution together and preserves unrelated stats.
func TestRecordCheckpointAttribution_AtomicUpsert(t *testing.T) {
	_, h, repoID := setupQueueRepo(t)
	ctx := context.Background()
	insertPendingLinked(t, h, repoID, "ck-atomic", "c-atomic", 1)
	if err := h.Queries.UpsertCheckpointStats(ctx, sqldb.UpsertCheckpointStatsParams{
		CheckpointID: "ck-atomic", SessionCount: 5, FilesChanged: 9,
	}); err != nil {
		t.Fatal(err)
	}

	if err := recordAttribution(ctx, h, "ck-atomic", 63, "v2"); err != nil {
		t.Fatal(err)
	}

	stats, err := h.Queries.GetCheckpointStats(ctx, "ck-atomic")
	if err != nil {
		t.Fatal(err)
	}
	if stats.AiPercentage != 63 || !stats.AttributionComputedAt.Valid ||
		!stats.AttributionVersion.Valid || stats.AttributionVersion.String != "v2" {
		t.Errorf("stats = %+v, want percentage, timestamp, and version all set", stats)
	}
	if stats.SessionCount != 5 || stats.FilesChanged != 9 {
		t.Errorf("session_count=%d files_changed=%d, want 5 and 9 preserved", stats.SessionCount, stats.FilesChanged)
	}

	// A missing stats row is created rather than silently matching nothing.
	insertPendingLinked(t, h, repoID, "ck-fresh", "c-fresh", 2)
	if err := recordAttribution(ctx, h, "ck-fresh", -1, "v1"); err != nil {
		t.Fatal(err)
	}
	fresh, err := h.Queries.GetCheckpointStats(ctx, "ck-fresh")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.AiPercentage != -1 || !fresh.AttributionComputedAt.Valid ||
		!fresh.AttributionVersion.Valid || fresh.AttributionVersion.String != "v1" {
		t.Errorf("fresh stats = %+v, want created with all three columns", fresh)
	}
}
