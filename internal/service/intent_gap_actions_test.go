package service

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/semanticash/cli/internal/intentgap"
)

// seedAgentEventWithTools inserts an assistant event carrying a
// tool_uses JSON envelope. Mirrors seedEvent but adds the tool_uses
// column so the action loader query can pick the row up.
func seedAgentEventWithTools(t *testing.T, db *sql.DB, eventID, sessionID, repoID, turnID, toolUses string, ts int64) {
	t.Helper()
	if _, err := db.Exec(
		`insert into agent_events(event_id, session_id, repository_id, ts, kind, role, summary, turn_id, tool_uses)
         values(?,?,?,?,?,?,?,?,?)`,
		eventID, sessionID, repoID, ts, "assistant", "assistant", "", turnID, toolUses,
	); err != nil {
		t.Fatalf("insert agent_events: %v", err)
	}
}

// A captured Edit event surfaces as a BundleAgentAction whose
// FilePath is read from tool_uses. No payload fetch is required for
// Edit because the path lives in the tool_uses envelope.
func TestSQLiteAgentActionLoader_EditFromToolUses(t *testing.T) {
	root, db, cleanup := initTurnLoaderRepo(t)
	defer cleanup()
	repoID := seedRepository(t, db, root)

	seedCheckpoint(t, db, "ck-1", repoID, 1000)
	seedCommitLink(t, db, "c1", repoID, "ck-1")
	seedSession(t, db, "sess-1", repoID, "ck-1")
	seedAgentEventWithTools(t, db, "ev-1", "sess-1", repoID, "turn-1",
		`{"tools":[{"name":"Edit","file_path":"internal/handler.go"}]}`, 500)

	loader := newSQLiteAgentActionLoader(root)
	got, err := loader.LoadActionsForCommits(context.Background(), []string{"c1"})
	if err != nil {
		t.Fatalf("LoadActionsForCommits: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d actions, want 1", len(got))
	}
	a := got[0]
	if a.ToolName != "Edit" {
		t.Errorf("ToolName = %q, want Edit", a.ToolName)
	}
	if a.FilePath != "internal/handler.go" {
		t.Errorf("FilePath = %q, want internal/handler.go", a.FilePath)
	}
	if a.TurnID != "turn-1" || a.CheckpointID != "ck-1" {
		t.Errorf("TurnID/CheckpointID = %q / %q", a.TurnID, a.CheckpointID)
	}
}

// A missing lineage database represents a fresh repository with no
// captures. The loader returns an empty result rather than an error.
func TestSQLiteAgentActionLoader_NoLineageDBIsEmpty(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".semantica"), 0o755); err != nil {
		t.Fatal(err)
	}
	loader := newSQLiteAgentActionLoader(root)
	got, err := loader.LoadActionsForCommits(context.Background(), []string{"c1"})
	if err != nil {
		t.Errorf("expected nil error for missing DB, got %v", err)
	}
	if got != nil {
		t.Errorf("expected nil actions for missing DB, got %v", got)
	}
}

// A captured Read event records lookup activity but is not agent
// activity that anchors evidence about file modifications. The
// loader must produce zero actions for read-only tools even when
// the row otherwise qualifies for the window.
func TestSQLiteAgentActionLoader_ReadOnlyToolsProduceNoActions(t *testing.T) {
	root, db, cleanup := initTurnLoaderRepo(t)
	defer cleanup()
	repoID := seedRepository(t, db, root)

	seedCheckpoint(t, db, "ck-1", repoID, 1000)
	seedCommitLink(t, db, "c1", repoID, "ck-1")
	seedSession(t, db, "sess-1", repoID, "ck-1")
	seedAgentEventWithTools(t, db, "ev-1", "sess-1", repoID, "turn-1",
		`{"tools":[{"name":"Read","file_path":"internal/handler.go"}]}`, 500)

	loader := newSQLiteAgentActionLoader(root)
	got, err := loader.LoadActionsForCommits(context.Background(), []string{"c1"})
	if err != nil {
		t.Fatalf("LoadActionsForCommits: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Read event leaked into the action bundle: %+v", got)
	}
}

