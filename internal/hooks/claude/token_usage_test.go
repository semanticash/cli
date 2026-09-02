package claude

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/provenance"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

func TestClaudeTokenUsagePreservesMeasuredZero(t *testing.T) {
	ctx := context.Background()
	repoPath := t.TempDir()
	semDir := filepath.Join(repoPath, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
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
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: uuid.NewString(),
		RootPath:     repoPath,
		CreatedAt:    1,
		EnabledAt:    1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := sqlstore.Close(h); err != nil {
		t.Fatal(err)
	}

	transcript := filepath.Join(repoPath, "session.jsonl")
	line := `{"type":"assistant","message":{"id":"msg-1","model":"claude-opus-4-6","role":"assistant","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":100,"output_tokens":30,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}},"timestamp":"2026-09-02T10:00:00Z","sessionId":"session-1"}`
	if err := os.WriteFile(transcript, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	events, _, err := (&Provider{}).ReadFromOffset(ctx, transcript, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	events[0].TurnID = "turn-1"
	if _, err := broker.WriteEventsToRepo(ctx, repoPath, events, nil); err != nil {
		t.Fatal(err)
	}

	usage, state, err := provenance.LoadTurnTokenUsage(ctx, repoPath, providerName, events[0].ProviderSessionID, "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	want := provenance.TurnTokenUsage{InputUncached: 100, Output: 30, CacheRead: 0, CacheWrite: 0}
	if usage == nil || *usage != want {
		t.Fatalf("LoadTurnTokenUsage() = %+v, want %+v", usage, want)
	}
	if state != provenance.TurnTokenUsageValid {
		t.Fatalf("state = %v, want valid", state)
	}
}
