package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"
)

// storeReadinessManifest stores a minimal version-1 manifest.
func storeReadinessManifest(t *testing.T, dir string) string {
	t.Helper()
	bs, err := blobs.NewStore(filepath.Join(dir, ".semantica", "objects"))
	if err != nil {
		t.Fatal(err)
	}
	m := blobs.Manifest{Version: 1, CreatedAt: 1, RepoRoot: "/r", Files: []blobs.ManifestFile{
		{Path: "a.txt", Blob: strings.Repeat("c", 64), Size: 1, Mode: 0o644},
	}}
	raw, err := m.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	hash, _, err := bs.Put(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

// readinessFixture inserts a complete checkpoint with a stored manifest.
func readinessFixture(t *testing.T, h *sqlstore.Handle, dir, repoID, cpID string) sqldb.Checkpoint {
	t.Helper()
	ctx := context.Background()
	insertPendingLinked(t, h, repoID, cpID, cpID+"-commit", 100)
	if err := h.Queries.CompleteCheckpoint(ctx, sqldb.CompleteCheckpointParams{
		ManifestHash: sql.NullString{String: storeReadinessManifest(t, dir), Valid: true},
		SizeBytes:    sql.NullInt64{Int64: 1, Valid: true},
		CompletedAt:  sql.NullInt64{Int64: 1, Valid: true},
		CheckpointID: cpID,
	}); err != nil {
		t.Fatal(err)
	}
	return readCheckpoint(t, h, cpID)
}

func upsertStats(t *testing.T, h *sqlstore.Handle, cpID string, aiPct float64, computed, pushed bool) {
	t.Helper()
	ctx := context.Background()
	if err := h.Queries.UpsertCheckpointStats(ctx, sqldb.UpsertCheckpointStatsParams{
		CheckpointID: cpID, SessionCount: 1, FilesChanged: 1,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if computed {
		if err := h.Queries.RecordCheckpointAttribution(ctx, sqldb.RecordCheckpointAttributionParams{
			CheckpointID:          cpID,
			AiPercentage:          aiPct,
			AttributionComputedAt: sql.NullInt64{Int64: now, Valid: true},
			AttributionVersion:    sql.NullString{String: "v1", Valid: true},
		}); err != nil {
			t.Fatal(err)
		}
	} else if _, err := h.DB.ExecContext(ctx, "update checkpoint_stats set ai_percentage = ? where checkpoint_id = ?", aiPct, cpID); err != nil {
		// Legacy shape: a percentage without completion markers.
		t.Fatal(err)
	}
	if pushed {
		if err := h.Queries.MarkCheckpointAttributionPushed(ctx, sqldb.MarkCheckpointAttributionPushedParams{
			AttributionPushedAt: sql.NullInt64{Int64: now, Valid: true},
			CheckpointID:        cpID,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func evalLocal(t *testing.T, dir string, h *sqlstore.Handle, cp sqldb.Checkpoint) AuditReadiness {
	t.Helper()
	return EvaluateAuditReadiness(context.Background(), h, dir+"/.semantica", cp, PolicyLocal)
}

// A complete checkpoint with computed attribution and no agent turns is ready.
func TestReadiness_CompleteWithFullEvidenceIsReady(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	cp := readinessFixture(t, h, dir, repoID, "ck-full")
	upsertStats(t, h, "ck-full", 42, true, false)

	ar := evalLocal(t, dir, h, cp)
	if ar.Manifest.State != ReadinessReady || ar.Attribution.State != ReadinessReady {
		t.Fatalf("manifest=%+v attribution=%+v, want ready", ar.Manifest, ar.Attribution)
	}
	if ar.Provenance.State != ReadinessNotRequired {
		t.Fatalf("provenance=%+v, want not_required with no turns", ar.Provenance)
	}
	if ar.Sync.State != ReadinessNotRequired {
		t.Fatalf("sync=%+v, want not_required when unconnected", ar.Sync)
	}
	if !ar.AuditReady || ar.Policy != "local" {
		t.Fatalf("verdict = %+v, want audit_ready under local policy", ar)
	}
}

// A version mismatch does not make stored attribution stale.
func TestReadiness_VersionMismatchStaysComplete(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	cp := readinessFixture(t, h, dir, repoID, "ck-v1")
	upsertStats(t, h, "ck-v1", 42, true, false) // stored as v1

	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "1") // configured as v2

	ar := evalLocal(t, dir, h, cp)
	if ar.Attribution.State != ReadinessReady {
		t.Fatalf("attribution = %+v, want ready despite version mismatch", ar.Attribution)
	}
	if !ar.AuditReady {
		t.Fatal("audit_ready = false, want complete with stored version visible")
	}
	if ar.AttributionVersion != "v1" {
		t.Errorf("attribution_version = %q, want stored v1", ar.AttributionVersion)
	}
}

// Legacy rows omit an unknown attribution version.
func TestReadiness_LegacyRowsOmitVersion(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	cp := readinessFixture(t, h, dir, repoID, "ck-legacy")
	ctx := context.Background()
	if err := h.Queries.UpsertCheckpointStats(ctx, sqldb.UpsertCheckpointStatsParams{
		CheckpointID: "ck-legacy", SessionCount: 1, FilesChanged: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// Old two-step shape: percentage and marker without a version.
	if _, err := h.DB.ExecContext(ctx,
		"update checkpoint_stats set ai_percentage = 10, attribution_computed_at = 1 where checkpoint_id = 'ck-legacy'"); err != nil {
		t.Fatal(err)
	}

	ar := evalLocal(t, dir, h, cp)
	if ar.Attribution.State != ReadinessReady {
		t.Fatalf("attribution = %+v, want ready", ar.Attribution)
	}
	if ar.AttributionVersion != "" {
		t.Errorf("attribution_version = %q, want empty for legacy NULL", ar.AttributionVersion)
	}
	data, err := json.Marshal(ar)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "attribution_version") {
		t.Errorf("JSON should omit attribution_version when unknown: %s", data)
	}
}

// Completion markers distinguish an empty result from a stage that never ran.
func TestReadiness_ZeroAIDistinguishedFromNeverComputed(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)

	ranEmpty := readinessFixture(t, h, dir, repoID, "ck-empty")
	upsertStats(t, h, "ck-empty", -1, true, false) // ran, nothing to attribute
	never := readinessFixture(t, h, dir, repoID, "ck-never")
	upsertStats(t, h, "ck-never", -1, false, false) // never recorded completion

	if got := evalLocal(t, dir, h, ranEmpty).Attribution; got.State != ReadinessReady {
		t.Fatalf("computed-and-empty = %+v, want ready", got)
	}
	got := evalLocal(t, dir, h, never).Attribution
	if got.State != ReadinessUnknown {
		t.Fatalf("never-computed = %+v, want unknown, never ready", got)
	}
}

// Checkpoint completion alone does not imply audit readiness.
func TestReadiness_CheckpointCompletionIsNotAuditReadiness(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	ctx := context.Background()
	cp := readinessFixture(t, h, dir, repoID, "ck-hole")
	upsertStats(t, h, "ck-hole", 42, true, false)

	// A turn without a provenance manifest represents missing evidence.
	if _, err := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: "src-r", RepositoryID: repoID, Provider: "claude-code",
		SourceKey: "k", LastSeenAt: 1, CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: "sess-r", ProviderSessionID: "ps-r", RepositoryID: repoID,
		Provider: "claude-code", SourceID: "src-r", StartedAt: 1, LastSeenAt: 1,
		MetadataJson: "{}",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID: "ev-r", SessionID: "sess-r", RepositoryID: repoID,
		Ts: 50, Kind: "assistant", EventSource: "hook",
		TurnID: sql.NullString{String: "turn-missing", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	ar := evalLocal(t, dir, h, cp)
	if cp.Status != "complete" {
		t.Fatalf("fixture must be complete, got %q", cp.Status)
	}
	if ar.Provenance.State != ReadinessFailed {
		t.Fatalf("provenance = %+v, want failed for the missing turn bundle", ar.Provenance)
	}
	if ar.AuditReady {
		t.Fatal("complete checkpoint with missing provenance must not be audit_ready")
	}
}

// Local and hosted policies apply different sync requirements.
func TestReadiness_PolicyScopesSyncRequirement(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	ctx := context.Background()
	cp := readinessFixture(t, h, dir, repoID, "ck-pol")
	upsertStats(t, h, "ck-pol", 42, true, false) // computed, never pushed

	semDir := dir + "/.semantica"
	s, err := util.ReadSettings(semDir)
	if err != nil {
		t.Fatal(err)
	}
	s.Connected = true
	if err := util.WriteSettings(semDir, s); err != nil {
		t.Fatal(err)
	}

	local := EvaluateAuditReadiness(ctx, h, semDir, cp, PolicyLocal)
	hosted := EvaluateAuditReadiness(ctx, h, semDir, cp, PolicyHosted)

	if local.Sync.State != ReadinessPending || hosted.Sync.State != ReadinessPending {
		t.Fatalf("sync local=%+v hosted=%+v, want pending when connected without a push record", local.Sync, hosted.Sync)
	}
	if !local.AuditReady {
		t.Fatalf("local policy must not require sync: %+v", local)
	}
	if hosted.AuditReady {
		t.Fatalf("hosted policy must require sync: %+v", hosted)
	}
	if local.Policy != "local" || hosted.Policy != "hosted" {
		t.Fatalf("policies must be named: %q / %q", local.Policy, hosted.Policy)
	}
}

// Pending and failed checkpoints remain unready.
func TestReadiness_PendingAndFailedCheckpoints(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	ctx := context.Background()

	insertPendingLinked(t, h, repoID, "ck-pend", "c-p", 100)
	pending := readCheckpoint(t, h, "ck-pend")
	ar := evalLocal(t, dir, h, pending)
	if ar.Manifest.State != ReadinessPending || ar.Attribution.State != ReadinessPending || ar.AuditReady {
		t.Fatalf("pending checkpoint: %+v", ar)
	}

	insertPendingLinked(t, h, repoID, "ck-dead", "c-d", 200)
	if _, err := h.Queries.FailCheckpoint(ctx, sqldb.FailCheckpointParams{
		CompletedAt:  sql.NullInt64{Int64: 1, Valid: true},
		LastError:    sql.NullString{String: "boom", Valid: true},
		CheckpointID: "ck-dead",
	}); err != nil {
		t.Fatal(err)
	}
	failed := readCheckpoint(t, h, "ck-dead")
	ar = evalLocal(t, dir, h, failed)
	if ar.Manifest.State != ReadinessFailed || ar.AuditReady {
		t.Fatalf("failed checkpoint: %+v", ar)
	}
	if ar.Manifest.Reason == "" {
		t.Fatal("failed manifest must carry the recorded error")
	}
}

// Legacy rows remain distinguishable without being reported as failures.
func TestReadiness_LegacyRowsReadAsUnknownNotFailed(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	cp := readinessFixture(t, h, dir, repoID, "ck-old")
	upsertStats(t, h, "ck-old", 37, false, false) // percentage from before markers

	got := evalLocal(t, dir, h, cp).Attribution
	if got.State != ReadinessReady {
		t.Fatalf("legacy computed percentage = %+v, want ready", got)
	}

	cpNoStats := readinessFixture(t, h, dir, repoID, "ck-nostats")
	got = evalLocal(t, dir, h, cpNoStats).Attribution
	if got.State != ReadinessUnknown {
		t.Fatalf("no stats row = %+v, want unknown", got)
	}
}

// A failed queue head blocks later work unless a completed successor passed it.
func TestQueueBlockage_GrandfatherRule(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	ctx := context.Background()

	// A terminally failed head blocks a later pending checkpoint.
	insertPendingLinked(t, h, repoID, "ck-b1", "c1", 100)
	if _, err := h.Queries.FailCheckpoint(ctx, sqldb.FailCheckpointParams{
		CompletedAt:  sql.NullInt64{Int64: 1, Valid: true},
		LastError:    sql.NullString{String: "enrichment exploded", Valid: true},
		CheckpointID: "ck-b1",
	}); err != nil {
		t.Fatal(err)
	}
	insertPendingLinked(t, h, repoID, "ck-b2", "c2", 200)

	blocked := queueBlockage(ctx, h, repoID)
	if blocked == nil || blocked.CheckpointID != "ck-b1" || blocked.LastError != "enrichment exploded" {
		t.Fatalf("blockage = %+v, want ck-b1 with its error", blocked)
	}

	// Historical failures followed by completed work do not block.
	if err := h.Queries.CompleteCheckpoint(ctx, sqldb.CompleteCheckpointParams{
		ManifestHash: sql.NullString{String: "mh", Valid: true},
		SizeBytes:    sql.NullInt64{Int64: 1, Valid: true},
		CompletedAt:  sql.NullInt64{Int64: 1, Valid: true},
		CheckpointID: "ck-b2",
	}); err != nil {
		t.Fatal(err)
	}
	if blocked := queueBlockage(ctx, h, repoID); blocked != nil {
		t.Fatalf("passed failure must not block: %+v", blocked)
	}
	_ = dir
}

// Real enrichment records the attribution completion marker.
func TestWorkerRun_RecordsAttributionCompletionMarker(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	t.Setenv("SEMANTICA_HOME", filepath.Join(dir, ".semantica-global"))
	t.Setenv("HOME", dir)

	gitRun(t, dir, "init")
	gitRun(t, dir, "config", "user.email", "t@t.t")
	gitRun(t, dir, "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "main.go")
	gitRun(t, dir, "commit", "-m", "init")
	commit := gitRun(t, dir, "rev-parse", "HEAD")

	enableSemantica(t, ctx, dir)

	semDir := filepath.Join(dir, ".semantica")
	settings, err := util.ReadSettings(semDir)
	if err != nil {
		t.Fatal(err)
	}
	// This test exercises enrichment only; avoid starting a detached playbook process.
	if settings.Automations == nil {
		settings.Automations = &util.Automations{}
	}
	settings.Automations.Playbook.Enabled = false
	if err := util.WriteSettings(semDir, settings); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(semDir, "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: "ck-real", RepositoryID: repoRow.RepositoryID,
		CreatedAt: time.Now().UnixMilli(), Kind: "auto", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: commit, RepositoryID: repoRow.RepositoryID,
		CheckpointID: "ck-real", LinkedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}

	svc := NewWorkerService(nil)
	if err := svc.Run(ctx, WorkerInput{CheckpointID: "ck-real", CommitHash: commit, RepoRoot: dir}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	stats, err := h.Queries.GetCheckpointStats(ctx, "ck-real")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if !stats.AttributionComputedAt.Valid {
		t.Fatal("real enrichment must record the attribution completion marker")
	}
	cp := readCheckpoint(t, h, "ck-real")
	ar := EvaluateAuditReadiness(ctx, h, semDir, cp, PolicyLocal)
	if ar.Attribution.State != ReadinessReady {
		t.Fatalf("attribution = %+v, want ready after real enrichment", ar.Attribution)
	}
}

// Failed packaging rows without bundle hashes do not count as provenance.
func TestReadiness_FailedPackagingRowIsNotPackaged(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	ctx := context.Background()
	cp := readinessFixture(t, h, dir, repoID, "ck-pm")
	upsertStats(t, h, "ck-pm", 42, true, false)

	if _, err := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: "src-pm", RepositoryID: repoID, Provider: "claude-code",
		SourceKey: "k", LastSeenAt: 1, CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: "sess-pm", ProviderSessionID: "ps-pm", RepositoryID: repoID,
		Provider: "claude-code", SourceID: "src-pm", StartedAt: 1, LastSeenAt: 1,
		MetadataJson: "{}",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID: "ev-pm", SessionID: "sess-pm", RepositoryID: repoID,
		Ts: 50, Kind: "assistant", EventSource: "hook",
		TurnID: sql.NullString{String: "turn-pm", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	// A failed packaging row with an empty bundle hash.
	upsertManifest := func(status, hash string) {
		t.Helper()
		if err := h.Queries.UpsertProvenanceManifest(ctx, sqldb.UpsertProvenanceManifestParams{
			ManifestID: "pm-1", RepositoryID: repoID, SessionID: "sess-pm",
			TurnID: "turn-pm", Provider: "claude-code", Kind: "turn_bundle",
			ProvenanceBundleHash: sqlstore.NullStr(hash),
			StartedAt:            1, Status: status, CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	upsertManifest("failed", "")
	got := evalLocal(t, dir, h, cp)
	if got.Provenance.State != ReadinessFailed {
		t.Fatalf("failed packaging row = %+v, must not read as packaged", got.Provenance)
	}
	if got.AuditReady {
		t.Fatal("audit_ready must be false over a failed bundle")
	}

	// The same turn with a real packaged bundle reads ready.
	upsertManifest("packaged", "bundlehash")
	got = evalLocal(t, dir, h, cp)
	if got.Provenance.State != ReadinessReady {
		t.Fatalf("packaged bundle = %+v, want ready", got.Provenance)
	}
}

// Unknown policies and unreadable settings fail closed.
func TestReadiness_FailsClosed(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	cp := readinessFixture(t, h, dir, repoID, "ck-fc")
	upsertStats(t, h, "ck-fc", 42, true, false)

	ar := EvaluateAuditReadiness(context.Background(), h, dir+"/.semantica", cp, ReadinessPolicy("pr-check-v2"))
	for name, comp := range map[string]ReadinessComponent{
		"manifest": ar.Manifest, "attribution": ar.Attribution,
		"provenance": ar.Provenance, "sync": ar.Sync,
	} {
		if comp.State != ReadinessUnknown {
			t.Errorf("unknown policy: %s = %+v, want unknown", name, comp)
		}
	}
	if ar.AuditReady {
		t.Fatal("unknown policy must not be audit_ready")
	}

	// Malformed settings must not read as "not connected".
	if err := os.WriteFile(filepath.Join(dir, ".semantica", "settings.json"), []byte("{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	ar = evalLocal(t, dir, h, cp)
	if ar.Sync.State != ReadinessUnknown {
		t.Fatalf("corrupt settings: sync = %+v, want unknown", ar.Sync)
	}
}

// Successful backfill records hosted sync evidence.
func TestBackfill_SuccessfulPushRecordsSyncMarker(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	t.Setenv("SEMANTICA_HOME", filepath.Join(dir, ".semantica-global"))
	t.Setenv("HOME", dir)
	t.Setenv("SEMANTICA_API_KEY", "test-key")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()
	t.Setenv("SEMANTICA_ENDPOINT", server.URL)

	gitRun(t, dir, "init")
	gitRun(t, dir, "config", "user.email", "t@t.t")
	gitRun(t, dir, "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "main.go")
	gitRun(t, dir, "commit", "-m", "init")
	commit := gitRun(t, dir, "rev-parse", "HEAD")

	enableSemantica(t, ctx, dir)
	semDir := filepath.Join(dir, ".semantica")
	settings, err := util.ReadSettings(semDir)
	if err != nil {
		t.Fatal(err)
	}
	settings.Connected = true
	settings.ConnectedRepoID = "hosted-backfill"
	if err := util.WriteSettings(semDir, settings); err != nil {
		t.Fatal(err)
	}

	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: "ck-bf", RepositoryID: repoRow.RepositoryID,
		CreatedAt: time.Now().UnixMilli(), Kind: "auto", Status: "complete",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: commit, RepositoryID: repoRow.RepositoryID,
		CheckpointID: "ck-bf", LinkedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := InitBackfillState(ctx, h, "hosted-backfill", repoRow.RepositoryID); err != nil {
		t.Fatal(err)
	}

	res := DrainBackfillBatch(ctx, dir, "hosted-backfill", 10)
	if res.Failed || res.Uploaded != 1 {
		t.Fatalf("backfill result = %+v, want one upload", res)
	}
	stats, err := h.Queries.GetCheckpointStats(ctx, "ck-bf")
	if err != nil {
		t.Fatalf("stats after backfill: %v", err)
	}
	if !stats.AttributionPushedAt.Valid {
		t.Fatal("backfilled push must record the sync marker")
	}
}

// Status JSON exposes readiness and queue blockage fields.
func TestStatus_JSONShapeCarriesReadiness(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	t.Setenv("SEMANTICA_HOME", filepath.Join(dir, ".semantica-global"))
	t.Setenv("HOME", dir)

	gitRun(t, dir, "init")
	gitRun(t, dir, "config", "user.email", "t@t.t")
	gitRun(t, dir, "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "a.go")
	gitRun(t, dir, "commit", "-m", "init")

	enableSemantica(t, ctx, dir)
	semDir := filepath.Join(dir, ".semantica")
	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	// A terminally failed head so blocked_by is exercised too.
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: "ck-blocked", RepositoryID: repoRow.RepositoryID,
		CreatedAt: time.Now().UnixMilli(), Kind: "auto", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: "deadbeef", RepositoryID: repoRow.RepositoryID,
		CheckpointID: "ck-blocked", LinkedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Queries.FailCheckpoint(ctx, sqldb.FailCheckpointParams{
		CompletedAt:  sql.NullInt64{Int64: 1, Valid: true},
		LastError:    sql.NullString{String: "enrichment exploded", Valid: true},
		CheckpointID: "ck-blocked",
	}); err != nil {
		t.Fatal(err)
	}
	_ = sqlstore.Close(h)

	res, err := NewStatusService().Status(ctx, StatusInput{RepoPath: dir})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	lastCP, ok := doc["last_checkpoint"].(map[string]any)
	if !ok || lastCP["status"] == "" {
		t.Fatalf("last_checkpoint.status missing: %v", doc["last_checkpoint"])
	}
	ar, ok := doc["audit_readiness"].(map[string]any)
	if !ok {
		t.Fatalf("audit_readiness missing: %s", raw)
	}
	if ar["policy"] != "local" {
		t.Errorf("policy = %v, want local", ar["policy"])
	}
	for _, comp := range []string{"manifest", "attribution", "provenance", "sync"} {
		m, ok := ar[comp].(map[string]any)
		if !ok || m["state"] == "" {
			t.Errorf("audit_readiness.%s.state missing: %v", comp, ar[comp])
		}
	}
	if _, ok := ar["audit_ready"].(bool); !ok {
		t.Errorf("audit_ready must be a boolean: %v", ar["audit_ready"])
	}
	blocked, ok := doc["blocked_by"].(map[string]any)
	if !ok || blocked["checkpoint_id"] != "ck-blocked" || blocked["last_error"] != "enrichment exploded" {
		t.Fatalf("blocked_by = %v, want ck-blocked with its error", doc["blocked_by"])
	}
}

// Failed uploads retain local provenance but make hosted sync fail.
func TestReadiness_FailedUploadReadsSyncFailed(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	ctx := context.Background()
	cp := readinessFixture(t, h, dir, repoID, "ck-fu")
	upsertStats(t, h, "ck-fu", 42, true, true) // computed and pushed

	if _, err := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: "src-fu", RepositoryID: repoID, Provider: "claude-code",
		SourceKey: "k", LastSeenAt: 1, CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: "sess-fu", ProviderSessionID: "ps-fu", RepositoryID: repoID,
		Provider: "claude-code", SourceID: "src-fu", StartedAt: 1, LastSeenAt: 1,
		MetadataJson: "{}",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID: "ev-fu", SessionID: "sess-fu", RepositoryID: repoID,
		Ts: 50, Kind: "assistant", EventSource: "hook",
		TurnID: sql.NullString{String: "turn-fu", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	// Packaged bundle (non-empty hash) whose upload failed terminally.
	if err := h.Queries.UpsertProvenanceManifest(ctx, sqldb.UpsertProvenanceManifestParams{
		ManifestID: "pm-fu", RepositoryID: repoID, SessionID: "sess-fu",
		TurnID: "turn-fu", Provider: "claude-code", Kind: "turn_bundle",
		ProvenanceBundleHash: sqlstore.NullStr("bundlehash"),
		StartedAt:            1, Status: "failed", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	semDir := dir + "/.semantica"
	s, err := util.ReadSettings(semDir)
	if err != nil {
		t.Fatal(err)
	}
	s.Connected = true
	if err := util.WriteSettings(semDir, s); err != nil {
		t.Fatal(err)
	}

	ar := EvaluateAuditReadiness(ctx, h, semDir, cp, PolicyHosted)
	if ar.Provenance.State != ReadinessReady {
		t.Fatalf("provenance = %+v, want ready (bundle exists locally)", ar.Provenance)
	}
	if ar.Sync.State != ReadinessFailed {
		t.Fatalf("sync = %+v, want failed for a terminal upload failure", ar.Sync)
	}
	if ar.AuditReady {
		t.Fatal("hosted policy must not be audit_ready over a failed upload")
	}
}

// Marker write failures leave the backfill cursor in place for retry.
func TestBackfill_MarkerFailureDoesNotAdvanceCursor(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	t.Setenv("SEMANTICA_HOME", filepath.Join(dir, ".semantica-global"))
	t.Setenv("HOME", dir)
	t.Setenv("SEMANTICA_API_KEY", "test-key")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()
	t.Setenv("SEMANTICA_ENDPOINT", server.URL)

	gitRun(t, dir, "init")
	gitRun(t, dir, "config", "user.email", "t@t.t")
	gitRun(t, dir, "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "main.go")
	gitRun(t, dir, "commit", "-m", "init")
	commit := gitRun(t, dir, "rev-parse", "HEAD")

	enableSemantica(t, ctx, dir)
	semDir := filepath.Join(dir, ".semantica")
	settings, err := util.ReadSettings(semDir)
	if err != nil {
		t.Fatal(err)
	}
	settings.Connected = true
	settings.ConnectedRepoID = "hosted-mf"
	if err := util.WriteSettings(semDir, settings); err != nil {
		t.Fatal(err)
	}

	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: "ck-mf2", RepositoryID: repoRow.RepositoryID,
		CreatedAt: time.Now().UnixMilli(), Kind: "auto", Status: "complete",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: commit, RepositoryID: repoRow.RepositoryID,
		CheckpointID: "ck-mf2", LinkedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := InitBackfillState(ctx, h, "hosted-mf", repoRow.RepositoryID); err != nil {
		t.Fatal(err)
	}

	orig := markAttributionPushedFn
	markAttributionPushedFn = func(ctx context.Context, h *sqlstore.Handle, checkpointID string) error {
		return errors.New("injected marker persistence failure")
	}
	res := DrainBackfillBatch(ctx, dir, "hosted-mf", 10)
	markAttributionPushedFn = orig
	if !res.Failed || res.Uploaded != 0 {
		t.Fatalf("marker failure must fail the batch without counting uploads: %+v", res)
	}
	if stats, err := h.Queries.GetCheckpointStats(ctx, "ck-mf2"); err == nil && stats.AttributionPushedAt.Valid {
		t.Fatal("marker must not be recorded by the failed path")
	}

	// The cursor did not advance: the next batch re-pushes and records.
	res = DrainBackfillBatch(ctx, dir, "hosted-mf", 10)
	if res.Failed || res.Uploaded != 1 {
		t.Fatalf("recovery batch = %+v, want one upload", res)
	}
	stats, err := h.Queries.GetCheckpointStats(ctx, "ck-mf2")
	if err != nil || !stats.AttributionPushedAt.Valid {
		t.Fatalf("recovered push must record the marker: %v %+v", err, stats)
	}
}

// Stored manifest readiness depends on format, scope, and commit anchoring.
func TestReadiness_ManifestClassification(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	ctx := context.Background()
	bs, err := blobs.NewStore(filepath.Join(dir, ".semantica", "objects"))
	if err != nil {
		t.Fatal(err)
	}
	marshal := func(m blobs.Manifest) []byte {
		raw, err := m.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	// store completes a checkpoint with the supplied manifest and optional link.
	store := func(cpID, kind, commitLink string, raw []byte) sqldb.Checkpoint {
		if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
			CheckpointID: cpID, RepositoryID: repoID, CreatedAt: 100, Kind: kind, Status: "pending",
		}); err != nil {
			t.Fatal(err)
		}
		if commitLink != "" {
			if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
				CommitHash: commitLink, RepositoryID: repoID, CheckpointID: cpID, LinkedAt: 1,
			}); err != nil {
				t.Fatal(err)
			}
		}
		hash, _, err := bs.Put(ctx, raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Queries.CompleteCheckpoint(ctx, sqldb.CompleteCheckpointParams{
			ManifestHash: sql.NullString{String: hash, Valid: true},
			SizeBytes:    sql.NullInt64{Int64: 1, Valid: true},
			CompletedAt:  sql.NullInt64{Int64: 1, Valid: true},
			CheckpointID: cpID,
		}); err != nil {
			t.Fatal(err)
		}
		return readCheckpoint(t, h, cpID)
	}
	sha1 := strings.Repeat("a", 40) // validCommitManifestBytes uses this hash.
	wsFile := blobs.ManifestFile{Path: "a.txt", Blob: strings.Repeat("c", 64), Size: 1, Mode: 0o644}
	workspaceManifest := marshal(blobs.Manifest{
		Version: 2, Scope: blobs.ScopeWorkspace, CreatedAt: 1, RepoRoot: "/r", Files: []blobs.ManifestFile{wsFile},
	})

	// A matching v2 commit manifest is verified.
	commit := evalLocal(t, dir, h, store("ck-commit", "auto", sha1, validCommitManifestBytes(t)))
	if commit.Manifest.State != ReadinessReady || commit.ManifestScope != "commit" || commit.ManifestIntegrity != "verified" {
		t.Errorf("commit manifest = %+v, want ready/commit/verified", commit)
	}

	// A mismatched link leaves the manifest unanchored.
	mismatch := evalLocal(t, dir, h, store("ck-mismatch", "auto", strings.Repeat("b", 40), validCommitManifestBytes(t)))
	if mismatch.Manifest.State != ReadinessFailed || mismatch.ManifestIntegrity != "unanchored" || mismatch.AuditReady {
		t.Errorf("commit manifest with a wrong link = %+v, want failed/unanchored/not-ready", mismatch)
	}

	// Invalid v2 manifests fail readiness.
	bad := evalLocal(t, dir, h, store("ck-bad", "auto", strings.Repeat("e", 40), invalidCommitManifestBytes(t)))
	if bad.Manifest.State != ReadinessFailed || bad.ManifestIntegrity != "invalid" || bad.AuditReady {
		t.Errorf("invalid manifest = %+v, want failed/invalid/not-ready", bad)
	}

	// An unlinked v2 workspace manifest is ready.
	ws := evalLocal(t, dir, h, store("ck-ws", "auto", "", workspaceManifest))
	if ws.Manifest.State != ReadinessReady || ws.ManifestScope != "workspace" || ws.ManifestIntegrity != "workspace" {
		t.Errorf("workspace manifest = %+v, want ready/workspace/workspace", ws)
	}

	// A linked v2 workspace manifest is unanchored.
	wsLinked := evalLocal(t, dir, h, store("ck-wslinked", "auto", strings.Repeat("d", 40), workspaceManifest))
	if wsLinked.Manifest.State != ReadinessFailed || wsLinked.ManifestIntegrity != "unanchored" {
		t.Errorf("workspace manifest on a linked checkpoint = %+v, want failed/unanchored", wsLinked)
	}

	// Version-1 manifests remain ready as legacy records.
	legacy := evalLocal(t, dir, h, store("ck-legacyv1", "auto", strings.Repeat("f", 40), marshal(blobs.Manifest{
		Version: 1, CreatedAt: 1, RepoRoot: "/r", Files: []blobs.ManifestFile{wsFile},
	})))
	if legacy.Manifest.State != ReadinessReady || legacy.ManifestIntegrity != "legacy" {
		t.Errorf("v1 manifest = %+v, want ready/legacy", legacy)
	}

	// Commit manifests on non-auto checkpoints are unanchored.
	nonAutoHash := strings.Repeat("9", 40)
	nonAutoManifest := marshal(blobs.Manifest{
		Version: 2, Scope: blobs.ScopeCommit, ObjectFormat: blobs.ObjectFormatSHA1,
		CommitHash: nonAutoHash, TreeID: nonAutoHash,
		Files: []blobs.ManifestFile{{Path: "a.go", Blob: strings.Repeat("c", 64), Size: 1, EntryType: blobs.EntryRegular, GitMode: "100644", GitObjectID: nonAutoHash}},
	})
	nonAuto := evalLocal(t, dir, h, store("ck-manual", "manual", nonAutoHash, nonAutoManifest))
	if nonAuto.Manifest.State != ReadinessFailed || nonAuto.ManifestIntegrity != "unanchored" {
		t.Errorf("commit manifest on a manual checkpoint = %+v, want failed/unanchored", nonAuto)
	}
}
