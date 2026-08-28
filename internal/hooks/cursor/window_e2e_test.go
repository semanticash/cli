package cursor

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
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/toolsnap"
)

// Cursor Shell hooks share a tool-use ID across pre and post events.
func TestParseHookEvent_PreToolUseShellPairsWithPost(t *testing.T) {
	p := &Provider{}
	parse := func(hookName, payload string) *hooks.Event {
		t.Helper()
		ev, err := p.ParseHookEvent(context.Background(), hookName, strings.NewReader(payload))
		if err != nil || ev == nil {
			t.Fatalf("parse %s: ev=%v err=%v", hookName, ev, err)
		}
		return ev
	}

	body := `{"conversation_id":"c","cwd":"/w","tool_name":"Shell","tool_use_id":"call 7"}`
	pre := parse("pre-tool-use", body)
	post := parse("post-tool-use", body)

	if pre.Type != hooks.ToolStepStarted || pre.ToolName != "Bash" {
		t.Fatalf("pre = {type:%v tool:%q}, want ToolStepStarted/Bash", pre.Type, pre.ToolName)
	}
	if post.Type != hooks.ToolStepCompleted || post.ToolName != "Bash" {
		t.Fatalf("post = {type:%v tool:%q}, want ToolStepCompleted/Bash", post.Type, post.ToolName)
	}
	if pre.ToolUseID == "" || pre.ToolUseID != post.ToolUseID {
		t.Fatalf("tool_use_id pre=%q post=%q, want equal and non-empty", pre.ToolUseID, post.ToolUseID)
	}
}

// A paired Cursor shell window produces a complete delta and evidence link.
func TestParseAndDispatch_CursorShellWindowProducesCompleteDelta(t *testing.T) {
	// Real Git and SQLite work can exceed the production deadline on slower CI
	// runners.
	t.Cleanup(hooks.SetToolWindowDeadlineForTest(30 * time.Second))

	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	ctx := context.Background()

	repoPath, semDir := newCursorRepoWorld(t)

	bh, err := broker.Open(ctx, filepath.Join(home, "repos.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = broker.Close(bh) }()
	if err := broker.Register(ctx, bh, repoPath, repoPath); err != nil {
		t.Fatal(err)
	}

	objDir, err := broker.GlobalObjectsDir()
	if err != nil {
		t.Fatal(err)
	}
	blobStore, err := blobs.NewStore(objDir)
	if err != nil {
		t.Fatal(err)
	}

	const sessionID = "conv-window"
	if err := hooks.SaveCaptureState(&hooks.CaptureState{
		SessionID: sessionID, Provider: providerName,
		TurnID: "turn-1", CWD: repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}

	p := &Provider{}
	dispatch := func(hookName, payload string) {
		t.Helper()
		ev, err := p.ParseHookEvent(ctx, hookName, strings.NewReader(payload))
		if err != nil || ev == nil {
			t.Fatalf("parse %s: ev=%v err=%v", hookName, ev, err)
		}
		if err := hooks.Dispatch(ctx, p, ev, bh, blobStore); err != nil {
			t.Fatalf("dispatch %s: %v", hookName, err)
		}
	}

	cwd := jsonQuote(repoPath)
	pre := `{"conversation_id":"` + sessionID + `","cwd":` + cwd +
		`,"tool_name":"Shell","tool_use_id":"call_window_1","tool_input":{"command":"make gen"}}`
	dispatch("pre-tool-use", pre)

	if err := os.WriteFile(filepath.Join(repoPath, "gen.txt"), []byte("generated line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	post := `{"conversation_id":"` + sessionID + `","cwd":` + cwd +
		`,"tool_name":"Shell","tool_use_id":"call_window_1","tool_input":{"command":"make gen"},"tool_output":"ok"}`
	dispatch("post-tool-use", post)

	deltas := cursorDeltasIn(t, semDir)
	if len(deltas) != 1 || deltas[0].Status != "complete" {
		t.Fatalf("deltas = %+v, want one complete", deltas)
	}
	if len(deltas[0].Actors) != 1 || deltas[0].Actors[0].Provider != providerName {
		t.Fatalf("actors = %+v, want %s", deltas[0].Actors, providerName)
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

	links := cursorLinksIn(t, semDir)
	if len(links) != 1 || links[0].kind != "tool_delta" ||
		links[0].groupID == "" || strings.Contains(links[0].groupID, ":") {
		t.Fatalf("links = %+v, want one complete (unprefixed) tool_delta link", links)
	}
	if links[0].eventID == "" || links[0].hash == "" {
		t.Fatalf("link missing event/hash: %+v", links[0])
	}
}

func jsonQuote(s string) string {
	b := strings.ReplaceAll(s, `\`, `\\`)
	b = strings.ReplaceAll(b, `"`, `\"`)
	return `"` + b + `"`
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// newCursorRepoWorld creates an enabled Git repository with local state.
func newCursorRepoWorld(t *testing.T) (repoPath, semDir string) {
	t.Helper()
	ctx := context.Background()

	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repoPath = filepath.Join(base, "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repoPath, "init", "-q", "-b", "main")
	gitRun(t, repoPath, "config", "user.email", "t@example.com")
	gitRun(t, repoPath, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repoPath, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repoPath, "add", ".")
	gitRun(t, repoPath, "commit", "-q", "-m", "init")

	semDir = filepath.Join(repoPath, ".semantica")
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
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: uuid.NewString(), RootPath: repoPath, CreatedAt: 1000, EnabledAt: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := sqlstore.Close(h); err != nil {
		t.Fatal(err)
	}
	return repoPath, semDir
}

func cursorDeltasIn(t *testing.T, semDir string) []*toolsnap.Delta {
	t.Helper()
	objects := filepath.Join(semDir, "objects")
	bs, err := blobs.NewStore(objects)
	if err != nil {
		t.Fatal(err)
	}
	var deltas []*toolsnap.Delta
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

type cursorLinkRow struct{ eventID, kind, hash, groupID string }

func cursorLinksIn(t *testing.T, semDir string) []cursorLinkRow {
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
	var out []cursorLinkRow
	for rows.Next() {
		var r cursorLinkRow
		if err := rows.Scan(&r.eventID, &r.kind, &r.hash, &r.groupID); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
