package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/store/blobs"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/toolsnap"
)

func TestCaptureReadinessCursorTimestampTies(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	ctx := context.Background()
	src := insertSource(t, h, repoID, "/session")
	session := insertSessionWithProvider(t, h, repoID, src, "session", "codex")
	insertWindowEvent(t, h, repoID, session, "old", 1000)
	insertPendingLinked(t, h, repoID, "previous", "previous-sha", 1000)
	insertWindowEvent(t, h, repoID, session, "post", 1000)
	insertPendingLinked(t, h, repoID, "current", "current-sha", 2000)
	previous := readCheckpoint(t, h, "previous")
	cp := readCheckpoint(t, h, "current")
	win := windowBetween(&previous, cp)
	if !win.useCursor {
		t.Fatal("test requires cursor-based boundaries")
	}
	rows, err := h.Queries.ListEventsInWindow(ctx, sqldb.ListEventsInWindowParams{
		RepositoryID: repoID, UseCursor: win.cursorFlag(), AfterCursor: win.cursorAfter(),
		UpToCursor: win.cursorUpTo(), AfterTs: win.afterTs, UpToTs: win.upToTs,
	})
	if err != nil || len(rows) != 1 || rows[0].EventID != "post" {
		t.Fatalf("post event not selected by attribution: %+v, %v", rows, err)
	}
	key := toolsnap.ToolKey{RepositoryID: repoID, Provider: "codex", SessionID: session, TurnID: "turn", ToolUseID: "post"}
	snap := toolsnap.RegistrySnapshot{Windows: []toolsnap.PendingToolSnapshot{{
		Key: key, GroupID: "group", StartedAt: 900, CompletedAt: 1000, Status: "complete",
	}}}
	r := freezeCheckpointCapture(cp, win, snap, nil, time.Now())
	if len(r.Members) != 1 || r.Members[0].Key != key {
		t.Fatalf("lower-boundary tie excluded from capture: %+v", r.Members)
	}
	bs, err := blobs.NewStore(filepath.Join(dir, ".semantica", "objects"))
	if err != nil {
		t.Fatal(err)
	}
	proofs, gaps, err := captureEvidence(ctx, h, bs, cp, win)
	if err != nil {
		t.Fatal(err)
	}
	evaluateCheckpointCapture(r, snap, nil, proofs, gaps, time.UnixMilli(r.Deadline))
	if r.Result.Status != "incomplete" {
		t.Fatalf("missing tied evidence reported complete: %+v", r.Result)
	}
	attribution := &AttributionResult{HumanLines: 1, TotalLines: 1}
	applyCaptureReadiness(attribution, &r.Result)
	if attribution.HumanLines != 0 || attribution.UnattributedLines != 1 {
		t.Fatalf("missing tied evidence became human attribution: %+v", attribution)
	}

	for _, tc := range []struct {
		name      string
		completed int64
		reason    string
	}{
		{"lower_tie_with_evidence", 1000, ""},
		{"before_upper", 1999, ""},
		{"upper_tie", 2000, "post_state_order_unknown"},
		{"after_upper", 2001, "post_state_after_checkpoint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := toolsnap.Delta{
				Scope: "tool", Status: "complete",
				Window:   toolsnap.Window{StartedAt: 900, CompletedAt: tc.completed, DurationMS: tc.completed - 900},
				Actors:   []toolsnap.Actor{{Provider: key.Provider, SessionID: key.SessionID, TurnID: key.TurnID}},
				ToolUses: []toolsnap.ToolUse{{ToolUseID: key.ToolUseID, ToolName: "Bash", EventID: "post", Actor: 0}},
			}
			raw, err := d.CanonicalBytes()
			if err != nil {
				t.Fatal(err)
			}
			hash, _, err := bs.Put(ctx, raw)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.DB.ExecContext(ctx, "delete from agent_event_evidence_links where event_id = 'post'"); err != nil {
				t.Fatal(err)
			}
			if err := h.Queries.InsertEvidenceLinkIfAbsent(ctx, sqldb.InsertEvidenceLinkIfAbsentParams{
				EventID: "post", EvidenceKind: "tool_delta", EvidenceHash: hash, GroupID: "group", CreatedAt: tc.completed,
			}); err != nil {
				t.Fatal(err)
			}
			proofs, gaps, err := captureEvidence(ctx, h, bs, cp, win)
			if err != nil || len(proofs) != 1 || proofs[key].Reason != tc.reason {
				t.Fatalf("post-state ordering: proofs=%+v gaps=%+v err=%v", proofs, gaps, err)
			}
			r := freezeCheckpointCapture(cp, win, snap, nil, time.Now())
			evaluateCheckpointCapture(r, snap, nil, proofs, gaps, time.UnixMilli(r.Deadline))
			want := "complete"
			if tc.reason != "" {
				want = "incomplete"
			}
			if r.Result.Status != want {
				t.Fatalf("status=%+v, want %s", r.Result, want)
			}
		})
	}
}

func TestCaptureReadinessLowerBoundaryPartialAndTombstoneTies(t *testing.T) {
	cp := sqldb.Checkpoint{CheckpointID: "cp", RepositoryID: "repo", CreatedAt: 2000}
	for _, at := range []int64{999, 1000, 1001} {
		snap := toolsnap.RegistrySnapshot{
			Tombstones: []toolsnap.Tombstone{{Key: captureKey("tombstone"), At: at}},
			Partials:   []toolsnap.PendingPartialRecord{{Key: captureKey("partial"), Timestamp: at, Reason: "timeout"}},
			Windows:    []toolsnap.PendingToolSnapshot{{Key: captureKey("window"), StartedAt: 900, CompletedAt: at, Status: "complete", GroupID: "group"}},
		}
		r := freezeCheckpointCapture(cp, tsWindow(1000, 2000), snap, nil, time.Now())
		wantGaps, wantMembers := 2, 1
		if at < 1000 {
			wantGaps, wantMembers = 0, 0
		}
		if len(r.FixedGaps) != wantGaps || len(r.Members) != wantMembers {
			t.Fatalf("at %d: gaps=%+v members=%+v", at, r.FixedGaps, r.Members)
		}
	}
}
