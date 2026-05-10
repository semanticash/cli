package handoff

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

func TestTruncateRunes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"shorter than cap", "hello", 10, "hello"},
		{"exactly cap", "hello", 5, "hello"},
		{"longer than cap", "hello world", 5, "hello..."},
		{"multibyte runes preserved", "héllo wörld", 6, "héllo ..."},
		{"empty string", "", 5, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncateRunes(tc.in, tc.n); got != tc.want {
				t.Errorf("truncateRunes(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
		})
	}
}

func TestSameRepo(t *testing.T) {
	repo := filepath.Clean(t.TempDir())
	sibling := filepath.Clean(t.TempDir())
	sub := filepath.Join(repo, "sub")

	cases := []struct {
		name      string
		root      string
		candidate string
		want      bool
	}{
		{"identical", repo, repo, true},
		{"subdir", repo, sub, true},
		{"sibling", repo, sibling, false},
		{"parent-not-subdir", sub, repo, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameRepo(tc.root, tc.candidate); got != tc.want {
				t.Errorf("sameRepo(%q, %q) = %v, want %v", tc.root, tc.candidate, got, tc.want)
			}
		})
	}
}

func TestAggregateFileTouches_CountsAndSorts(t *testing.T) {
	events := []sqldb.AgentEvent{
		newToolUseEvent(t, `{"tools":[{"name":"Edit","file_path":"a.go"}]}`),
		newToolUseEvent(t, `{"tools":[{"name":"Edit","file_path":"a.go"}]}`),
		newToolUseEvent(t, `{"tools":[{"name":"Write","file_path":"a.go"}]}`),
		newToolUseEvent(t, `{"tools":[{"name":"Edit","file_path":"b.go"}]}`),
	}

	got := aggregateFileTouches(events)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(got), got)
	}
	if got[0].Path != "a.go" || got[0].Total != 3 {
		t.Errorf("expected a.go with total 3 first, got %+v", got[0])
	}
	if !strings.Contains(got[0].Summary, "Edit x2") || !strings.Contains(got[0].Summary, "Write") {
		t.Errorf("summary missing expected components: %q", got[0].Summary)
	}
	if got[1].Path != "b.go" || got[1].Total != 1 {
		t.Errorf("expected b.go with total 1 second, got %+v", got[1])
	}
}

func TestAggregateFileTouches_IgnoresMalformed(t *testing.T) {
	events := []sqldb.AgentEvent{
		newToolUseEvent(t, `not json`),
		newToolUseEvent(t, `{"tools":[{"name":"","file_path":""}]}`),
		newToolUseEvent(t, `{"tools":[{"name":"Edit","file_path":""}]}`),
		newToolUseEvent(t, `{"tools":[{"name":"","file_path":"a.go"}]}`),
	}
	got := aggregateFileTouches(events)
	if len(got) != 0 {
		t.Errorf("expected no aggregated touches, got %+v", got)
	}
}

func TestAggregateFileTouches_CapsAtMax(t *testing.T) {
	var events []sqldb.AgentEvent
	for i := 0; i < maxFilesInTouchSummary+10; i++ {
		path := "f" + itoaLazy(i) + ".go"
		events = append(events, newToolUseEvent(t, `{"tools":[{"name":"Edit","file_path":"`+path+`"}]}`))
	}
	got := aggregateFileTouches(events)
	if len(got) != maxFilesInTouchSummary {
		t.Errorf("expected len %d, got %d", maxFilesInTouchSummary, len(got))
	}
}

func TestExtractLastPrompt_PicksMostRecentUserEvent(t *testing.T) {
	// Events are passed in descending ts order from the sqlc query.
	events := []sqldb.AgentEvent{
		userEvent("most recent prompt"),
		assistantEvent("answer"),
		userEvent("older prompt"),
	}
	got := extractLastPrompt(events)
	if got != "most recent prompt" {
		t.Errorf("got %q, want %q", got, "most recent prompt")
	}
}

