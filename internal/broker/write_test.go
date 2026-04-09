package broker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	_ "modernc.org/sqlite"
)

// tempRepoWithDB creates a temporary repo directory with a migrated lineage DB
// and a registered repository row. Returns the repo root path.
func tempRepoWithDB(t *testing.T, repoPath string) string {
	t.Helper()

	// WriteEventsToRepo emits implementation observations into the global
	// implementations DB, so tests that do not explicitly choose a
	// SEMANTICA_HOME must never use the developer's real ~/.semantica state.
	if os.Getenv("SEMANTICA_HOME") == "" {
		t.Setenv("SEMANTICA_HOME", filepath.Join(filepath.Dir(repoPath), ".semantica-global"))
	}

	semDir := filepath.Join(repoPath, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(semDir, "lineage.db")
	ctx := context.Background()

	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: uuid.NewString(),
		RootPath:     repoPath,
		CreatedAt:    1000,
		EnabledAt:    1000,
	}); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	return repoPath
}

func TestWriteEventsToRepo_Basic(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "myrepo")
	tempRepoWithDB(t, repoPath)

	ctx := context.Background()

	events := []RawEvent{
		{
			EventID:           "evt-1",
			SourceKey:         "/data/session.jsonl",
			Provider:          "claude_code",
			Timestamp:         2000,
			Kind:              "assistant",
			Role:              "assistant",
			ToolUsesJSON:      `{"tools":[{"name":"Edit","file_path":"/myrepo/main.go"}]}`,
			Summary:           "edited main.go",
			ProviderSessionID: "sess-abc",
			SessionStartedAt:  1500,
			SessionMetaJSON:   `{"source_key":"/data/session.jsonl"}`,
		},
		{
			EventID:           "evt-2",
			SourceKey:         "/data/session.jsonl",
			Provider:          "claude_code",
			Timestamp:         3000,
			Kind:              "user",
			Role:              "user",
			ProviderSessionID: "sess-abc",
			SessionStartedAt:  1500,
			SessionMetaJSON:   `{"source_key":"/data/session.jsonl"}`,
		},
	}

	sids, err := WriteEventsToRepo(ctx, repoPath, events, nil)
	if err != nil {
		t.Fatalf("WriteEventsToRepo: %v", err)
	}

	if len(sids) != 1 {
		t.Fatalf("expected 1 session ID, got %d", len(sids))
	}

	// Verify events were written by opening DB and querying.
	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	evts, err := h.Queries.ListAgentEventsBySession(ctx, sqldb.ListAgentEventsBySessionParams{
		SessionID: sids[0],
		Limit:     100,
	})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(evts) != 2 {
		t.Errorf("expected 2 events in DB, got %d", len(evts))
	}
}

func TestWriteEventsToRepo_Idempotent(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "myrepo")
	tempRepoWithDB(t, repoPath)

	ctx := context.Background()

	events := []RawEvent{
		{
			EventID:           "evt-1",
			SourceKey:         "/data/session.jsonl",
			Provider:          "claude_code",
			Timestamp:         2000,
			Kind:              "assistant",
			ProviderSessionID: "sess-abc",
			SessionStartedAt:  1500,
			SessionMetaJSON:   `{}`,
		},
	}

	// Write twice.
	_, err := WriteEventsToRepo(ctx, repoPath, events, nil)
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	sids, err := WriteEventsToRepo(ctx, repoPath, events, nil)
	if err != nil {
		t.Fatalf("second write: %v", err)
	}

	// Verify only 1 event exists (INSERT OR IGNORE).
	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	evts, err := h.Queries.ListAgentEventsBySession(ctx, sqldb.ListAgentEventsBySessionParams{
		SessionID: sids[0],
		Limit:     100,
	})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(evts) != 1 {
		t.Errorf("expected 1 event (idempotent), got %d", len(evts))
	}
}

