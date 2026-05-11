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

// TestExtractRecentUserPrompts_ChronologicalOrderAndCap covers the
// recent-prompt contract: walk the event slice for up to N user
// prompts and return them oldest-first so the rendered list reads as
// a session arc. Events come in descending ts order from the sqlc
// query.
func TestExtractRecentUserPrompts_ChronologicalOrderAndCap(t *testing.T) {
	events := []sqldb.AgentEvent{
		userEvent("most recent prompt"),
		assistantEvent("answer 5"),
		userEvent("fourth prompt"),
		assistantEvent("answer 4"),
		userEvent("third prompt"),
		assistantEvent("answer 3"),
		userEvent("second prompt"),
		assistantEvent("answer 2"),
		userEvent("first prompt"),
		assistantEvent("answer 1"),
		userEvent("ancient prompt that should be cut"),
	}
	got := extractRecentUserPrompts(events, 5)
	want := []string{
		"first prompt",
		"second prompt",
		"third prompt",
		"fourth prompt",
		"most recent prompt",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d prompts, want %d:\n%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestExtractRecentUserPrompts_FewerThanCap(t *testing.T) {
	events := []sqldb.AgentEvent{
		userEvent("two"),
		userEvent("one"),
	}
	got := extractRecentUserPrompts(events, 5)
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Errorf("got %v, want [one two]", got)
	}
}

func TestExtractRecentUserPrompts_SkipsEmptyAndNonUserEvents(t *testing.T) {
	events := []sqldb.AgentEvent{
		assistantEvent("not user"),
		{}, // role unset
		userEvent(""),
		userEvent("real prompt"),
	}
	got := extractRecentUserPrompts(events, 5)
	if len(got) != 1 || got[0] != "real prompt" {
		t.Errorf("got %v, want [real prompt]", got)
	}
}

// TestExtractRecentUserPrompts_FiltersToolResults guards the
// signal-quality fix: Claude Code emits tool_result events with
// role="user" because those are user-side responses to assistant
// tool calls. They look like prompts to a naive role-only filter
// but are actually bash/edit/read output. Including them in the
// bundle pollutes the "recent prompts" section with shell stdout
// the next agent doesn't care about.
func TestExtractRecentUserPrompts_FiltersToolResults(t *testing.T) {
	events := []sqldb.AgentEvent{
		userEvent("real prompt 2"),
		{
			Role:    sql.NullString{Valid: true, String: "user"},
			Kind:    "tool_result",
			Summary: sql.NullString{Valid: true, String: "File updated successfully."},
		},
		{
			Role:    sql.NullString{Valid: true, String: "user"},
			Kind:    "tool_result",
			Summary: sql.NullString{Valid: true, String: "$ ls -la\\nfoo bar"},
		},
		userEvent("real prompt 1"),
	}
	got := extractRecentUserPrompts(events, 5)
	if len(got) != 2 || got[0] != "real prompt 1" || got[1] != "real prompt 2" {
		t.Errorf("got %v, want [real prompt 1, real prompt 2]", got)
	}
}

func TestExtractRecentUserPrompts_TruncatesEachEntry(t *testing.T) {
	long := strings.Repeat("x", maxPromptChars+50)
	events := []sqldb.AgentEvent{userEvent(long)}
	got := extractRecentUserPrompts(events, 5)
	if len(got) != 1 {
		t.Fatalf("got %d prompts, want 1", len(got))
	}
	if !strings.HasSuffix(got[0], "...") {
		t.Errorf("expected ellipsis suffix on truncated prompt: %q", got[0])
	}
}

// TestSessionStartTime_PicksEarliestNonZeroTimestamp confirms the
// session-start anchor is the oldest event in the bundle, used as
// `--since` for the recent-commits git query.
func TestSessionStartTime_PicksEarliestNonZeroTimestamp(t *testing.T) {
	events := []sqldb.AgentEvent{
		{Ts: 30_000},
		{Ts: 10_000},
		{Ts: 20_000},
	}
	got := sessionStartTime(events)
	if got.UnixMilli() != 10_000 {
		t.Errorf("got %d, want 10_000", got.UnixMilli())
	}
}

func TestSessionStartTime_EmptySliceReturnsZero(t *testing.T) {
	got := sessionStartTime(nil)
	if !got.IsZero() {
		t.Errorf("expected zero time, got %v", got)
	}
}

// TestSessionStartTime_OldestEventHasZeroTimestamp is the
// regression for the seed-from-zero bug. The query returns events
// in descending ts order; the oldest entry is at the slice's tail.
// If that tail entry has Ts == 0 (unset / stripped during capture),
// older code seeded earliest=0 and the `e.Ts < earliest` test
// could never replace it (Ts is non-negative), so the function
// would falsely return time.Time{} even when other events in the
// slice had perfectly good positive timestamps.
func TestSessionStartTime_OldestEventHasZeroTimestamp(t *testing.T) {
	events := []sqldb.AgentEvent{
		{Ts: 30_000},
		{Ts: 10_000},
		{Ts: 20_000},
		{Ts: 0}, // stripped or unset
	}
	got := sessionStartTime(events)
	if got.UnixMilli() != 10_000 {
		t.Errorf("got %d, want 10_000 (the smallest positive ts)", got.UnixMilli())
	}
}

// TestSessionStartTime_AllZeroTimestampsReturnsZero is the natural
// counterpart: when no event has a positive ts, there's no anchor
// for `git log --since`, so the function returns zero and the
// caller skips the "recent commits in this session" section
// entirely rather than emitting a meaningless `--since=0` query.
func TestSessionStartTime_AllZeroTimestampsReturnsZero(t *testing.T) {
	events := []sqldb.AgentEvent{
		{Ts: 0},
		{Ts: 0},
	}
	got := sessionStartTime(events)
	if !got.IsZero() {
		t.Errorf("expected zero time when no positive ts present, got %v", got)
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
		Repo:          "myrepo",
		Branch:        "main",
		Provider:      "claude-code",
		SessionID:     "sess-abc123",
		GeneratedAt:   "2026-05-08T08:30:00Z",
		RecentPrompts: []string{"first ask", "fix the auth bug"},
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
		"Recent user prompts",
		"- first ask",
		"- fix the auth bug",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered bundle missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestRenderBundle_RecentCommitsAndUncommittedSections covers the
// recent-commit and uncommitted-work sections.
func TestRenderBundle_RecentCommitsAndUncommittedSections(t *testing.T) {
	body := renderBundle(bundleView{
		Repo:        "myrepo",
		Branch:      "feat/x",
		Provider:    "claude-code",
		SessionID:   "sess-x",
		GeneratedAt: "2026-05-10T08:30:00Z",
		RecentCommits: []string{
			"abcd123 feat: add handler",
			"abce456 chore: lint fixes",
		},
		UncommittedList: " M src/auth.go\n?? src/auth_test.go",
		UncommittedDiff: "@@ -1 +1,2 @@\n-old\n+new\n+more",
	})
	out := string(body)

	for _, want := range []string{
		"## Recent commits during this session",
		"- abcd123 feat: add handler",
		"- abce456 chore: lint fixes",
		"## Working tree changes (uncommitted)",
		"Files:",
		" M src/auth.go",
		"?? src/auth_test.go",
		"Diff (redacted, bounded):",
		"```diff",
		"+new",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered bundle missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestRenderBundle_UncommittedListWithoutDiffStillRenders covers
// the redaction-failed-on-diff branch in readUncommittedWork: the
// file list should still appear so the next session knows what
// changed, even if the diff itself was dropped.
func TestRenderBundle_UncommittedListWithoutDiffStillRenders(t *testing.T) {
	body := renderBundle(bundleView{
		Repo:            "myrepo",
		Provider:        "claude-code",
		SessionID:       "sess-x",
		GeneratedAt:     "2026-05-10T08:30:00Z",
		UncommittedList: " M src/auth.go",
		// UncommittedDiff intentionally empty (redaction failed).
	})
	out := string(body)
	if !strings.Contains(out, " M src/auth.go") {
		t.Errorf("file list missing when diff redaction dropped diff:\n%s", out)
	}
	if strings.Contains(out, "Diff (redacted, bounded):") {
		t.Errorf("diff section should be omitted when diff is empty:\n%s", out)
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
//   - the bundle does not carry raw error strings or the lineage.db
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
// The fixture plants complete rows and distinct events for each
// provider. The capture state declares cursor, so the bundle must
// contain the cursor event text and never the claude_code text.
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

// --- Lineage-fallback tests ---
//
// These cover the between-turn path: when no active capture
// state matches (because the Stop hook deleted it after the
// agent's last response), the resolver falls back to the most-
// recent parent session with events in agent_sessions. Without
// this, `semantica handoff --write` invoked from a terminal
// between turns would always error with "no agent session active"
// even though all the durable session data is right there in
// lineage.db.

// TestWrite_LineageFallback_NoCaptureState covers the normal
// between-turn case: no capture state file exists for the repo,
// but lineage.db has a fresh parent session with events. The bundle
// must assemble successfully and use the provider name shape that
// `handoff continue` recognizes.
func TestWrite_LineageFallback_NoCaptureState(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t)

	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open lineage.db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	now := time.Now()
	repoID := "repo-fallback"
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID,
		RootPath:     repoPath,
		CreatedAt:    now.UnixMilli(),
		EnabledAt:    now.UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	source, err := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID:     "source-fallback",
		RepositoryID: repoID,
		Provider:     "claude_code",
		SourceKey:    "default",
		LastSeenAt:   now.UnixMilli(),
		CreatedAt:    now.UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID:         "local-uuid-fallback",
		ProviderSessionID: "claude-provider-session",
		RepositoryID:      repoID,
		Provider:          "claude_code",
		SourceID:          source.SourceID,
		StartedAt:         now.Add(-10 * time.Minute).UnixMilli(),
		LastSeenAt:        now.Add(-5 * time.Minute).UnixMilli(),
		MetadataJson:      "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID:      "evt-user-1",
		SessionID:    session.SessionID,
		RepositoryID: repoID,
		Ts:           now.Add(-2 * time.Minute).UnixMilli(),
		Kind:         "user",
		Role:         sql.NullString{Valid: true, String: "user"},
		Summary:      sql.NullString{Valid: true, String: "Refactor the auth handler into a separate package."},
		EventSource:  "hook",
	}); err != nil {
		t.Fatal(err)
	}

	// Capture-state directory is set up but empty: zero active
	// capture states. This is the between-turn condition.
	setupCaptureDir(t)

	res, err := NewService().Write(ctx, Input{RepoPath: repoPath, Now: now})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.Provider != "claude-code" {
		t.Errorf("Result.Provider = %q, want hook-form %q", res.Provider, "claude-code")
	}
	if res.SessionID != "claude-provider-session" {
		t.Errorf("Result.SessionID = %q, want provider-session-id %q",
			res.SessionID, "claude-provider-session")
	}
	body := string(res.Bytes)
	if !strings.Contains(body, "Refactor the auth handler into a separate package.") {
		t.Errorf("fallback bundle missing user prompt:\n%s", body)
	}
	// Bundle header line must use the hook-form provider name so
	// `handoff continue` recognizes the agent.
	if !strings.Contains(body, "(claude-code)") {
		t.Errorf("bundle header should use hook-form provider name; got:\n%s", body)
	}
	if strings.Contains(body, "(claude_code)") {
		t.Errorf("bundle should not use DB-form provider name; got:\n%s", body)
	}
	for _, note := range []string{
		noteLineageMissing, noteLineageUnavail, noteSessionUnknown, noteEventsUnavail,
	} {
		if strings.Contains(body, note) {
			t.Errorf("lineage-fallback bundle contained degraded note %q:\n%s", note, body)
		}
	}
}

// TestWrite_LineageFallback_ProviderCanonicalization confirms the
// hook-form translation for every provider whose DB and hook
// names diverge. Without this, `handoff continue` would receive
// `claude_code` / `gemini_cli` and fail provider matching.
func TestWrite_LineageFallback_ProviderCanonicalization(t *testing.T) {
	cases := []struct {
		dbName       string
		wantHookName string
	}{
		{"claude_code", "claude-code"},
		{"gemini_cli", "gemini-cli"},
		{"cursor", "cursor"},
		{"copilot", "copilot"},
		{"kiro-cli", "kiro-cli"},
		{"kiro-ide", "kiro-ide"},
	}
	for _, tc := range cases {
		t.Run(tc.dbName, func(t *testing.T) {
			if got := hookProviderName(tc.dbName); got != tc.wantHookName {
				t.Errorf("hookProviderName(%q) = %q, want %q", tc.dbName, got, tc.wantHookName)
			}
		})
	}
}

// TestWrite_LineageFallback_SkipsSubagentSessions confirms the
// fallback query's parent-only filter: a child / subagent session
// (parent_session_id set) must not be picked as the handoff
// source even if it has more recent events than any parent. A
// subagent transcript out of context would be misleading content
// for a fresh top-level session.
func TestWrite_LineageFallback_SkipsSubagentSessions(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t)

	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open lineage.db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	now := time.Now()
	repoID := "repo-subagent"
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID,
		RootPath:     repoPath,
		CreatedAt:    now.UnixMilli(),
		EnabledAt:    now.UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	src, _ := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: "src", RepositoryID: repoID, Provider: "claude_code",
		SourceKey: "default", LastSeenAt: now.UnixMilli(), CreatedAt: now.UnixMilli(),
	})

	// Older parent session.
	parent, _ := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: "parent-uuid", ProviderSessionID: "parent-provider-sess",
		RepositoryID: repoID, Provider: "claude_code", SourceID: src.SourceID,
		StartedAt:    now.Add(-20 * time.Minute).UnixMilli(),
		LastSeenAt:   now.Add(-15 * time.Minute).UnixMilli(),
		MetadataJson: "{}",
	})
	// Newer subagent session (parent_session_id set).
	_, _ = h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: "subagent-uuid", ProviderSessionID: "subagent-provider-sess",
		ParentSessionID: sql.NullString{Valid: true, String: parent.SessionID},
		RepositoryID:    repoID, Provider: "claude_code", SourceID: src.SourceID,
		StartedAt:    now.Add(-5 * time.Minute).UnixMilli(),
		LastSeenAt:   now.Add(-1 * time.Minute).UnixMilli(),
		MetadataJson: "{}",
	})

	// Events on BOTH: parent has the prompt we expect in the bundle;
	// subagent's "search query" should be filtered out.
	_ = h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID: "evt-parent", SessionID: parent.SessionID, RepositoryID: repoID,
		Ts:   now.Add(-10 * time.Minute).UnixMilli(),
		Kind: "user", Role: sql.NullString{Valid: true, String: "user"},
		Summary:     sql.NullString{Valid: true, String: "PARENT prompt that should appear"},
		EventSource: "hook",
	})
	_ = h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID: "evt-subagent", SessionID: "subagent-uuid", RepositoryID: repoID,
		Ts:   now.Add(-2 * time.Minute).UnixMilli(),
		Kind: "user", Role: sql.NullString{Valid: true, String: "user"},
		Summary:     sql.NullString{Valid: true, String: "SUBAGENT prompt that must not appear"},
		EventSource: "hook",
	})

	setupCaptureDir(t)

	res, err := NewService().Write(ctx, Input{RepoPath: repoPath, Now: now})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.SessionID != "parent-provider-sess" {
		t.Errorf("fallback picked %q, want parent-provider-sess (subagent should be skipped)", res.SessionID)
	}
	body := string(res.Bytes)
	if !strings.Contains(body, "PARENT prompt that should appear") {
		t.Errorf("bundle missing parent prompt:\n%s", body)
	}
	if strings.Contains(body, "SUBAGENT prompt that must not appear") {
		t.Errorf("bundle leaked subagent prompt:\n%s", body)
	}
}

