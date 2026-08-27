package hooks

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/toolsnap"
)

// toolWindowWorld contains an enabled test repository and broker handle.
type toolWindowWorld struct {
	repoPath string
	semDir   string
	repoID   string
	bh       *broker.Handle
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func newToolWindowWorld(t *testing.T, home string, name string) *toolWindowWorld {
	t.Helper()
	// Real Git and SQLite work can exceed production hook budgets on slow CI.
	setToolWindowDeadline(t, 30*time.Second)
	setToolWindowRefReleaseWait(t, 30*time.Second)
	ctx := context.Background()

	repoPath, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repoPath = filepath.Join(repoPath, name)
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repoPath, "init", "-q", "-b", "main")
	gitIn(t, repoPath, "config", "user.email", "t@example.com")
	gitIn(t, repoPath, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repoPath, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repoPath, "add", ".")
	gitIn(t, repoPath, "commit", "-q", "-m", "init")

	semDir := filepath.Join(repoPath, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(semDir, "lineage.db")
	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	repoID := uuid.NewString()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: repoPath, CreatedAt: 1000, EnabledAt: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := sqlstore.Close(h); err != nil {
		t.Fatal(err)
	}

	bh, err := broker.Open(ctx, filepath.Join(home, "repos.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Register(ctx, bh, repoPath, repoPath); err != nil {
		t.Fatal(err)
	}
	return &toolWindowWorld{repoPath: repoPath, semDir: semDir, repoID: repoID, bh: bh}
}

func startedEvent(session, toolUse, cwd string) *Event {
	return &Event{
		Type: ToolStepStarted, SessionID: session,
		ToolUseID: toolUse, ToolName: "Bash", CWD: cwd,
		Timestamp: time.Now().UnixMilli(),
	}
}

func windowsIn(t *testing.T, semDir string) []toolsnap.PendingToolSnapshot {
	t.Helper()
	reg, err := toolsnap.OpenRegistry(semDir)
	if err != nil {
		t.Fatal(err)
	}
	wins, err := reg.Stale(context.Background(), time.Now().UnixMilli()+int64(time.Hour/time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	return wins
}

// The handler uses capture-state CWD and records the pre-tool bytes.
func TestHandleToolStepStartedRegistersWindow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-1", Provider: "claude-code",
		TurnID: "turn-1", CWD: w.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.repoPath, "a.txt"), []byte("dirty at pre\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Empty event CWD exercises the state-CWD fallback.
	if err := handleToolStepStarted(context.Background(), "claude-code", startedEvent("sess-1", "toolu_1", ""), w.bh); err != nil {
		t.Fatalf("handler: %v", err)
	}

	wins := windowsIn(t, w.semDir)
	if len(wins) != 1 {
		t.Fatalf("windows = %+v, want one", wins)
	}
	win := wins[0]
	if win.Key.RepositoryID != w.repoID || win.Key.Provider != "claude_code" ||
		win.Key.SessionID != "sess-1" || win.Key.TurnID != "turn-1" || win.Key.ToolUseID != "toolu_1" {
		t.Fatalf("key = %+v", win.Key)
	}
	out, err := exec.Command("git", "--git-dir",
		filepath.Join(w.semDir, "tool-snapshots.git"),
		"cat-file", "blob", win.TreeHash+":a.txt").Output()
	if err != nil {
		t.Fatalf("read snapshot blob: %v", err)
	}
	if string(out) != "dirty at pre\n" {
		t.Fatalf("snapshot content = %q", out)
	}
}

// With nested enabled repositories, the deepest match owns the window.
func TestHandleToolStepStartedSelectsDeepestRepo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	parent := newToolWindowWorld(t, home, "parent")

	childPath := filepath.Join(parent.repoPath, "nested", "child")
	if err := os.MkdirAll(filepath.Dir(childPath), 0o755); err != nil {
		t.Fatal(err)
	}
	child := newToolWindowWorldAt(t, parent.bh, childPath)

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-n", Provider: "claude-code", TurnID: "turn-1",
		CWD: child.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := handleToolStepStarted(context.Background(), "claude-code", startedEvent("sess-n", "toolu_n", child.repoPath), parent.bh); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if wins := windowsIn(t, child.semDir); len(wins) != 1 {
		t.Fatalf("child windows = %+v, want one", wins)
	}
	if wins := windowsIn(t, parent.semDir); len(wins) != 0 {
		t.Fatalf("parent windows = %+v, want none", wins)
	}
}

// Without an active turn the handler is a silent no-op.
func TestHandleToolStepStartedNoActiveTurn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")

	if err := handleToolStepStarted(context.Background(), "claude-code", startedEvent("sess-none", "toolu_1", w.repoPath), w.bh); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if wins := windowsIn(t, w.semDir); len(wins) != 0 {
		t.Fatalf("windows = %+v, want none", wins)
	}
}

// setToolWindowDeadline overrides the capture deadline for one test.
func setToolWindowDeadline(t *testing.T, d time.Duration) {
	t.Helper()
	orig := toolWindowDeadline
	toolWindowDeadline = d
	t.Cleanup(func() { toolWindowDeadline = orig })
}

// setToolWindowRefReleaseWait overrides the ref-release budget for one test.
func setToolWindowRefReleaseWait(t *testing.T, d time.Duration) {
	t.Helper()
	orig := toolWindowRefReleaseWait
	toolWindowRefReleaseWait = d
	t.Cleanup(func() { toolWindowRefReleaseWait = orig })
}

// A registry lock timeout records a diagnostic and no window.
func TestHandleToolStepStartedLockTimeoutDiagnostic(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-l", Provider: "claude-code", TurnID: "turn-1",
		CWD: w.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// Cancel before capture to exercise the lock-timeout path.
	toolWindowPreCapture = func(ctx context.Context) context.Context {
		c, cancel := context.WithCancel(ctx)
		cancel()
		return c
	}
	t.Cleanup(func() { toolWindowPreCapture = nil })

	if err := handleToolStepStarted(context.Background(), "claude-code", startedEvent("sess-l", "toolu_l", w.repoPath), w.bh); err != nil {
		t.Fatalf("handler: %v", err)
	}

	logBytes, err := os.ReadFile(filepath.Join(w.semDir, "activity.log"))
	if err != nil {
		t.Fatalf("activity log: %v", err)
	}
	if !strings.Contains(string(logBytes), toolsnap.ReasonLockTimeout) {
		t.Fatalf("activity log = %q, want %s diagnostic", logBytes, toolsnap.ReasonLockTimeout)
	}
	if wins := windowsIn(t, w.semDir); len(wins) != 0 {
		t.Fatalf("windows = %+v, want none after lock timeout", wins)
	}
}

// A working directory outside every enabled repository fails open.
func TestHandleToolStepStartedOutsideRepos(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-o", Provider: "claude-code", TurnID: "turn-1",
		CWD: t.TempDir(), Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := handleToolStepStarted(context.Background(), "claude-code", startedEvent("sess-o", "toolu_o", ""), w.bh); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if wins := windowsIn(t, w.semDir); len(wins) != 0 {
		t.Fatalf("windows = %+v, want none", wins)
	}
}

func postBashEvent(session, toolUse, cwd, command string) *Event {
	return &Event{
		Type: ToolStepCompleted, SessionID: session,
		ToolUseID: toolUse, ToolName: "Bash", CWD: cwd,
		ToolInput: []byte(`{"command":` + strconv.Quote(command) + `}`),
		Timestamp: time.Now().UnixMilli(),
	}
}

func bashRawEvent(id, toolUse, session string) broker.RawEvent {
	return broker.RawEvent{
		EventID: id, SourceKey: "/data/" + session + ".jsonl",
		Provider: "claude_code", Timestamp: time.Now().UnixMilli(),
		Kind: "assistant", Role: "assistant",
		TurnID: "turn-1", ToolUseID: toolUse, ToolName: "Bash",
		EventSource: "hook", ProviderSessionID: session,
		SessionStartedAt: 1500, SessionMetaJSON: `{"source_key":"x"}`,
	}
}

// findDeltas scans the repo CAS for canonical tool deltas.
func findDeltas(t *testing.T, semDir string) []*toolsnap.Delta {
	t.Helper()
	var deltas []*toolsnap.Delta
	objects := filepath.Join(semDir, "objects")
	bs, err := blobs.NewStore(objects)
	if err != nil {
		t.Fatal(err)
	}
	_ = filepath.WalkDir(objects, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		raw, err := bs.Get(context.Background(), filepath.Base(path))
		if err != nil {
			return nil
		}
		if delta, err := toolsnap.ParseDelta(raw); err == nil {
			deltas = append(deltas, delta)
		}
		return nil
	})
	return deltas
}