func TestWriteEventsToRepo_DedupsTranscriptPromptWhenHookPromptExists(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "myrepo")
	tempRepoWithDB(t, repoPath)

	ctx := context.Background()

	events := []RawEvent{
		{
			EventID:           "evt-hook-prompt",
			SourceKey:         "/data/session.jsonl",
			Provider:          "claude_code",
			Timestamp:         1000,
			Kind:              "user",
			Role:              "user",
			Summary:           "Create a file",
			TurnID:            "turn-1",
			EventSource:       "hook",
			ProviderSessionID: "sess-abc",
			SessionStartedAt:  900,
			SessionMetaJSON:   `{"source_key":"/data/session.jsonl"}`,
		},
		{
			EventID:           "evt-transcript-prompt",
			SourceKey:         "/data/session.jsonl",
			Provider:          "claude_code",
			Timestamp:         999,
			Kind:              "user",
			Role:              "user",
			Summary:           "Create a file",
			TurnID:            "turn-1",
			EventSource:       "transcript",
			ProviderSessionID: "sess-abc",
			SessionStartedAt:  900,
			SessionMetaJSON:   `{"source_key":"/data/session.jsonl"}`,
		},
		{
			EventID:           "evt-tool-result",
			SourceKey:         "/data/session.jsonl",
			Provider:          "claude_code",
			Timestamp:         1100,
			Kind:              "tool_result",
			Role:              "user",
			Summary:           "File created successfully",
			TurnID:            "turn-1",
			EventSource:       "transcript",
			ProviderSessionID: "sess-abc",
			SessionStartedAt:  900,
			SessionMetaJSON:   `{"source_key":"/data/session.jsonl"}`,
		},
	}

	sids, err := WriteEventsToRepo(ctx, repoPath, events, nil)
	if err != nil {
		t.Fatalf("WriteEventsToRepo: %v", err)
	}
	if len(sids) != 1 {
		t.Fatalf("expected 1 session ID, got %d", len(sids))
	}

	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	evts, err := h.Queries.ListAgentEventsBySession(ctx, sqldb.ListAgentEventsBySessionParams{
		SessionID: sids[0],
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(evts) != 2 {
		t.Fatalf("expected 2 events after prompt dedup, got %d", len(evts))
	}

	var promptCount, toolResultCount int
	for _, evt := range evts {
		if evt.Kind == "user" && evt.Role.Valid && evt.Role.String == "user" {
			promptCount++
			if evt.EventSource != "hook" {
				t.Fatalf("expected surviving prompt to be hook-sourced, got event_source=%v", evt.EventSource)
			}
		}
		if evt.Kind == "tool_result" {
			toolResultCount++
		}
	}
	if promptCount != 1 {
		t.Fatalf("expected exactly 1 prompt event, got %d", promptCount)
	}
	if toolResultCount != 1 {
		t.Fatalf("expected tool_result event to be preserved, got %d", toolResultCount)
	}
}

func TestWriteEventsToRepo_MultipleSessions(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "myrepo")
	tempRepoWithDB(t, repoPath)

	ctx := context.Background()

	events := []RawEvent{
		{
			EventID:           "evt-1",
			SourceKey:         "/data/session1.jsonl",
			Provider:          "claude_code",
			Timestamp:         2000,
			Kind:              "assistant",
			ProviderSessionID: "sess-111",
			SessionStartedAt:  1500,
			SessionMetaJSON:   `{}`,
		},
		{
			EventID:           "evt-2",
			SourceKey:         "/data/session2.jsonl",
			Provider:          "claude_code",
			Timestamp:         3000,
			Kind:              "user",
			ProviderSessionID: "sess-222",
			SessionStartedAt:  2500,
			SessionMetaJSON:   `{}`,
		},
	}

	sids, err := WriteEventsToRepo(ctx, repoPath, events, nil)
	if err != nil {
		t.Fatalf("WriteEventsToRepo: %v", err)
	}

	if len(sids) != 2 {
		t.Errorf("expected 2 session IDs (different sources), got %d", len(sids))
	}
}

func TestWriteEventsToRepo_Empty(t *testing.T) {
	ctx := context.Background()
	sids, err := WriteEventsToRepo(ctx, "/nonexistent", nil, nil)
	if err != nil {
		t.Fatalf("expected no error for empty events, got %v", err)
	}
	if sids != nil {
		t.Errorf("expected nil session IDs for empty events")
	}
}