// TestWrite_LineageFallback_SkipsEventlessSessions confirms the
// fallback query's has-events filter: a session that exists in
// agent_sessions but has no rows in agent_events shouldn't be
// picked, even if it's the only candidate. An eventless session
// produces an empty bundle which is worse than no bundle.
func TestWrite_LineageFallback_SkipsEventlessSessions(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t)

	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open lineage.db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	now := time.Now()
	repoID := "repo-eventless"
	_ = h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: repoPath,
		CreatedAt: now.UnixMilli(), EnabledAt: now.UnixMilli(),
	})
	src, _ := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: "src", RepositoryID: repoID, Provider: "claude_code",
		SourceKey: "default", LastSeenAt: now.UnixMilli(), CreatedAt: now.UnixMilli(),
	})
	_, _ = h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: "empty-uuid", ProviderSessionID: "empty-sess",
		RepositoryID: repoID, Provider: "claude_code", SourceID: src.SourceID,
		StartedAt:    now.Add(-1 * time.Minute).UnixMilli(),
		LastSeenAt:   now.Add(-1 * time.Minute).UnixMilli(),
		MetadataJson: "{}",
	})
	// No events at all for this session.

	setupCaptureDir(t)

	_, err = NewService().Write(ctx, Input{RepoPath: repoPath, Now: now})
	if !errors.Is(err, ErrNoSession) {
		t.Errorf("expected ErrNoSession when only candidate session has no events; got %v", err)
	}
}

