package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/intentgap"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
)

// initTurnLoaderRepo creates a repository with a migrated lineage database.
func initTurnLoaderRepo(t *testing.T) (string, *sql.DB, func()) {
	t.Helper()
	repoRoot := t.TempDir()
	semDir := filepath.Join(repoRoot, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(semDir, "lineage.db")
	if err := sqlstore.MigratePath(context.Background(), dbPath); err != nil {
		t.Fatalf("MigratePath: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return repoRoot, db, func() { _ = db.Close() }
}

// seedRepository creates the parent row required by lineage tables.
func seedRepository(t *testing.T, db *sql.DB, root string) string {
	t.Helper()
	repoID := "repo-1"
	if _, err := db.Exec(
		`insert into repositories(repository_id, root_path, created_at) values(?,?,?)`,
		repoID, root, time.Now().UnixMilli(),
	); err != nil {
		t.Fatal(err)
	}
	return repoID
}

// seedCheckpoint inserts a checkpoint row at the given created_at.
func seedCheckpoint(t *testing.T, db *sql.DB, id, repoID string, createdAt int64) {
	t.Helper()
	if _, err := db.Exec(
		`insert into checkpoints(checkpoint_id, repository_id, created_at, kind, status) values(?,?,?,?,?)`,
		id, repoID, createdAt, "auto", "complete",
	); err != nil {
		t.Fatalf("insert checkpoint: %v", err)
	}
}

// seedCommitLink links a commit to a checkpoint.
func seedCommitLink(t *testing.T, db *sql.DB, commitHash, repoID, checkpointID string) {
	t.Helper()
	if _, err := db.Exec(
		`insert into commit_links(commit_hash, repository_id, checkpoint_id, linked_at) values(?,?,?,?)`,
		commitHash, repoID, checkpointID, time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert commit_link: %v", err)
	}
}

// seedAgentSource creates the parent row required by agent sessions.
func seedAgentSource(t *testing.T, db *sql.DB, repoID string) string {
	t.Helper()
	srcID := "src-" + repoID
	if _, err := db.Exec(
		`insert or ignore into agent_sources(source_id, repository_id, provider, source_key, last_seen_at, created_at) values(?,?,?,?,?,?)`,
		srcID, repoID, "claude_code", "/dev/null", time.Now().UnixMilli(), time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert agent_sources: %v", err)
	}
	return srcID
}

// seedSession inserts an agent_sessions row and links it to a checkpoint.
func seedSession(t *testing.T, db *sql.DB, sessionID, repoID, checkpointID string) {
	t.Helper()
	srcID := seedAgentSource(t, db, repoID)
	if _, err := db.Exec(
		`insert into agent_sessions(session_id, provider_session_id, repository_id, provider, source_id, started_at, last_seen_at, metadata_json) values(?,?,?,?,?,?,?,?)`,
		sessionID, sessionID, repoID, "claude_code", srcID, time.Now().UnixMilli(), time.Now().UnixMilli(), "{}",
	); err != nil {
		t.Fatalf("insert agent_sessions: %v", err)
	}
	if _, err := db.Exec(
		`insert into session_checkpoints(session_id, checkpoint_id) values(?,?)`,
		sessionID, checkpointID,
	); err != nil {
		t.Fatalf("insert session_checkpoints: %v", err)
	}
}

// seedEvent inserts one agent_events row. Pass empty turnID / role
// to exercise rejection paths.
func seedEvent(t *testing.T, db *sql.DB, eventID, sessionID, repoID, role, kind, turnID, summary string, ts int64) {
	t.Helper()
	if _, err := db.Exec(
		`insert into agent_events(event_id, session_id, repository_id, ts, kind, role, summary, turn_id) values(?,?,?,?,?,?,?,?)`,
		eventID, sessionID, repoID, ts, kind, role, summary, turnID,
	); err != nil {
		t.Fatalf("insert agent_events: %v", err)
	}
}

// hashOf is the same excerpt-hash function the loader uses.
func hashOf(s string) string {
	const cap = 400
	if len(s) > cap {
		s = s[:cap] + "...(truncated)"
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// A captured prompt produces one fully populated bundle turn.
func TestSQLiteTurnLoader_HappyPath(t *testing.T) {
	root, db, cleanup := initTurnLoaderRepo(t)
	defer cleanup()
	repoID := seedRepository(t, db, root)

	seedCheckpoint(t, db, "ck-1", repoID, 1000)
	seedCommitLink(t, db, "c1", repoID, "ck-1")
	seedSession(t, db, "sess-1", repoID, "ck-1")
	seedEvent(t, db, "ev-1", "sess-1", repoID, "user", "user", "turn-1", "add input validation", 500)

	loader := newSQLiteTurnLoader(root)
	turns, err := loader.LoadTurnsForCommits(context.Background(), []string{"c1"})
	if err != nil {
		t.Fatalf("LoadTurnsForCommits: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(turns))
	}
	got := turns[0]
	if got.TurnID != "turn-1" {
		t.Errorf("TurnID = %q, want turn-1 (nullableString regression?)", got.TurnID)
	}
	if got.PromptExcerpt != "add input validation" {
		t.Errorf("PromptExcerpt = %q", got.PromptExcerpt)
	}
	if got.PromptExcerptHash != hashOf("add input validation") {
		t.Errorf("PromptExcerptHash = %q, want sha256 of excerpt", got.PromptExcerptHash)
	}
	if got.CommitHash != "c1" {
		t.Errorf("CommitHash = %q", got.CommitHash)
	}
}

// Tool-result events are excluded even when their role is user.
func TestSQLiteTurnLoader_ExcludesNonUserKindEvents(t *testing.T) {
	root, db, cleanup := initTurnLoaderRepo(t)
	defer cleanup()
	repoID := seedRepository(t, db, root)

	seedCheckpoint(t, db, "ck-1", repoID, 1000)
	seedCommitLink(t, db, "c1", repoID, "ck-1")
	seedSession(t, db, "sess-1", repoID, "ck-1")
	// One real prompt, one tool-result that shouldn't surface.
	seedEvent(t, db, "ev-real", "sess-1", repoID, "user", "user", "turn-real", "real prompt", 400)
	seedEvent(t, db, "ev-tool", "sess-1", repoID, "user", "tool", "turn-tool", "tool result", 500)

	loader := newSQLiteTurnLoader(root)
	turns, _ := loader.LoadTurnsForCommits(context.Background(), []string{"c1"})
	if len(turns) != 1 || turns[0].TurnID != "turn-real" {
		t.Errorf("expected only turn-real; got %+v", turns)
	}
}

// Each commit receives prompts from its own checkpoint window.
func TestSQLiteTurnLoader_TimeWindowExcludesPreviousCommitEvents(t *testing.T) {
	root, db, cleanup := initTurnLoaderRepo(t)
	defer cleanup()
	repoID := seedRepository(t, db, root)

	// Two commits with checkpoints at t=1000 and t=2000.
	seedCheckpoint(t, db, "ck-1", repoID, 1000)
	seedCheckpoint(t, db, "ck-2", repoID, 2000)
	seedCommitLink(t, db, "c1", repoID, "ck-1")
	seedCommitLink(t, db, "c2", repoID, "ck-2")

	// One session linked to both checkpoints (multi-commit session).
	seedSession(t, db, "sess-1", repoID, "ck-1")
	if _, err := db.Exec(
		`insert into session_checkpoints(session_id, checkpoint_id) values(?,?)`, "sess-1", "ck-2",
	); err != nil {
		t.Fatal(err)
	}

	// Prompts in distinct windows.
	seedEvent(t, db, "ev-old", "sess-1", repoID, "user", "user", "turn-old", "first PR work", 500)
	seedEvent(t, db, "ev-new", "sess-1", repoID, "user", "user", "turn-new", "second PR work", 1500)

	loader := newSQLiteTurnLoader(root)

	c1Turns, _ := loader.LoadTurnsForCommits(context.Background(), []string{"c1"})
	if len(c1Turns) != 1 || c1Turns[0].TurnID != "turn-old" {
		t.Errorf("c1 should yield only turn-old; got %+v", c1Turns)
	}
	c2Turns, _ := loader.LoadTurnsForCommits(context.Background(), []string{"c2"})
	if len(c2Turns) != 1 || c2Turns[0].TurnID != "turn-new" {
		t.Errorf("c2 should yield only turn-new; got %+v", c2Turns)
	}
}

// A turn that appears in multiple sessions linked to the same
// checkpoint is returned once.
func TestSQLiteTurnLoader_DeduplicatesRepeatedTurnIDs(t *testing.T) {
	root, db, cleanup := initTurnLoaderRepo(t)
	defer cleanup()
	repoID := seedRepository(t, db, root)

	seedCheckpoint(t, db, "ck-1", repoID, 1000)
	seedCommitLink(t, db, "c1", repoID, "ck-1")
	seedSession(t, db, "sess-a", repoID, "ck-1")
	seedSession(t, db, "sess-b", repoID, "ck-1")
	// Same turn_id in both sessions.
	seedEvent(t, db, "ev-a", "sess-a", repoID, "user", "user", "turn-dup", "x", 500)
	seedEvent(t, db, "ev-b", "sess-b", repoID, "user", "user", "turn-dup", "x", 600)

	loader := newSQLiteTurnLoader(root)
	turns, _ := loader.LoadTurnsForCommits(context.Background(), []string{"c1"})
	if len(turns) != 1 {
		t.Errorf("expected 1 deduped turn; got %d (%+v)", len(turns), turns)
	}
}

// Fresh repo with no .semantica/lineage.db returns nil/nil (legitimate
// empty), distinct from corrupt-DB which would return
// ErrLineageUnavailable.
func TestSQLiteTurnLoader_MissingDBIsLegitimateEmpty(t *testing.T) {
	root := t.TempDir()
	loader := newSQLiteTurnLoader(root)
	turns, err := loader.LoadTurnsForCommits(context.Background(), []string{"c1"})
	if err != nil {
		t.Errorf("missing DB should be empty/nil; got err %v", err)
	}
	if turns != nil {
		t.Errorf("turns = %v, want nil", turns)
	}
}

// Redaction failures stop loading instead of producing incomplete analysis.
func TestSQLiteTurnLoader_RedactionFailureSurfacesError(t *testing.T) {
	root, db, cleanup := initTurnLoaderRepo(t)
	defer cleanup()
	repoID := seedRepository(t, db, root)

	seedCheckpoint(t, db, "ck-1", repoID, 1000)
	seedCommitLink(t, db, "c1", repoID, "ck-1")
	seedSession(t, db, "sess-1", repoID, "ck-1")
	seedEvent(t, db, "ev-1", "sess-1", repoID, "user", "user", "turn-1",
		"any prompt content", 500)

	prev := redactString
	redactString = func(string) (string, error) { return "", errors.New("forced") }
	defer func() { redactString = prev }()

	loader := newSQLiteTurnLoader(root)
	turns, err := loader.LoadTurnsForCommits(context.Background(), []string{"c1"})
	if !errors.Is(err, intentgap.ErrRedactionFailed) {
		t.Errorf("err = %v, want ErrRedactionFailed", err)
	}
	if turns != nil {
		t.Errorf("turns = %v, want nil on fail-closed", turns)
	}
}

// Prompt hashes are computed from the redacted text shown to the analyzer.
func TestSQLiteTurnLoader_RedactsExcerptBeforeHashing(t *testing.T) {
	root, db, cleanup := initTurnLoaderRepo(t)
	defer cleanup()
	repoID := seedRepository(t, db, root)

	seedCheckpoint(t, db, "ck-1", repoID, 1000)
	seedCommitLink(t, db, "c1", repoID, "ck-1")
	seedSession(t, db, "sess-1", repoID, "ck-1")
	// Use a value recognized by the generic API-key redactor.
	seedEvent(t, db, "ev-1", "sess-1", repoID, "user", "user", "turn-1",
		"please use api_key = sk-1234567890abcdef1234567890abcdef for the bucket", 500)

	loader := newSQLiteTurnLoader(root)
	turns, err := loader.LoadTurnsForCommits(context.Background(), []string{"c1"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(turns))
	}
	got := turns[0]
	if strings.Contains(got.PromptExcerpt, "sk-1234567890abcdef") {
		t.Errorf("excerpt was NOT redacted: %q", got.PromptExcerpt)
	}
	if !strings.Contains(got.PromptExcerpt, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in excerpt: %q", got.PromptExcerpt)
	}
	if got.PromptExcerptHash != hashOf(got.PromptExcerpt) {
		t.Errorf("hash does not match redacted text bytes")
	}
}

// A corrupt DB surfaces as ErrLineageUnavailable so the analyzer can
// refuse to run LLM analysis. Distinct from "no captures yet".
func TestSQLiteTurnLoader_CorruptDBSurfacesAsLineageUnavailable(t *testing.T) {
	root := t.TempDir()
	semDir := filepath.Join(root, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write garbage to the lineage.db path. sqlite open will then
	// either fail at open or at first query.
	if err := os.WriteFile(filepath.Join(semDir, "lineage.db"), []byte("garbage not a sqlite db"), 0o644); err != nil {
		t.Fatal(err)
	}
	loader := newSQLiteTurnLoader(root)
	_, err := loader.LoadTurnsForCommits(context.Background(), []string{"c1"})
	if !errors.Is(err, intentgap.ErrLineageUnavailable) {
		t.Errorf("expected ErrLineageUnavailable, got %v", err)
	}
}
