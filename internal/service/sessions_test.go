package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

func TestSessionServiceGetSession_CountsOnlyActualToolCalls(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	enableSvc, err := NewEnableService(EnableServiceOptions{RepoPath: dir})
	if err != nil {
		t.Fatalf("new enable service: %v", err)
	}
	if _, err := enableSvc.Enable(ctx, EnableOptions{}); err != nil {
		t.Fatalf("enable: %v", err)
	}

	dbPath := dir + "/.semantica/lineage.db"
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, dir)
	if err != nil {
		t.Fatalf("repo row: %v", err)
	}

	now := time.Now().UnixMilli()
	sourceRow, err := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID:     uuid.NewString(),
		RepositoryID: repoRow.RepositoryID,
		Provider:     "claude_code",
		SourceKey:    "/tmp/test-session.jsonl",
		LastSeenAt:   now,
		CreatedAt:    now,
	})
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}

	sessionRow, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID:         uuid.NewString(),
		ProviderSessionID: "sess-test",
		RepositoryID:      repoRow.RepositoryID,
		Provider:          "claude_code",
		SourceID:          sourceRow.SourceID,
		StartedAt:         now,
		LastSeenAt:        now,
		MetadataJson:      `{}`,
	})
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}

	insertEvent := func(eventID, kind, role, toolUses, toolName, eventSource string, ts int64) {
		t.Helper()
		if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
			EventID:      eventID,
			SessionID:    sessionRow.SessionID,
			RepositoryID: repoRow.RepositoryID,
			Ts:           ts,
			Kind:         kind,
			Role:         sqlstore.NullStr(role),
			ToolUses:     sqlstore.NullStr(toolUses),
			ToolName:     sqlstore.NullStr(toolName),
			EventSource:  eventSource,
		}); err != nil {
			t.Fatalf("insert event %s: %v", eventID, err)
		}
	}

	insertEvent("evt-text", "assistant", "assistant", `{"content_types":["text"]}`, "", "transcript", now+1)
	insertEvent("evt-search", "assistant", "assistant", `{"content_types":["tool_use"],"tools":[{"name":"ToolSearch"}]}`, "", "transcript", now+2)
	insertEvent("evt-agent", "assistant", "assistant", "", "Agent", "hook", now+3)
	insertEvent("evt-result", "tool_result", "user", `{"tools":[{"name":"Read","file_path":"tmp/file.txt"}]}`, "", "transcript", now+4)

	svc := NewSessionService()
	info, err := svc.GetSession(ctx, SessionDetailInput{
		RepoPath:  dir,
		SessionID: sessionRow.SessionID,
	})
	if err != nil {
		t.Fatalf("get session: %v", err)
	}

	if info.StepCount != 4 {
		t.Fatalf("step count = %d, want 4", info.StepCount)
	}
	if info.ToolCallCount != 2 {
		t.Fatalf("tool call count = %d, want 2", info.ToolCallCount)
	}
}

func TestSessionServiceGetSession_DedupesStreamedAssistantTokensByProviderEventID(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	enableSvc, err := NewEnableService(EnableServiceOptions{RepoPath: dir})
	if err != nil {
		t.Fatalf("new enable service: %v", err)
	}
	if _, err := enableSvc.Enable(ctx, EnableOptions{}); err != nil {
		t.Fatalf("enable: %v", err)
	}

	dbPath := dir + "/.semantica/lineage.db"
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, dir)
	if err != nil {
		t.Fatalf("repo row: %v", err)
	}

	now := time.Now().UnixMilli()
	sourceRow, err := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID:     uuid.NewString(),
		RepositoryID: repoRow.RepositoryID,
		Provider:     "claude_code",
		SourceKey:    "/tmp/test-dedupe.jsonl",
		LastSeenAt:   now,
		CreatedAt:    now,
	})
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}

	sessionRow, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID:         uuid.NewString(),
		ProviderSessionID: "sess-dedupe",
		RepositoryID:      repoRow.RepositoryID,
		Provider:          "claude_code",
		SourceID:          sourceRow.SourceID,
		StartedAt:         now,
		LastSeenAt:        now,
		MetadataJson:      `{}`,
	})
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}

	insert := func(eventID, providerEventID string, ts, tokensIn, tokensOut int64) {
		t.Helper()
		if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
			EventID:         eventID,
			SessionID:       sessionRow.SessionID,
			RepositoryID:    repoRow.RepositoryID,
			Ts:              ts,
			Kind:            "assistant",
			Role:            sqlstore.NullStr("assistant"),
			ProviderEventID: sqlstore.NullStr(providerEventID),
			TokensIn:        sqlstore.NullInt64(tokensIn),
			TokensOut:       sqlstore.NullInt64(tokensOut),
			Summary:         sqlstore.NullStr("assistant row"),
			EventSource:     "transcript",
		}); err != nil {
			t.Fatalf("insert event %s: %v", eventID, err)
		}
	}

	// Same Claude message streamed twice: input should count once, output should
	// take the max observed value.
	insert("evt-1a", "msg-1", now+1, 100, 2)
	insert("evt-1b", "msg-1", now+2, 100, 40)
	insert("evt-2a", "msg-2", now+3, 200, 10)
	insert("evt-2b", "msg-2", now+4, 200, 60)

	svc := NewSessionService()
	info, err := svc.GetSession(ctx, SessionDetailInput{
		RepoPath:  dir,
		SessionID: sessionRow.SessionID,
	})
	if err != nil {
		t.Fatalf("get session: %v", err)
	}

	if info.TokensIn != 300 {
		t.Fatalf("tokens_in = %d, want 300", info.TokensIn)
	}
	if info.TokensOut != 100 {
		t.Fatalf("tokens_out = %d, want 100", info.TokensOut)
	}
}