func TestExtractLastPrompt_NoUserEvents(t *testing.T) {
	events := []sqldb.AgentEvent{
		assistantEvent("only assistant"),
	}
	if got := extractLastPrompt(events); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestExtractLastAssistant_PicksMostRecent(t *testing.T) {
	events := []sqldb.AgentEvent{
		assistantEvent("most recent answer"),
		userEvent("question"),
		assistantEvent("older answer"),
	}
	got := extractLastAssistant(events)
	if got != "most recent answer" {
		t.Errorf("got %q, want %q", got, "most recent answer")
	}
}

func TestExtractLastPrompt_TruncatesAtCap(t *testing.T) {
	long := strings.Repeat("x", maxPromptChars+50)
	events := []sqldb.AgentEvent{userEvent(long)}
	got := extractLastPrompt(events)
	if len([]rune(got)) != maxPromptChars+3 { // cap + "..."
		t.Errorf("expected truncation to %d runes plus ellipsis, got len %d", maxPromptChars, len([]rune(got)))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected ellipsis suffix, got %q", got[len(got)-5:])
	}
}

func TestRedactString_EmptyStaysEmpty(t *testing.T) {
	if got := redactString(""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestRedactString_HappyPathPassesThrough(t *testing.T) {
	in := "Please deploy the staging service."
	got := redactString(in)
	if got != in {
		t.Errorf("ordinary prose should pass through redactor unchanged: got %q, want %q", got, in)
	}
}

func TestRenderBundle_Headline(t *testing.T) {
	body := renderBundle(bundleView{
		Repo:        "myrepo",
		Branch:      "main",
		Provider:    "claude-code",
		SessionID:   "sess-abc123",
		GeneratedAt: "2026-05-08T08:30:00Z",
		LastPrompt:  "fix the auth bug",
		FileTouches: []fileTouch{
			{Path: "src/auth.go", Summary: "Edit x2", Total: 2},
		},
	})
	out := string(body)

	for _, want := range []string{
		"# Session continuation: myrepo on main",
		"sess-abc123",
		"claude-code",
		"## Files touched this session",
		"src/auth.go (Edit x2)",
		"## Where I left off",
		"fix the auth bug",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered bundle missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestRenderBundle_NoteSurfaced(t *testing.T) {
	body := renderBundle(bundleView{
		Repo:        "myrepo",
		Provider:    "claude-code",
		SessionID:   "sess-x",
		GeneratedAt: "2026-05-08T08:30:00Z",
		Note:        "lineage.db not present",
	})
	if !strings.Contains(string(body), "lineage.db not present") {
		t.Errorf("note not surfaced: %s", body)
	}
}

func TestRenderBundle_EmptySessionStillRenders(t *testing.T) {
	body := renderBundle(bundleView{
		Repo:        "myrepo",
		Provider:    "claude-code",
		SessionID:   "sess-x",
		GeneratedAt: "2026-05-08T08:30:00Z",
	})
	if !strings.Contains(string(body), "No file-touching tool events recorded") {
		t.Errorf("expected empty-session placeholder, got: %s", body)
	}
	if !strings.Contains(string(body), "No prompt or assistant message available") {
		t.Errorf("expected no-prompt placeholder, got: %s", body)
	}
}

// --- fixture helpers ---

func newToolUseEvent(t *testing.T, toolUsesJSON string) sqldb.AgentEvent {
	t.Helper()
	return sqldb.AgentEvent{
		ToolUses: sql.NullString{Valid: true, String: toolUsesJSON},
	}
}

func userEvent(summary string) sqldb.AgentEvent {
	return sqldb.AgentEvent{
		Role:    sql.NullString{Valid: true, String: "user"},
		Summary: sql.NullString{Valid: true, String: summary},
	}
}

func assistantEvent(summary string) sqldb.AgentEvent {
	return sqldb.AgentEvent{
		Role:    sql.NullString{Valid: true, String: "assistant"},
		Summary: sql.NullString{Valid: true, String: summary},
	}
}

// itoaLazy avoids importing strconv just for the cap test.
func itoaLazy(n int) string {
	if n == 0 {
		return "0"
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}

// --- Resolver integration tests ---
//
// The resolver picks a single Claude Code parent capture state for
// the current repo. Wrong-session selection is worse than no
// handoff, so each filter is exercised explicitly.

func TestResolveSession_SingleMatch(t *testing.T) {
	repoA := t.TempDir()
	now := time.Now()

	baseDir := setupCaptureDir(t)
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "sess-1",
		Provider:  "claude-code",
		CWD:       repoA,
		Timestamp: now.UnixMilli(),
	})

	got, err := resolveSession(repoA, now)
	if err != nil {
		t.Fatalf("resolveSession: %v", err)
	}
	if got.SessionID != "sess-1" {
		t.Errorf("got %q, want sess-1", got.SessionID)
	}
}

func TestResolveSession_ZeroMatches_ErrNoSession(t *testing.T) {
	repoA := t.TempDir()
	now := time.Now()

	setupCaptureDir(t) // empty

	_, err := resolveSession(repoA, now)
	if !errors.Is(err, ErrNoSession) {
		t.Errorf("err = %v, want ErrNoSession", err)
	}
}

func TestResolveSession_MultipleMatches_ErrAmbiguous(t *testing.T) {
	repoA := t.TempDir()
	now := time.Now()

	baseDir := setupCaptureDir(t)
	writeCaptureState(t, baseDir, captureFixture{SessionID: "a", Provider: "claude-code", CWD: repoA, Timestamp: now.UnixMilli()})
	writeCaptureState(t, baseDir, captureFixture{SessionID: "b", Provider: "claude-code", CWD: repoA, Timestamp: now.UnixMilli()})

	_, err := resolveSession(repoA, now)
	if !errors.Is(err, ErrAmbiguousSession) {
		t.Errorf("err = %v, want ErrAmbiguousSession", err)
	}
}

// TestResolveSession_NonClaudeProvidersResolve confirms the resolver
// is provider-agnostic. A session captured under cursor (or kiro-cli,
// or any other Semantica-tracked agent) is just as eligible to be
// the handoff source as a Claude Code session.
func TestResolveSession_CursorProviderResolves(t *testing.T) {
	repoA := t.TempDir()
	now := time.Now()

	baseDir := setupCaptureDir(t)
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "cursor-1",
		Provider:  "cursor",
		CWD:       repoA,
		Timestamp: now.UnixMilli(),
	})

	got, err := resolveSession(repoA, now)
	if err != nil {
		t.Fatalf("resolveSession: %v", err)
	}
	if got.SessionID != "cursor-1" {
		t.Errorf("got %q, want cursor-1", got.SessionID)
	}
	if got.Provider != "cursor" {
		t.Errorf("provider = %q, want cursor", got.Provider)
	}
}

func TestResolveSession_KiroCLIProviderResolves(t *testing.T) {
	repoA := t.TempDir()
	now := time.Now()

	baseDir := setupCaptureDir(t)
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "kiro-1",
		Provider:  "kiro-cli",
		CWD:       repoA,
		Timestamp: now.UnixMilli(),
	})

	got, err := resolveSession(repoA, now)
	if err != nil {
		t.Fatalf("resolveSession: %v", err)
	}
	if got.SessionID != "kiro-1" {
		t.Errorf("got %q, want kiro-1", got.SessionID)
	}
}