// TestWrite_LineageFallback_NotUsedWhenCaptureStateExists verifies
// that an active capture state always takes precedence. When capture
// state exists for an in-flight session that hasn't been
// registered in agent_sessions yet (race between the
// prompt-submit hook writing the capture state and the worker
// upserting the lineage row), the resolver must not fall back to
// the previous session via resolveFromLineage. Doing so would
// silently swap a different session's content under the
// in-flight session's provider/identity.
//
// Setup: active capture state names "new-session-not-yet-in-db",
// an older lineage parent session with events sits in
// agent_sessions, and lineage has no row for the new session
// yet. Assertion: the bundle is rendered (degraded), names the
// new session in the header, and does not carry the older
// session's prompts.
func TestWrite_LineageFallback_NotUsedWhenCaptureStateExists(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t)

	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open lineage.db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	now := time.Now()
	repoID := "repo-race"
	_ = h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: repoPath,
		CreatedAt: now.UnixMilli(), EnabledAt: now.UnixMilli(),
	})
	src, _ := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: "src", RepositoryID: repoID, Provider: "claude_code",
		SourceKey: "default", LastSeenAt: now.UnixMilli(), CreatedAt: now.UnixMilli(),
	})
	// Older session: this is the trap. If the resolver falls
	// through to the lineage fallback, it picks up this session's
	// events and attributes them to the in-flight capture state.
	older, _ := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: "older-uuid", ProviderSessionID: "older-provider-sess",
		RepositoryID: repoID, Provider: "claude_code", SourceID: src.SourceID,
		StartedAt:    now.Add(-30 * time.Minute).UnixMilli(),
		LastSeenAt:   now.Add(-20 * time.Minute).UnixMilli(),
		MetadataJson: "{}",
	})
	_ = h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID: "evt-older", SessionID: older.SessionID, RepositoryID: repoID,
		Ts:   now.Add(-25 * time.Minute).UnixMilli(),
		Kind: "user", Role: sql.NullString{Valid: true, String: "user"},
		Summary:     sql.NullString{Valid: true, String: "OLDER-SESSION PROMPT - must not appear in the bundle"},
		EventSource: "hook",
	})

	// Active capture state for a brand-new session that lineage
	// has not registered yet. This is the "in-flight before worker
	// catches up" race condition we're guarding against.
	baseDir := setupCaptureDir(t)
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "new-session-not-yet-in-db",
		Provider:  "claude-code",
		CWD:       repoPath,
		Timestamp: now.UnixMilli(),
	})

	res, err := NewService().Write(ctx, Input{RepoPath: repoPath, Now: now})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	body := string(res.Bytes)

	// Bundle must name the in-flight session, not the older one.
	if res.SessionID != "new-session-not-yet-in-db" {
		t.Errorf("Result.SessionID = %q, want the in-flight capture state's id", res.SessionID)
	}
	if !strings.Contains(body, "new-session-not-yet-in-db") {
		t.Errorf("bundle header should name the in-flight session:\n%s", body)
	}
	// The older session's prompt must not appear anywhere in the bundle.
	if strings.Contains(body, "OLDER-SESSION PROMPT") {
		t.Errorf("bundle leaked content from an unrelated older session:\n%s", body)
	}
	// We expect the degraded header-only path here, with the
	// session-unknown note explaining the missing lineage row.
	if !strings.Contains(body, noteSessionUnknown) {
		t.Errorf("expected the session-unknown degraded note when in-flight session has no lineage row:\n%s", body)
	}
}

