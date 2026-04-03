package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// TestBuildPushPayload_EnrichesFromCheckpoint verifies that buildPushPayload
// reads session_count and providers scoped to the checkpoint - not the whole
// repo - and includes them in the returned payload.
func TestBuildPushPayload_EnrichesFromCheckpoint(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()

	repoID := insertRepo(t, h, 100_000)

	// Create two checkpoints: cp1 uses claude_code, cp2 uses cursor.
	cp1 := insertCheckpoint(t, h, repoID, 200_000, "auto")
	cp2 := insertCheckpoint(t, h, repoID, 300_000, "auto")

	// Stats for each.
	if err := h.Queries.UpsertCheckpointStats(ctx, sqldb.UpsertCheckpointStatsParams{
		CheckpointID: cp1,
		SessionCount: 2,
		FilesChanged: 1,
	}); err != nil {
		t.Fatalf("upsert stats cp1: %v", err)
	}
	if err := h.Queries.UpsertCheckpointStats(ctx, sqldb.UpsertCheckpointStatsParams{
		CheckpointID: cp2,
		SessionCount: 5,
		FilesChanged: 3,
	}); err != nil {
		t.Fatalf("upsert stats cp2: %v", err)
	}

	// Source + sessions for different providers.
	src1 := insertSource(t, h, repoID, "claude-source")
	sess1 := insertSession(t, h, repoID, src1, "sess-claude")
	linkSession(t, h, sess1, cp1)

	// Create a cursor source (different provider).
	_, err := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID:     "src-cursor",
		RepositoryID: repoID,
		Provider:     "cursor",
		SourceKey:    "cursor-source",
		LastSeenAt:   300_000,
		CreatedAt:    300_000,
	})
	if err != nil {
		t.Fatalf("insert cursor source: %v", err)
	}
	cursorSess, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID:         "sess-cursor",
		ProviderSessionID: "sess-cursor-prov",
		RepositoryID:      repoID,
		Provider:          "cursor",
		SourceID:          "src-cursor",
		StartedAt:         300_000,
		LastSeenAt:        300_000,
		MetadataJson:      `{}`,
	})
	if err != nil {
		t.Fatalf("insert cursor session: %v", err)
	}
	linkSession(t, h, cursorSess.SessionID, cp2)

	// Build payload for cp1 - should see only claude_code, session_count=2.
	result1 := &AttributionResult{
		AILines: 10, HumanLines: 40, TotalLines: 50,
		FilesTotal: 1, FilesAITouched: 1,
	}
	p1 := buildPushPayload(ctx, h, result1,
		"https://github.com/test/repo.git", "main", "aaa111", "commit 1", cp1)

	if p1.SessionCount != 2 {
		t.Errorf("cp1 session_count = %d, want 2", p1.SessionCount)
	}
	if len(p1.Providers) != 1 || p1.Providers[0] != "claude_code" {
		t.Errorf("cp1 providers = %v, want [claude_code]", p1.Providers)
	}

	// Build payload for cp2 - should see only cursor, session_count=5.
	result2 := &AttributionResult{
		AILines: 30, HumanLines: 20, TotalLines: 50,
		FilesTotal: 3, FilesAITouched: 2,
	}
	p2 := buildPushPayload(ctx, h, result2,
		"https://github.com/test/repo.git", "feature", "bbb222", "commit 2", cp2)

	if p2.SessionCount != 5 {
		t.Errorf("cp2 session_count = %d, want 5", p2.SessionCount)
	}
	if len(p2.Providers) != 1 || p2.Providers[0] != "cursor" {
		t.Errorf("cp2 providers = %v, want [cursor]", p2.Providers)
	}

	// Verify attribution fields pass through.
	if p2.AILines != 30 {
		t.Errorf("ai_lines = %d, want 30", p2.AILines)
	}
	if p2.Branch != "feature" {
		t.Errorf("branch = %q, want feature", p2.Branch)
	}
	if p2.AttrVersion != "v1" {
		t.Errorf("attr_version = %q, want v1", p2.AttrVersion)
	}
}

// TestBuildPushPayload_NoEnrichmentData verifies graceful fallback when the
// checkpoint has no stats or linked sessions.
func TestBuildPushPayload_NoEnrichmentData(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()

	repoID := insertRepo(t, h, 100_000)
	cpID := insertCheckpoint(t, h, repoID, 200_000, "auto")

	result := &AttributionResult{
		CommitHash: "abc123",
		AILines:    5, HumanLines: 95, TotalLines: 100,
	}

	p := buildPushPayload(ctx, h, result,
		"https://github.com/test/repo.git", "main", "abc123", "test commit", cpID)

	if p.SessionCount != 0 {
		t.Errorf("session_count = %d, want 0", p.SessionCount)
	}
	if len(p.Providers) != 0 {
		t.Errorf("providers = %v, want empty", p.Providers)
	}
	if p.AILines != 5 || p.HumanLines != 95 {
		t.Errorf("attribution mismatch: ai=%d human=%d", p.AILines, p.HumanLines)
	}
}