// TestResolveSession_MixedProviders_StillAmbiguous confirms the
// ambiguity check no longer respects provider boundaries: two
// active sessions in the same repo (one Claude Code, one Cursor)
// produce an ambiguity error, not a silent pick of Claude Code.
// Users who genuinely have two agents open at once need to close
// one before handing off.
func TestResolveSession_MixedProviders_StillAmbiguous(t *testing.T) {
	repoA := t.TempDir()
	now := time.Now()

	baseDir := setupCaptureDir(t)
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "claude-1",
		Provider:  "claude-code",
		CWD:       repoA,
		Timestamp: now.UnixMilli(),
	})
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "cursor-1",
		Provider:  "cursor",
		CWD:       repoA,
		Timestamp: now.UnixMilli(),
	})

	_, err := resolveSession(repoA, now)
	if !errors.Is(err, ErrAmbiguousSession) {
		t.Errorf("err = %v, want ErrAmbiguousSession", err)
	}
}

func TestResolveSession_StaleTimestamp_Filtered(t *testing.T) {
	repoA := t.TempDir()
	now := time.Now()
	stale := now.Add(-recentSessionWindow - time.Hour).UnixMilli()

	baseDir := setupCaptureDir(t)
	writeCaptureState(t, baseDir, captureFixture{SessionID: "stale", Provider: "claude-code", CWD: repoA, Timestamp: stale})

	_, err := resolveSession(repoA, now)
	if !errors.Is(err, ErrNoSession) {
		t.Errorf("err = %v, want ErrNoSession (stale timestamp must be filtered)", err)
	}
}