// TestWrite_LineageFallback_StaleSessionIsSkipped guards the
// recency filter: a parent session whose last_seen_at is older
// than 24h must not be picked, matching the capture-state
// resolver's freshness threshold.
func TestWrite_LineageFallback_StaleSessionIsSkipped(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t)

	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open lineage.db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	now := time.Now()
	repoID := "repo-stale"
	_ = h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: repoPath,
		CreatedAt: now.UnixMilli(), EnabledAt: now.UnixMilli(),
	})
	src, _ := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: "src", RepositoryID: repoID, Provider: "claude_code",
		SourceKey: "default", LastSeenAt: now.UnixMilli(), CreatedAt: now.UnixMilli(),
	})
	stale, _ := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: "stale-uuid", ProviderSessionID: "stale-sess",
		RepositoryID: repoID, Provider: "claude_code", SourceID: src.SourceID,
		StartedAt:    now.Add(-72 * time.Hour).UnixMilli(),
		LastSeenAt:   now.Add(-48 * time.Hour).UnixMilli(),
		MetadataJson: "{}",
	})
	_ = h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID: "evt-stale", SessionID: stale.SessionID, RepositoryID: repoID,
		Ts:   now.Add(-48 * time.Hour).UnixMilli(),
		Kind: "user", Role: sql.NullString{Valid: true, String: "user"},
		Summary:     sql.NullString{Valid: true, String: "stale prompt"},
		EventSource: "hook",
	})

	setupCaptureDir(t)

	_, err = NewService().Write(ctx, Input{RepoPath: repoPath, Now: now})
	if !errors.Is(err, ErrNoSession) {
		t.Errorf("expected ErrNoSession for stale-only session; got %v", err)
	}
}

// --from override tests cover explicit source selection across
// providers. The override must:
//   - Pick the named provider's most-recent parent session with
//     events, regardless of which agent currently holds the
//     active capture state.
//   - Translate hook-form names (claude-code, gemini-cli) to the
//     underscore DB form for the agent_sessions.provider filter.
//   - Refuse when no recent session matches, with a typed error
//     the command layer can shape into a helpful message.
//   - Honor the same recency window and parent-only filter the
//     lineage fallback uses, so it doesn't surface stale or
//     subagent rows.

// fromOverrideFixture sets up a repo with a Claude parent session
// that has events and a Gemini active capture state. Returns the
// repo path, the lineage handle (caller closes), and the event text
// used to verify source selection.
type fromOverrideFixture struct {
	repoPath   string
	h          *sqlstore.Handle
	claudeText string
	geminiText string
	now        time.Time
}