// TestCompleteToolWindowProducesDelta covers the complete hook lifecycle.
func TestCompleteToolWindowProducesDelta(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-d", Provider: "claude-code",
		TurnID: "turn-1", CWD: w.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := handleToolStepStarted(ctx, "claude-code", startedEvent("sess-d", "toolu_d", w.repoPath), w.bh); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(w.repoPath, "gen.txt"), []byte("generated line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	post := postBashEvent("sess-d", "toolu_d", w.repoPath, "some-generator --write")
	events := []broker.RawEvent{bashRawEvent("evt-close", "toolu_d", "sess-d")}
	if windowHandled != completeToolWindow(ctx, "claude-code", post, w.bh, nil, events) {
		t.Fatal("window completion not handled")
	}

	if wins := windowsIn(t, w.semDir); len(wins) != 0 {
		t.Fatalf("windows after closure = %+v", wins)
	}
	deltas := findDeltas(t, w.semDir)
	if len(deltas) != 1 {
		t.Fatalf("deltas = %d, want one", len(deltas))
	}
	d := deltas[0]
	if d.Scope != "tool" || d.Status != "complete" {
		t.Fatalf("delta = %+v", d)
	}
	if len(d.ToolUses) != 1 || d.ToolUses[0].ToolUseID != "toolu_d" ||
		d.ToolUses[0].EventID != "evt-close" ||
		d.ToolUses[0].CommandSummary != "some-generator --write" {
		t.Fatalf("tool uses = %+v", d.ToolUses)
	}
	if len(d.Actors) != 1 || d.Actors[0].Provider != "claude_code" {
		t.Fatalf("actors = %+v", d.Actors)
	}
	found := false
	for _, f := range d.Files {
		if f.Path == "gen.txt" && f.Operation == "create" && len(f.Hunks) == 1 &&
			f.Hunks[0].NewLines[0] == "generated line" {
			found = true
		}
	}
	if !found {
		t.Fatalf("files = %+v, want gen.txt creation with its line", d.Files)
	}
	h, err := sqlstore.Open(ctx, filepath.Join(w.semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	var count int
	if err := h.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM agent_events WHERE tool_use_id = 'toolu_d' AND event_source = 'hook'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("closing events in lineage db = %d, want 1", count)
	}

	links := linksIn(t, w.semDir)
	if len(links) != 1 || links[0].EventID != "evt-close" || links[0].Kind != "tool_delta" || links[0].Hash == "" {
		t.Fatalf("links = %+v, want one tool_delta link for evt-close", links)
	}
	bs, err := blobs.NewStore(filepath.Join(w.semDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := bs.Get(ctx, links[0].Hash)
	if err != nil {
		t.Fatalf("linked blob unreadable: %v", err)
	}
	if linked, err := toolsnap.ParseDelta(raw); err != nil || linked.ToolUses[0].EventID != "evt-close" {
		t.Fatalf("linked blob is not the closing delta: %v", err)
	}

	if refs := snapshotRefsIn(t, w.repoPath, w.semDir); len(refs) != 0 {
		t.Fatalf("refs after closure = %v, want none", refs)
	}
}

// Codex Bash windows produce Codex-attributed deltas.
func TestCompleteToolWindowProducesDelta_Codex(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-cx", Provider: "codex",
		TurnID: "turn-1", CWD: w.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := handleToolStepStarted(ctx, "codex", startedEvent("sess-cx", "toolu_cx", w.repoPath), w.bh); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.repoPath, "gen.txt"), []byte("generated line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	post := postBashEvent("sess-cx", "toolu_cx", w.repoPath, "make generate")
	closing := broker.RawEvent{
		EventID: "evt-cx", SourceKey: "/data/sess-cx.jsonl",
		Provider: "codex", Timestamp: time.Now().UnixMilli(),
		Kind: "assistant", Role: "assistant",
		TurnID: "turn-1", ToolUseID: "toolu_cx", ToolName: "Bash",
		EventSource: "hook", ProviderSessionID: "sess-cx",
		SessionStartedAt: 1500, SessionMetaJSON: `{"source_key":"x"}`,
	}
	if windowHandled != completeToolWindow(ctx, "codex", post, w.bh, nil, []broker.RawEvent{closing}) {
		t.Fatal("codex window completion not handled")
	}

	deltas := findDeltas(t, w.semDir)
	if len(deltas) != 1 || deltas[0].Status != "complete" {
		t.Fatalf("deltas = %+v, want one complete", deltas)
	}
	if len(deltas[0].Actors) != 1 || deltas[0].Actors[0].Provider != "codex" {
		t.Fatalf("actors = %+v, want codex", deltas[0].Actors)
	}
	found := false
	for _, f := range deltas[0].Files {
		if f.Path == "gen.txt" && f.Operation == "create" &&
			len(f.Hunks) == 1 && f.Hunks[0].NewLines[0] == "generated line" {
			found = true
		}
	}
	if !found {
		t.Fatalf("files = %+v, want gen.txt creation", deltas[0].Files)
	}
	links := linksIn(t, w.semDir)
	if len(links) != 1 || links[0].EventID != "evt-cx" ||
		links[0].GroupID == "" || strings.Contains(links[0].GroupID, ":") {
		t.Fatalf("links = %+v, want one complete (unprefixed) tool_delta link", links)
	}
}

// TestCompleteToolWindowConcurrentGroup covers overlapping Bash tools.
func TestCompleteToolWindowConcurrentGroup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-g", Provider: "claude-code",
		TurnID: "turn-1", CWD: w.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := handleToolStepStarted(ctx, "claude-code", startedEvent("sess-g", "toolu_a", w.repoPath), w.bh); err != nil {
		t.Fatal(err)
	}
	if err := handleToolStepStarted(ctx, "claude-code", startedEvent("sess-g", "toolu_b", w.repoPath), w.bh); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.repoPath, "shared.txt"), []byte("written during overlap\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The first completion persists its event while the group stays open.
	postA := postBashEvent("sess-g", "toolu_a", w.repoPath, "first")
	if windowHandled != completeToolWindow(ctx, "claude-code", postA, w.bh, nil, []broker.RawEvent{bashRawEvent("evt-a", "toolu_a", "sess-g")}) {
		t.Fatal("non-final completion did not write its event under the lock")
	}
	if wins := windowsIn(t, w.semDir); len(wins) != 2 {
		t.Fatalf("windows after first completion = %+v", wins)
	}
	h, err := sqlstore.Open(ctx, filepath.Join(w.semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	var evtA int
	if err := h.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM agent_events WHERE event_id = 'evt-a'").Scan(&evtA); err != nil {
		t.Fatal(err)
	}
	_ = sqlstore.Close(h)
	if evtA != 1 {
		t.Fatalf("member event not durable before closure: count = %d", evtA)
	}

	postB := postBashEvent("sess-g", "toolu_b", w.repoPath, "second")
	if windowHandled != completeToolWindow(ctx, "claude-code", postB, w.bh, nil, []broker.RawEvent{bashRawEvent("evt-b", "toolu_b", "sess-g")}) {
		t.Fatal("final completion not handled")
	}
	deltas := findDeltas(t, w.semDir)
	if len(deltas) != 1 {
		t.Fatalf("deltas = %d, want one group delta", len(deltas))
	}
	d := deltas[0]
	if d.Scope != "concurrent_group" || len(d.ToolUses) != 2 || len(d.Actors) != 1 {
		t.Fatalf("delta = scope %s uses %d actors %d", d.Scope, len(d.ToolUses), len(d.Actors))
	}
	summaries := map[string]string{}
	for _, u := range d.ToolUses {
		summaries[u.ToolUseID] = u.CommandSummary
	}
	if summaries["toolu_a"] != "first" || summaries["toolu_b"] != "second" {
		t.Fatalf("summaries = %v", summaries)
	}
	ids := map[string]string{}
	for _, u := range d.ToolUses {
		ids[u.ToolUseID] = u.EventID
	}
	if ids["toolu_a"] != "evt-a" || ids["toolu_b"] != "evt-b" {
		t.Fatalf("member event ids = %v", ids)
	}

	links := linksIn(t, w.semDir)
	if len(links) != 2 || links[0].EventID != "evt-a" || links[1].EventID != "evt-b" ||
		links[0].Hash == "" || links[0].Hash != links[1].Hash || links[0].GroupID != links[1].GroupID {
		t.Fatalf("links = %+v, want both members on one delta", links)
	}
	if refs := snapshotRefsIn(t, w.repoPath, w.semDir); len(refs) != 0 {
		t.Fatalf("refs after closure = %v, want none", refs)
	}
}

type evidenceLinkRow struct {
	EventID string
	Kind    string
	Hash    string
	GroupID string
}

func linksIn(t *testing.T, semDir string) []evidenceLinkRow {
	t.Helper()
	ctx := context.Background()
	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	rows, err := h.DB.QueryContext(ctx,
		"SELECT event_id, evidence_kind, evidence_hash, group_id FROM agent_event_evidence_links ORDER BY event_id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []evidenceLinkRow
	for rows.Next() {
		var r evidenceLinkRow
		if err := rows.Scan(&r.EventID, &r.Kind, &r.Hash, &r.GroupID); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func snapshotRefsIn(t *testing.T, repoPath, semDir string) map[string]string {
	t.Helper()
	ctx := context.Background()
	rc, err := toolsnap.ResolveRepoContext(ctx, repoPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := toolsnap.OpenStore(ctx, rc, semDir)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := store.ListRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return refs
}

// recordStages installs the capture seam and returns the recorder.
func recordStages(t *testing.T) *[]string {
	t.Helper()
	var stages []string
	toolWindowCaptureSeam = func(stage string) { stages = append(stages, stage) }
	t.Cleanup(func() { toolWindowCaptureSeam = nil })
	return &stages
}

// A durable delta skips capture and diff work on retry.
func TestRetryWithPersistedDeltaSkipsRecompute(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-r", Provider: "claude-code",
		TurnID: "turn-1", CWD: w.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := handleToolStepStarted(ctx, "claude-code", startedEvent("sess-r", "toolu_r", w.repoPath), w.bh); err != nil {
		t.Fatal(err)
	}
	reg, err := toolsnap.OpenRegistry(w.semDir)
	if err != nil {
		t.Fatal(err)
	}
	// A persisted delta requires its event row.
	if _, err := broker.WriteEventsToRepo(ctx, w.repoPath, []broker.RawEvent{bashRawEvent("evt-r", "toolu_r", "sess-r")}, nil); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("publication failed after delta persisted")
	_, err = reg.Complete(ctx, toolsnap.ToolKey{
		RepositoryID: w.repoID, Provider: "claude_code",
		SessionID: "sess-r", TurnID: "turn-1", ToolUseID: "toolu_r",
	}, toolsnap.CompletionInfo{EventID: "evt-r", At: 500}, nil,
		func(_ []toolsnap.PendingToolSnapshot, _ *toolsnap.GroupFinal, _ bool, _ func() error) (toolsnap.FinalizeResult, error) {
			return toolsnap.FinalizeResult{Final: toolsnap.GroupFinal{
				PostTreeHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				DeltaHash:    "persisted-delta", CapturedAt: 500,
			}}, boom
		})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}

	// Later changes must not enter the durable delta.
	if err := os.WriteFile(filepath.Join(w.repoPath, "later.txt"), []byte("after failure\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stages := recordStages(t)
	post := postBashEvent("sess-r", "toolu_r", w.repoPath, "retry")
	if windowHandled != completeToolWindow(ctx, "claude-code", post, w.bh, nil, []broker.RawEvent{bashRawEvent("evt-r", "toolu_r", "sess-r")}) {
		t.Fatal("retry not handled")
	}
	if len(*stages) != 0 {
		t.Fatalf("forbidden recompute stages ran: %v", *stages)
	}
	if wins := windowsIn(t, w.semDir); len(wins) != 0 {
		t.Fatalf("group not closed: %+v", wins)
	}
	links := linksIn(t, w.semDir)
	if len(links) != 1 || links[0].EventID != "evt-r" || links[0].Hash != "persisted-delta" {
		t.Fatalf("links = %+v, want evt-r on the persisted delta", links)
	}
}

func TestMissingPreSnapshotPersistsLinkedPartial(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-mp", Provider: "claude-code",
		TurnID: "turn-1", CWD: w.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// No pre hook ran for this tool use.
	evtMP := strings.Repeat("ab", 32)
	post := postBashEvent("sess-mp", "toolu_mp", w.repoPath, "cmd")
	post.Timestamp = 5000
	events := []broker.RawEvent{bashRawEvent(evtMP, "toolu_mp", "sess-mp")}
	if windowHandled != completeToolWindow(ctx, "claude-code", post, w.bh, nil, events) {
		t.Fatal("missing-pre partial not handled")
	}

	deltas := findDeltas(t, w.semDir)
	if len(deltas) != 1 || deltas[0].Status != "partial" || deltas[0].Reason != "pre_snapshot_missing" {
		t.Fatalf("deltas = %+v, want one pre_snapshot_missing partial", deltas)
	}
	links := linksIn(t, w.semDir)
	if len(links) != 1 || links[0].EventID != evtMP || links[0].Kind != "tool_delta" ||
		links[0].GroupID != "pre_snapshot_missing:"+evtMP {
		t.Fatalf("links = %+v, want the partial linked to the closing event", links)
	}
	h, err := sqlstore.Open(ctx, filepath.Join(w.semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := h.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM agent_events WHERE event_id = ?", evtMP).Scan(&count); err != nil {
		t.Fatal(err)
	}
	_ = sqlstore.Close(h)
	if count != 1 {
		t.Fatalf("closing event rows = %d, want 1", count)
	}
	// The durable link consumed the pending record.
	reg, err := toolsnap.OpenRegistry(w.semDir)
	if err != nil {
		t.Fatal(err)
	}
	if recs, err := reg.PendingPartialRecords(); err != nil || len(recs) != 0 {
		t.Fatalf("records after link = %+v err = %v, want consumed", recs, err)
	}

	// Reparse the duplicate with a later timestamp and active turn.
	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-mp", Provider: "claude-code",
		TurnID: "turn-2", CWD: w.repoPath, Timestamp: 2,
	}); err != nil {
		t.Fatal(err)
	}
	dup := postBashEvent("sess-mp", "toolu_mp", w.repoPath, "cmd")
	dup.Timestamp = 9000
	if windowHandled != completeToolWindow(ctx, "claude-code", dup, w.bh, nil, []broker.RawEvent{bashRawEvent(evtMP, "toolu_mp", "sess-mp")}) {
		t.Fatal("duplicate delivery not handled")
	}
	if deltas := findDeltas(t, w.semDir); len(deltas) != 1 || deltas[0].Window.StartedAt != 5000 {
		t.Fatalf("deltas after duplicate = %+v, want the original window only", deltas)
	}
	if links := linksIn(t, w.semDir); len(links) != 1 {
		t.Fatalf("links after duplicate = %+v, want one", links)
	}
}

func TestPartialReplayUsesRecordedFields(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-rp", Provider: "claude-code",
		TurnID: "turn-2", CWD: w.repoPath, Timestamp: 2,
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate a recorded delivery that stopped before link creation.
	evtRP := strings.Repeat("cd", 32)
	reg, err := toolsnap.OpenRegistry(w.semDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.LoadOrRecordPendingPartial(toolsnap.PendingPartialRecord{
		Key: toolsnap.ToolKey{
			RepositoryID: w.repoID, Provider: "claude_code",
			SessionID: "sess-rp", TurnID: "turn-1", ToolUseID: "toolu_rp",
		},
		EventID: evtRP, Reason: "pre_snapshot_missing",
		ToolName: "Bash", CommandSummary: "original-cmd", Timestamp: 5000,
	}); err != nil {
		t.Fatal(err)
	}

	// Reparse with different values; the record remains authoritative.
	post := postBashEvent("sess-rp", "toolu_rp", w.repoPath, "reparsed-cmd")
	post.Timestamp = 9000
	if windowHandled != completeToolWindow(ctx, "claude-code", post, w.bh, nil, []broker.RawEvent{bashRawEvent(evtRP, "toolu_rp", "sess-rp")}) {
		t.Fatal("replay not handled")
	}

	deltas := findDeltas(t, w.semDir)
	if len(deltas) != 1 {
		t.Fatalf("deltas = %d, want one", len(deltas))
	}
	d := deltas[0]
	if d.Window.StartedAt != 5000 || d.Actors[0].TurnID != "turn-1" ||
		d.ToolUses[0].CommandSummary != "original-cmd" {
		t.Fatalf("delta = %+v, want the recorded fields, not the reparsed payload", d)
	}
	if links := linksIn(t, w.semDir); len(links) != 1 || links[0].EventID != evtRP {
		t.Fatalf("links = %+v", links)
	}
	if recs, err := reg.PendingPartialRecords(); err != nil || len(recs) != 0 {
		t.Fatalf("records after replay = %+v err = %v, want consumed", recs, err)
	}
}

func TestConflictingEvidenceLinkBlocksClosure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-cf", Provider: "claude-code",
		TurnID: "turn-1", CWD: w.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := handleToolStepStarted(ctx, "claude-code", startedEvent("sess-cf", "toolu_cf", w.repoPath), w.bh); err != nil {
		t.Fatal(err)
	}
	wins := windowsIn(t, w.semDir)
	if len(wins) != 1 {
		t.Fatal("window missing")
	}

	// Seed a conflicting link for the same event and group.
	if _, err := broker.WriteEventsToRepo(ctx, w.repoPath, []broker.RawEvent{bashRawEvent("evt-cf", "toolu_cf", "sess-cf")}, nil); err != nil {
		t.Fatal(err)
	}
	if err := broker.WriteEvidenceLinksToRepo(ctx, w.repoPath, []broker.EvidenceLink{{
		EventID: "evt-cf", EvidenceKind: "tool_delta",
		EvidenceHash: "foreign-hash", GroupID: wins[0].GroupID, CreatedAt: 2,
	}}); err != nil {
		t.Fatal(err)
	}

	post := postBashEvent("sess-cf", "toolu_cf", w.repoPath, "cmd")
	if windowHandled == completeToolWindow(ctx, "claude-code", post, w.bh, nil, []broker.RawEvent{bashRawEvent("evt-cf", "toolu_cf", "sess-cf")}) {
		t.Fatal("conflicting link accepted as closure")
	}

	// The conflict retains the group, link, and refs for repair.
	reg, err := toolsnap.OpenRegistry(w.semDir)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := reg.PendingFinalizations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Final == nil || pending[0].Final.DeltaHash == "" {
		t.Fatalf("pending = %+v, want retained group with delta identity", pending)
	}
	links := linksIn(t, w.semDir)
	if len(links) != 1 || links[0].Hash != "foreign-hash" {
		t.Fatalf("links = %+v, want the foreign link preserved", links)
	}
	if refs := snapshotRefsIn(t, w.repoPath, w.semDir); len(refs) == 0 {
		t.Fatal("refs released despite blocked closure")
	}
}

// A post ref prevents workspace recapture during recovery.
func TestPostRefPreventsWorkspaceRecapture(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-p", Provider: "claude-code",
		TurnID: "turn-1", CWD: w.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := handleToolStepStarted(ctx, "claude-code", startedEvent("sess-p", "toolu_p", w.repoPath), w.bh); err != nil {
		t.Fatal(err)
	}
	wins := windowsIn(t, w.semDir)
	if len(wins) != 1 {
		t.Fatal("window missing")
	}

	// Preserve the tool's post state before simulating failed publication.
	if err := os.WriteFile(filepath.Join(w.repoPath, "tool.txt"), []byte("tool change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rc, err := toolsnap.ResolveRepoContext(ctx, w.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := toolsnap.OpenStore(ctx, rc, w.semDir)
	if err != nil {
		t.Fatal(err)
	}
	postSnap, err := store.CaptureBefore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureRef(ctx, toolsnap.GroupPostRef(store.WorktreeID(), wins[0].GroupID), postSnap.TreeHash); err != nil {
		t.Fatal(err)
	}

	// This change must not enter the recovered delta.
	if err := os.WriteFile(filepath.Join(w.repoPath, "unrelated.txt"), []byte("after crash\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stages := recordStages(t)
	post := postBashEvent("sess-p", "toolu_p", w.repoPath, "crashed-tool")
	if windowHandled != completeToolWindow(ctx, "claude-code", post, w.bh, nil, []broker.RawEvent{bashRawEvent("evt-p", "toolu_p", "sess-p")}) {
		t.Fatal("completion not handled")
	}
	for _, s := range *stages {
		if s == "capture_after" {
			t.Fatal("workspace recaptured despite published post ref")
		}
	}
	deltas := findDeltas(t, w.semDir)
	if len(deltas) != 1 {
		t.Fatalf("deltas = %d", len(deltas))
	}
	paths := map[string]bool{}
	for _, f := range deltas[0].Files {
		paths[f.Path] = true
	}
	if !paths["tool.txt"] || paths["unrelated.txt"] {
		t.Fatalf("delta paths = %v, want the ref state only", paths)
	}
}

// A closed key rejects duplicate pre hooks.
func TestDuplicatePreAfterClosureIsNoOp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-dp", Provider: "claude-code",
		TurnID: "turn-1", CWD: w.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := handleToolStepStarted(ctx, "claude-code", startedEvent("sess-dp", "toolu_dp", w.repoPath), w.bh); err != nil {
		t.Fatal(err)
	}
	post := postBashEvent("sess-dp", "toolu_dp", w.repoPath, "cmd")
	if windowHandled != completeToolWindow(ctx, "claude-code", post, w.bh, nil, []broker.RawEvent{bashRawEvent("evt-dp", "toolu_dp", "sess-dp")}) {
		t.Fatal("closure not handled")
	}

	if err := handleToolStepStarted(ctx, "claude-code", startedEvent("sess-dp", "toolu_dp", w.repoPath), w.bh); err != nil {
		t.Fatal(err)
	}
	if wins := windowsIn(t, w.semDir); len(wins) != 0 {
		t.Fatalf("closed key reopened a window: %+v", wins)
	}
}

// Event-write failure leaves no resumable post identity.
func TestEventWriteFailureLeavesNoResumableIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-ef", Provider: "claude-code",
		TurnID: "turn-1", CWD: w.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := handleToolStepStarted(ctx, "claude-code", startedEvent("sess-ef", "toolu_ef", w.repoPath), w.bh); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.repoPath, "gen.txt"), []byte("tool output\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Disable the repository to reject the event write.
	enabledPath := filepath.Join(w.semDir, "enabled")
	if err := os.Remove(enabledPath); err != nil {
		t.Fatal(err)
	}
	post := postBashEvent("sess-ef", "toolu_ef", w.repoPath, "cmd")
	if windowHandled == completeToolWindow(ctx, "claude-code", post, w.bh, nil, []broker.RawEvent{bashRawEvent("evt-ef", "toolu_ef", "sess-ef")}) {
		t.Fatal("failed event write reported handled")
	}
	if err := os.WriteFile(enabledPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// The intent remains recoverable.
	receipts, err := os.ReadDir(filepath.Join(w.semDir, "tool-windows", "receipts"))
	if err != nil || len(receipts) != 1 {
		t.Fatalf("intent receipt before event write: entries=%v err=%v", receipts, err)
	}

	// No post identity may survive the failed event write.
	if deltas := findDeltas(t, w.semDir); len(deltas) != 0 {
		t.Fatalf("deltas after failed event write: %+v", deltas)
	}
	reg, err := toolsnap.OpenRegistry(w.semDir)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := reg.PendingFinalizations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Final != nil {
		t.Fatalf("pending = %+v, want the group retained with no identity", pending)
	}

	// Later changes must not enter the retry.
	if err := os.WriteFile(filepath.Join(w.repoPath, "later.txt"), []byte("after the window\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The retry emits partial evidence without another snapshot.
	stages := recordStages(t)
	if windowHandled != completeToolWindow(ctx, "claude-code", post, w.bh, nil, []broker.RawEvent{bashRawEvent("evt-ef", "toolu_ef", "sess-ef")}) {
		t.Fatal("retry not handled")
	}
	for _, s := range *stages {
		if s == "capture_after" {
			t.Fatal("workspace recaptured after recorded completion")
		}
	}
	deltas := findDeltas(t, w.semDir)
	if len(deltas) != 1 {
		t.Fatalf("deltas after retry = %d, want one", len(deltas))
	}
	if deltas[0].Status != "partial" || deltas[0].Reason != toolsnap.ReasonPostSnapshotLost {
		t.Fatalf("delta = status %q reason %q, want terminal partial", deltas[0].Status, deltas[0].Reason)
	}
	for _, f := range deltas[0].Files {
		if f.Path == "later.txt" || f.Path == "gen.txt" {
			t.Fatalf("terminal partial carries workspace bytes: %+v", deltas[0].Files)
		}
	}
}

// newToolWindowWorldAt creates an enabled repository on an existing broker.
func newToolWindowWorldAt(t *testing.T, bh *broker.Handle, repoPath string) *toolWindowWorld {
	t.Helper()
	setToolWindowDeadline(t, 30*time.Second)
	setToolWindowRefReleaseWait(t, 30*time.Second)
	ctx := context.Background()
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repoPath, "init", "-q", "-b", "main")
	gitIn(t, repoPath, "config", "user.email", "t@example.com")
	gitIn(t, repoPath, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repoPath, "inner.txt"), []byte("inner\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repoPath, "add", ".")
	gitIn(t, repoPath, "commit", "-q", "-m", "init")

	semDir := filepath.Join(repoPath, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(semDir, "lineage.db")
	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	repoID := uuid.NewString()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: repoPath, CreatedAt: 1000, EnabledAt: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := sqlstore.Close(h); err != nil {
		t.Fatal(err)
	}
	if err := broker.Register(ctx, bh, repoPath, repoPath); err != nil {
		t.Fatal(err)
	}
	return &toolWindowWorld{repoPath: repoPath, semDir: semDir, repoID: repoID, bh: bh}
}