func TestResolveSession_ZeroTimestamp_Filtered(t *testing.T) {
	repoA := t.TempDir()
	now := time.Now()

	baseDir := setupCaptureDir(t)
	writeCaptureState(t, baseDir, captureFixture{SessionID: "zero-ts", Provider: "claude-code", CWD: repoA, Timestamp: 0})

	_, err := resolveSession(repoA, now)
	if !errors.Is(err, ErrNoSession) {
		t.Errorf("err = %v, want ErrNoSession (zero timestamp must be filtered)", err)
	}
}

func TestResolveSession_OtherRepo_Filtered(t *testing.T) {
	repoA := t.TempDir()
	repoB := t.TempDir()
	now := time.Now()

	baseDir := setupCaptureDir(t)
	writeCaptureState(t, baseDir, captureFixture{SessionID: "in-b", Provider: "claude-code", CWD: repoB, Timestamp: now.UnixMilli()})

	_, err := resolveSession(repoA, now)
	if !errors.Is(err, ErrNoSession) {
		t.Errorf("err = %v, want ErrNoSession (other-repo states must be filtered)", err)
	}
}

func TestResolveSession_EmptyCWD_Filtered(t *testing.T) {
	repoA := t.TempDir()
	now := time.Now()

	baseDir := setupCaptureDir(t)
	writeCaptureState(t, baseDir, captureFixture{SessionID: "no-cwd", Provider: "claude-code", CWD: "", Timestamp: now.UnixMilli()})

	_, err := resolveSession(repoA, now)
	if !errors.Is(err, ErrNoSession) {
		t.Errorf("err = %v, want ErrNoSession (CWD-less states cannot be attributed safely)", err)
	}
}

func TestResolveSession_SubagentState_Filtered(t *testing.T) {
	repoA := t.TempDir()
	now := time.Now()

	baseDir := setupCaptureDir(t)
	// Subagent capture states use StateKey != SessionID. Resolver must
	// pick the parent only.
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "parent",
		StateKey:  "parent",
		Provider:  "claude-code",
		CWD:       repoA,
		Timestamp: now.UnixMilli(),
	})
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "child-uuid",
		StateKey:  "subagent-key-distinct",
		Provider:  "claude-code",
		CWD:       repoA,
		Timestamp: now.UnixMilli(),
	})

	got, err := resolveSession(repoA, now)
	if err != nil {
		t.Fatalf("resolveSession: %v", err)
	}
	if got.SessionID != "parent" {
		t.Errorf("got %q, want parent (subagent state must be filtered)", got.SessionID)
	}
}

func TestResolveSession_SubdirCWD_Counts(t *testing.T) {
	repoA := t.TempDir()
	sub := filepath.Join(repoA, "src", "auth")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	baseDir := setupCaptureDir(t)
	writeCaptureState(t, baseDir, captureFixture{SessionID: "in-subdir", Provider: "claude-code", CWD: sub, Timestamp: now.UnixMilli()})

	got, err := resolveSession(repoA, now)
	if err != nil {
		t.Fatalf("resolveSession: %v", err)
	}
	if got.SessionID != "in-subdir" {
		t.Errorf("got %q, want in-subdir (subdir CWD should count as same repo)", got.SessionID)
	}
}

// captureFixture covers the CaptureState fields the resolver looks
// at. JSON encoding matches the on-disk schema.
type captureFixture struct {
	SessionID string
	StateKey  string
	Provider  string
	CWD       string
	Timestamp int64
}