func setupFromOverrideFixture(t *testing.T) fromOverrideFixture {
	t.Helper()
	ctx := context.Background()
	repoPath := initGitRepo(t)

	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open lineage.db: %v", err)
	}
	t.Cleanup(func() { _ = sqlstore.Close(h) })

	now := time.Now()
	repoID := "repo-from-override"
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: repoPath,
		CreatedAt: now.UnixMilli(), EnabledAt: now.UnixMilli(),
	}); err != nil {
		t.Fatalf("insert repository: %v", err)
	}

	const claudeText = "CLAUDE WORK - the user did this with Claude and wants it in the bundle"
	const geminiText = "GEMINI WORK - separate gemini session, not what --from claude-code asked for"

	mkSession := func(label, dbProvider, eventText string, lastSeen time.Time) {
		src, _ := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
			SourceID: "src-" + label, RepositoryID: repoID, Provider: dbProvider,
			SourceKey: "key-" + label, LastSeenAt: lastSeen.UnixMilli(), CreatedAt: lastSeen.UnixMilli(),
		})
		sess, _ := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
			SessionID: "sess-" + label, ProviderSessionID: "prov-" + label,
			RepositoryID: repoID, Provider: dbProvider, SourceID: src.SourceID,
			StartedAt:    lastSeen.Add(-5 * time.Minute).UnixMilli(),
			LastSeenAt:   lastSeen.UnixMilli(),
			MetadataJson: "{}",
		})
		_ = h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
			EventID: "evt-" + label, SessionID: sess.SessionID, RepositoryID: repoID,
			Ts:   lastSeen.Add(-1 * time.Minute).UnixMilli(),
			Kind: "user", Role: sql.NullString{Valid: true, String: "user"},
			Summary:     sql.NullString{Valid: true, String: eventText},
			EventSource: "hook",
		})
	}

	// Claude is older than the active Gemini session. The default
	// lineage resolver would not pick this row, but --from
	// claude-code should.
	mkSession("claude", "claude_code", claudeText, now.Add(-15*time.Minute))
	// Gemini session: the currently-active agent. We register a
	// lineage row so the active-capture-state resolver would find
	// it absent the override; this lets us assert the override
	// actually moves past the captureState branch.
	mkSession("gemini", "gemini_cli", geminiText, now.Add(-2*time.Minute))

	// Active Gemini capture state: this is what the user's
	// current session looks like from the resolver's perspective.
	baseDir := setupCaptureDir(t)
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "prov-gemini",
		Provider:  "gemini-cli",
		CWD:       repoPath,
		Timestamp: now.UnixMilli(),
	})

	return fromOverrideFixture{
		repoPath:   repoPath,
		h:          h,
		claudeText: claudeText,
		geminiText: geminiText,
		now:        now,
	}
}

// TestWrite_FromOverride_PicksClaudeOverActiveGemini verifies that
// explicit provider selection wins over the active capture state.
// The bundle should use Claude's header and events, not Gemini's.
func TestWrite_FromOverride_PicksClaudeOverActiveGemini(t *testing.T) {
	fx := setupFromOverrideFixture(t)

	res, err := NewService().Write(context.Background(), Input{
		RepoPath: fx.repoPath,
		Now:      fx.now,
		From:     "claude-code",
	})
	if err != nil {
		t.Fatalf("Write with --from claude-code: %v", err)
	}
	body := string(res.Bytes)

	if !strings.Contains(body, fx.claudeText) {
		t.Errorf("bundle missing Claude work; --from did not pick the Claude session:\n%s", body)
	}
	if strings.Contains(body, fx.geminiText) {
		t.Errorf("bundle leaked Gemini work; --from must bypass the active session:\n%s", body)
	}
	if res.Provider != "claude-code" {
		t.Errorf("Result.Provider = %q, want claude-code (so `handoff continue` defaults to Claude)", res.Provider)
	}
	if res.SessionID != "prov-claude" {
		t.Errorf("Result.SessionID = %q, want prov-claude", res.SessionID)
	}
}

// TestWrite_FromOverride_TranslatesHookFormToDBForm pins that the
// flag accepts hook-form provider names (the same shape `handoff
// continue` uses and what users see in skill docs). Internally
// the resolver must translate claude-code -> claude_code and
// gemini-cli -> gemini_cli for the agent_sessions.provider filter.
// Without the translation the SQL filter would never match.
func TestWrite_FromOverride_TranslatesHookFormToDBForm(t *testing.T) {
	fx := setupFromOverrideFixture(t)

	for _, hookForm := range []string{"claude-code", "gemini-cli"} {
		t.Run(hookForm, func(t *testing.T) {
			res, err := NewService().Write(context.Background(), Input{
				RepoPath: fx.repoPath,
				Now:      fx.now,
				From:     hookForm,
			})
			if err != nil {
				t.Fatalf("Write with --from %s: %v", hookForm, err)
			}
			if res.Provider != hookForm {
				t.Errorf("Result.Provider = %q, want %q", res.Provider, hookForm)
			}
		})
	}
}

// TestWrite_FromOverride_NoMatchingProviderErrors pins the
// error path: --from names a provider with no recent session in
// the repo. Must return ErrNoFromMatch so the command layer can
// say "no <provider> sessions found; check the name or drop --from."
func TestWrite_FromOverride_NoMatchingProviderErrors(t *testing.T) {
	fx := setupFromOverrideFixture(t)

	// Repo has claude_code and gemini_cli sessions; ask for cursor.
	_, err := NewService().Write(context.Background(), Input{
		RepoPath: fx.repoPath,
		Now:      fx.now,
		From:     "cursor",
	})
	if !errors.Is(err, ErrNoFromMatch) {
		t.Errorf("expected ErrNoFromMatch when --from has no match; got %v", err)
	}
}

