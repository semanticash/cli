package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/toolsnap"
)

func TestCaptureEvidenceGroupSpansCheckpointBoundary(t *testing.T) {
	dir, h, repoID := setupQueueRepo(t)
	ctx := context.Background()
	source := insertSource(t, h, repoID, "/session")
	session := insertSessionWithProvider(t, h, repoID, source, "session", "codex")
	bs, err := blobs.NewStore(filepath.Join(dir, ".semantica", "objects"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []struct {
		id string
		ts int64
	}{{"a", 20}, {"b", 80}} {
		if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{EventID: e.id, SessionID: session, RepositoryID: repoID, Ts: e.ts, Kind: "assistant", Role: sqlstore.NullStr("assistant"), TurnID: sqlstore.NullStr("turn")}); err != nil {
			t.Fatal(err)
		}
	}
	delta := toolsnap.Delta{
		Scope: "concurrent_group", Status: "complete", Window: toolsnap.Window{StartedAt: 10, CompletedAt: 80, DurationMS: 70},
		Actors:   []toolsnap.Actor{{Provider: "codex", SessionID: session, TurnID: "turn"}},
		ToolUses: []toolsnap.ToolUse{{ToolUseID: "a", ToolName: "Bash", EventID: "a", Actor: 0}, {ToolUseID: "b", ToolName: "Bash", EventID: "b", Actor: 0}},
	}
	raw, err := delta.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	hash, _, err := bs.Put(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if err := h.Queries.InsertEvidenceLinkIfAbsent(ctx, sqldb.InsertEvidenceLinkIfAbsentParams{EventID: id, EvidenceKind: "tool_delta", EvidenceHash: hash, GroupID: "group", CreatedAt: 80}); err != nil {
			t.Fatal(err)
		}
	}
	cp := sqldb.Checkpoint{CheckpointID: "cp", RepositoryID: repoID, CreatedAt: 100}
	win := tsWindow(50, 100)
	a := toolsnap.ToolKey{RepositoryID: repoID, Provider: "codex", SessionID: session, TurnID: "turn", ToolUseID: "a"}
	b := a
	b.ToolUseID = "b"
	snap := toolsnap.RegistrySnapshot{Windows: []toolsnap.PendingToolSnapshot{
		{Key: a, GroupID: "group", StartedAt: 10, CompletedAt: 20, Status: "complete"},
		{Key: b, GroupID: "group", StartedAt: 15, CompletedAt: 80, Status: "complete"},
	}}
	r := freezeCheckpointCapture(cp, win, snap, nil, time.Now())
	if len(r.Members) != 1 || r.Members[0].Key != b {
		t.Fatalf("incorrect boundary membership: %+v", r.Members)
	}
	evidence, gaps, err := captureEvidence(ctx, h, bs, cp, win)
	if err != nil || len(gaps) != 0 || len(evidence) != 1 {
		t.Fatalf("valid cross-boundary group rejected: evidence=%v gaps=%v err=%v", evidence, gaps, err)
	}
	evaluateCheckpointCapture(r, toolsnap.RegistrySnapshot{}, nil, evidence, gaps, time.Now())
	if r.Result.Status != "complete" {
		t.Fatalf("relevant B not settled: %+v", r.Result)
	}
	// Older members still require links to validate the full group.
	if _, err := h.DB.ExecContext(ctx, "delete from agent_event_evidence_links where event_id = 'a'"); err != nil {
		t.Fatal(err)
	}
	evidence, gaps, err = captureEvidence(ctx, h, bs, cp, win)
	if err != nil || len(evidence) != 0 || len(gaps) != 1 || gaps[0].Reason != "evidence_member_not_linked" {
		t.Fatalf("missing full-group member accepted: evidence=%v gaps=%v err=%v", evidence, gaps, err)
	}
}