// setupCaptureDir redirects broker.GlobalBase via SEMANTICA_HOME and
// returns the path the resolver will scan. Same pattern as the
// health-package tests; SEMANTICA_HOME works on every platform
// (HOME does not redirect os.UserHomeDir on Windows).
func setupCaptureDir(t *testing.T) string {
	t.Helper()
	semHome := filepath.Join(t.TempDir(), "semantica-home")
	t.Setenv("SEMANTICA_HOME", semHome)
	baseDir := filepath.Join(semHome, "capture")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return baseDir
}

func writeCaptureState(t *testing.T, baseDir string, f captureFixture) {
	t.Helper()
	// JSON-encode CWD so Windows backslashes escape correctly.
	cwdJSON, err := json.Marshal(f.CWD)
	if err != nil {
		t.Fatal(err)
	}
	stateKey := f.StateKey
	if stateKey == "" {
		stateKey = f.SessionID
	}
	body := `{"session_id":"` + f.SessionID +
		`","state_key":"` + stateKey +
		`","provider":"` + f.Provider +
		`","transcript_ref":"x","transcript_offset":0,"timestamp":` + itoaInt64(f.Timestamp) +
		`,"cwd":` + string(cwdJSON) + `}`
	path := filepath.Join(baseDir, "capture-"+f.SessionID+".json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- End-to-end integration test ---
//
// This test covers the session-ID resolution chain (provider session
// ID to local UUID via repo + agent_sessions). Without it the helper
// tests can stay green while real bundles come out empty.
//
// What the test wires up:
//   - a real git repo on disk so Write can resolve repo root
//   - a real lineage.db at <repo>/.semantica/lineage.db
//   - a repository, agent_source, agent_session, and agent_events
//     row with realistic shapes (provider_session_id matches the
//     capture state's SessionID; events have user/assistant role and
//     a tool_uses JSON blob)
//   - a capture state file pointing at this repo
//
// What it asserts:
//   - the session resolver picks the right capture state
//   - the bundle includes the redacted user prompt
//   - the bundle includes the file-touch summary
//   - the bundle does NOT carry raw error strings or the lineage.db
//     absolute path
func TestWrite_EndToEnd_PopulatesBundleFromLineageDB(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t)

	// Open lineage.db at the canonical location so the service finds it.
	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open lineage.db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	now := time.Now()

	// Repository row keyed by canonical repo path.
	repoID := "repo-id-test"
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID,
		RootPath:     repoPath,
		CreatedAt:    now.UnixMilli(),
		EnabledAt:    now.UnixMilli(),
	}); err != nil {
		t.Fatalf("insert repository: %v", err)
	}

	// Agent source.
	source, err := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID:     "source-id-test",
		RepositoryID: repoID,
		Provider:     "claude_code",
		SourceKey:    "default",
		LastSeenAt:   now.UnixMilli(),
		CreatedAt:    now.UnixMilli(),
	})
	if err != nil {
		t.Fatalf("upsert agent source: %v", err)
	}

	// Agent session: provider_session_id must match the capture state's
	// SessionID. The local session_id is a separate UUID, so the bundle
	// path must resolve through agent_sessions before querying events.
	const providerSessionID = "claude-sess-xyz"
	session, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID:         "local-uuid-distinct-from-provider-id",
		ProviderSessionID: providerSessionID,
		RepositoryID:      repoID,
		Provider:          "claude_code",
		SourceID:          source.SourceID,
		StartedAt:         now.UnixMilli(),
		LastSeenAt:        now.UnixMilli(),
		MetadataJson:      "{}",
	})
	if err != nil {
		t.Fatalf("upsert agent session: %v", err)
	}

	// Insert events keyed by the local session_id. ts is descending
	// because ListAgentEventsBySession orders by ts desc.
	events := []sqldb.InsertAgentEventParams{
		{
			EventID:      "evt-user-recent",
			SessionID:    session.SessionID,
			RepositoryID: repoID,
			Ts:           now.Add(-1 * time.Minute).UnixMilli(),
			Kind:         "user",
			Role:         sql.NullString{Valid: true, String: "user"},
			Summary:      sql.NullString{Valid: true, String: "Please add a unit test for the auth handler."},
			EventSource:  "hook",
		},
		{
			EventID:      "evt-asst-recent",
			SessionID:    session.SessionID,
			RepositoryID: repoID,
			Ts:           now.Add(-30 * time.Second).UnixMilli(),
			Kind:         "assistant",
			Role:         sql.NullString{Valid: true, String: "assistant"},
			Summary:      sql.NullString{Valid: true, String: "I added the test and ran it green."},
			EventSource:  "hook",
		},
		{
			EventID:      "evt-tool-edit",
			SessionID:    session.SessionID,
			RepositoryID: repoID,
			Ts:           now.Add(-45 * time.Second).UnixMilli(),
			Kind:         "tool_use",
			Role:         sql.NullString{Valid: true, String: "assistant"},
			ToolName:     sql.NullString{Valid: true, String: "Edit"},
			ToolUses:     sql.NullString{Valid: true, String: `{"tools":[{"name":"Edit","file_path":"src/auth/handler.go"}]}`},
			EventSource:  "hook",
		},
		{
			EventID:      "evt-tool-edit-2",
			SessionID:    session.SessionID,
			RepositoryID: repoID,
			Ts:           now.Add(-40 * time.Second).UnixMilli(),
			Kind:         "tool_use",
			Role:         sql.NullString{Valid: true, String: "assistant"},
			ToolName:     sql.NullString{Valid: true, String: "Edit"},
			ToolUses:     sql.NullString{Valid: true, String: `{"tools":[{"name":"Edit","file_path":"src/auth/handler.go"}]}`},
			EventSource:  "hook",
		},
		{
			EventID:      "evt-tool-write",
			SessionID:    session.SessionID,
			RepositoryID: repoID,
			Ts:           now.Add(-35 * time.Second).UnixMilli(),
			Kind:         "tool_use",
			Role:         sql.NullString{Valid: true, String: "assistant"},
			ToolName:     sql.NullString{Valid: true, String: "Write"},
			ToolUses:     sql.NullString{Valid: true, String: `{"tools":[{"name":"Write","file_path":"src/auth/handler_test.go"}]}`},
			EventSource:  "hook",
		},
	}
	for _, e := range events {
		if err := h.Queries.InsertAgentEvent(ctx, e); err != nil {
			t.Fatalf("insert event %s: %v", e.EventID, err)
		}
	}

	// Capture state: SessionID is the provider session ID, CWD is the
	// repo. Provider name uses the hook-registry form ("claude-code"),
	// not the storage form ("claude_code").
	baseDir := setupCaptureDir(t)
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: providerSessionID,
		Provider:  "claude-code",
		CWD:       repoPath,
		Timestamp: now.UnixMilli(),
	})

	svc := NewService()
	res, err := svc.Write(ctx, Input{RepoPath: repoPath, Now: now})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res == nil {
		t.Fatal("Write returned nil result")
	}
	if res.SessionID != providerSessionID {
		t.Errorf("res.SessionID = %q, want %q", res.SessionID, providerSessionID)
	}

	body := string(res.Bytes)

	// Prompt and file-touch summary should both be present; the whole
	// point of this test is that the resolution chain populated them.
	if !strings.Contains(body, "Please add a unit test for the auth handler.") {
		t.Errorf("bundle missing user prompt:\n%s", body)
	}
	if !strings.Contains(body, "src/auth/handler.go (Edit x2)") {
		t.Errorf("bundle missing aggregated edit count for handler.go:\n%s", body)
	}
	if !strings.Contains(body, "src/auth/handler_test.go (Write)") {
		t.Errorf("bundle missing write entry for handler_test.go:\n%s", body)
	}
	if !strings.Contains(body, "I added the test and ran it green.") {
		t.Errorf("bundle missing assistant message:\n%s", body)
	}

	// None of the degraded-bundle notes should appear when the chain
	// fully resolves.
	for _, note := range []string{
		noteLineageMissing,
		noteLineageUnavail,
		noteSessionUnknown,
		noteEventsUnavail,
	} {
		if strings.Contains(body, note) {
			t.Errorf("happy-path bundle contained degraded note %q:\n%s", note, body)
		}
	}

	// Degraded notes must not leak raw absolute paths or SQL details.
	if strings.Contains(body, dbPath) {
		t.Errorf("bundle leaked absolute lineage.db path %q", dbPath)
	}
	if strings.Contains(body, "no such") || strings.Contains(body, "sql:") {
		t.Errorf("bundle leaked SQL error fragment:\n%s", body)
	}
}