// TestWrite_FromOverride_SkipsStaleSessions pins that --from
// honors the same recency window the other resolvers use. A
// session whose last_seen is outside the window must not be
// returned, even if it's the only one matching the provider, or
// otherwise a months-old Claude session could be silently
// resurrected as a handoff source.
func TestWrite_FromOverride_SkipsStaleSessions(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t)

	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open lineage.db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	now := time.Now()
	repoID := "repo-stale"
	_ = h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: repoPath,
		CreatedAt: now.UnixMilli(), EnabledAt: now.UnixMilli(),
	})

	stale := now.Add(-48 * time.Hour) // outside the 24h window
	src, _ := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: "src-stale", RepositoryID: repoID, Provider: "claude_code",
		SourceKey: "key", LastSeenAt: stale.UnixMilli(), CreatedAt: stale.UnixMilli(),
	})
	sess, _ := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: "stale-sess", ProviderSessionID: "stale-prov",
		RepositoryID: repoID, Provider: "claude_code", SourceID: src.SourceID,
		StartedAt: stale.UnixMilli(), LastSeenAt: stale.UnixMilli(),
		MetadataJson: "{}",
	})
	_ = h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID: "evt-stale", SessionID: sess.SessionID, RepositoryID: repoID,
		Ts: stale.UnixMilli(), Kind: "user",
		Role:        sql.NullString{Valid: true, String: "user"},
		Summary:     sql.NullString{Valid: true, String: "stale claude work"},
		EventSource: "hook",
	})

	setupCaptureDir(t) // no active state

	_, err = NewService().Write(ctx, Input{
		RepoPath: repoPath,
		Now:      now,
		From:     "claude-code",
	})
	if !errors.Is(err, ErrNoFromMatch) {
		t.Errorf("expected ErrNoFromMatch for stale-only session; got %v", err)
	}
}

// TestWrite_FromOverride_SkipsSubagentSessions pins that --from
// uses the same parent-only filter the lineage fallback uses.
// Subagent sessions (parent_session_id set) are not standalone
// conversations and must not be returned as the source.
func TestWrite_FromOverride_SkipsSubagentSessions(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t)

	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open lineage.db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	now := time.Now()
	repoID := "repo-subagent"
	_ = h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: repoPath,
		CreatedAt: now.UnixMilli(), EnabledAt: now.UnixMilli(),
	})

	src, _ := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: "src-sub", RepositoryID: repoID, Provider: "claude_code",
		SourceKey: "key", LastSeenAt: now.UnixMilli(), CreatedAt: now.UnixMilli(),
	})
	// Subagent row: parent_session_id set, would otherwise be the
	// most-recent claude_code session in the repo.
	sub, _ := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: "sub-sess", ProviderSessionID: "sub-prov",
		ParentSessionID: sql.NullString{Valid: true, String: "parent-uuid"},
		RepositoryID:    repoID, Provider: "claude_code", SourceID: src.SourceID,
		StartedAt: now.UnixMilli(), LastSeenAt: now.UnixMilli(),
		MetadataJson: "{}",
	})
	_ = h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID: "evt-sub", SessionID: sub.SessionID, RepositoryID: repoID,
		Ts: now.UnixMilli(), Kind: "user",
		Role:        sql.NullString{Valid: true, String: "user"},
		Summary:     sql.NullString{Valid: true, String: "subagent work"},
		EventSource: "hook",
	})

	setupCaptureDir(t)

	_, err = NewService().Write(ctx, Input{
		RepoPath: repoPath,
		Now:      now,
		From:     "claude-code",
	})
	if !errors.Is(err, ErrNoFromMatch) {
		t.Errorf("expected ErrNoFromMatch when only a subagent row exists; got %v", err)
	}
}

// TestWrite_FromOverride_RefusesWhenLineageDBMissing verifies that
// explicit provider selection requires lineage data. Without this,
// the degraded path could write a bundle under the active provider's
// identity instead of the requested source provider.
func TestWrite_FromOverride_RefusesWhenLineageDBMissing(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t)
	// Deliberately do not create .semantica/lineage.db.

	// The active Gemini state should not be used when --from asks
	// for Claude.
	baseDir := setupCaptureDir(t)
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "active-gemini-sess",
		Provider:  "gemini-cli",
		CWD:       repoPath,
		Timestamp: time.Now().UnixMilli(),
	})

	_, err := NewService().Write(ctx, Input{
		RepoPath: repoPath,
		Now:      time.Now(),
		From:     "claude-code",
	})
	if !errors.Is(err, ErrNoFromMatch) {
		t.Fatalf("expected ErrNoFromMatch when --from set and lineage.db missing; got %v", err)
	}
	if !strings.Contains(err.Error(), "lineage.db not found") {
		t.Errorf("error should surface the lineage-missing reason; got %v", err)
	}
}

// TestWrite_FromOverride_RefusesWhenLineageDBUnreadable verifies
// that unreadable lineage data cannot fall back to the active
// provider when --from is set.
func TestWrite_FromOverride_RefusesWhenLineageDBUnreadable(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t)

	// Write a garbage file at the expected lineage.db path so
	// sqlstore.Open fails. The exact failure cause is opaque to
	// the test; only the refusal behavior matters.
	semDir := filepath.Join(repoPath, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "lineage.db"),
		[]byte("not a sqlite database\x00garbage"), 0o644); err != nil {
		t.Fatal(err)
	}

	baseDir := setupCaptureDir(t)
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "active-gemini-sess",
		Provider:  "gemini-cli",
		CWD:       repoPath,
		Timestamp: time.Now().UnixMilli(),
	})

	_, err := NewService().Write(ctx, Input{
		RepoPath: repoPath,
		Now:      time.Now(),
		From:     "claude-code",
	})
	if !errors.Is(err, ErrNoFromMatch) {
		t.Fatalf("expected ErrNoFromMatch when --from set and lineage.db unreadable; got %v", err)
	}
}

