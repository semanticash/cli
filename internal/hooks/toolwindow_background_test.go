package hooks

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/broker"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	"github.com/semanticash/cli/internal/toolsnap"
)

// Detached Bash commands do not open windows. Ordinary Bash commands do.
func TestBackgroundBashPreSkipsWindow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-bg", Provider: "claude-code",
		TurnID: "turn-1", CWD: w.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}

	bg := startedEvent("sess-bg", "toolu_bg", w.repoPath)
	bg.ToolInput = []byte(`{"command":"sleep 20 && echo hi > bg.txt","description":"bg","run_in_background":true}`)
	if err := handleToolStepStarted(ctx, "claude-code", bg, w.bh); err != nil {
		t.Fatal(err)
	}
	if wins := windowsIn(t, w.semDir); len(wins) != 0 {
		t.Fatalf("background command opened a window: %+v", wins)
	}

	// An ordinary Bash command still opens a window.
	norm := startedEvent("sess-bg", "toolu_norm", w.repoPath)
	norm.ToolInput = []byte(`{"command":"echo hi"}`)
	if err := handleToolStepStarted(ctx, "claude-code", norm, w.bh); err != nil {
		t.Fatal(err)
	}
	if wins := windowsIn(t, w.semDir); len(wins) != 1 {
		t.Fatalf("ordinary Bash windows = %+v, want exactly one", wins)
	}
}

// A detached Bash command records a fileless partial and preserves its event.
func TestBackgroundBashPostRecordsPartial(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-bg2", Provider: "claude-code",
		TurnID: "turn-1", CWD: w.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}

	evt := strings.Repeat("cd", 32)
	post := postBashEvent("sess-bg2", "toolu_bg2", w.repoPath, "sleep 20 && echo hi > bg.txt")
	post.ToolInput = []byte(`{"command":"sleep 20 && echo hi > bg.txt","description":"bg","run_in_background":true}`)
	post.Timestamp = 5000
	events := []broker.RawEvent{bashRawEvent(evt, "toolu_bg2", "sess-bg2")}
	if windowHandled != completeToolWindow(ctx, "claude-code", post, w.bh, nil, events) {
		t.Fatal("background partial not handled")
	}

	deltas := findDeltas(t, w.semDir)
	if len(deltas) != 1 || deltas[0].Status != "partial" || deltas[0].Reason != toolsnap.ReasonBackgroundCommand {
		t.Fatalf("deltas = %+v, want one background_command partial", deltas)
	}
	if len(deltas[0].Files) != 0 {
		t.Fatalf("background partial carries file attribution: %+v", deltas[0].Files)
	}
	links := linksIn(t, w.semDir)
	if len(links) != 1 || links[0].EventID != evt || links[0].Kind != "tool_delta" ||
		links[0].GroupID != "background_command:"+evt {
		t.Fatalf("links = %+v, want the partial linked to the closing event", links)
	}
	if wins := windowsIn(t, w.semDir); len(wins) != 0 {
		t.Fatalf("background command left a window open: %+v", wins)
	}

	// Preserve the Bash event for audit. Scorer tests cover attribution suppression.
	h, err := sqlstore.Open(ctx, filepath.Join(w.semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	var count int
	if err := h.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM agent_events WHERE event_id = ?", evt).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("background Bash event rows = %d, want 1 (event preserved for audit)", count)
	}
}
