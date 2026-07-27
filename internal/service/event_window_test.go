package service

import (
	"context"
	"testing"

	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

func insertWindowEvent(t *testing.T, h *sqlstore.Handle, repoID, sessionID, eventID string, ts int64) {
	t.Helper()
	if err := h.Queries.InsertAgentEvent(context.Background(), sqldb.InsertAgentEventParams{
		EventID: eventID, SessionID: sessionID, RepositoryID: repoID,
		Ts: ts, Kind: "message", EventSource: "hook",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEventWindow_EqualTimestampCheckpointsPartitionByCursor(t *testing.T) {
	_, h, repoID := setupQueueRepo(t)
	ctx := context.Background()

	if _, err := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: "src-1", RepositoryID: repoID, Provider: "claude-code",
		SourceKey: "k", LastSeenAt: 1, CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: "sess-1", ProviderSessionID: "ps-1", RepositoryID: repoID,
		Provider: "claude-code", SourceID: "src-1", StartedAt: 1, LastSeenAt: 1,
		MetadataJson: "{}",
	}); err != nil {
		t.Fatal(err)
	}

	const sharedTs = int64(100) // every row lands in the same millisecond
	insertWindowEvent(t, h, repoID, "sess-1", "e1", sharedTs)
	insertWindowEvent(t, h, repoID, "sess-1", "e2", sharedTs)
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: "cp-a", RepositoryID: repoID, CreatedAt: sharedTs,
		Kind: "auto", Status: "complete",
	}); err != nil {
		t.Fatal(err)
	}
	insertWindowEvent(t, h, repoID, "sess-1", "e3", sharedTs)
	insertWindowEvent(t, h, repoID, "sess-1", "e4", sharedTs)
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: "cp-b", RepositoryID: repoID, CreatedAt: sharedTs,
		Kind: "auto", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}

	cpA, err := h.Queries.GetCheckpointByID(ctx, "cp-a")
	if err != nil {
		t.Fatal(err)
	}
	cpB, err := h.Queries.GetCheckpointByID(ctx, "cp-b")
	if err != nil {
		t.Fatal(err)
	}

	win := windowBetween(&cpA, cpB)
	if !win.useCursor {
		t.Fatalf("both checkpoints carry cursors; window should use them: %+v", win)
	}
	rows, err := h.Queries.ListEventsInWindow(ctx, sqldb.ListEventsInWindowParams{
		RepositoryID: repoID,
		UseCursor:    win.cursorFlag(),
		AfterCursor:  win.cursorAfter(),
		UpToCursor:   win.cursorUpTo(),
		AfterTs:      win.afterTs,
		UpToTs:       win.upToTs,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.EventID] = true
	}
	if len(got) != 2 || !got["e3"] || !got["e4"] {
		t.Fatalf("cursor window = %v, want exactly {e3, e4}", got)
	}

	tsRows, err := h.Queries.ListEventsInWindow(ctx, sqldb.ListEventsInWindowParams{
		RepositoryID: repoID,
		AfterTs:      sharedTs,
		UpToTs:       sharedTs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tsRows) != 0 {
		t.Fatalf("timestamp window unexpectedly returned %d rows", len(tsRows))
	}

	sessions, err := h.Queries.ListSessionsWithEventsInWindow(ctx, sqldb.ListSessionsWithEventsInWindowParams{
		RepositoryID: repoID,
		UseCursor:    win.cursorFlag(),
		AfterCursor:  win.cursorAfter(),
		UpToCursor:   win.cursorUpTo(),
		AfterTs:      win.afterTs,
		UpToTs:       win.upToTs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0] != "sess-1" {
		t.Fatalf("session window = %v, want [sess-1]", sessions)
	}
}

func TestEventWindow_LateReplayedEventLandsInWindow(t *testing.T) {
	_, h, repoID := setupQueueRepo(t)
	ctx := context.Background()

	if _, err := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: "src-1", RepositoryID: repoID, Provider: "claude-code",
		SourceKey: "k", LastSeenAt: 1, CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: "sess-1", ProviderSessionID: "ps-1", RepositoryID: repoID,
		Provider: "claude-code", SourceID: "src-1", StartedAt: 1, LastSeenAt: 1,
		MetadataJson: "{}",
	}); err != nil {
		t.Fatal(err)
	}

	insertWindowEvent(t, h, repoID, "sess-1", "e0", 100)
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: "cp-prev", RepositoryID: repoID, CreatedAt: 100,
		Kind: "auto", Status: "complete",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: "cp-cur", RepositoryID: repoID, CreatedAt: 200,
		Kind: "auto", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	insertWindowEvent(t, h, repoID, "sess-1", "e-late", 150)

	cpPrev, err := h.Queries.GetCheckpointByID(ctx, "cp-prev")
	if err != nil {
		t.Fatal(err)
	}
	cpCur, err := h.Queries.GetCheckpointByID(ctx, "cp-cur")
	if err != nil {
		t.Fatal(err)
	}
	win := windowBetween(&cpPrev, cpCur)
	if !win.useCursor {
		t.Fatalf("cursor mode expected: %+v", win)
	}
	rows, err := h.Queries.ListEventsInWindow(ctx, sqldb.ListEventsInWindowParams{
		RepositoryID: repoID,
		UseCursor:    win.cursorFlag(),
		AfterCursor:  win.cursorAfter(),
		UpToCursor:   win.cursorUpTo(),
		AfterTs:      win.afterTs,
		UpToTs:       win.upToTs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].EventID != "e-late" {
		got := make([]string, len(rows))
		for i, r := range rows {
			got[i] = r.EventID
		}
		t.Fatalf("window = %v, want [e-late]: late-replayed events must not "+
			"be excluded by the checkpoint's cursor snapshot", got)
	}
}

func TestEventWindow_TimestampFallbackForPreCursorHistory(t *testing.T) {
	_, h, repoID := setupQueueRepo(t)
	ctx := context.Background()

	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: "cp-old", RepositoryID: repoID, CreatedAt: 50,
		Kind: "auto", Status: "complete",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.ExecContext(ctx,
		"update checkpoints set event_cursor = null where checkpoint_id = 'cp-old'"); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: "cp-new", RepositoryID: repoID, CreatedAt: 100,
		Kind: "auto", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}

	cpOld, err := h.Queries.GetCheckpointByID(ctx, "cp-old")
	if err != nil {
		t.Fatal(err)
	}
	cpNew, err := h.Queries.GetCheckpointByID(ctx, "cp-new")
	if err != nil {
		t.Fatal(err)
	}
	win := windowBetween(&cpOld, cpNew)
	if win.useCursor {
		t.Fatalf("missing predecessor cursor must fall back to timestamps: %+v", win)
	}
	if win.afterTs != 50 || win.upToTs != 100 {
		t.Fatalf("timestamp bounds = (%d, %d], want (50, 100]", win.afterTs, win.upToTs)
	}
}
