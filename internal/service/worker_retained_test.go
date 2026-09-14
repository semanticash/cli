package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/semanticash/cli/internal/broker"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

func TestDrainOnce_DeliversRetainedCaptureWithoutCheckpointOrGate(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	setupDrainEnv(t, root)
	t.Setenv("SEMANTICA_DURABLE_ROUTING", "0")
	semDir := filepath.Join(root, ".semantica")
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o600); err != nil {
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
	defer func() { _ = sqlstore.Close(h) }()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{RepositoryID: "repo", RootPath: root, CreatedAt: 1, EnabledAt: 1}); err != nil {
		t.Fatal(err)
	}
	ev := broker.RawEvent{EventID: "queued", Provider: "test", ProviderSessionID: "session", TurnID: "turn", SourceKey: "transcript", Kind: "assistant", Timestamp: 1}
	matches := []broker.RepoMatch{{Repo: broker.RegisteredRepo{Path: root, CanonicalPath: broker.CanonicalRepoPath(root), Active: true}, Events: []broker.RawEvent{ev}}}
	if _, err := broker.RetainEvents(ctx, []broker.RawEvent{ev}, matches, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := DrainOnce(ctx, func(context.Context, WorkerInput) error { t.Fatal("unexpected checkpoint"); return nil }); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := h.DB.QueryRowContext(ctx, `SELECT count(*) FROM agent_events WHERE event_id = ? AND turn_id = ?`, ev.EventID, ev.TurnID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("worker delivered %d events, want 1", count)
	}
}
