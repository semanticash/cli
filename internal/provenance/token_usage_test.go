package provenance

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
)

func TestLoadTurnTokenUsage_DeduplicatesAndScopesRootSession(t *testing.T) {
	ctx := context.Background()
	repoRoot, semDir, _ := newSyncRepo(t, ctx)
	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	var sessionID, repoID, sourceID string
	if err := h.DB.QueryRowContext(ctx,
		"select session_id, repository_id, source_id from agent_sessions where provider_session_id = 'ps'",
	).Scan(&sessionID, &repoID, &sourceID); err != nil {
		t.Fatal(err)
	}
	insertUsageEvent(t, h.DB, "e1", sessionID, repoID, "turn-1", "message-1", 1, 2, 3, 4)
	insertUsageEvent(t, h.DB, "e2", sessionID, repoID, "turn-1", "message-1", 1, 5, 3, 4)
	insertUsageEvent(t, h.DB, "e3", sessionID, repoID, "turn-1", "message-2", 0, 0, 0, 0)

	if _, err := h.DB.ExecContext(ctx,
		`insert into agent_sessions (session_id, provider_session_id, parent_session_id, repository_id, provider, source_id, started_at, last_seen_at, metadata_json)
		 values ('child', 'child-provider', ?, ?, 'codex', ?, 1, 1, '{}')`, sessionID, repoID, sourceID); err != nil {
		t.Fatal(err)
	}
	insertUsageEvent(t, h.DB, "child-e1", "child", repoID, "turn-1", "child-message", 100, 100, 100, 100)

	usage, err := LoadTurnTokenUsage(ctx, repoRoot, "codex", "ps", "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	want := TurnTokenUsage{InputUncached: 1, Output: 5, CacheRead: 3, CacheWrite: 4}
	if usage == nil || *usage != want {
		t.Fatalf("LoadTurnTokenUsage() = %+v, want %+v", usage, want)
	}
}

func TestLoadTurnTokenUsage_InvalidRowFailsClosed(t *testing.T) {
	ctx := context.Background()
	repoRoot, semDir, _ := newSyncRepo(t, ctx)
	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	var sessionID, repoID string
	if err := h.DB.QueryRowContext(ctx,
		"select session_id, repository_id from agent_sessions where provider_session_id = 'ps'",
	).Scan(&sessionID, &repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.ExecContext(ctx,
		`insert into agent_events (event_id, session_id, repository_id, ts, kind, role, tokens_in, turn_id, event_source)
		 values ('partial', ?, ?, 1, 'assistant', 'assistant', 1, 'turn-1', 'hook')`, sessionID, repoID); err != nil {
		t.Fatal(err)
	}
	usage, err := LoadTurnTokenUsage(ctx, repoRoot, "codex", "ps", "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	if usage != nil {
		t.Fatalf("LoadTurnTokenUsage() = %+v, want unavailable", usage)
	}
}

func TestLoadTurnTokenUsage_InvalidDuplicateFailsClosed(t *testing.T) {
	ctx := context.Background()
	repoRoot, semDir, _ := newSyncRepo(t, ctx)
	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	var sessionID, repoID string
	if err := h.DB.QueryRowContext(ctx,
		"select session_id, repository_id from agent_sessions where provider_session_id = 'ps'",
	).Scan(&sessionID, &repoID); err != nil {
		t.Fatal(err)
	}
	insertUsageEvent(t, h.DB, "valid", sessionID, repoID, "turn-1", "message-1", 1, 2, 3, 4)
	if _, err := h.DB.ExecContext(ctx,
		`insert into agent_events (event_id, session_id, repository_id, ts, kind, role, tokens_in, provider_event_id, turn_id, event_source)
		 values ('partial', ?, ?, 2, 'assistant', 'assistant', 1, 'message-1', 'turn-1', 'hook')`, sessionID, repoID); err != nil {
		t.Fatal(err)
	}

	usage, err := LoadTurnTokenUsage(ctx, repoRoot, "codex", "ps", "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	if usage != nil {
		t.Fatalf("LoadTurnTokenUsage() = %+v, want unavailable", usage)
	}
}

func TestLoadTurnTokenUsage_OverflowFailsClosed(t *testing.T) {
	ctx := context.Background()
	repoRoot, semDir, _ := newSyncRepo(t, ctx)
	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	var sessionID, repoID string
	if err := h.DB.QueryRowContext(ctx,
		"select session_id, repository_id from agent_sessions where provider_session_id = 'ps'",
	).Scan(&sessionID, &repoID); err != nil {
		t.Fatal(err)
	}
	const maxInt64 = int64(1<<63 - 1)
	insertUsageEvent(t, h.DB, "max", sessionID, repoID, "turn-1", "message-1", maxInt64, 0, 0, 0)
	insertUsageEvent(t, h.DB, "one", sessionID, repoID, "turn-1", "message-2", 1, 0, 0, 0)

	usage, err := LoadTurnTokenUsage(ctx, repoRoot, "codex", "ps", "turn-1")
	if err == nil {
		t.Fatalf("LoadTurnTokenUsage() = %+v, nil error; want overflow error", usage)
	}
	if usage != nil {
		t.Fatalf("LoadTurnTokenUsage() = %+v, want unavailable", usage)
	}
}

func TestLoadTurnTokenUsage_NegativeRowCannotCancel(t *testing.T) {
	ctx := context.Background()
	repoRoot, semDir, _ := newSyncRepo(t, ctx)
	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	var sessionID, repoID string
	if err := h.DB.QueryRowContext(ctx,
		"select session_id, repository_id from agent_sessions where provider_session_id = 'ps'",
	).Scan(&sessionID, &repoID); err != nil {
		t.Fatal(err)
	}
	insertUsageEvent(t, h.DB, "negative", sessionID, repoID, "turn-1", "message-1", -5, 1, 1, 1)
	insertUsageEvent(t, h.DB, "positive", sessionID, repoID, "turn-1", "message-2", 10, 1, 1, 1)
	usage, err := LoadTurnTokenUsage(ctx, repoRoot, "codex", "ps", "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	if usage != nil {
		t.Fatalf("LoadTurnTokenUsage() = %+v, want unavailable", usage)
	}
}

func TestPackageTurn_TokenUsageIsFrozenAndMonotonic(t *testing.T) {
	ctx := context.Background()
	repoRoot, semDir, repoStore := newSyncRepo(t, ctx)
	first := &TurnTokenUsage{InputUncached: 10, Output: 20, CacheRead: 30, CacheWrite: 40}
	PackageTurn(ctx, repoRoot, TurnContext{
		TurnID: "turn-1", SessionID: "ps", Provider: "codex", StartedAt: 1, CompletedAt: 2, TokenUsage: first,
	}, repoStore)
	PackageTurn(ctx, repoRoot, TurnContext{
		TurnID: "turn-1", SessionID: "ps", Provider: "codex", StartedAt: 1, CompletedAt: 2,
	}, repoStore)
	PackageTurn(ctx, repoRoot, TurnContext{
		TurnID: "turn-1", SessionID: "ps", Provider: "codex", StartedAt: 1, CompletedAt: 2,
		TokenUsage: &TurnTokenUsage{InputUncached: 90, Output: 90, CacheRead: 90, CacheWrite: 90},
	}, repoStore)

	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	var in, out, read, write sql.NullInt64
	if err := h.DB.QueryRowContext(ctx,
		"select tokens_in, tokens_out, tokens_cache_read, tokens_cache_create from provenance_manifests where turn_id = 'turn-1'",
	).Scan(&in, &out, &read, &write); err != nil {
		t.Fatal(err)
	}
	if !in.Valid || in.Int64 != 10 || out.Int64 != 20 || read.Int64 != 30 || write.Int64 != 40 {
		t.Fatalf("stored usage = %v/%v/%v/%v, want first candidate", in, out, read, write)
	}

	results, err := SyncPendingTurns(ctx, repoRoot, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("SyncPendingTurns() returned %d results, want 1", len(results))
	}
	var env struct {
		TokenUsage *TurnTokenUsage `json:"token_usage"`
	}
	if err := json.Unmarshal(results[0].Envelope, &env); err != nil {
		t.Fatal(err)
	}
	if env.TokenUsage == nil || *env.TokenUsage != *first {
		t.Fatalf("envelope usage = %+v, want %+v", env.TokenUsage, first)
	}
}

func TestPackageTurn_TokenUsageBackfillsUnavailable(t *testing.T) {
	ctx := context.Background()
	repoRoot, semDir, repoStore := newSyncRepo(t, ctx)
	base := TurnContext{TurnID: "turn-1", SessionID: "ps", Provider: "codex", StartedAt: 1, CompletedAt: 2}
	PackageTurn(ctx, repoRoot, base, repoStore)
	base.TokenUsage = &TurnTokenUsage{InputUncached: 1, Output: 2, CacheRead: 3, CacheWrite: 4}
	PackageTurn(ctx, repoRoot, base, repoStore)

	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	var in, out, read, write sql.NullInt64
	if err := h.DB.QueryRowContext(ctx,
		"select tokens_in, tokens_out, tokens_cache_read, tokens_cache_create from provenance_manifests where turn_id = 'turn-1'",
	).Scan(&in, &out, &read, &write); err != nil {
		t.Fatal(err)
	}
	if !in.Valid || in.Int64 != 1 || out.Int64 != 2 || read.Int64 != 3 || write.Int64 != 4 {
		t.Fatalf("stored usage = %v/%v/%v/%v, want backfilled candidate", in, out, read, write)
	}
}

func insertUsageEvent(t *testing.T, db *sql.DB, eventID, sessionID, repoID, turnID, providerEventID string, in, out, read, write int64) {
	t.Helper()
	if _, err := db.Exec(
		`insert into agent_events (event_id, session_id, repository_id, ts, kind, role, tokens_in, tokens_out, tokens_cache_read, tokens_cache_create, provider_event_id, turn_id, event_source)
		 values (?, ?, ?, 1, 'assistant', 'assistant', ?, ?, ?, ?, ?, ?, 'hook')`,
		eventID, sessionID, repoID, in, out, read, write, providerEventID, turnID,
	); err != nil {
		t.Fatal(err)
	}
}
