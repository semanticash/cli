package provenance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/semanticash/cli/internal/store/blobs"
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

func TestExtractResponseHashFromBytes(t *testing.T) {
	got := extractResponseHashFromBytes([]byte(`{"version":2,"response":{"status":"complete","hash":"rh1"}}`))
	if got != "rh1" {
		t.Errorf("complete: got %q, want rh1", got)
	}
	if got := extractResponseHashFromBytes([]byte(`{"version":2,"response":{"status":"missing"}}`)); got != "" {
		t.Errorf("missing: got %q, want empty", got)
	}
	if got := extractResponseHashFromBytes([]byte(`{"version":1,"steps":[]}`)); got != "" {
		t.Errorf("v1: got %q, want empty", got)
	}
}

// newSyncRepo creates a connected repository with a Codex session.
func newSyncRepo(t *testing.T, ctx context.Context) (repoRoot, semDir string, repoStore *blobs.Store) {
	t.Helper()
	repoRoot = t.TempDir()
	semDir = filepath.Join(repoRoot, ".semantica")
	if err := os.MkdirAll(filepath.Join(semDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := util.WriteSettings(semDir, util.Settings{Enabled: true, ConnectedRepoID: "cr"}); err != nil {
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
	repoID := uuid.NewString()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: repoRoot, CreatedAt: 1, EnabledAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	src, err := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: uuid.NewString(), RepositoryID: repoID, Provider: "codex",
		SourceKey: "/f.jsonl", LastSeenAt: 1, CreatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: uuid.NewString(), ProviderSessionID: "ps", RepositoryID: repoID,
		SourceID: src.SourceID, Provider: "codex", StartedAt: 1, LastSeenAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	_ = sqlstore.Close(h)
	repoStore, err = blobs.NewStore(filepath.Join(semDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	return repoRoot, semDir, repoStore
}

type bundleResponseView struct {
	EventID     string `json:"event_id"`
	Hash        string `json:"hash"`
	Summary     string `json:"summary"`
	Status      string `json:"status"`
	CompletedAt int64  `json:"completed_at"`
}

// loadPackagedBundle reads the packaged turn's version and response metadata.
func loadPackagedBundle(t *testing.T, ctx context.Context, semDir string, repoStore *blobs.Store) (int, *bundleResponseView) {
	t.Helper()
	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	var bundleHash string
	if err := h.DB.QueryRowContext(ctx,
		"select provenance_bundle_hash from provenance_manifests where turn_id = ?", "turn-1").
		Scan(&bundleHash); err != nil || bundleHash == "" {
		t.Fatalf("read bundle hash: %v (hash %q)", err, bundleHash)
	}
	raw, err := repoStore.Get(ctx, bundleHash)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	var b struct {
		Version  int                 `json:"version"`
		Response *bundleResponseView `json:"response"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("unmarshal bundle: %v", err)
	}
	return b.Version, b.Response
}

// Sync includes the response object without changing its bytes.
func TestSyncPendingTurns_IncludesTurnResponseObject(t *testing.T) {
	ctx := context.Background()
	repoRoot, _, repoStore := newSyncRepo(t, ctx)
	cand := RedactAndStoreResponse(ctx, repoStore, "ev-1", "the final answer", 500)
	if cand.Status != responseComplete || cand.Hash == "" {
		t.Fatalf("candidate = %+v", cand)
	}
	wantBytes, err := repoStore.Get(ctx, cand.Hash)
	if err != nil {
		t.Fatal(err)
	}
	PackageTurn(ctx, repoRoot, TurnContext{
		TurnID: "turn-1", SessionID: "ps", Provider: "codex", CWD: repoRoot,
		StartedAt: 1, CompletedAt: 2, ResponseCandidate: cand,
	}, repoStore)

	results, err := SyncPendingTurns(ctx, repoRoot, 0, 50)
	if err != nil {
		t.Fatalf("SyncPendingTurns: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	r := results[0]
	if r.Skipped {
		t.Fatal("result skipped, want a successful envelope")
	}
	var env struct {
		Objects []struct {
			Kind string `json:"kind"`
			Hash string `json:"hash"`
		} `json:"objects"`
	}
	if err := json.Unmarshal(r.Envelope, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	found := false
	for _, o := range env.Objects {
		if o.Kind == "turn_response" {
			found = true
			if o.Hash != cand.Hash {
				t.Errorf("turn_response hash = %q, want %q", o.Hash, cand.Hash)
			}
		}
	}
	if !found {
		t.Fatalf("no turn_response object in envelope: %+v", env.Objects)
	}
	if got := r.RedactedBlobs[cand.Hash]; !bytes.Equal(got, wantBytes) {
		t.Errorf("RedactedBlobs[%s] = %q, want the stored response object %q", cand.Hash, got, wantBytes)
	}
}

// Sync skips a manifest whose response object is missing.
func TestSyncPendingTurns_MissingResponseObjectFailsClosed(t *testing.T) {
	ctx := context.Background()
	repoRoot, semDir, repoStore := newSyncRepo(t, ctx)
	cand := RedactAndStoreResponse(ctx, repoStore, "ev-1", "the final answer", 500)
	PackageTurn(ctx, repoRoot, TurnContext{
		TurnID: "turn-1", SessionID: "ps", Provider: "codex", CWD: repoRoot,
		StartedAt: 1, CompletedAt: 2, ResponseCandidate: cand,
	}, repoStore)

	// Remove the object referenced by the bundle.
	blobPath := filepath.Join(semDir, "objects", cand.Hash[:2], cand.Hash[2:4], cand.Hash)
	if err := os.Remove(blobPath); err != nil {
		t.Fatalf("remove response blob: %v", err)
	}
	results, err := SyncPendingTurns(ctx, repoRoot, 0, 50)
	if err != nil {
		t.Fatalf("SyncPendingTurns: %v", err)
	}
	if len(results) != 1 || !results[0].Skipped {
		t.Fatalf("results = %+v, want one skipped (fail closed)", results)
	}
}

// Failed delta redaction omits the object and its bundle reference.
func TestBuildSyncResult_FailedDeltaRedactionStripsReference(t *testing.T) {
	ctx := context.Background()
	repoRoot, semDir, repoStore := newSyncRepo(t, ctx)

	// This canonical delta contains a secret that spans hunk lines.
	deltaBlob := canonicalDelta(t, "", []string{
		"-----BEGIN RSA PRIVATE KEY-----",
		"MIIEpAIBAAKCAQEA0Z3VS5JJcds3xfn/ygWyF8PbnGcY5unA67hFdJBEEH6kMRMD",
		"-----END RSA PRIVATE KEY-----",
	})
	deltaHash, _, err := repoStore.Put(ctx, deltaBlob)
	if err != nil {
		t.Fatal(err)
	}
	promptHash, _, err := repoStore.Put(ctx, []byte("do the thing"))
	if err != nil {
		t.Fatal(err)
	}
	bundleBytes, _ := json.Marshal(map[string]any{
		"version": 1, "provider": "codex", "session_id": "sess", "turn_id": "turn-1", "started_at": 1,
		"prompt": map[string]any{"event_id": "pe", "blob_hash": promptHash},
		"steps": []map[string]any{
			{"event_id": "e1", "ts": 2, "tool_name": "Bash", "delta_hash": deltaHash},
		},
	})
	bundleHash, _, err := repoStore.Put(ctx, bundleBytes)
	if err != nil {
		t.Fatal(err)
	}

	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	result := buildSyncResult(ctx, h, repoStore, &util.Settings{Enabled: true, ConnectedRepoID: "cr"},
		sqldb.ProvenanceManifest{
			ManifestID:           "mid",
			TurnID:               "turn-1",
			Provider:             "codex",
			ProvenanceBundleHash: sqlstore.NullStr(bundleHash),
			StartedAt:            1,
		}, repoRoot)

	if result.Skipped {
		t.Fatal("a failed delta must not fail the whole turn")
	}
	var env struct {
		Objects []struct {
			Kind string `json:"kind"`
			Hash string `json:"hash"`
		} `json:"objects"`
	}
	if err := json.Unmarshal(result.Envelope, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	var bundleUploadHash string
	for _, o := range env.Objects {
		if o.Kind == "tool_delta" {
			t.Errorf("failed delta should not be uploaded, found object %+v", o)
		}
		if o.Kind == "bundle" {
			bundleUploadHash = o.Hash
		}
	}
	if bundleUploadHash == "" {
		t.Fatalf("no bundle object in envelope: %+v", env.Objects)
	}
	if strings.Contains(string(result.RedactedBlobs[bundleUploadHash]), "delta_hash") {
		t.Errorf("uploaded bundle retains a dangling delta_hash: %s", result.RedactedBlobs[bundleUploadHash])
	}
}

// Response metadata follows the status and completion-time contract.
func TestPackageTurn_ResponseBlockByStatus(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name            string
		cand            ResponseCandidate
		turnCompletedAt int64
		wantStatus      string
		wantTime        int64 // 0 means "positive but exact value unimportant"; -1 means must be 0
	}{
		{"empty", ResponseCandidate{Status: responseEmpty, EventID: "e", CompletedAt: 500}, 2, responseEmpty, 500},
		{"missing", ResponseCandidate{Status: responseMissing, EventID: "e"}, 2, responseMissing, -1},
		{"unsupported", ResponseCandidate{Status: responseUnsupported}, 2, responseUnsupported, -1},
		{"empty_falls_back_to_turn_time", ResponseCandidate{Status: responseEmpty, EventID: "e"}, 7, responseEmpty, 7},
		// Empty avoids CAS validation, isolating missing-time handling.
		{"empty_no_time_downgrades_to_missing", ResponseCandidate{Status: responseEmpty, EventID: "e"}, 0, responseMissing, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repoRoot, semDir, repoStore := newSyncRepo(t, ctx)
			PackageTurn(ctx, repoRoot, TurnContext{
				TurnID: "turn-1", SessionID: "ps", Provider: "codex", CWD: repoRoot,
				StartedAt: 1, CompletedAt: tc.turnCompletedAt, ResponseCandidate: tc.cand,
			}, repoStore)

			version, resp := loadPackagedBundle(t, ctx, semDir, repoStore)
			if version != 2 {
				t.Errorf("version = %d, want 2", version)
			}
			if resp == nil {
				t.Fatal("no response block")
			}
			if resp.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", resp.Status, tc.wantStatus)
			}
			if resp.Status != responseComplete && (resp.Hash != "" || resp.Summary != "") {
				t.Errorf("non-complete %q carries hash=%q summary=%q", resp.Status, resp.Hash, resp.Summary)
			}
			switch {
			case tc.wantTime == -1:
				if resp.CompletedAt != 0 {
					t.Errorf("completed_at = %d, want 0", resp.CompletedAt)
				}
			case tc.wantTime > 0:
				if resp.CompletedAt != tc.wantTime {
					t.Errorf("completed_at = %d, want %d", resp.CompletedAt, tc.wantTime)
				}
			}
		})
	}
}
