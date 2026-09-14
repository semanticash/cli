package service

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/hooks/codex"
	"github.com/semanticash/cli/internal/store/blobs"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/toolsnap"
)

func captureKey(id string) toolsnap.ToolKey {
	return toolsnap.ToolKey{RepositoryID: "repo", Provider: "codex", SessionID: "session", TurnID: "turn", ToolUseID: id}
}

func TestCaptureReadinessFreezesRelevantWindowsAndDeadline(t *testing.T) {
	cp := sqldb.Checkpoint{CheckpointID: "cp", RepositoryID: "repo", CreatedAt: 100}
	snap := toolsnap.RegistrySnapshot{Windows: []toolsnap.PendingToolSnapshot{
		{Key: captureKey("old-active"), StartedAt: 10, Status: "active", GroupID: "g"},
		{Key: captureKey("done"), StartedAt: 60, CompletedAt: 90, Status: "complete", GroupID: "g"},
		{Key: captureKey("previous"), StartedAt: 10, CompletedAt: 40, Status: "complete", GroupID: "old"},
		{Key: captureKey("later"), StartedAt: 101, Status: "active", GroupID: "g"},
	}}
	now := time.UnixMilli(200)
	r := freezeCheckpointCapture(cp, tsWindow(50, 100), snap, nil, now)
	if len(r.Members) != 2 {
		t.Fatalf("members = %+v", r.Members)
	}
	deadline := r.Deadline
	evaluateCheckpointCapture(r, snap, nil, nil, nil, now)
	if r.Result.Status != "pending" {
		t.Fatalf("status = %s", r.Result.Status)
	}
	snap.Windows = append(snap.Windows, toolsnap.PendingToolSnapshot{Key: captureKey("even-later"), StartedAt: 300, Status: "active"})
	evaluateCheckpointCapture(r, snap, nil, nil, nil, time.UnixMilli(deadline))
	if r.Result.Status != "incomplete" || len(r.Result.Gaps) != 2 || len(r.Members) != 2 || r.Deadline != deadline {
		t.Fatalf("later commands changed frozen scope: %+v", r)
	}
}

func TestCaptureReadinessRequiresDurableEvidenceNotDisappearance(t *testing.T) {
	cp := sqldb.Checkpoint{CheckpointID: "cp", RepositoryID: "repo", CreatedAt: 100}
	snap := toolsnap.RegistrySnapshot{Windows: []toolsnap.PendingToolSnapshot{{Key: captureKey("a"), StartedAt: 10, Status: "active", GroupID: "g"}}}
	r := freezeCheckpointCapture(cp, tsWindow(0, 100), snap, nil, time.Now())
	evaluateCheckpointCapture(r, toolsnap.RegistrySnapshot{}, nil, nil, nil, time.UnixMilli(r.Deadline))
	if r.Result.Status != "incomplete" || r.Result.Gaps[0].Reason != "completion_evidence_missing" {
		t.Fatalf("disappeared window was treated as captured: %+v", r.Result)
	}
	evaluateCheckpointCapture(r, toolsnap.RegistrySnapshot{}, nil, map[toolsnap.ToolKey]captureProof{captureKey("a"): {GroupID: "wrong-group"}}, nil, time.Now())
	if r.Result.Status != "incomplete" {
		t.Fatal("unrelated group settled the window")
	}
	evaluateCheckpointCapture(r, toolsnap.RegistrySnapshot{}, nil, map[toolsnap.ToolKey]captureProof{captureKey("a"): {GroupID: "g"}}, nil, time.Now())
	if r.Result.Status != "complete" {
		t.Fatalf("verified evidence did not settle window: %+v", r.Result)
	}
}

func TestCaptureWaitDoesNotConsumeFailureAttempts(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	dir, h, repoID := setupQueueRepo(t)
	insertPendingLinked(t, h, repoID, "capture-wait", "c1", 100)
	orig := workerProcess
	workerProcess = func(_ *WorkerService, _ context.Context, _ WorkerInput) error {
		return &captureNotSettled{At: time.Now().Add(time.Minute)}
	}
	t.Cleanup(func() { workerProcess = orig })
	for range retryMaxAttempts + 2 {
		_, err := h.DB.Exec("update checkpoints set next_attempt_at = 0 where checkpoint_id = 'capture-wait'")
		if err != nil {
			t.Fatal(err)
		}
		err = NewWorkerService(nil).Run(context.Background(), WorkerInput{CheckpointID: "capture-wait", RepoRoot: dir})
		var scheduled *ErrRetryScheduled
		if !errors.As(err, &scheduled) {
			t.Fatalf("wait = %v", err)
		}
		cp := readCheckpoint(t, h, "capture-wait")
		if cp.Status != "pending" || cp.AttemptCount != 0 || cp.LeaseOwner.Valid {
			t.Fatalf("capture waiting spent failure budget: %+v", cp)
		}
	}
}

