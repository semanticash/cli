package broker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/semanticash/cli/internal/store/impldb"
)

// ensureImplDB creates the implementations.db with migrations applied
// so that OpenNoMigrate can find it.
func ensureImplDB(t *testing.T, globalDir string) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(globalDir, "implementations.db")
	h, err := impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("create impldb: %v", err)
	}
	_ = impldb.Close(h)
}

func TestEmitObservation_WritesToGlobalDB(t *testing.T) {
	dir := t.TempDir()
	globalDir := filepath.Join(dir, "semantica-home")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SEMANTICA_HOME", globalDir)
	ensureImplDB(t, globalDir)

	ctx := context.Background()

	EmitObservation(ctx, Observation{
		Provider:          "claude_code",
		ProviderSessionID: "sess-abc",
		ParentSessionID:   "",
		SourceProjectPath: "/projects/api",
		TargetRepoPath:    "/repos/api",
		EventTs:           2000,
	})

	dbPath := filepath.Join(globalDir, "implementations.db")
	h, err := impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open impldb: %v", err)
	}
	defer func() { _ = impldb.Close(h) }()

	pending, err := h.Queries.ListPendingObservations(ctx, 10)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d observations, want 1", len(pending))
	}

	obs := pending[0]
	if obs.Provider != "claude_code" {
		t.Errorf("provider: got %q, want claude_code", obs.Provider)
	}
	if obs.ProviderSessionID != "sess-abc" {
		t.Errorf("provider_session_id: got %q, want sess-abc", obs.ProviderSessionID)
	}
	if !obs.SourceProjectPath.Valid || obs.SourceProjectPath.String != "/projects/api" {
		t.Errorf("source_project_path: got %v, want /projects/api", obs.SourceProjectPath)
	}
	if obs.TargetRepoPath != "/repos/api" {
		t.Errorf("target_repo_path: got %q", obs.TargetRepoPath)
	}
	if obs.EventTs != 2000 {
		t.Errorf("event_ts: got %d, want 2000", obs.EventTs)
	}
}

func TestEmitObservation_WithParentSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_HOME", dir)
	ensureImplDB(t, dir)

	ctx := context.Background()

	EmitObservation(ctx, Observation{
		Provider:          "claude_code",
		ProviderSessionID: "child-sess",
		ParentSessionID:   "parent-sess",
		SourceProjectPath: "/projects/api",
		TargetRepoPath:    "/repos/api",
		EventTs:           3000,
	})

	dbPath := filepath.Join(dir, "implementations.db")
	h, err := impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open impldb: %v", err)
	}
	defer func() { _ = impldb.Close(h) }()

	pending, _ := h.Queries.ListPendingObservations(ctx, 10)
	if len(pending) != 1 {
		t.Fatalf("got %d observations, want 1", len(pending))
	}
	if !pending[0].ParentSessionID.Valid || pending[0].ParentSessionID.String != "parent-sess" {
		t.Errorf("parent_session_id: got %v", pending[0].ParentSessionID)
	}
}

func TestEmitObservation_FailOpenWhenNoHome(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", "/nonexistent/path/that/does/not/exist")

	ctx := context.Background()

	// Should not panic or error — just silently skip.
	EmitObservation(ctx, Observation{
		Provider:          "claude_code",
		ProviderSessionID: "sess-fail",
		TargetRepoPath:    "/repos/api",
		EventTs:           1000,
	})
}

func TestEmitObservation_CreatesDBOnFirstCall(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_HOME", dir)
	// Do NOT call ensureImplDB — the DB file does not exist yet.

	ctx := context.Background()

	// Should create the DB and write the observation (first-call fallback).
	EmitObservation(ctx, Observation{
		Provider:          "claude_code",
		ProviderSessionID: "sess-first",
		TargetRepoPath:    "/repos/api",
		EventTs:           1000,
	})

	// Verify the DB was created and the observation was written.
	dbPath := filepath.Join(dir, "implementations.db")
	h, err := impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open impldb: %v", err)
	}
	defer func() { _ = impldb.Close(h) }()

	pending, _ := h.Queries.ListPendingObservations(ctx, 10)
	if len(pending) != 1 {
		t.Fatalf("got %d observations, want 1 (created on first call)", len(pending))
	}
	if pending[0].ProviderSessionID != "sess-first" {
		t.Errorf("session: got %q", pending[0].ProviderSessionID)
	}
}

