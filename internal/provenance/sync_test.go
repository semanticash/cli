package provenance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"
)

func TestFormatLoadOrRedactReason_RedactErrorHasStablePrefix(t *testing.T) {
	got := formatLoadOrRedactReason("prompt", "abc12345deadbeef", &redactError{err: errors.New("init failed")})
	const want = "redaction failed: prompt: init failed"
	if got != want {
		t.Errorf("formatLoadOrRedactReason(redact) = %q, want %q", got, want)
	}
}

func TestFormatLoadOrRedactReason_RedactErrorAcrossKinds(t *testing.T) {
	cases := []string{"prompt", "step_provenance", "bundle"}
	for _, kind := range cases {
		t.Run(kind, func(t *testing.T) {
			got := formatLoadOrRedactReason(kind, "abc12345", &redactError{err: errors.New("apply failed")})
			wantPrefix := "redaction failed: " + kind + ": "
			if !strings.HasPrefix(got, wantPrefix) {
				t.Errorf("formatLoadOrRedactReason(%s) = %q, want prefix %q", kind, got, wantPrefix)
			}
		})
	}
}

func TestFormatLoadOrRedactReason_LoadErrorIsNotConflatedWithRedaction(t *testing.T) {
	got := formatLoadOrRedactReason("step_provenance", "deadbeef12345678", &loadError{err: errors.New("not found")})
	if strings.HasPrefix(got, "redaction failed:") {
		t.Errorf("load error should not use redaction-failed prefix, got %q", got)
	}
	if !strings.Contains(got, "deadbeef") {
		t.Errorf("expected hash prefix in load-error reason, got %q", got)
	}
}

func TestFormatLoadOrRedactReason_TruncatesHashTo8Chars(t *testing.T) {
	got := formatLoadOrRedactReason("step_provenance", "0123456789abcdef", &loadError{err: errors.New("not found")})
	if !strings.Contains(got, "01234567 ") {
		t.Errorf("expected 8-char hash prefix in reason, got %q", got)
	}
	if strings.Contains(got, "0123456789") {
		t.Errorf("hash should be truncated to 8 chars, got %q", got)
	}
}

func TestFormatLoadOrRedactReason_ShortHashNotTruncated(t *testing.T) {
	got := formatLoadOrRedactReason("prompt", "abc", &loadError{err: errors.New("not found")})
	if !strings.Contains(got, " abc ") {
		t.Errorf("short hash should be passed through, got %q", got)
	}
}

func TestFormatLoadOrRedactReason_UntaggedErrorFallback(t *testing.T) {
	got := formatLoadOrRedactReason("prompt", "abc12345", errors.New("something else"))
	if strings.HasPrefix(got, "redaction failed:") {
		t.Errorf("untagged error should not use redaction-failed prefix, got %q", got)
	}
}

func TestRedactionFailedReason_StablePrefix(t *testing.T) {
	cases := []struct {
		kind string
		want string
	}{
		{"prompt", "redaction failed: prompt: boom"},
		{"step_provenance", "redaction failed: step_provenance: boom"},
		{"bundle", "redaction failed: bundle: boom"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			got := redactionFailedReason(tc.kind, errors.New("boom"))
			if got != tc.want {
				t.Errorf("redactionFailedReason(%q, boom) = %q, want %q", tc.kind, got, tc.want)
			}
		})
	}
}

func TestFormatLoadOrRedactReason_RoutesRedactErrorsThroughHelper(t *testing.T) {
	wrapped := errors.New("apply failed")
	got := formatLoadOrRedactReason("prompt", "abc12345", &redactError{err: wrapped})
	want := redactionFailedReason("prompt", wrapped)
	if got != want {
		t.Errorf("formatLoadOrRedactReason redact path = %q, want %q (must equal redactionFailedReason output)", got, want)
	}
}

// TestSyncPendingTurns_WatermarkSemantics covers the packaged-manifest
// watermark filter. A non-zero watermark is an upper bound on
// created_at; watermark=0 drains all packaged manifests.
func TestSyncPendingTurns_WatermarkSemantics(t *testing.T) {
	ctx := context.Background()
	const manifestCreatedAt int64 = 2000

	cases := []struct {
		name      string
		watermark int64
		wantCount int
	}{
		{
			name:      "watermark earlier than manifest excludes it",
			watermark: 1000,
			wantCount: 0,
		},
		{
			name:      "watermark=0 drains all packaged manifests",
			watermark: 0,
			wantCount: 1,
		},
		{
			name:      "watermark later than manifest includes it",
			watermark: 3000,
			wantCount: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Each subtest gets its own repo because the fixture omits
			// the bundle blob, so a returned manifest is marked failed.
			repoRoot := setupPackagedManifestRepo(t, ctx, manifestCreatedAt)
			results, err := SyncPendingTurns(ctx, repoRoot, tc.watermark, 50)
			if err != nil {
				t.Fatalf("SyncPendingTurns(watermark=%d): %v", tc.watermark, err)
			}
			if len(results) != tc.wantCount {
				t.Errorf("watermark=%d: got %d results, want %d", tc.watermark, len(results), tc.wantCount)
			}
		})
	}
}

// setupPackagedManifestRepo creates a temp repo with one packaged
// provenance manifest. The handle is closed before returning so
// SyncPendingTurns can open its own.
func setupPackagedManifestRepo(t *testing.T, ctx context.Context, manifestCreatedAt int64) string {
	t.Helper()

	repoRoot := t.TempDir()
	semDir := filepath.Join(repoRoot, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatalf("mkdir .semantica: %v", err)
	}
	// SyncPendingTurns returns early unless settings declare a connected
	// repo. The connected repo ID value is not used by this test.
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