// TestWrite_EndToEnd_PopulatesBundleFromCursorSession confirms that
// Cursor capture states populate handoff bundles end-to-end.
func TestWrite_EndToEnd_PopulatesBundleFromCursorSession(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t)

	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open lineage.db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	now := time.Now()
	repoID := "repo-cursor"
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID,
		RootPath:     repoPath,
		CreatedAt:    now.UnixMilli(),
		EnabledAt:    now.UnixMilli(),
	}); err != nil {
		t.Fatalf("insert repository: %v", err)
	}
	source, err := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID:     "source-cursor",
		RepositoryID: repoID,
		Provider:     "cursor",
		SourceKey:    "default",
		LastSeenAt:   now.UnixMilli(),
		CreatedAt:    now.UnixMilli(),
	})
	if err != nil {
		t.Fatalf("upsert agent source: %v", err)
	}
	const providerSessionID = "cursor-sess-abc"
	session, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID:         "local-uuid-cursor",
		ProviderSessionID: providerSessionID,
		RepositoryID:      repoID,
		Provider:          "cursor",
		SourceID:          source.SourceID,
		StartedAt:         now.UnixMilli(),
		LastSeenAt:        now.UnixMilli(),
		MetadataJson:      "{}",
	})
	if err != nil {
		t.Fatalf("upsert agent session: %v", err)
	}
	if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID:      "evt-cursor-user",
		SessionID:    session.SessionID,
		RepositoryID: repoID,
		Ts:           now.Add(-1 * time.Minute).UnixMilli(),
		Kind:         "user",
		Role:         sql.NullString{Valid: true, String: "user"},
		Summary:      sql.NullString{Valid: true, String: "Refactor the auth middleware to be testable."},
		EventSource:  "hook",
	}); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	baseDir := setupCaptureDir(t)
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: providerSessionID,
		Provider:  "cursor",
		CWD:       repoPath,
		Timestamp: now.UnixMilli(),
	})

	res, err := NewService().Write(ctx, Input{RepoPath: repoPath, Now: now})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.SessionID != providerSessionID {
		t.Errorf("res.SessionID = %q, want %q", res.SessionID, providerSessionID)
	}
	if res.Provider != "cursor" {
		t.Errorf("res.Provider = %q, want cursor", res.Provider)
	}
	body := string(res.Bytes)
	if !strings.Contains(body, "Refactor the auth middleware to be testable.") {
		t.Errorf("bundle missing user prompt from cursor session:\n%s", body)
	}
	for _, note := range []string{
		noteLineageMissing, noteLineageUnavail, noteSessionUnknown, noteEventsUnavail,
	} {
		if strings.Contains(body, note) {
			t.Errorf("cursor happy-path bundle contained degraded note %q:\n%s", note, body)
		}
	}
}