func TestWriteEventsToRepo_EmitsObservation(t *testing.T) {
	dir := t.TempDir()
	globalDir := filepath.Join(dir, "semantica-home")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SEMANTICA_HOME", globalDir)
	ensureImplDB(t, globalDir)

	repoPath := filepath.Join(dir, "myrepo")
	tempRepoWithDB(t, repoPath)

	ctx := context.Background()

	events := []RawEvent{
		{
			EventID:           "evt-obs-1",
			SourceKey:         "/data/session.jsonl",
			Provider:          "claude_code",
			Timestamp:         2000,
			Kind:              "assistant",
			Role:              "assistant",
			ProviderSessionID: "sess-obs",
			SessionStartedAt:  1500,
			SessionMetaJSON:   `{}`,
			SourceProjectPath: "/projects/cli",
		},
		{
			EventID:           "evt-obs-2",
			SourceKey:         "/data/session.jsonl",
			Provider:          "claude_code",
			Timestamp:         3000,
			Kind:              "user",
			Role:              "user",
			ProviderSessionID: "sess-obs",
			SessionStartedAt:  1500,
			SourceProjectPath: "/projects/cli",
		},
	}

	_, err := WriteEventsToRepo(ctx, repoPath, events, nil)
	if err != nil {
		t.Fatalf("WriteEventsToRepo: %v", err)
	}

	dbPath := filepath.Join(globalDir, "implementations.db")
	h, err := impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open impldb: %v", err)
	}
	defer func() { _ = impldb.Close(h) }()

	pending, _ := h.Queries.ListPendingObservations(ctx, 10)
	if len(pending) != 1 {
		t.Fatalf("got %d observations, want 1 (deduped by provider+session)", len(pending))
	}

	obs := pending[0]
	if obs.Provider != "claude_code" {
		t.Errorf("provider: got %q", obs.Provider)
	}
	if obs.ProviderSessionID != "sess-obs" {
		t.Errorf("session: got %q", obs.ProviderSessionID)
	}
	if obs.TargetRepoPath != repoPath {
		t.Errorf("target: got %q, want %q", obs.TargetRepoPath, repoPath)
	}
	if !obs.SourceProjectPath.Valid || obs.SourceProjectPath.String != "/projects/cli" {
		t.Errorf("source_project_path: got %v, want /projects/cli", obs.SourceProjectPath)
	}
	if obs.EventTs != 3000 {
		t.Errorf("event_ts: got %d, want 3000 (latest event)", obs.EventTs)
	}
}

func TestWriteEventsToRepo_AggregatesAcrossSourceKeys(t *testing.T) {
	dir := t.TempDir()
	globalDir := filepath.Join(dir, "semantica-home")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SEMANTICA_HOME", globalDir)
	ensureImplDB(t, globalDir)

	repoPath := filepath.Join(dir, "myrepo")
	tempRepoWithDB(t, repoPath)

	ctx := context.Background()

	// Same provider_session_id but different source_keys → two groups in write.go.
	// The observation should aggregate: latest event_ts, non-empty parent.
	events := []RawEvent{
		{
			EventID:           "evt-sk1-1",
			SourceKey:         "/data/session-a.jsonl",
			Provider:          "claude_code",
			Timestamp:         2000,
			Kind:              "assistant",
			Role:              "assistant",
			ProviderSessionID: "sess-multi",
			SessionStartedAt:  1500,
			SessionMetaJSON:   `{}`,
			SourceProjectPath: "/projects/cli",
		},
		{
			EventID:           "evt-sk2-1",
			SourceKey:         "/data/session-b.jsonl",
			Provider:          "claude_code",
			Timestamp:         5000,
			Kind:              "assistant",
			Role:              "assistant",
			ProviderSessionID: "sess-multi",
			ParentSessionID:   "parent-sess",
			SessionStartedAt:  1500,
			SessionMetaJSON:   `{}`,
			SourceProjectPath: "/projects/cli",
		},
	}

	_, err := WriteEventsToRepo(ctx, repoPath, events, nil)
	if err != nil {
		t.Fatalf("WriteEventsToRepo: %v", err)
	}

	dbPath := filepath.Join(globalDir, "implementations.db")
	h, err := impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open impldb: %v", err)
	}
	defer func() { _ = impldb.Close(h) }()

	pending, _ := h.Queries.ListPendingObservations(ctx, 10)
	if len(pending) != 1 {
		t.Fatalf("got %d observations, want 1 (aggregated across source keys)", len(pending))
	}

	obs := pending[0]
	if obs.EventTs != 5000 {
		t.Errorf("event_ts: got %d, want 5000 (max across groups)", obs.EventTs)
	}
	if !obs.ParentSessionID.Valid || obs.ParentSessionID.String != "parent-sess" {
		t.Errorf("parent_session_id: got %v, want parent-sess (from second group)", obs.ParentSessionID)
	}
}