func TestCaptureReadinessMissingCompletionThroughDispatchAndWorker(t *testing.T) {
	for _, missing := range []bool{false, true} {
		name := "completed"
		if missing {
			name = "missing_terminal_hook"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			w := newCommitWorld(t)
			home := t.TempDir()
			t.Setenv("SEMANTICA_HOME", home)
			t.Setenv("SEMANTICA_ATTRIBUTION_V2", "1")
			t.Setenv("SEMANTICA_PLAYBOOK", "0")
			w.write(t, "base.txt", "base\n")
			w.git("add", ".")
			w.git("commit", "-m", "base")
			bh, err := broker.Open(ctx, filepath.Join(home, "repos.json"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = broker.Close(bh) })
			if err := broker.Register(ctx, bh, w.dir, w.dir); err != nil {
				t.Fatal(err)
			}
			global, err := blobs.NewStore(filepath.Join(home, "objects"))
			if err != nil {
				t.Fatal(err)
			}
			if err := hooks.SaveCaptureState(&hooks.CaptureState{SessionID: "readiness-session", Provider: "codex", TurnID: "readiness-turn", CWD: w.dir, Timestamp: time.Now().UnixMilli()}); err != nil {
				t.Fatal(err)
			}
			dispatch := func(id string, post bool) {
				t.Helper()
				kind := hooks.ToolStepStarted
				if post {
					kind = hooks.ToolStepCompleted
				}
				e := &hooks.Event{Type: kind, SessionID: "readiness-session", TurnID: "readiness-turn", ToolUseID: id,
					ToolName: "Bash", CWD: w.dir, Timestamp: time.Now().UnixMilli(),
					ToolInput: []byte(`{"command":"generate"}`), ToolResponse: []byte(`{"exit_code":0,"stdout":""}`)}
				if err := hooks.Dispatch(ctx, codex.New(), e, bh, global); err != nil {
					t.Fatal(err)
				}
			}
			dispatch("a", false)
			// Process exit is independent of delivery of the provider's post hook.
			cmd := exec.Command("git", "--version")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
			if !missing {
				dispatch("a", true)
			}
			for _, id := range []string{"b", "c"} {
				dispatch(id, false)
				w.write(t, id+".txt", "generated content for "+id+"\n")
				dispatch(id, true)
			}
			snap, err := toolsnap.InspectRegistry(filepath.Join(w.dir, ".semantica"))
			if err != nil {
				t.Fatal(err)
			}
			if missing && len(snap.Windows) != 3 {
				t.Fatalf("expected active A blocking completed B/C, got %+v", snap.Windows)
			}
			if !missing && len(snap.Windows) != 0 {
				t.Fatalf("normal post hooks did not close windows: %+v", snap.Windows)
			}
			w.git("add", ".")
			w.git("commit", "-m", "generated files")
			sha := w.git("rev-parse", "HEAD")
			cpID := "capture-checkpoint"
			insertPendingLinked(t, w.h, w.repoID, cpID, sha, time.Now().UnixMilli())
			service := NewWorkerService(nil)
			input := WorkerInput{RepoRoot: w.dir, CheckpointID: cpID, CommitHash: sha}
			err = service.Run(ctx, input)
			if missing {
				var wait *ErrRetryScheduled
				if !errors.As(err, &wait) {
					t.Fatalf("attribution ran before capture settled: %v", err)
				}
				cp := readCheckpoint(t, w.h, cpID)
				if cp.Status != "pending" || cp.AttemptCount != 0 {
					t.Fatalf("checkpoint should await capture: %+v", cp)
				}
				r, readErr := readCheckpointCapture(ctx, w.h, cp)
				if readErr != nil {
					t.Fatal(readErr)
				}
				// Expire the frozen deadline without waiting in the test.
				r.Deadline = time.Now().Add(-time.Second).UnixMilli()
				if err := saveCheckpointCapture(ctx, w.h, r); err != nil {
					t.Fatal(err)
				}
				if _, err := w.h.DB.Exec("update checkpoints set next_attempt_at = 0 where checkpoint_id = ?", cpID); err != nil {
					t.Fatal(err)
				}
				dispatch("later", false)
				w.write(t, "later.txt", "not part of the commit\n")
				err = service.Run(ctx, input)
			}
			if err != nil {
				t.Fatal(err)
			}
			cp := readCheckpoint(t, w.h, cpID)
			if cp.Status != "complete" {
				t.Fatalf("checkpoint did not finish: %+v", cp)
			}
			result, err := NewAttributionService().AttributeCommit(ctx, AttributionInput{RepoPath: w.dir, CommitHash: sha})
			if err != nil {
				t.Fatal(err)
			}
			if result.Capture == nil {
				t.Fatal("capture status was not persisted")
			}
			explanation, err := NewExplainService().Explain(ctx, ExplainInput{RepoPath: w.dir, Ref: sha})
			if err != nil {
				t.Fatal(err)
			}
			if explanation.Capture == nil || explanation.Capture.Status != result.Capture.Status || explanation.UnattributedLines != result.UnattributedLines {
				t.Fatalf("Explain lost capture uncertainty: %+v", explanation)
			}
			if explanation.LinesAdded != explanation.AILines+explanation.HumanLines+explanation.UnattributedLines {
				t.Fatalf("Explain lost unmatched lines: %+v", explanation)
			}
			for _, file := range explanation.TopFiles {
				if file.TotalLines != file.AILines+file.HumanLines+file.UnattributedLines {
					t.Fatalf("Explain file lost unmatched lines: %+v", file)
				}
			}
			status, err := NewStatusService().Status(ctx, StatusInput{RepoPath: w.dir})
			if err != nil {
				t.Fatal(err)
			}
			if missing {
				if len(status.AITrend) != 0 || explanation.FilesHumanOnly != 0 || explanation.FilesUnattributed == 0 {
					t.Fatalf("incomplete capture presented as ordinary attribution: trend=%v explain=%+v", status.AITrend, explanation)
				}
				if result.Capture.Status != "incomplete" || result.HumanLines != 0 || result.UnattributedLines == 0 {
					t.Fatalf("missing capture became human attribution: %+v", result)
				}
				audit := EvaluateAuditReadiness(ctx, w.h, filepath.Join(w.dir, ".semantica"), cp, PolicyLocal)
				if audit.AuditReady || audit.Attribution.State != ReadinessUnknown {
					t.Fatalf("partial attribution reported audit-ready: %+v", audit)
				}
				var published int
				if err := w.h.DB.QueryRow("select count(*) from agent_event_evidence_links").Scan(&published); err != nil {
					t.Fatal(err)
				}
				if published != 0 {
					t.Fatal("missing post hook unexpectedly produced delta evidence")
				}
				for _, gap := range result.Capture.Gaps {
					if gap.Key != nil && gap.Key.ToolUseID == "later" {
						t.Fatal("later command extended frozen checkpoint scope")
					}
				}
				payload := buildPushPayload(ctx, w.h, result, "", "", sha, "", cpID)
				if payload.Capture == nil || payload.Capture.Status != "incomplete" || payload.HumanLines != 0 || payload.UnattributedLines == 0 {
					t.Fatalf("upload lost capture uncertainty: %+v", payload)
				}
				// Delayed hooks cannot erase the checkpoint's recorded gap.
				dispatch("a", true)
				dispatch("later", true)
				again, err := NewAttributionService().AttributeCommit(ctx, AttributionInput{RepoPath: w.dir, CommitHash: sha})
				if err != nil || again.Capture == nil || again.Capture.Status != "incomplete" || again.HumanLines != 0 {
					t.Fatalf("late evidence erased capture gap: %+v, %v", again, err)
				}
			} else if result.Capture.Status != "complete" || len(status.AITrend) != 1 {
				t.Fatalf("normal completion incorrectly degraded: %+v", result.Capture)
			}
		})
	}
}

func TestApplyCaptureReadinessPreservesLineAccounting(t *testing.T) {
	r := &AttributionResult{AILines: 4, HumanLines: 6, TotalLines: 10, Files: []FileAttribution{{HumanLines: 6, Classification: "human"}}}
	applyCaptureReadiness(r, &CaptureReadiness{Status: "incomplete", Gaps: []CaptureGap{{Reason: "completion_missing"}}})
	if r.HumanLines != 0 || r.UnattributedLines != 6 || r.AILines+r.UnattributedLines != r.TotalLines || r.Files[0].Classification != "unattributed" || r.Files[0].HumanLines != 0 {
		t.Fatalf("incorrect uncertainty presentation: %+v", r)
	}
	if !strings.Contains(strings.Join(r.Diagnostics.Notes, " "), "incomplete") {
		t.Fatal("missing capture diagnostic")
	}
}
