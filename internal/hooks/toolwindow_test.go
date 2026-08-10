package hooks

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/platform"
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

// A held registry lock produces only the activity-log diagnostic.
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
	lockPath := filepath.Join(w.semDir, "tool-windows", "registry.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	if err := platform.LockFile(holder); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := handleToolStepStarted(ctx, "claude-code", startedEvent("sess-l", "toolu_l", w.repoPath), w.bh); err != nil {
		t.Fatalf("handler: %v", err)
	}

	logBytes, err := os.ReadFile(filepath.Join(w.semDir, "activity.log"))
	if err != nil {
		t.Fatalf("activity log: %v", err)
	}
	if !strings.Contains(string(logBytes), toolsnap.ReasonLockTimeout) {
		t.Fatalf("activity log = %q, want %s diagnostic", logBytes, toolsnap.ReasonLockTimeout)
	}
	if err := platform.UnlockFile(holder); err != nil {
		t.Fatal(err)
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

// newToolWindowWorldAt creates an enabled repository on an existing broker.
func newToolWindowWorldAt(t *testing.T, bh *broker.Handle, repoPath string) *toolWindowWorld {
	t.Helper()
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