// TestBuildPushPayload_JSONContract verifies that the serialized payload
// matches the backend's expected field names and omitempty behavior.
func TestBuildPushPayload_JSONContract(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()

	repoID := insertRepo(t, h, 100_000)
	cpID := insertCheckpoint(t, h, repoID, 200_000, "auto")
	if err := h.Queries.UpsertCheckpointStats(ctx, sqldb.UpsertCheckpointStatsParams{
		CheckpointID: cpID,
		SessionCount: 3,
		FilesChanged: 2,
	}); err != nil {
		t.Fatalf("upsert stats: %v", err)
	}

	srcID := insertSource(t, h, repoID, "src")
	sessID := insertSession(t, h, repoID, srcID, "sess-1")
	linkSession(t, h, sessID, cpID)

	result := &AttributionResult{
		AIExactLines:     10,
		AIFormattedLines: 5,
		AIModifiedLines:  2,
		AILines:          15,
		HumanLines:       85,
		TotalLines:       100,
		FilesTotal:       3,
		FilesAITouched:   2,
		Files: []FileAttribution{
			{Path: "main.go", AIExactLines: 10, TotalLines: 50},
		},
		Diagnostics: AttributionDiagnostics{EventsConsidered: 7},
	}

	p := buildPushPayload(ctx, h, result,
		"https://github.com/test/repo.git", "main", "abc123", "fix: bug", cpID)

	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Required fields always present.
	required := map[string]any{
		"remote_url":          "https://github.com/test/repo.git",
		"commit_hash":        "abc123",
		"attribution_version": "v1",
		"ai_lines":           float64(15),
		"human_lines":        float64(85),
		"total_lines":        float64(100),
		"session_count":      float64(3),
	}
	for k, want := range required {
		got, ok := m[k]
		if !ok {
			t.Errorf("missing field %q", k)
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}

	// providers should be checkpoint-scoped.
	providers, ok := m["providers"].([]any)
	if !ok || len(providers) != 1 {
		t.Errorf("providers = %v, want [claude_code]", m["providers"])
	} else if providers[0] != "claude_code" {
		t.Errorf("providers[0] = %v, want claude_code", providers[0])
	}

	// files should be a non-empty array.
	files, ok := m["files"].([]any)
	if !ok || len(files) != 1 {
		t.Errorf("files = %v, want 1-element array", m["files"])
	}

	// diagnostics should be an object.
	diag, ok := m["diagnostics"].(map[string]any)
	if !ok {
		t.Errorf("diagnostics not an object: %T", m["diagnostics"])
	} else if diag["events_considered"] != float64(7) {
		t.Errorf("diagnostics.events_considered = %v, want 7", diag["events_considered"])
	}
}

// TestBuildPushPayload_IncludesPlaybookSummary verifies that when a checkpoint
// has a saved playbook summary, buildPushPayload reads it and populates the
// PlaybookSummary field with the formatted "intent | outcome" string.
func TestBuildPushPayload_IncludesPlaybookSummary(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()

	repoID := insertRepo(t, h, 100_000)
	cpID := insertCheckpoint(t, h, repoID, 200_000, "auto")

	// Save a playbook summary for this checkpoint.
	summaryJSON := `{"title":"Add auth","intent":"Implement login flow","outcome":"Login page renders and authenticates","learnings":[],"friction":[],"open_items":[],"keywords":[]}`
	if err := h.Queries.SaveCheckpointSummary(ctx, sqldb.SaveCheckpointSummaryParams{
		CheckpointID: cpID,
		SummaryJson:  sql.NullString{String: summaryJSON, Valid: true},
		SummaryModel: sql.NullString{String: "claude-sonnet-4-6", Valid: true},
	}); err != nil {
		t.Fatalf("save summary: %v", err)
	}

	result := &AttributionResult{
		AILines: 10, HumanLines: 90, TotalLines: 100,
	}
	p := buildPushPayload(ctx, h, result,
		"https://github.com/test/repo.git", "main", "abc123", "add auth", cpID)

	if len(p.PlaybookJSON) == 0 {
		t.Error("PlaybookJSON should be populated from checkpoint summary")
	}

	// Verify the JSON contains the structured playbook fields.
	var pb map[string]any
	if err := json.Unmarshal(p.PlaybookJSON, &pb); err != nil {
		t.Fatalf("unmarshal PlaybookJSON: %v", err)
	}
	if pb["intent"] != "Implement login flow" {
		t.Errorf("intent = %v, want 'Implement login flow'", pb["intent"])
	}

	// Verify it serializes in the payload JSON.
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["playbook_json"]; !ok {
		t.Error("playbook_json missing from JSON output")
	}
}

// TestBuildPushPayload_NoSummary_OmitsField verifies that when no playbook
// summary exists, the field is empty and omitted from JSON.
func TestBuildPushPayload_NoSummary_OmitsField(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()

	repoID := insertRepo(t, h, 100_000)
	cpID := insertCheckpoint(t, h, repoID, 200_000, "auto")

	result := &AttributionResult{AILines: 5, HumanLines: 95, TotalLines: 100}
	p := buildPushPayload(ctx, h, result,
		"https://github.com/test/repo.git", "main", "abc123", "fix bug", cpID)

	if len(p.PlaybookJSON) != 0 {
		t.Errorf("PlaybookJSON should be nil when no summary exists, got %s", string(p.PlaybookJSON))
	}

	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["playbook_json"]; ok {
		t.Error("playbook_json should be omitted when empty")
	}
}

// TestBuildPushPayload_OmitsEmptyOptionals verifies omitempty tags prevent
// zero-value noise in the JSON.
func TestBuildPushPayload_OmitsEmptyOptionals(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()

	repoID := insertRepo(t, h, 100_000)
	cpID := insertCheckpoint(t, h, repoID, 200_000, "auto")

	result := &AttributionResult{}
	p := buildPushPayload(ctx, h, result,
		"https://github.com/test/repo.git", "", "", "", cpID)

	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	omitted := []string{
		"branch", "commit_subject",
		"session_count", "providers", "playbook_summary",
	}

	// cli_version is always set (defaults to "dev" at build time).
	if v, ok := m["cli_version"]; !ok || v != "dev" {
		t.Errorf("cli_version = %v, want \"dev\"", v)
	}
	for _, f := range omitted {
		if _, ok := m[f]; ok {
			t.Errorf("expected %q to be omitted when empty", f)
		}
	}
}

// TestCheckpointCompletion_AfterDerivedData verifies that derived data can be
// written while a checkpoint is still pending, and that completion happens
// only after enrichment is done.
func TestCheckpointCompletion_AfterDerivedData(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()

	repoID := insertRepo(t, h, 100_000)

	// Start with a pending checkpoint.
	cpID := "cp-sequencing-test"
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpID,
		RepositoryID: repoID,
		CreatedAt:    200_000,
		Kind:         "auto",
		Status:       "pending",
	}); err != nil {
		t.Fatal(err)
	}

	// Write derived data before completion.
	if err := h.Queries.UpsertCheckpointStats(ctx, sqldb.UpsertCheckpointStatsParams{
		CheckpointID: cpID,
		SessionCount: 3,
		FilesChanged: 7,
	}); err != nil {
		t.Fatal(err)
	}

	// The checkpoint should remain pending.
	cp, err := h.Queries.GetCheckpointByID(ctx, cpID)
	if err != nil {
		t.Fatal(err)
	}
	if cp.Status != "pending" {
		t.Fatalf("checkpoint status = %q after stats upsert, want pending", cp.Status)
	}

	// Derived data should already be visible.
	stats, err := h.Queries.GetCheckpointStats(ctx, cpID)
	if err != nil {
		t.Fatal("stats should exist while checkpoint is still pending")
	}
	if stats.SessionCount != 3 || stats.FilesChanged != 7 {
		t.Errorf("stats = {sessions:%d, files:%d}, want {3, 7}", stats.SessionCount, stats.FilesChanged)
	}

	// Complete the checkpoint after enrichment.
	if err := h.Queries.CompleteCheckpoint(ctx, sqldb.CompleteCheckpointParams{
		ManifestHash: sql.NullString{String: "fakehash", Valid: true},
		SizeBytes:    sql.NullInt64{Int64: 1000, Valid: true},
		CompletedAt:  sql.NullInt64{Int64: 200_001, Valid: true},
		CheckpointID: cpID,
	}); err != nil {
		t.Fatal(err)
	}

	// Completion should not clear the derived data.
	cp, _ = h.Queries.GetCheckpointByID(ctx, cpID)
	if cp.Status != "complete" {
		t.Fatalf("checkpoint status = %q after CompleteCheckpoint, want complete", cp.Status)
	}
	stats, _ = h.Queries.GetCheckpointStats(ctx, cpID)
	if stats.SessionCount != 3 {
		t.Errorf("stats.SessionCount = %d after completion, want 3", stats.SessionCount)
	}
}
