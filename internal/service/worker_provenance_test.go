package service

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"
)

// TestDrainAllPackagedProvenance_PassesZeroWatermark pins the worker
// drain contract: post-completion provenance sync must drain all
// packaged manifests, including manifests packaged after the checkpoint
// timestamp.
func TestDrainAllPackagedProvenance_PassesZeroWatermark(t *testing.T) {
	semDir := t.TempDir()
	if err := util.WriteSettings(semDir, util.Settings{
		Enabled:         true,
		Connected:       true,
		ConnectedRepoID: "test-connected-repo",
	}); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	var called atomic.Int32
	var capturedWatermark atomic.Int64
	capturedWatermark.Store(-1)

	orig := syncProvenanceFn
	t.Cleanup(func() { syncProvenanceFn = orig })
	syncProvenanceFn = func(ctx context.Context, repoRoot string, watermark int64) syncProvenanceResult {
		called.Add(1)
		capturedWatermark.Store(watermark)
		return syncProvenanceResult{}
	}

	drainAllPackagedProvenance(context.Background(), semDir, "/fake/repo")

	if called.Load() == 0 {
		t.Fatal("syncProvenanceFn was not invoked; drainAllPackagedProvenance short-circuited")
	}
	if got := capturedWatermark.Load(); got != 0 {
		t.Errorf("watermark passed to syncProvenanceFn = %d, want 0", got)
	}
}

// TestDrainAllPackagedProvenance_SkipsWhenNotConnected keeps local-only
// repos from doing provenance sync work.
func TestDrainAllPackagedProvenance_SkipsWhenNotConnected(t *testing.T) {
	semDir := t.TempDir()

	var called atomic.Int32
	orig := syncProvenanceFn
	t.Cleanup(func() { syncProvenanceFn = orig })
	syncProvenanceFn = func(ctx context.Context, repoRoot string, watermark int64) syncProvenanceResult {
		called.Add(1)
		return syncProvenanceResult{}
	}

	drainAllPackagedProvenance(context.Background(), semDir, "/fake/repo")

	if called.Load() != 0 {
		t.Errorf("syncProvenanceFn was invoked %d times on unconnected repo; want 0", called.Load())
	}
}

// TestSyncProvenance_WatermarkExcludesLaterManifest covers the
// timestamp boundary used by the worker drain. A watermark earlier than
// the manifest excludes it; watermark=0 admits it.
//
// This test does not exercise the HTTP upload itself. The manifest
// fixture omits a bundle blob, so SyncAndUpload's per-result path
// surfaces it as Failed rather than Uploaded. The assertion here is
// whether the manifest reaches the upload pipeline at all.
func TestSyncProvenance_WatermarkExcludesLaterManifest(t *testing.T) {
	ctx := context.Background()
	const manifestCreatedAt int64 = 2000

	cases := []struct {
		name          string
		watermark     int64
		wantProcessed int
	}{
		{
			name:          "watermark earlier than manifest filters it out",
			watermark:     1000,
			wantProcessed: 0,
		},
		{
			name:          "watermark=0 admits the manifest to the upload pipeline",
			watermark:     0,
			wantProcessed: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repoRoot := setupWorkerProvenanceRepo(t, ctx, manifestCreatedAt)

			// Use API-key auth so the test reaches the watermark
			// boundary without touching the keychain.
			t.Setenv("SEMANTICA_API_KEY", "test-fake-key")
			t.Setenv("SEMANTICA_ENDPOINT", "http://127.0.0.1:1")

			out := syncProvenance(ctx, repoRoot, tc.watermark)
			if out.Processed != tc.wantProcessed {
				t.Errorf("watermark=%d: Processed = %d, want %d", tc.watermark, out.Processed, tc.wantProcessed)
			}
			if out.AuthFailed {
				t.Errorf("watermark=%d: AuthFailed = true; SEMANTICA_API_KEY path should bypass refresh", tc.watermark)
			}
		})
	}
}

// setupWorkerProvenanceRepo creates a temp repo containing a single
// packaged provenance_manifests row with the supplied created_at.
// The fixture is intentionally minimal: no bundle blob is written,
// so the upload pipeline will surface the manifest as a missing-blob
// failure if it reaches that point.
func setupWorkerProvenanceRepo(t *testing.T, ctx context.Context, manifestCreatedAt int64) string {
	t.Helper()

	repoRoot := t.TempDir()
	semDir := filepath.Join(repoRoot, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatalf("mkdir .semantica: %v", err)
	}
	if err := util.WriteSettings(semDir, util.Settings{
		Enabled:         true,
		ConnectedRepoID: "test-connected-repo",
	}); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	dbPath := filepath.Join(semDir, "lineage.db")
	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatalf("migrate lineage.db: %v", err)
	}
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open lineage.db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	repoID := uuid.NewString()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID,
		RootPath:     repoRoot,
		CreatedAt:    manifestCreatedAt,
		EnabledAt:    manifestCreatedAt,
	}); err != nil {
		t.Fatalf("insert repository: %v", err)
	}

	sourceID := uuid.NewString()
	if _, err := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID:     sourceID,
		RepositoryID: repoID,
		Provider:     "test",
		SourceKey:    "test-source",
		LastSeenAt:   manifestCreatedAt,
		CreatedAt:    manifestCreatedAt,
	}); err != nil {
		t.Fatalf("insert source: %v", err)
	}

	sessionID := uuid.NewString()
	if _, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID:         sessionID,
		ProviderSessionID: "provider-session-1",
		RepositoryID:      repoID,
		Provider:          "test",
		SourceID:          sourceID,
		StartedAt:         manifestCreatedAt,
		LastSeenAt:        manifestCreatedAt,
		MetadataJson:      "{}",
	}); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	if err := h.Queries.UpsertProvenanceManifest(ctx, sqldb.UpsertProvenanceManifestParams{
		ManifestID:   uuid.NewString(),
		RepositoryID: repoID,
		SessionID:    sessionID,
		TurnID:       uuid.NewString(),
		Provider:     "test",
		Kind:         "turn_bundle",
		StartedAt:    manifestCreatedAt,
		Status:       "packaged",
		CreatedAt:    manifestCreatedAt,
		UpdatedAt:    manifestCreatedAt,
	}); err != nil {
		t.Fatalf("upsert manifest: %v", err)
	}

	return repoRoot
}