// TestWrite_FromOverride_RefusesWhenRepoRowMissing verifies that a
// missing repository row does not fall back to the active provider
// when --from is set.
func TestWrite_FromOverride_RefusesWhenRepoRowMissing(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t)

	// Open lineage.db so the file exists with valid schema, but do
	// not insert the repository row.
	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open lineage.db: %v", err)
	}
	_ = sqlstore.Close(h)

	baseDir := setupCaptureDir(t)
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "active-gemini-sess",
		Provider:  "gemini-cli",
		CWD:       repoPath,
		Timestamp: time.Now().UnixMilli(),
	})

	_, err = NewService().Write(ctx, Input{
		RepoPath: repoPath,
		Now:      time.Now(),
		From:     "claude-code",
	})
	if !errors.Is(err, ErrNoFromMatch) {
		t.Fatalf("expected ErrNoFromMatch when --from set and repo row missing; got %v", err)
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("error should surface the repo-not-registered reason; got %v", err)
	}
}

// TestWrite_FromOverride_BypassesAmbiguousActiveSession pins the
// safety-vs-explicitness tradeoff: when the user explicitly names
// --from, an ambiguous active capture state is no longer a
// blocker. The point of --from is to ignore the active state, so
// requiring it to be unambiguous would defeat the purpose. The
// default path (no --from) still refuses on ambiguity, covered by
// existing tests.
func TestWrite_FromOverride_BypassesAmbiguousActiveSession(t *testing.T) {
	fx := setupFromOverrideFixture(t)

	// Write a second active capture state. With no --from this is
	// ErrAmbiguousSession; with --from it should be a no-op for
	// the resolver.
	baseDir := os.Getenv("SEMANTICA_HOME")
	if baseDir == "" {
		t.Fatal("setupFromOverrideFixture should have set SEMANTICA_HOME")
	}
	writeCaptureState(t, filepath.Join(baseDir, "capture"), captureFixture{
		SessionID: "second-active-prov",
		Provider:  "cursor",
		CWD:       fx.repoPath,
		Timestamp: fx.now.UnixMilli(),
	})

	res, err := NewService().Write(context.Background(), Input{
		RepoPath: fx.repoPath,
		Now:      fx.now,
		From:     "claude-code",
	})
	if err != nil {
		t.Fatalf("Write should succeed despite ambiguous active state when --from is set; got %v", err)
	}
	if !strings.Contains(string(res.Bytes), fx.claudeText) {
		t.Errorf("bundle missing Claude work; --from did not override ambiguous capture state")
	}
}

// Ambiguous active sessions are grouped by provider before the
// writer decides whether it needs a caller choice:
//   - 1 distinct provider: auto-route via --from <that provider>
//   - 2+ distinct providers: bubble AmbiguousActiveSessionError
//     with the candidate list so the command layer can pick.

// TestWrite_MultipleSameProviderSessions_AutoRoutes pins the
// "duplicates collapse to one provider" rule. Two Claude capture
// states are active; one Claude lineage row with events exists.
// Write must succeed and produce a Claude bundle without
// surfacing ErrAmbiguousSession.
func TestWrite_MultipleSameProviderSessions_AutoRoutes(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t)

	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open lineage.db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	now := time.Now()
	repoID := "repo-same-provider-dup"
	_ = h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: repoPath,
		CreatedAt: now.UnixMilli(), EnabledAt: now.UnixMilli(),
	})
	src, _ := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: "src", RepositoryID: repoID, Provider: "claude_code",
		SourceKey: "default", LastSeenAt: now.UnixMilli(), CreatedAt: now.UnixMilli(),
	})
	sess, _ := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: "claude-uuid", ProviderSessionID: "claude-prov",
		RepositoryID: repoID, Provider: "claude_code", SourceID: src.SourceID,
		StartedAt:  now.Add(-5 * time.Minute).UnixMilli(),
		LastSeenAt: now.UnixMilli(), MetadataJson: "{}",
	})
	const claudeText = "CLAUDE PROMPT - should appear when duplicates collapse"
	_ = h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID: "evt-claude", SessionID: sess.SessionID, RepositoryID: repoID,
		Ts:   now.Add(-1 * time.Minute).UnixMilli(),
		Kind: "user", Role: sql.NullString{Valid: true, String: "user"},
		Summary:     sql.NullString{Valid: true, String: claudeText},
		EventSource: "hook",
	})

	// Two Claude capture states in the same repo collapse to one
	// source provider.
	baseDir := setupCaptureDir(t)
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "active-claude-1",
		Provider:  "claude-code",
		CWD:       repoPath,
		Timestamp: now.UnixMilli(),
	})
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "active-claude-2",
		Provider:  "claude-code",
		CWD:       repoPath,
		Timestamp: now.UnixMilli(),
	})

	res, err := NewService().Write(ctx, Input{RepoPath: repoPath, Now: now})
	if err != nil {
		t.Fatalf("Write should succeed when duplicates collapse to one provider; got %v", err)
	}
	body := string(res.Bytes)
	if !strings.Contains(body, claudeText) {
		t.Errorf("bundle missing claude content; auto-route did not resolve via --from:\n%s", body)
	}
	if res.Provider != "claude-code" {
		t.Errorf("Result.Provider = %q, want claude-code", res.Provider)
	}
}

// TestWrite_AutoCollapse_LineageMissingReturnsAutoSelectFailed
// verifies that auto-selected providers use ErrAutoSelectFailed
// when the selected provider has no recent lineage row.
func TestWrite_AutoCollapse_LineageMissingReturnsAutoSelectFailed(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t)

	// Set up lineage.db with a registered repo row, but no
	// claude_code sessions. The two active capture states will
	// collapse to claude-code; the from-resolver will then find
	// no lineage row.
	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open lineage.db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	now := time.Now()
	repoID := "repo-auto-no-lineage"
	_ = h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: repoPath,
		CreatedAt: now.UnixMilli(), EnabledAt: now.UnixMilli(),
	})

	// Two active claude-code capture states collapse to one
	// provider and route through --from internally. No claude_code
	// lineage row exists, so the from-resolver fails.
	baseDir := setupCaptureDir(t)
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "claude-1", Provider: "claude-code",
		CWD: repoPath, Timestamp: now.UnixMilli(),
	})
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "claude-2", Provider: "claude-code",
		CWD: repoPath, Timestamp: now.UnixMilli(),
	})

	_, err = NewService().Write(ctx, Input{RepoPath: repoPath, Now: now})
	if err == nil {
		t.Fatal("expected an error when auto-collapsed provider has no lineage row")
	}
	if !errors.Is(err, ErrAutoSelectFailed) {
		t.Errorf("expected ErrAutoSelectFailed; got %v", err)
	}
	// Keep the explicit --from sentinel separate so the command
	// layer can render the right hint for auto-selected providers.
	if errors.Is(err, ErrNoFromMatch) {
		t.Errorf("auto-collapse failure must not match ErrNoFromMatch (it would "+
			"trigger the wrong command-layer surface message); got %v", err)
	}
}

