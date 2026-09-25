package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/hooks/codex"
	"github.com/semanticash/cli/internal/store/blobs"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

func TestCaptureReadinessCrossRepoObservation(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	t.Setenv("SEMANTICA_PLAYBOOK", "0")
	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	a, b := newCommitWorld(t), newCommitWorld(t)
	for _, w := range []*commitWorld{a, b} {
		w.write(t, "base.txt", "base\n")
		w.git("add", ".")
		w.git("commit", "-m", "base")
	}
	bh, err := broker.Open(ctx, filepath.Join(home, "repos.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close(bh) })
	for _, w := range []*commitWorld{a, b} {
		if err := broker.Register(ctx, bh, w.dir, w.dir); err != nil {
			t.Fatal(err)
		}
	}
	global, err := blobs.NewStore(filepath.Join(home, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	dispatch := func(kind hooks.EventType) {
		t.Helper()
		input, err := json.Marshal(map[string]string{"command": "generate", "workdir": a.dir})
		if err != nil {
			t.Fatal(err)
		}
		event := &hooks.Event{Type: kind, SessionID: "cross-repo", ProviderTurnID: "turn",
			CWD: a.dir, Timestamp: time.Now().UnixMilli(), Prompt: "Update the other repository"}
		if kind == hooks.ToolStepStarted || kind == hooks.ToolStepCompleted {
			event.ToolName, event.ToolUseID = "Bash", "generate"
			event.ToolInput, event.ToolResponse = input, []byte(`{"exit_code":0}`)
		}
		if err := hooks.Dispatch(ctx, codex.New(), event, bh, global); err != nil {
			t.Fatal(err)
		}
	}
	dispatch(hooks.PromptSubmitted)
	dispatch(hooks.ToolStepStarted)
	b.write(t, "generated.txt", "unproven cross-repository change\n")
	dispatch(hooks.ToolStepCompleted)
	// Independent Write evidence must retain its AI attribution.
	b.write(t, "direct.txt", "directly captured edit\n")
	src := insertSource(t, b.h, b.repoID, "/direct")
	session := insertSessionWithProvider(t, b.h, b.repoID, src, "direct", "claude_code")
	insertEventWithPayload(t, b.h, b.bs, session, b.repoID, b.dir, time.Now().UnixMilli(), "direct.txt", "directly captured edit\n")
	dispatch(hooks.AgentCompleted)
	var observations, deltas int
	if err := b.h.DB.QueryRow("select count(*) from agent_events where event_source='turn_observation'").Scan(&observations); err != nil {
		t.Fatal(err)
	}
	if err := b.h.DB.QueryRow("select count(*) from agent_event_evidence_links where evidence_kind='tool_delta'").Scan(&deltas); err != nil {
		t.Fatal(err)
	}
	if observations != 1 || deltas != 0 {
		t.Fatalf("expected observation without destination tool capture: observations=%d deltas=%d", observations, deltas)
	}
	b.git("add", ".")
	b.git("commit", "-m", "cross-repository edits")
	sha := b.git("rev-parse", "HEAD")
	insertPendingLinked(t, b.h, b.repoID, "observed-checkpoint", sha, time.Now().UnixMilli())
	if err := NewWorkerService(nil).Run(ctx, WorkerInput{RepoRoot: b.dir, CheckpointID: "observed-checkpoint", CommitHash: sha}); err != nil {
		t.Fatal(err)
	}
	result, err := NewAttributionService().AttributeCommit(ctx, AttributionInput{RepoPath: b.dir, CommitHash: sha})
	if err != nil {
		t.Fatal(err)
	}
	if result.Capture == nil || result.Capture.Status != "incomplete" || result.HumanLines != 0 || result.UnattributedLines != 1 || result.AILines != 1 {
		t.Fatalf("observation lost uncertainty or direct AI evidence: %+v", result)
	}
	// Checkpoints saved by older binaries must receive the same protection.
	cp := readCheckpoint(t, b.h, "observed-checkpoint")
	recorded, err := readCheckpointCapture(ctx, b.h, cp)
	if err != nil {
		t.Fatal(err)
	}
	recorded.Result = CaptureReadiness{Status: "complete"}
	if err := saveCheckpointCapture(ctx, b.h, recorded); err != nil {
		t.Fatal(err)
	}
	again, err := NewAttributionService().AttributeCommit(ctx, AttributionInput{RepoPath: b.dir, CommitHash: sha})
	if err != nil || again.HumanLines != 0 || again.UnattributedLines != 1 || again.AILines != 1 {
		t.Fatalf("saved complete status concealed uncertainty: %+v, %v", again, err)
	}
}

func TestTurnObservationGapsRespectCheckpointScope(t *testing.T) {
	_, h, repoID := setupQueueRepo(t)
	ctx := context.Background()
	src := insertSource(t, h, repoID, "/session")
	session := insertSessionWithProvider(t, h, repoID, src, "session", "codex")
	add := func(id string, ts int64, kind, source string) {
		t.Helper()
		if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
			EventID: id, SessionID: session, RepositoryID: repoID, Ts: ts, Kind: kind, EventSource: source,
		}); err != nil {
			t.Fatal(err)
		}
	}
	add("previous", 1000, "context", "turn_observation")
	insertPendingLinked(t, h, repoID, "previous-cp", "previous-sha", 1000)
	previous := readCheckpoint(t, h, "previous-cp")
	add("lower-tie", 1000, "context", "turn_observation")
	add("inside", 1500, "context", "turn_observation")
	add("ordinary-context", 1500, "context", "hook")
	add("wrong-kind", 1500, "assistant", "turn_observation")
	add("upper-tie", 2000, "context", "turn_observation")
	insertPendingLinked(t, h, repoID, "current-cp", "current-sha", 2000)
	cp := readCheckpoint(t, h, "current-cp")
	add("after-cursor", 2000, "context", "turn_observation")
	add("later", 2001, "context", "turn_observation")
	for _, tc := range []struct {
		name string
		win  eventWindow
		repo string
		want []string
	}{
		{"cursor", windowBetween(&previous, cp), repoID, []string{"lower-tie", "inside", "upper-tie"}},
		{"timestamps", tsWindow(1000, 2000), repoID, []string{"inside", "upper-tie", "after-cursor"}},
		{"other-repository", windowBetween(&previous, cp), "other-repo", nil},
		{"outside-window", tsWindow(2001, 3000), repoID, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkpoint := cp
			checkpoint.RepositoryID = tc.repo
			gaps, err := turnObservationGaps(ctx, h, checkpoint, tc.win)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, gap := range gaps {
				if gap.Reason != "turn_observation_authorship_unknown" {
					t.Fatalf("unexpected gap: %+v", gap)
				}
				ids = append(ids, gap.GroupID)
			}
			if !slices.Equal(ids, tc.want) {
				t.Fatalf("gap events=%v, want %v", ids, tc.want)
			}
		})
	}
}