func TestWriteEventsToRepo_BlobPropagation(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Create a source (broker) blob store and store a payload.
	srcObjectsDir := filepath.Join(dir, "broker-objects")
	srcStore, err := blobs.NewStore(srcObjectsDir)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"type":"assistant","message":{"content":"hello world"}}`)
	hash, _, err := srcStore.Put(ctx, payload)
	if err != nil {
		t.Fatalf("put blob: %v", err)
	}

	// Create target repo.
	repoPath := filepath.Join(dir, "target-repo")
	tempRepoWithDB(t, repoPath)

	events := []RawEvent{
		{
			EventID:           "evt-blob-1",
			SourceKey:         "/data/session.jsonl",
			Provider:          "claude_code",
			Timestamp:         2000,
			Kind:              "assistant",
			Role:              "assistant",
			PayloadHash:       hash,
			ProviderSessionID: "sess-blob",
			SessionStartedAt:  1500,
			SessionMetaJSON:   `{}`,
		},
	}

	sids, err := WriteEventsToRepo(ctx, repoPath, events, srcStore)
	if err != nil {
		t.Fatalf("WriteEventsToRepo: %v", err)
	}
	if len(sids) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sids))
	}

	// Verify the blob was copied to the target repo's object store.
	targetObjectsDir := filepath.Join(repoPath, ".semantica", "objects")
	targetStore, err := blobs.NewStore(targetObjectsDir)
	if err != nil {
		t.Fatalf("open target store: %v", err)
	}
	got, err := targetStore.Get(ctx, hash)
	if err != nil {
		t.Fatalf("blob not found in target store: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("blob content mismatch: got %q", got)
	}

	// Verify event was written with the payload_hash.
	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	evts, err := h.Queries.ListAgentEventsBySession(ctx, sqldb.ListAgentEventsBySessionParams{
		SessionID: sids[0],
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evts))
	}
	if !evts[0].PayloadHash.Valid || evts[0].PayloadHash.String != hash {
		t.Errorf("expected payload_hash=%s, got %v", hash, evts[0].PayloadHash)
	}
}

func TestWriteEventsToRepo_BlobCopyFailure_ClearsHash(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Create a source blob store but do NOT put any blob - simulates a
	// missing blob (e.g., broker store corruption or race).
	srcObjectsDir := filepath.Join(dir, "broker-objects")
	srcStore, err := blobs.NewStore(srcObjectsDir)
	if err != nil {
		t.Fatal(err)
	}

	// Create target repo.
	repoPath := filepath.Join(dir, "target-repo")
	tempRepoWithDB(t, repoPath)

	events := []RawEvent{
		{
			EventID:           "evt-dangling",
			SourceKey:         "/data/session.jsonl",
			Provider:          "claude_code",
			Timestamp:         2000,
			Kind:              "assistant",
			PayloadHash:       "deadbeef0000000000000000000000000000000000000000000000000000cafe",
			ProviderSessionID: "sess-dangle",
			SessionStartedAt:  1500,
			SessionMetaJSON:   `{}`,
		},
	}

	sids, err := WriteEventsToRepo(ctx, repoPath, events, srcStore)
	if err != nil {
		t.Fatalf("WriteEventsToRepo: %v", err)
	}

	// Verify event was written but payload_hash is NULL (not dangling).
	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	evts, err := h.Queries.ListAgentEventsBySession(ctx, sqldb.ListAgentEventsBySessionParams{
		SessionID: sids[0],
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evts))
	}
	if evts[0].PayloadHash.Valid {
		t.Errorf("expected NULL payload_hash (blob copy failed), got %q", evts[0].PayloadHash.String)
	}
}

func TestWriteEventsToRepo_CrossRepoProvenance(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Create target repo at a different path than the session origin.
	repoPath := filepath.Join(dir, "api")
	tempRepoWithDB(t, repoPath)

	originPath := filepath.Join(dir, "cli")

	events := []RawEvent{
		{
			EventID:           "evt-xrepo-1",
			SourceKey:         "/data/session.jsonl",
			Provider:          "claude_code",
			Timestamp:         2000,
			Kind:              "assistant",
			Role:              "assistant",
			ProviderSessionID: "sess-xrepo",
			SessionStartedAt:  1500,
			SessionMetaJSON:   `{}`,
			SourceProjectPath: originPath,
		},
	}

	sids, err := WriteEventsToRepo(ctx, repoPath, events, nil)
	if err != nil {
		t.Fatalf("WriteEventsToRepo: %v", err)
	}
	if len(sids) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sids))
	}

	// Verify session has source_repo_path set (cross-repo).
	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	sess, err := h.Queries.GetAgentSessionByID(ctx, sids[0])
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if !sess.SourceRepoPath.Valid {
		t.Fatal("expected non-NULL source_repo_path for cross-repo session")
	}
	if sess.SourceRepoPath.String != originPath {
		t.Errorf("expected source_repo_path=%s, got %s", originPath, sess.SourceRepoPath.String)
	}

	// Verify ListCrossRepoSessions returns it.
	repo, _ := h.Queries.GetRepositoryByRootPath(ctx, repoPath)
	crossRepo, _ := h.Queries.ListCrossRepoSessions(ctx, sqldb.ListCrossRepoSessionsParams{
		RepositoryID: repo.RepositoryID,
		Limit:        100,
	})
	if len(crossRepo) != 1 {
		t.Errorf("expected 1 cross-repo session, got %d", len(crossRepo))
	}
}

func TestWriteEventsToRepo_SameRepoNoProvenance(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	repoPath := filepath.Join(dir, "cli")
	tempRepoWithDB(t, repoPath)

	// Session originated from the same repo.
	events := []RawEvent{
		{
			EventID:           "evt-local-1",
			SourceKey:         "/data/session.jsonl",
			Provider:          "claude_code",
			Timestamp:         2000,
			Kind:              "assistant",
			ProviderSessionID: "sess-local",
			SessionStartedAt:  1500,
			SessionMetaJSON:   `{}`,
			SourceProjectPath: repoPath, // same as target
		},
	}

	sids, err := WriteEventsToRepo(ctx, repoPath, events, nil)
	if err != nil {
		t.Fatalf("WriteEventsToRepo: %v", err)
	}

	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	sess, err := h.Queries.GetAgentSessionByID(ctx, sids[0])
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	// Same repo - source_repo_path should be NULL.
	if sess.SourceRepoPath.Valid {
		t.Errorf("expected NULL source_repo_path for same-repo session, got %q", sess.SourceRepoPath.String)
	}

	// Verify ListCrossRepoSessions returns nothing.
	repo, _ := h.Queries.GetRepositoryByRootPath(ctx, repoPath)
	crossRepo, _ := h.Queries.ListCrossRepoSessions(ctx, sqldb.ListCrossRepoSessionsParams{
		RepositoryID: repo.RepositoryID,
		Limit:        100,
	})
	if len(crossRepo) != 0 {
		t.Errorf("expected 0 cross-repo sessions, got %d", len(crossRepo))
	}
}

func TestWriteEventsToRepo_MultiSessionSource(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "myrepo")
	tempRepoWithDB(t, repoPath)

	ctx := context.Background()

	// Two events from the same source_key but different provider session IDs.
	// This simulates Cursor's ai-code-tracking.db where multiple conversations
	// share one source file.
	events := []RawEvent{
		{
			EventID:           "evt-conv1",
			SourceKey:         "/data/ai-code-tracking.db",
			Provider:          "cursor",
			Timestamp:         2000,
			Kind:              "assistant",
			ProviderSessionID: "conversation-aaa",
			SessionStartedAt:  1500,
			SessionMetaJSON:   `{}`,
		},
		{
			EventID:           "evt-conv2",
			SourceKey:         "/data/ai-code-tracking.db",
			Provider:          "cursor",
			Timestamp:         3000,
			Kind:              "assistant",
			ProviderSessionID: "conversation-bbb",
			SessionStartedAt:  2500,
			SessionMetaJSON:   `{}`,
		},
	}

	sids, err := WriteEventsToRepo(ctx, repoPath, events, nil)
	if err != nil {
		t.Fatalf("WriteEventsToRepo: %v", err)
	}

	if len(sids) != 2 {
		t.Fatalf("expected 2 session IDs (different provider sessions from same source), got %d", len(sids))
	}

	// Verify each session has exactly 1 event.
	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	for i, sid := range sids {
		evts, err := h.Queries.ListAgentEventsBySession(ctx, sqldb.ListAgentEventsBySessionParams{
			SessionID: sid,
			Limit:     100,
		})
		if err != nil {
			t.Fatalf("list events for session %d: %v", i, err)
		}
		if len(evts) != 1 {
			t.Errorf("session %d: expected 1 event, got %d", i, len(evts))
		}
	}
}

// TestWriteEventsToRepo_HookCapturedBlobIsToolAttributable covers the case
// where a hook-captured Claude payload blob is copied into the repo-local
// store and remains usable for attribution after routing.
func TestWriteEventsToRepo_HookCapturedBlobIsToolAttributable(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Global hook capture blob store.
	globalObjectsDir := filepath.Join(dir, "global-objects")
	globalStore, err := blobs.NewStore(globalObjectsDir)
	if err != nil {
		t.Fatal(err)
	}

	// Claude JSONL line with an Edit tool call, as stored by capture.
	payload := []byte(`{"type":"assistant","message":{"id":"msg_01","model":"claude-sonnet-4-20250514","role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"Edit","input":{"file_path":"/repo/main.go","old_string":"foo","new_string":"bar"}}],"usage":{"input_tokens":100,"output_tokens":50}}}`)
	hash, _, err := globalStore.Put(ctx, payload)
	if err != nil {
		t.Fatalf("put blob: %v", err)
	}

	// Target repo with its own DB and object store.
	repoPath := filepath.Join(dir, "repo")
	tempRepoWithDB(t, repoPath)

	// Event as produced by ReadFromOffset - tool_uses extracted, payload stored.
	events := []RawEvent{
		{
			EventID:           "evt-edit-1",
			SourceKey:         "/workspace/.claude/projects/abc123/session.jsonl",
			Provider:          "claude_code",
			Timestamp:         2000,
			Kind:              "assistant",
			Role:              "assistant",
			ToolUsesJSON:      `{"content_types":["tool_use"],"tools":[{"name":"Edit","file_path":"/repo/main.go","file_op":"edit"}]}`,
			PayloadHash:       hash,
			Summary:           "Edit(/repo/main.go)",
			FilePaths:         []string{"/repo/main.go"},
			ProviderSessionID: "sess-hook",
			SessionStartedAt:  1500,
			SessionMetaJSON:   `{"source_key":"/workspace/.claude/projects/abc123/session.jsonl"}`,
		},
	}

	sids, err := WriteEventsToRepo(ctx, repoPath, events, globalStore)
	if err != nil {
		t.Fatalf("WriteEventsToRepo: %v", err)
	}
	if len(sids) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sids))
	}

	// Open repo DB and verify the event.
	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	evts, err := h.Queries.ListAgentEventsBySession(ctx, sqldb.ListAgentEventsBySessionParams{
		SessionID: sids[0],
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evts))
	}

	ev := evts[0]

	// Contract 1: payload_hash is non-empty.
	if !ev.PayloadHash.Valid || ev.PayloadHash.String == "" {
		t.Fatal("payload_hash is empty - blob was not propagated from global store")
	}
	if ev.PayloadHash.String != hash {
		t.Errorf("payload_hash mismatch: got %s, want %s", ev.PayloadHash.String, hash)
	}

	// Contract 2: blob is readable from the repo's local store.
	repoStore, err := blobs.NewStore(filepath.Join(repoPath, ".semantica", "objects"))
	if err != nil {
		t.Fatalf("open repo store: %v", err)
	}
	got, err := repoStore.Get(ctx, ev.PayloadHash.String)
	if err != nil {
		t.Fatalf("blob not found in repo store: %v", err)
	}
	if string(got) != string(payload) {
		t.Error("blob content mismatch after copy")
	}

	// Contract 3: tool_uses passes the hasEditOrWrite pre-filter
	// (the same check attribution.go uses to decide if an event is tool-attributable).
	if !ev.ToolUses.Valid {
		t.Fatal("tool_uses is NULL")
	}
	toolStr := ev.ToolUses.String
	if !strings.Contains(toolStr, `"Edit"`) {
		t.Errorf("tool_uses missing Edit: %s", toolStr)
	}
}

func TestWriteEventsToRepo_SubdirLaunchIsNotCrossRepo(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	repoPath := filepath.Join(dir, "cli")
	tempRepoWithDB(t, repoPath)

	// Session launched from a subdirectory inside the repo.
	events := []RawEvent{
		{
			EventID:           "evt-subdir-1",
			SourceKey:         "/data/session.jsonl",
			Provider:          "claude_code",
			Timestamp:         2000,
			Kind:              "assistant",
			ProviderSessionID: "sess-subdir",
			SessionStartedAt:  1500,
			SessionMetaJSON:   `{}`,
			SourceProjectPath: filepath.Join(repoPath, "cmd", "server"),
		},
	}

	sids, err := WriteEventsToRepo(ctx, repoPath, events, nil)
	if err != nil {
		t.Fatalf("WriteEventsToRepo: %v", err)
	}

	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	sess, err := h.Queries.GetAgentSessionByID(ctx, sids[0])
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	// Subdirectory launch is same-repo - source_repo_path should be NULL.
	if sess.SourceRepoPath.Valid {
		t.Errorf("expected NULL source_repo_path for subdirectory launch, got %q", sess.SourceRepoPath.String)
	}
}