func TestSessionServiceListSessions_DedupesStreamedAssistantTokensByProviderEventID(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	enableSvc, err := NewEnableService(EnableServiceOptions{RepoPath: dir})
	if err != nil {
		t.Fatalf("new enable service: %v", err)
	}
	if _, err := enableSvc.Enable(ctx, EnableOptions{}); err != nil {
		t.Fatalf("enable: %v", err)
	}

	dbPath := dir + "/.semantica/lineage.db"
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, dir)
	if err != nil {
		t.Fatalf("repo row: %v", err)
	}

	now := time.Now().UnixMilli()
	sourceRow, err := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID:     uuid.NewString(),
		RepositoryID: repoRow.RepositoryID,
		Provider:     "claude_code",
		SourceKey:    "/tmp/test-dedupe-list.jsonl",
		LastSeenAt:   now,
		CreatedAt:    now,
	})
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}

	sessionRow, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID:         uuid.NewString(),
		ProviderSessionID: "sess-dedupe-list",
		RepositoryID:      repoRow.RepositoryID,
		Provider:          "claude_code",
		SourceID:          sourceRow.SourceID,
		StartedAt:         now,
		LastSeenAt:        now,
		MetadataJson:      `{}`,
	})
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}

	insert := func(eventID, providerEventID string, ts, tokensIn, tokensOut int64) {
		t.Helper()
		if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
			EventID:         eventID,
			SessionID:       sessionRow.SessionID,
			RepositoryID:    repoRow.RepositoryID,
			Ts:              ts,
			Kind:            "assistant",
			Role:            sqlstore.NullStr("assistant"),
			ProviderEventID: sqlstore.NullStr(providerEventID),
			TokensIn:        sqlstore.NullInt64(tokensIn),
			TokensOut:       sqlstore.NullInt64(tokensOut),
			Summary:         sqlstore.NullStr("assistant row"),
			EventSource:     "transcript",
		}); err != nil {
			t.Fatalf("insert event %s: %v", eventID, err)
		}
	}

	insert("evt-1a", "msg-1", now+1, 2631, 2)
	insert("evt-1b", "msg-1", now+2, 2631, 84)
	insert("evt-2a", "msg-2", now+3, 3384, 69)
	insert("evt-2b", "msg-2", now+4, 3384, 198)
	insert("evt-3a", "msg-3", now+5, 3871, 662)

	svc := NewSessionService()
	tree, err := svc.ListSessions(ctx, SessionListInput{
		RepoPath: dir,
		Limit:    10,
	})
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(tree.Roots) != 1 {
		t.Fatalf("root sessions = %d, want 1", len(tree.Roots))
	}
	if tree.Roots[0].TokensIn != 9886 {
		t.Fatalf("tokens_in = %d, want 9886", tree.Roots[0].TokensIn)
	}
	if tree.Roots[0].TokensOut != 944 {
		t.Fatalf("tokens_out = %d, want 944", tree.Roots[0].TokensOut)
	}
}