// TestWrite_DuplicateProviderSessionID_PicksByProvider confirms that
// provider_session_id collisions across providers do not mix events.
// The (repository_id, provider, provider_session_id) unique index
// allows the same provider_session_id to appear under different
// providers, so the matcher must use the capture state's provider
// aliases before reading events.
//
// Setup plants two complete agent_session rows + distinct events
// per provider. The capture state declares the cursor provider;
// the bundle must contain the cursor event text and never the
// claude_code event text.
func TestWrite_DuplicateProviderSessionID_PicksByProvider(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t)

	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open lineage.db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	now := time.Now()
	repoID := "repo-collision"
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID,
		RootPath:     repoPath,
		CreatedAt:    now.UnixMilli(),
		EnabledAt:    now.UnixMilli(),
	}); err != nil {
		t.Fatalf("insert repository: %v", err)
	}

	// The shared provider_session_id is the wedge: both providers
	// register their session under the same wire identifier.
	const sharedProviderSessionID = "shared-sess"

	insert := func(label, dbProvider, eventText string) string {
		t.Helper()
		src, err := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
			SourceID:     "source-" + label,
			RepositoryID: repoID,
			Provider:     dbProvider,
			SourceKey:    "key-" + label,
			LastSeenAt:   now.UnixMilli(),
			CreatedAt:    now.UnixMilli(),
		})
		if err != nil {
			t.Fatalf("upsert source for %s: %v", label, err)
		}
		sess, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
			SessionID:         "local-" + label,
			ProviderSessionID: sharedProviderSessionID,
			RepositoryID:      repoID,
			Provider:          dbProvider,
			SourceID:          src.SourceID,
			StartedAt:         now.UnixMilli(),
			LastSeenAt:        now.UnixMilli(),
			MetadataJson:      "{}",
		})
		if err != nil {
			t.Fatalf("upsert session for %s: %v", label, err)
		}
		if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
			EventID:      "evt-" + label,
			SessionID:    sess.SessionID,
			RepositoryID: repoID,
			Ts:           now.Add(-1 * time.Minute).UnixMilli(),
			Kind:         "user",
			Role:         sql.NullString{Valid: true, String: "user"},
			Summary:      sql.NullString{Valid: true, String: eventText},
			EventSource:  "hook",
		}); err != nil {
			t.Fatalf("insert event for %s: %v", label, err)
		}
		return sess.SessionID
	}

	const claudeText = "CLAUDE PROVIDER PROMPT - should not appear in cursor handoff"
	const cursorText = "CURSOR PROVIDER PROMPT - should appear in cursor handoff"

	insert("claude", "claude_code", claudeText)
	insert("cursor", "cursor", cursorText)

	// Capture state declares the cursor provider with the shared
	// provider_session_id. The resolver must pick the cursor row,
	// not the claude_code row.
	baseDir := setupCaptureDir(t)
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: sharedProviderSessionID,
		Provider:  "cursor",
		CWD:       repoPath,
		Timestamp: now.UnixMilli(),
	})

	res, err := NewService().Write(ctx, Input{RepoPath: repoPath, Now: now})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	body := string(res.Bytes)

	if !strings.Contains(body, cursorText) {
		t.Errorf("bundle missing cursor prompt; the resolver did not match by provider:\n%s", body)
	}
	if strings.Contains(body, claudeText) {
		t.Errorf("bundle leaked claude prompt - the resolver picked the wrong provider:\n%s", body)
	}
}

