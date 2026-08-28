package hooks

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/broker"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
)

// agentEventCount returns the number of raw agent events stored in a repo.
func agentEventCount(t *testing.T, repoPath string) int {
	t.Helper()
	dbPath := filepath.Join(repoPath, ".semantica", "lineage.db")
	h, err := sqlstore.Open(context.Background(), dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open lineage db: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	var n int
	if err := h.DB.QueryRowContext(context.Background(), "select count(*) from agent_events").Scan(&n); err != nil {
		t.Fatalf("count agent_events: %v", err)
	}
	return n
}

// Session-routed windows do not persist repository targets.
func TestSessionRoutedShell_WritesNoReceipt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-s", Provider: "claude-code", TurnID: "turn-1", CWD: w.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	ev := startedEvent("sess-s", "toolu_s", w.repoPath)
	if err := handleToolStepStarted(context.Background(), "claude-code", ev, w.bh); err != nil {
		t.Fatalf("pre hook: %v", err)
	}
	if wins := windowsIn(t, w.semDir); len(wins) != 1 {
		t.Fatalf("windows = %d, want 1", len(wins))
	}
	if rec, err := LoadToolWindowTarget(receiptKeyFor("claude-code", ev)); err != nil || rec != nil {
		t.Errorf("session-routed window wrote a receipt: (%+v, %v)", rec, err)
	}
}

// A shell command running in another repository records its event and delta
// there, even when the post hook carries only the session directory.
func TestCrossRepoShell_RoutesToCommandRepoAndPersistsTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	ctx := context.Background()

	a := newToolWindowWorld(t, home, "repoA")
	bPath := filepath.Join(t.TempDir(), "repoB")
	b := newToolWindowWorldAt(t, a.bh, bPath)

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-x", Provider: "cursor", TurnID: "turn-1", CWD: a.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// The session is in A and the command runs in B.
	pre := &Event{
		Type: ToolStepStarted, SessionID: "sess-x", ToolUseID: "toolu_x", ToolName: "Bash",
		CWD: a.repoPath, EffectiveCWD: b.repoPath, Timestamp: time.Now().UnixMilli(),
	}
	if err := handleToolStepStarted(ctx, "cursor", pre, a.bh); err != nil {
		t.Fatalf("pre hook: %v", err)
	}

	if wins := windowsIn(t, b.semDir); len(wins) != 1 {
		t.Fatalf("repo B windows = %d, want 1 (command repo)", len(wins))
	}
	if wins := windowsIn(t, a.semDir); len(wins) != 0 {
		t.Fatalf("repo A windows = %d, want 0 (no false capture in the session repo)", len(wins))
	}
	preKey := receiptKeyFor("cursor", pre)
	if rec, err := LoadToolWindowTarget(preKey); err != nil || rec == nil || rec.RepoPath != b.repoPath {
		t.Fatalf("persisted target = (%+v, %v), want repo B", rec, err)
	}

	if err := os.WriteFile(filepath.Join(b.repoPath, "gen.txt"), []byte("generated line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Deactivation proves the post hook uses the persisted target.
	if err := broker.Deactivate(ctx, a.bh, b.repoPath); err != nil {
		t.Fatal(err)
	}

	// The post hook carries only the session directory.
	post := postBashEvent("sess-x", "toolu_x", a.repoPath, "sqlc generate")
	events := []broker.RawEvent{bashRawEvent("evt-x", "toolu_x", "sess-x")}
	if completeToolWindow(ctx, "cursor", post, a.bh, nil, events) != windowHandled {
		t.Fatal("window completion not handled")
	}

	bDeltas := findDeltas(t, b.semDir)
	if len(bDeltas) != 1 {
		t.Fatalf("repo B deltas = %d, want 1", len(bDeltas))
	}
	foundGen := false
	for _, f := range bDeltas[0].Files {
		if f.Path == "gen.txt" && f.Operation == "create" {
			foundGen = true
		}
	}
	if !foundGen {
		t.Errorf("repo B delta missing gen.txt creation: %+v", bDeltas[0].Files)
	}
	if aDeltas := findDeltas(t, a.semDir); len(aDeltas) != 0 {
		t.Errorf("repo A deltas = %d, want 0 (no source-repo leakage)", len(aDeltas))
	}
	// Completion removes the persisted target.
	if rec, err := LoadToolWindowTarget(preKey); err != nil || rec != nil {
		t.Errorf("persisted target not cleaned up: (%+v, %v)", rec, err)
	}
}

// Dispatch suppresses a Cursor shell completion without a persisted target.
func TestDispatch_MissingCursorReceipt_WritesNoEventToSessionRepo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	ctx := context.Background()
	a := newToolWindowWorld(t, home, "repoA")

	// No pre hook runs, so no target exists.
	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-cur", Provider: "cursor", TurnID: "turn-1", CWD: a.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	cursorProv := &fakeDirectProvider{
		fakeProvider: fakeProvider{name: "cursor"},
		directEvents: []broker.RawEvent{bashRawEvent("evt-cur", "toolu_cur", "sess-cur")},
	}
	post := &Event{
		Type: ToolStepCompleted, SessionID: "sess-cur", ToolUseID: "toolu_cur",
		ToolName: "Bash", CWD: a.repoPath, Timestamp: time.Now().UnixMilli(),
	}
	if err := Dispatch(ctx, cursorProv, post, a.bh, nil); err != nil {
		t.Fatalf("dispatch cursor: %v", err)
	}
	if n := agentEventCount(t, a.repoPath); n != 0 {
		t.Fatalf("repo A agent_events = %d, want 0 (suppressed, not session-routed)", n)
	}

	// The control confirms that the event is otherwise routable to A.
	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-ctl", Provider: "claude-code", TurnID: "turn-1", CWD: a.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	ctlProv := &fakeDirectProvider{
		fakeProvider: fakeProvider{name: "claude-code"},
		directEvents: []broker.RawEvent{bashRawEvent("evt-ctl", "toolu_ctl", "sess-ctl")},
	}
	ctlPost := &Event{
		Type: ToolStepCompleted, SessionID: "sess-ctl", ToolUseID: "toolu_ctl",
		ToolName: "Bash", CWD: a.repoPath, Timestamp: time.Now().UnixMilli(),
	}
	if err := Dispatch(ctx, ctlProv, ctlPost, a.bh, nil); err != nil {
		t.Fatalf("dispatch control: %v", err)
	}
	if n := agentEventCount(t, a.repoPath); n != 1 {
		t.Fatalf("repo A agent_events = %d after control, want 1 (event is deliverable)", n)
	}
}