// TestWrite_AutoCollapse_LineageDBMissingReturnsAutoSelectFailed
// verifies the same sentinel choice when lineage.db is missing.
func TestWrite_AutoCollapse_LineageDBMissingReturnsAutoSelectFailed(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t)
	// Deliberately do not create .semantica/lineage.db.

	now := time.Now()
	baseDir := setupCaptureDir(t)
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "claude-1", Provider: "claude-code",
		CWD: repoPath, Timestamp: now.UnixMilli(),
	})
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "claude-2", Provider: "claude-code",
		CWD: repoPath, Timestamp: now.UnixMilli(),
	})

	_, err := NewService().Write(ctx, Input{RepoPath: repoPath, Now: now})
	if !errors.Is(err, ErrAutoSelectFailed) {
		t.Fatalf("expected ErrAutoSelectFailed when auto-collapsed + lineage.db missing; got %v", err)
	}
	if errors.Is(err, ErrNoFromMatch) {
		t.Errorf("auto-collapse failure must not match ErrNoFromMatch; got %v", err)
	}
}

// TestWrite_MultipleDistinctProviders_ReturnsTypedError pins the
// other half of the contract: 2+ distinct providers must surface
// an AmbiguousActiveSessionError carrying the candidate list. The
// errors.Is(err, ErrAmbiguousSession) check must still pass so
// existing callers don't break.
func TestWrite_MultipleDistinctProviders_ReturnsTypedError(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t)

	now := time.Now()
	baseDir := setupCaptureDir(t)
	// One Claude, two Gemini, one Cursor. After dedup we expect
	// three providers in the error payload.
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "c1", Provider: "claude-code",
		CWD: repoPath, Timestamp: now.UnixMilli(),
	})
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "g1", Provider: "gemini-cli",
		CWD: repoPath, Timestamp: now.Add(-1 * time.Second).UnixMilli(),
	})
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "g2", Provider: "gemini-cli",
		CWD: repoPath, Timestamp: now.Add(-2 * time.Second).UnixMilli(),
	})
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "cur1", Provider: "cursor",
		CWD: repoPath, Timestamp: now.Add(-3 * time.Second).UnixMilli(),
	})

	_, err := NewService().Write(ctx, Input{RepoPath: repoPath, Now: now})
	if err == nil {
		t.Fatal("expected an error when multiple distinct providers are active")
	}

	// Sentinel check still works for downstream callers.
	if !errors.Is(err, ErrAmbiguousSession) {
		t.Errorf("errors.Is(err, ErrAmbiguousSession) = false; want true. err = %v", err)
	}

	// Typed unwrap exposes the candidate list.
	var amb *AmbiguousActiveSessionError
	if !errors.As(err, &amb) {
		t.Fatalf("errors.As to AmbiguousActiveSessionError failed; err = %v", err)
	}
	if len(amb.Providers) != 3 {
		t.Errorf("Providers length = %d, want 3; got %+v", len(amb.Providers), amb.Providers)
	}

	// Sort: most-recent first. The Claude state has the latest
	// timestamp, so it should head the list.
	if amb.Providers[0].Provider != "claude-code" {
		t.Errorf("Providers[0] = %q, want claude-code (most recent)", amb.Providers[0].Provider)
	}

	// Gemini has two active states; the count must reflect that.
	var geminiCount int
	for _, p := range amb.Providers {
		if p.Provider == "gemini-cli" {
			geminiCount = p.Count
			break
		}
	}
	if geminiCount != 2 {
		t.Errorf("gemini-cli count = %d, want 2", geminiCount)
	}
}

// TestListActiveProviders_FiltersByRepoAndRecency pins the helper
// in isolation: only states matching the repo and inside the
// recency window count. Sort order (most-recent first) is also
// asserted because it drives the picker's default-highlight.
func TestListActiveProviders_FiltersByRepoAndRecency(t *testing.T) {
	repoA := t.TempDir()
	repoB := t.TempDir()
	now := time.Now()

	baseDir := setupCaptureDir(t)
	// Match: in repoA, recent.
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "a-claude", Provider: "claude-code",
		CWD: repoA, Timestamp: now.UnixMilli(),
	})
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "a-gemini", Provider: "gemini-cli",
		CWD: repoA, Timestamp: now.Add(-5 * time.Minute).UnixMilli(),
	})
	// Filtered out: in repoB.
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "b-cursor", Provider: "cursor",
		CWD: repoB, Timestamp: now.UnixMilli(),
	})
	// Filtered out: stale (older than 24h window).
	writeCaptureState(t, baseDir, captureFixture{
		SessionID: "a-stale", Provider: "copilot",
		CWD: repoA, Timestamp: now.Add(-48 * time.Hour).UnixMilli(),
	})

	providers, err := listActiveProviders(repoA, now)
	if err != nil {
		t.Fatalf("listActiveProviders: %v", err)
	}
	if len(providers) != 2 {
		t.Fatalf("got %d providers, want 2; providers=%+v", len(providers), providers)
	}
	if providers[0].Provider != "claude-code" {
		t.Errorf("providers[0] = %q, want claude-code (most recent)", providers[0].Provider)
	}
	if providers[1].Provider != "gemini-cli" {
		t.Errorf("providers[1] = %q, want gemini-cli", providers[1].Provider)
	}
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