// TestWrite_NoEvents_FallsBackToSessionUnknownNote confirms the
// session-resolution chain reports unknown when the capture state
// references a provider session ID that lineage.db never registered.
// This keeps an unknown provider session from silently producing an
// empty-looking bundle.
func TestWrite_NoMatchingSession_ReportsSessionUnknown(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t)

	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open lineage.db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	now := time.Now()
	repoID := "repo-id-test"
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID,
		RootPath:     repoPath,
		CreatedAt:    now.UnixMilli(),
		EnabledAt:    now.UnixMilli(),
	}); err != nil {
		t.Fatalf("insert repository: %v", err)
	}
	// Note: no agent_session row. The capture state below references
	// a provider session ID that lineage.db has no record of.

	baseDir := setupCaptureDir(t)
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "claude-unregistered-sess",
		Provider:  "claude-code",
		CWD:       repoPath,
		Timestamp: now.UnixMilli(),
	})

	svc := NewService()
	res, err := svc.Write(ctx, Input{RepoPath: repoPath, Now: now})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	body := string(res.Bytes)
	if !strings.Contains(body, noteSessionUnknown) {
		t.Errorf("expected session-unknown note in bundle:\n%s", body)
	}
}

// initGitRepo creates a temp dir, runs `git init`, and returns the
// canonical (symlink-resolved) repo path so callers can use it
// uniformly for DB inserts and capture-state CWDs.
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init", dir)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		canonical = filepath.Clean(dir)
	}
	return canonical
}

func itoaInt64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}