// Actions arrive ordered oldest first so the assembler can drop the
// prefix when applying a most-recent retention cap. This pins the
// SQL ORDER BY clause against accidental changes.
func TestSQLiteAgentActionLoader_OrderedOldestFirst(t *testing.T) {
	root, db, cleanup := initTurnLoaderRepo(t)
	defer cleanup()
	repoID := seedRepository(t, db, root)

	seedCheckpoint(t, db, "ck-1", repoID, 1000)
	seedCommitLink(t, db, "c1", repoID, "ck-1")
	seedSession(t, db, "sess-1", repoID, "ck-1")

	seedAgentEventWithTools(t, db, "ev-late", "sess-1", repoID, "turn-2",
		`{"tools":[{"name":"Edit","file_path":"b.go"}]}`, 800)
	seedAgentEventWithTools(t, db, "ev-early", "sess-1", repoID, "turn-1",
		`{"tools":[{"name":"Edit","file_path":"a.go"}]}`, 400)

	loader := newSQLiteAgentActionLoader(root)
	got, err := loader.LoadActionsForCommits(context.Background(), []string{"c1"})
	if err != nil {
		t.Fatalf("LoadActionsForCommits: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d actions, want 2", len(got))
	}
	if got[0].FilePath != "a.go" || got[1].FilePath != "b.go" {
		t.Errorf("order = %q,%q; want a.go,b.go (oldest first)", got[0].FilePath, got[1].FilePath)
	}
}

// A captured Read event accompanying an Edit event in the same
// envelope is dropped at the per-pair filter while the Edit is
// kept.
func TestSQLiteAgentActionLoader_MixedEnvelopeOnlyEmitsMutatingTools(t *testing.T) {
	root, db, cleanup := initTurnLoaderRepo(t)
	defer cleanup()
	repoID := seedRepository(t, db, root)

	seedCheckpoint(t, db, "ck-1", repoID, 1000)
	seedCommitLink(t, db, "c1", repoID, "ck-1")
	seedSession(t, db, "sess-1", repoID, "ck-1")
	seedAgentEventWithTools(t, db, "ev-1", "sess-1", repoID, "turn-1",
		`{"tools":[{"name":"Edit","file_path":"a.go"},{"name":"Read","file_path":"b.go"}]}`, 500)

	loader := newSQLiteAgentActionLoader(root)
	got, err := loader.LoadActionsForCommits(context.Background(), []string{"c1"})
	if err != nil {
		t.Fatalf("LoadActionsForCommits: %v", err)
	}
	if len(got) != 1 || got[0].ToolName != "Edit" {
		t.Errorf("expected only the Edit action, got %+v", got)
	}
}

// One checkpoint can back several commits. The loader deduplicates
// repeated actions across commit queries.
func TestSQLiteAgentActionLoader_DedupsAcrossCommitsSharingCheckpoint(t *testing.T) {
	root, db, cleanup := initTurnLoaderRepo(t)
	defer cleanup()
	repoID := seedRepository(t, db, root)

	seedCheckpoint(t, db, "ck-1", repoID, 1000)
	// Two commits backed by the same checkpoint.
	seedCommitLink(t, db, "c1", repoID, "ck-1")
	seedCommitLink(t, db, "c2", repoID, "ck-1")
	seedSession(t, db, "sess-1", repoID, "ck-1")
	seedAgentEventWithTools(t, db, "ev-1", "sess-1", repoID, "turn-1",
		`{"tools":[{"name":"Edit","file_path":"a.go"}]}`, 500)

	loader := newSQLiteAgentActionLoader(root)
	got, err := loader.LoadActionsForCommits(context.Background(), []string{"c1", "c2"})
	if err != nil {
		t.Fatalf("LoadActionsForCommits: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d actions, want 1 (duplicated across shared checkpoint)", len(got))
	}
	if got[0].FilePath != "a.go" {
		t.Errorf("FilePath = %q, want a.go", got[0].FilePath)
	}
}

// Compile-time check: the production loader satisfies the interface.
var _ intentgap.AgentActionLoader = (*sqliteAgentActionLoader)(nil)