// A missing target suppresses completion instead of using the session repo.
func TestCrossRepoShell_MissingReceiptFailsClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	ctx := context.Background()

	a := newToolWindowWorld(t, home, "repoA")
	bPath := filepath.Join(t.TempDir(), "repoB")
	b := newToolWindowWorldAt(t, a.bh, bPath)

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-x", Provider: "cursor", TurnID: "turn-1", CWD: a.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	pre := &Event{
		Type: ToolStepStarted, SessionID: "sess-x", ToolUseID: "toolu_x", ToolName: "Bash",
		CWD: a.repoPath, EffectiveCWD: b.repoPath, Timestamp: time.Now().UnixMilli(),
	}
	if err := handleToolStepStarted(ctx, "cursor", pre, a.bh); err != nil {
		t.Fatalf("pre hook: %v", err)
	}

	// Remove the target before the post hook arrives.
	preKey := receiptKeyFor("cursor", pre)
	if err := DeleteToolWindowTarget(preKey); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(b.repoPath, "gen.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	post := postBashEvent("sess-x", "toolu_x", a.repoPath, "sqlc generate")
	events := []broker.RawEvent{bashRawEvent("evt-x", "toolu_x", "sess-x")}
	// Suppression prevents session routing.
	if got := completeToolWindow(ctx, "cursor", post, a.bh, nil, events); got != windowSuppressed {
		t.Fatalf("disposition = %v, want windowSuppressed (fail closed)", got)
	}
	if d := findDeltas(t, a.semDir); len(d) != 0 {
		t.Errorf("repo A deltas = %d, want 0", len(d))
	}
	if d := findDeltas(t, b.semDir); len(d) != 0 {
		t.Errorf("repo B deltas = %d, want 0", len(d))
	}
}

// A repository identity mismatch suppresses completion.
func TestCrossRepoShell_IdentityMismatchFailsClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	ctx := context.Background()

	a := newToolWindowWorld(t, home, "repoA")
	bPath := filepath.Join(t.TempDir(), "repoB")
	b := newToolWindowWorldAt(t, a.bh, bPath)

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-x", Provider: "cursor", TurnID: "turn-1", CWD: a.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	pre := &Event{
		Type: ToolStepStarted, SessionID: "sess-x", ToolUseID: "toolu_x", ToolName: "Bash",
		CWD: a.repoPath, EffectiveCWD: b.repoPath, Timestamp: time.Now().UnixMilli(),
	}
	if err := handleToolStepStarted(ctx, "cursor", pre, a.bh); err != nil {
		t.Fatalf("pre hook: %v", err)
	}

	// Keep the path but replace its repository identity.
	preKey := receiptKeyFor("cursor", pre)
	if err := SaveToolWindowTarget(preKey, b.repoPath, "corrupted-repo-id"); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(b.repoPath, "gen.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	post := postBashEvent("sess-x", "toolu_x", a.repoPath, "sqlc generate")
	events := []broker.RawEvent{bashRawEvent("evt-x", "toolu_x", "sess-x")}
	if got := completeToolWindow(ctx, "cursor", post, a.bh, nil, events); got != windowSuppressed {
		t.Fatalf("disposition = %v, want windowSuppressed (fail closed)", got)
	}
	if d := findDeltas(t, a.semDir); len(d) != 0 {
		t.Errorf("repo A deltas = %d, want 0", len(d))
	}
	if d := findDeltas(t, b.semDir); len(d) != 0 {
		t.Errorf("repo B deltas = %d, want 0", len(d))
	}
}
