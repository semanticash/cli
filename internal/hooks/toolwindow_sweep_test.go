package hooks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/store/blobs"
	"github.com/semanticash/cli/internal/toolsnap"
)

// strandGroup leaves an all-complete group awaiting finalization.
func strandGroup(t *testing.T, w *toolWindowWorld, session, toolUse, eventID string) {
	t.Helper()
	reg, err := toolsnap.OpenRegistry(w.semDir)
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("finalization died")
	if _, err := reg.Complete(context.Background(), toolsnap.ToolKey{
		RepositoryID: w.repoID, Provider: "claude_code",
		SessionID: session, TurnID: "turn-1", ToolUseID: toolUse,
	}, toolsnap.CompletionInfo{EventID: eventID, At: 500, CommandSummary: "cmd"}, nil,
		func(_ []toolsnap.PendingToolSnapshot, _ *toolsnap.GroupFinal, _ bool, _ func() error) (toolsnap.FinalizeResult, error) {
			return toolsnap.FinalizeResult{}, boom
		}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
}

// A durable post ref resumes without rereading the workspace.
func TestSweepResumesStrandedGroupFromPostRef(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-sw", Provider: "claude-code",
		TurnID: "turn-1", CWD: w.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := handleToolStepStarted(ctx, "claude-code", startedEvent("sess-sw", "toolu_sw", w.repoPath), w.bh); err != nil {
		t.Fatal(err)
	}
	wins := windowsIn(t, w.semDir)
	if len(wins) != 1 {
		t.Fatal("window missing")
	}

	// Preserve the post state and events without publishing closure.
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
	if _, err := broker.WriteEventsToRepo(ctx, w.repoPath, []broker.RawEvent{bashRawEvent("evt-sw", "toolu_sw", "sess-sw")}, nil); err != nil {
		t.Fatal(err)
	}
	strandGroup(t, w, "sess-sw", "toolu_sw", "evt-sw")

	// Exclude changes made after the captured post state.
	if err := os.WriteFile(filepath.Join(w.repoPath, "later.txt"), []byte("after crash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stages := recordStages(t)
	report, err := SweepToolWindows(ctx, w.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if report.GroupsResumed != 1 || report.GroupsTerminal != 0 || report.Errors != 0 {
		t.Fatalf("report = %+v, want one resumed group", report)
	}
	for _, s := range *stages {
		if s == "capture_after" {
			t.Fatal("sweep recaptured the workspace")
		}
	}

	deltas := findDeltas(t, w.semDir)
	if len(deltas) != 1 || deltas[0].Status != "complete" {
		t.Fatalf("deltas = %+v, want one complete delta", deltas)
	}
	paths := map[string]bool{}
	for _, f := range deltas[0].Files {
		paths[f.Path] = true
	}
	if !paths["tool.txt"] || paths["later.txt"] {
		t.Fatalf("delta paths = %v, want the ref state only", paths)
	}
	if links := linksIn(t, w.semDir); len(links) != 1 || links[0].EventID != "evt-sw" {
		t.Fatalf("links = %+v", links)
	}
	if wins := windowsIn(t, w.semDir); len(wins) != 0 {
		t.Fatalf("group not closed: %+v", wins)
	}
	if refs := snapshotRefsIn(t, w.repoPath, w.semDir); len(refs) != 0 {
		t.Fatalf("refs after sweep = %v, want none", refs)
	}
}

// Missing post state closes as linked terminal partial evidence.
func TestSweepTerminalPartialForIdentitylessGroup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-tp", Provider: "claude-code",
		TurnID: "turn-1", CWD: w.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := handleToolStepStarted(ctx, "claude-code", startedEvent("sess-tp", "toolu_tp", w.repoPath), w.bh); err != nil {
		t.Fatal(err)
	}
	if _, err := broker.WriteEventsToRepo(ctx, w.repoPath, []broker.RawEvent{bashRawEvent("evt-tp", "toolu_tp", "sess-tp")}, nil); err != nil {
		t.Fatal(err)
	}
	strandGroup(t, w, "sess-tp", "toolu_tp", "evt-tp")

	report, err := SweepToolWindows(ctx, w.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if report.GroupsTerminal != 1 || report.GroupsResumed != 0 || report.LinksSkipped != 0 {
		t.Fatalf("report = %+v, want one terminal group", report)
	}
	deltas := findDeltas(t, w.semDir)
	if len(deltas) != 1 || deltas[0].Status != "partial" || deltas[0].Reason != toolsnap.ReasonPostSnapshotLost {
		t.Fatalf("deltas = %+v, want post_snapshot_lost partial", deltas)
	}
	if links := linksIn(t, w.semDir); len(links) != 1 || links[0].EventID != "evt-tp" {
		t.Fatalf("links = %+v", links)
	}
	if wins := windowsIn(t, w.semDir); len(wins) != 0 {
		t.Fatalf("group not closed: %+v", wins)
	}
}

// Recovery skips links for missing member events.
func TestSweepSkipsLinksForMissingEvents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-ms", Provider: "claude-code",
		TurnID: "turn-1", CWD: w.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := handleToolStepStarted(ctx, "claude-code", startedEvent("sess-ms", "toolu_ms", w.repoPath), w.bh); err != nil {
		t.Fatal(err)
	}
	// Simulate a missing member event.
	strandGroup(t, w, "sess-ms", "toolu_ms", "evt-ms")

	report, err := SweepToolWindows(ctx, w.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if report.GroupsTerminal != 1 || report.LinksSkipped != 1 || report.Errors != 0 {
		t.Fatalf("report = %+v, want terminal close with one skipped link", report)
	}
	if links := linksIn(t, w.semDir); len(links) != 0 {
		t.Fatalf("links = %+v, want none", links)
	}
	if wins := windowsIn(t, w.semDir); len(wins) != 0 {
		t.Fatalf("group not closed: %+v", wins)
	}
}

// A pending partial is removed after its link becomes durable.
func TestSweepReplaysPendingPartial(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	evtID := strings.Repeat("ef", 32)
	reg, err := toolsnap.OpenRegistry(w.semDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.LoadOrRecordPendingPartial(toolsnap.PendingPartialRecord{
		Key: toolsnap.ToolKey{
			RepositoryID: w.repoID, Provider: "claude_code",
			SessionID: "sess-pp", TurnID: "turn-1", ToolUseID: "toolu_pp",
		},
		EventID: evtID, Reason: "pre_snapshot_missing",
		ToolName: "Bash", CommandSummary: "cmd", Timestamp: 5000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := broker.WriteEventsToRepo(ctx, w.repoPath, []broker.RawEvent{bashRawEvent(evtID, "toolu_pp", "sess-pp")}, nil); err != nil {
		t.Fatal(err)
	}

	report, err := SweepToolWindows(ctx, w.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if report.PartialsReplayed != 1 || report.Errors != 0 {
		t.Fatalf("report = %+v, want one replayed partial", report)
	}
	links := linksIn(t, w.semDir)
	if len(links) != 1 || links[0].EventID != evtID || links[0].GroupID != "pre_snapshot_missing:"+evtID {
		t.Fatalf("links = %+v", links)
	}
	if deltas := findDeltas(t, w.semDir); len(deltas) != 1 || deltas[0].Window.StartedAt != 5000 {
		t.Fatalf("deltas = %+v, want the recorded partial", deltas)
	}
	if !report.Maintenance.PruneRan {
		t.Fatalf("maintenance did not run after recovery: %+v", report.Maintenance)
	}
	// The durable link consumes the record.
	if recs, err := reg.PendingPartialRecords(); err != nil || len(recs) != 0 {
		t.Fatalf("records after replay = %+v err = %v, want consumed", recs, err)
	}

	// Recovery is idempotent.
	again, err := SweepToolWindows(ctx, w.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if again.PartialsReplayed != 0 || again.Errors != 0 {
		t.Fatalf("second sweep = %+v, want a no-op", again)
	}
	if links := linksIn(t, w.semDir); len(links) != 1 {
		t.Fatalf("links after second sweep = %+v", links)
	}
}

// A sweep reclaims a sealed group and links its completed members.
func TestSweepReclaimsStuckActiveGroup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	reg, err := toolsnap.OpenRegistry(w.semDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	old := now - 2*toolsnap.DefaultStaleActiveAge.Milliseconds()
	mkKey := func(tool string) toolsnap.ToolKey {
		return toolsnap.ToolKey{
			RepositoryID: w.repoID, Provider: "claude_code",
			SessionID: "sess-rc", TurnID: "turn-1", ToolUseID: tool,
		}
	}
	stuck := toolsnap.PendingToolSnapshot{
		Key: mkKey("toolu_stuck"), ToolName: "Bash",
		SnapshotRef: "refs/semantica/tool-windows/x", TreeHash: "t", HeadHash: "h",
		ObjectFormat: "sha1", StartedAt: old,
	}
	if _, err := reg.Begin(ctx, stuck); err != nil {
		t.Fatal(err)
	}
	done := stuck
	done.Key = mkKey("toolu_done")
	done.StartedAt = old + 1
	if _, err := reg.Begin(ctx, done); err != nil {
		t.Fatal(err)
	}
	evtID := strings.Repeat("fa", 32)
	if _, err := reg.Complete(ctx, done.Key,
		toolsnap.CompletionInfo{EventID: evtID, At: old + 2, CommandSummary: "cmd"}, nil,
		func([]toolsnap.PendingToolSnapshot, *toolsnap.GroupFinal, bool, func() error) (toolsnap.FinalizeResult, error) {
			t.Fatal("finalize invoked for a pinned group")
			return toolsnap.FinalizeResult{}, nil
		}); err != nil {
		t.Fatal(err)
	}
	if _, err := broker.WriteEventsToRepo(ctx, w.repoPath, []broker.RawEvent{bashRawEvent(evtID, "toolu_done", "sess-rc")}, nil); err != nil {
		t.Fatal(err)
	}

	report, err := SweepToolWindows(ctx, w.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if report.GroupsReclaimed != 1 || report.MembersTombstoned != 2 || report.PartialsReplayed != 1 || report.Errors != 0 {
		t.Fatalf("report = %+v, want one reclaimed group replayed in the same pass", report)
	}
	links := linksIn(t, w.semDir)
	if len(links) != 1 || links[0].EventID != evtID ||
		links[0].GroupID != toolsnap.ReasonStaleActiveWindow+":"+evtID {
		t.Fatalf("links = %+v, want stale_active_window partial evidence", links)
	}
	for _, k := range []toolsnap.ToolKey{stuck.Key, done.Key} {
		if ts, err := reg.HasTombstone(k); err != nil || !ts {
			t.Fatalf("tombstone %s missing: %v err = %v", k.ToolUseID, ts, err)
		}
	}
	if wins := windowsIn(t, w.semDir); len(wins) != 0 {
		t.Fatalf("windows = %+v, want the pinned group removed", wins)
	}

	// Repeated sweeps are no-ops.
	again, err := SweepToolWindows(ctx, w.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if again.GroupsReclaimed != 0 || again.PartialsReplayed != 0 || again.Errors != 0 {
		t.Fatalf("second sweep = %+v, want a no-op", again)
	}
	if links := linksIn(t, w.semDir); len(links) != 1 {
		t.Fatalf("links after second sweep = %+v", links)
	}
}

// Reclamation succeeds before a later snapshot-store failure.
func TestSweepReclaimsDespiteBrokenStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	reg, err := toolsnap.OpenRegistry(w.semDir)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(tool string, at int64) toolsnap.PendingToolSnapshot {
		return toolsnap.PendingToolSnapshot{
			Key: toolsnap.ToolKey{
				RepositoryID: w.repoID, Provider: "claude_code",
				SessionID: "s", TurnID: "t", ToolUseID: tool,
			},
			ToolName: "Bash", SnapshotRef: "refs/x", TreeHash: "th",
			HeadHash: "hh", ObjectFormat: "sha1", StartedAt: at,
		}
	}
	if _, err := reg.Begin(ctx, mk("tu-a", 1000)); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Begin(ctx, mk("tu-b", 1001)); err != nil {
		t.Fatal(err)
	}

	// Replace the snapshot store with a file.
	storePath := filepath.Join(w.semDir, "tool-snapshots.git")
	if err := os.RemoveAll(storePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(storePath, []byte("not a git dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := SweepToolWindows(ctx, w.repoPath)
	if err == nil {
		t.Fatal("sweep succeeded despite a broken snapshot store")
	}
	if report.GroupsReclaimed != 1 || report.MembersTombstoned != 2 {
		t.Fatalf("report = %+v, want reclamation before store init", report)
	}
	if wins := windowsIn(t, w.semDir); len(wins) != 0 {
		t.Fatalf("windows = %+v, want the reclaimed group removed", wins)
	}
}

// A post hook for an abandoned window produces no tool-delta evidence.
func TestPostAfterAbandonmentFabricatesNoEvidence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-ab", Provider: "claude-code",
		TurnID: "turn-1", CWD: w.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := handleToolStepStarted(ctx, "claude-code", startedEvent("sess-ab", "toolu_ab", w.repoPath), w.bh); err != nil {
		t.Fatal(err)
	}
	wins := windowsIn(t, w.semDir)
	if len(wins) != 1 {
		t.Fatal("window missing")
	}

	// Tombstone the window before removal.
	reg, err := toolsnap.OpenRegistry(w.semDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.WriteTombstone(wins[0].Key, 2); err != nil {
		t.Fatal(err)
	}
	if err := reg.RemoveGroup(ctx, wins[0].GroupID); err != nil {
		t.Fatal(err)
	}

	post := postBashEvent("sess-ab", "toolu_ab", w.repoPath, "cmd")
	if windowHandled == completeToolWindow(ctx, "claude-code", post, w.bh, nil, []broker.RawEvent{bashRawEvent("evt-ab", "toolu_ab", "sess-ab")}) {
		t.Fatal("tombstoned window reported handled")
	}
	if deltas := findDeltas(t, w.semDir); len(deltas) != 0 {
		t.Fatalf("deltas = %+v, want none", deltas)
	}
	if links := linksIn(t, w.semDir); len(links) != 0 {
		t.Fatalf("links = %+v, want none", links)
	}

	// A duplicate pre hook must not reopen the identity.
	if err := handleToolStepStarted(ctx, "claude-code", startedEvent("sess-ab", "toolu_ab", w.repoPath), w.bh); err != nil {
		t.Fatal(err)
	}
	if wins := windowsIn(t, w.semDir); len(wins) != 0 {
		t.Fatalf("tombstoned key reopened a window: %+v", wins)
	}
}

// Conflicting evidence preserves the pending record and existing link.
func TestSweepPartialConflictKeepsRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	evtID := strings.Repeat("dd", 32)
	reg, err := toolsnap.OpenRegistry(w.semDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.LoadOrRecordPendingPartial(toolsnap.PendingPartialRecord{
		Key: toolsnap.ToolKey{
			RepositoryID: w.repoID, Provider: "claude_code",
			SessionID: "sess-cx", TurnID: "turn-1", ToolUseID: "toolu_cx",
		},
		EventID: evtID, Reason: "pre_snapshot_missing",
		ToolName: "Bash", CommandSummary: "cmd", Timestamp: 5000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := broker.WriteEventsToRepo(ctx, w.repoPath, []broker.RawEvent{bashRawEvent(evtID, "toolu_cx", "sess-cx")}, nil); err != nil {
		t.Fatal(err)
	}
	if err := broker.WriteEvidenceLinksToRepo(ctx, w.repoPath, []broker.EvidenceLink{{
		EventID: evtID, EvidenceKind: "tool_delta",
		EvidenceHash: "foreign-hash", GroupID: "pre_snapshot_missing:" + evtID, CreatedAt: 2,
	}}); err != nil {
		t.Fatal(err)
	}

	report, err := SweepToolWindows(ctx, w.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if report.PartialsReplayed != 0 || report.Errors == 0 {
		t.Fatalf("report = %+v, want a counted conflict, no replay", report)
	}
	if links := linksIn(t, w.semDir); len(links) != 1 || links[0].Hash != "foreign-hash" {
		t.Fatalf("links = %+v, want the foreign link preserved", links)
	}
	if recs, err := reg.PendingPartialRecords(); err != nil || len(recs) != 1 {
		t.Fatalf("records = %+v err = %v, want the repair pointer kept", recs, err)
	}
}

// InspectToolWindows inventories recovery state without changing it.
func TestInspectToolWindows(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-in", Provider: "claude-code",
		TurnID: "turn-1", CWD: w.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := handleToolStepStarted(ctx, "claude-code", startedEvent("sess-in", "toolu_in", w.repoPath), w.bh); err != nil {
		t.Fatal(err)
	}
	reg, err := toolsnap.OpenRegistry(w.semDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.LoadOrRecordPendingPartial(toolsnap.PendingPartialRecord{
		Key: toolsnap.ToolKey{
			RepositoryID: w.repoID, Provider: "claude_code",
			SessionID: "sess-in", TurnID: "turn-1", ToolUseID: "toolu_in2",
		},
		EventID: strings.Repeat("aa", 32), Reason: "pre_snapshot_missing",
		ToolName: "Bash", Timestamp: 5000,
	}); err != nil {
		t.Fatal(err)
	}

	status, err := InspectToolWindows(ctx, w.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if status.ActiveWindows != 1 || status.PendingPartials != 1 ||
		status.PendingFinalizations != 0 || status.StaleWindows != 0 {
		t.Fatalf("status = %+v", status)
	}
	// Inspection changed nothing.
	if wins := windowsIn(t, w.semDir); len(wins) != 1 {
		t.Fatalf("windows after inspect = %+v", wins)
	}
}

// strandObserveGroup leaves an observation-only group awaiting finalization.
func strandObserveGroup(t *testing.T, w *toolWindowWorld, session, toolUse, eventID string) {
	t.Helper()
	ctx := context.Background()
	rc, err := toolsnap.ResolveRepoContext(ctx, w.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := toolsnap.OpenStore(ctx, rc, w.semDir)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := toolsnap.OpenRegistry(w.semDir)
	if err != nil {
		t.Fatal(err)
	}
	obsKey := toolsnap.ToolKey{
		RepositoryID: w.repoID, Provider: "claude_code",
		SessionID: session, TurnID: "turn-1", ToolUseID: toolUse, Mode: toolsnap.ToolModeObserve,
	}
	if _, err := reg.CaptureAndBegin(ctx, store, obsKey, "Bash", 400); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("observe finalization died")
	if _, err := reg.Complete(ctx, obsKey, toolsnap.CompletionInfo{EventID: eventID, At: 500, CommandSummary: "cmd"}, nil,
		func(_ []toolsnap.PendingToolSnapshot, _ *toolsnap.GroupFinal, _ bool, _ func() error) (toolsnap.FinalizeResult, error) {
			return toolsnap.FinalizeResult{}, boom
		}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
}

// Observation recovery closes the group and releases refs without publishing links.
func TestSweepObserveGroupRecoversWithoutPublishing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	strandObserveGroup(t, w, "sess-obs", "toolu_obs", "evt-obs")

	report, err := SweepToolWindows(ctx, w.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if report.Errors != 0 {
		t.Fatalf("report = %+v, want no errors", report)
	}
	if links := linksIn(t, w.semDir); len(links) != 0 {
		t.Fatalf("observe recovery published links: %+v", links)
	}
	if wins := windowsIn(t, w.semDir); len(wins) != 0 {
		t.Fatalf("observe group not closed: %+v", wins)
	}
	if refs := snapshotRefsIn(t, w.repoPath, w.semDir); len(refs) != 0 {
		t.Fatalf("observe refs not cleaned: %v", refs)
	}
}

// Pending observation-only partials are removed without publishing links.
func TestSweepObservePartialDiscardedWithoutPublishing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	reg, err := toolsnap.OpenRegistry(w.semDir)
	if err != nil {
		t.Fatal(err)
	}
	hexEvent := strings.Repeat("a", 64)
	if _, err := reg.LoadOrRecordPendingPartial(toolsnap.PendingPartialRecord{
		Key: toolsnap.ToolKey{
			RepositoryID: w.repoID, Provider: "claude_code", SessionID: "s", TurnID: "turn-1",
			ToolUseID: "toolu_obs", Mode: toolsnap.ToolModeObserve,
		},
		EventID: hexEvent, Reason: toolsnap.ReasonBackgroundCommand, ToolName: "Bash", Timestamp: 500,
	}); err != nil {
		t.Fatal(err)
	}

	report, err := SweepToolWindows(ctx, w.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if report.Errors != 0 || report.PartialsReplayed != 0 {
		t.Fatalf("report = %+v, want nothing replayed", report)
	}
	if links := linksIn(t, w.semDir); len(links) != 0 {
		t.Fatalf("observe partial published links: %+v", links)
	}
	recs, err := reg.PendingPartialRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Fatalf("observe partial not discarded: %+v", recs)
	}
}

// Recovery keeps publication separate for groups with different modes.
func TestSweepPublishAndObserveGroupsIndependent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	// Leave a publishing group with a post ref and a persisted event.
	if err := SaveCaptureState(&CaptureState{
		SessionID: "sess-pub", Provider: "claude-code", TurnID: "turn-1", CWD: w.repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := handleToolStepStarted(ctx, "claude-code", startedEvent("sess-pub", "toolu_pub", w.repoPath), w.bh); err != nil {
		t.Fatal(err)
	}
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
	pubWins := windowsIn(t, w.semDir)
	if len(pubWins) != 1 {
		t.Fatalf("publish window missing: %+v", pubWins)
	}
	postSnap, err := store.CaptureBefore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureRef(ctx, toolsnap.GroupPostRef(store.WorktreeID(), pubWins[0].GroupID), postSnap.TreeHash); err != nil {
		t.Fatal(err)
	}
	if _, err := broker.WriteEventsToRepo(ctx, w.repoPath, []broker.RawEvent{bashRawEvent("evt-pub", "toolu_pub", "sess-pub")}, nil); err != nil {
		t.Fatal(err)
	}
	strandGroup(t, w, "sess-pub", "toolu_pub", "evt-pub")

	// Add an interrupted observation in the same repository.
	strandObserveGroup(t, w, "sess-obs", "toolu_obs", "evt-obs")

	report, err := SweepToolWindows(ctx, w.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if report.Errors != 0 {
		t.Fatalf("report = %+v, want no errors", report)
	}
	// Only the publishing member receives an evidence link.
	links := linksIn(t, w.semDir)
	if len(links) != 1 || links[0].EventID != "evt-pub" {
		t.Fatalf("links = %+v, want only the publishing member", links)
	}
	if wins := windowsIn(t, w.semDir); len(wins) != 0 {
		t.Fatalf("groups not closed: %+v", wins)
	}
	if refs := snapshotRefsIn(t, w.repoPath, w.semDir); len(refs) != 0 {
		t.Fatalf("refs not cleaned: %v", refs)
	}
}

// Registry completion preserves publication isolation in both completion orders.
func TestObserveAndPublishCompletionOrders(t *testing.T) {
	for _, observeFirst := range []bool{false, true} {
		name := "publish_first"
		if observeFirst {
			name = "observe_first"
		}
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("SEMANTICA_HOME", home)
			w := newToolWindowWorld(t, home, "repo")
			ctx := context.Background()

			rc, err := toolsnap.ResolveRepoContext(ctx, w.repoPath)
			if err != nil {
				t.Fatal(err)
			}
			store, err := toolsnap.OpenStore(ctx, rc, w.semDir)
			if err != nil {
				t.Fatal(err)
			}
			reg, err := toolsnap.OpenRegistry(w.semDir)
			if err != nil {
				t.Fatal(err)
			}

			pubKey := toolsnap.ToolKey{RepositoryID: w.repoID, Provider: "claude_code", SessionID: "s-pub", TurnID: "turn-1", ToolUseID: "toolu_pub"}
			obsKey := pubKey
			obsKey.SessionID = "s-obs"
			obsKey.ToolUseID = "toolu_obs"
			obsKey.Mode = toolsnap.ToolModeObserve

			if _, err := reg.CaptureAndBegin(ctx, store, pubKey, "Bash", 100); err != nil {
				t.Fatal(err)
			}
			if _, err := reg.CaptureAndBegin(ctx, store, obsKey, "Bash", 101); err != nil {
				t.Fatal(err)
			}
			if _, err := broker.WriteEventsToRepo(ctx, w.repoPath, []broker.RawEvent{bashRawEvent("evt-pub", "toolu_pub", "s-pub")}, nil); err != nil {
				t.Fatal(err)
			}

			completePublish := func() {
				cleanup := map[string]string{}
				closed, err := reg.Complete(ctx, pubKey, toolsnap.CompletionInfo{EventID: "evt-pub", At: 600, CommandSummary: "cmd"}, nil,
					func(members []toolsnap.PendingToolSnapshot, _ *toolsnap.GroupFinal, _ bool, recordIntent func() error) (toolsnap.FinalizeResult, error) {
						_ = recordIntent()
						if err := broker.WriteEvidenceLinksToRepo(ctx, w.repoPath, []broker.EvidenceLink{{
							EventID: "evt-pub", EvidenceKind: evidenceKindToolDelta, EvidenceHash: strings.Repeat("b", 64),
							GroupID: members[0].GroupID, CreatedAt: 600,
						}}); err != nil {
							return toolsnap.FinalizeResult{}, err
						}
						collectGroupRefs(cleanup, store, members, members[0].GroupID, "")
						return toolsnap.FinalizeResult{Done: true}, nil
					})
				if err != nil || !closed {
					t.Fatalf("complete publish: closed=%v err=%v", closed, err)
				}
				releaseGroupRefs(ctx, reg, store, cleanup)
			}
			repoBlobs, err := blobs.NewStore(filepath.Join(w.semDir, "objects"))
			if err != nil {
				t.Fatal(err)
			}
			completeObserve := func() {
				cleanup := map[string]string{}
				closed, err := reg.Complete(ctx, obsKey, toolsnap.CompletionInfo{EventID: "evt-obs", At: 601, CommandSummary: "cmd"}, nil,
					func(members []toolsnap.PendingToolSnapshot, prior *toolsnap.GroupFinal, retry bool, recordIntent func() error) (toolsnap.FinalizeResult, error) {
						return finalizeObserveGroup(ctx, store, rc.HeadAnchor(), repoBlobs, members, prior, retry, recordIntent, cleanup, &Event{Timestamp: 601})
					})
				if err != nil || !closed {
					t.Fatalf("complete observe: closed=%v err=%v", closed, err)
				}
				releaseGroupRefs(ctx, reg, store, cleanup)
			}

			if observeFirst {
				completeObserve()
				completePublish()
			} else {
				completePublish()
				completeObserve()
			}

			links := linksIn(t, w.semDir)
			if len(links) != 1 || links[0].EventID != "evt-pub" {
				t.Fatalf("links = %+v, want only the publishing member", links)
			}
			if wins := windowsIn(t, w.semDir); len(wins) != 0 {
				t.Fatalf("groups not closed: %+v", wins)
			}
			if refs := snapshotRefsIn(t, w.repoPath, w.semDir); len(refs) != 0 {
				t.Fatalf("refs not cleaned: %v", refs)
			}
			// Resolve the observation by window identity after reopening.
			reopened, err := toolsnap.OpenRegistry(w.semDir)
			if err != nil {
				t.Fatal(err)
			}
			deltaHash, found, err := reopened.ClosureDelta(obsKey)
			if err != nil || !found || deltaHash == "" {
				t.Fatalf("observe delta not retrievable by identity: hash=%q found=%v err=%v", deltaHash, found, err)
			}
			if _, err := repoBlobs.Get(ctx, deltaHash); err != nil {
				t.Fatalf("preserved delta blob missing for %s: %v", deltaHash, err)
			}
		})
	}
}

// Recovery preserves the observation and releases its post-tree ref.
func TestSweepObserveGroupReleasesPostRef(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	rc, err := toolsnap.ResolveRepoContext(ctx, w.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := toolsnap.OpenStore(ctx, rc, w.semDir)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := toolsnap.OpenRegistry(w.semDir)
	if err != nil {
		t.Fatal(err)
	}
	obsKey := toolsnap.ToolKey{
		RepositoryID: w.repoID, Provider: "claude_code",
		SessionID: "s-obs", TurnID: "turn-1", ToolUseID: "toolu_obs", Mode: toolsnap.ToolModeObserve,
	}
	win, err := reg.CaptureAndBegin(ctx, store, obsKey, "Bash", 400)
	if err != nil {
		t.Fatal(err)
	}
	// Leave a post-tree ref from an interrupted capture.
	if err := os.WriteFile(filepath.Join(w.repoPath, "obs.txt"), []byte("observed change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	postSnap, err := store.CaptureBefore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	postRef := toolsnap.GroupPostRef(store.WorktreeID(), win.GroupID)
	if err := store.EnsureRef(ctx, postRef, postSnap.TreeHash); err != nil {
		t.Fatal(err)
	}
	// Strand the group awaiting finalization.
	boom := errors.New("observe finalization died")
	if _, err := reg.Complete(ctx, obsKey, toolsnap.CompletionInfo{EventID: "evt-obs", At: 500, CommandSummary: "cmd"}, nil,
		func(_ []toolsnap.PendingToolSnapshot, _ *toolsnap.GroupFinal, _ bool, _ func() error) (toolsnap.FinalizeResult, error) {
			return toolsnap.FinalizeResult{}, boom
		}); !errors.Is(err, boom) {
		t.Fatal(err)
	}

	if _, err := SweepToolWindows(ctx, w.repoPath); err != nil {
		t.Fatal(err)
	}
	if links := linksIn(t, w.semDir); len(links) != 0 {
		t.Fatalf("observe recovery published links: %+v", links)
	}
	// Release both pre- and post-snapshot refs.
	refs, err := store.ListRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := refs[postRef]; present {
		t.Fatalf("post-tree ref left behind: %v", refs)
	}
	if refs := snapshotRefsIn(t, w.repoPath, w.semDir); len(refs) != 0 {
		t.Fatalf("refs not cleaned: %v", refs)
	}
	// Resolve the recovered delta by window identity and verify the change.
	reopened, err := toolsnap.OpenRegistry(w.semDir)
	if err != nil {
		t.Fatal(err)
	}
	deltaHash, foundDelta, err := reopened.ClosureDelta(obsKey)
	if err != nil || !foundDelta || deltaHash == "" {
		t.Fatalf("recovered observe delta not retrievable: hash=%q found=%v err=%v", deltaHash, foundDelta, err)
	}
	repoBlobs, err := blobs.NewStore(filepath.Join(w.semDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := repoBlobs.Get(ctx, deltaHash)
	if err != nil {
		t.Fatalf("preserved delta blob missing: %v", err)
	}
	parsed, err := toolsnap.ParseDelta(raw)
	if err != nil {
		t.Fatalf("parse preserved delta: %v", err)
	}
	sawObs := false
	for _, f := range parsed.Files {
		if f.Path == "obs.txt" {
			sawObs = true
		}
	}
	if !sawObs {
		t.Fatalf("preserved delta did not capture obs.txt: %+v", parsed.Files)
	}
}

// Recovery preserves a recorded partial reason and timestamp.
func TestSweepObservePreservesRecordedPartialReason(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	rc, err := toolsnap.ResolveRepoContext(ctx, w.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := toolsnap.OpenStore(ctx, rc, w.semDir)
	if err != nil {
		t.Fatal(err)
	}
	repoBlobs, err := blobs.NewStore(filepath.Join(w.semDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	target := &toolWindowTarget{repoPath: w.repoPath, semDir: w.semDir}

	obsKey := toolsnap.ToolKey{
		RepositoryID: w.repoID, Provider: "claude_code",
		SessionID: "s-obs", TurnID: "turn-1", ToolUseID: "toolu_obs", Mode: toolsnap.ToolModeObserve,
	}
	members := []toolsnap.PendingToolSnapshot{{
		Key: obsKey, GroupID: "g-obs", Status: "complete",
		EventID: "evt-obs", CompletedAt: 500, TreeHash: "tree", SnapshotRef: "ref",
	}}
	// The previous attempt recorded a timeout without a post tree.
	prior := &toolsnap.GroupFinal{PartialReason: toolsnap.ReasonTimeout, CapturedAt: 500}

	cleanupRefs := map[string]string{}
	report := &SweepReport{}
	res, terminal, err := sweepFinalizeGroup(ctx, store, repoBlobs, target, members, prior, cleanupRefs, report)
	if err != nil {
		t.Fatalf("sweepFinalizeGroup: %v", err)
	}
	if !terminal {
		t.Fatal("a recorded partial must be terminal")
	}
	// Preserve the partial reason, timestamp, and blob identity.
	if res.Final.PartialReason != toolsnap.ReasonTimeout || res.Final.CapturedAt != 500 {
		t.Fatalf("res.Final = %+v, want the recorded timeout partial at 500", res.Final)
	}
	if res.Final.DeltaHash == "" {
		t.Fatal("partial observation lost its delta identity on closure")
	}
	raw, err := repoBlobs.Get(ctx, res.Final.DeltaHash)
	if err != nil {
		t.Fatalf("preserved partial delta blob missing: %v", err)
	}
	parsed, err := toolsnap.ParseDelta(raw)
	if err != nil {
		t.Fatalf("parse delta: %v", err)
	}
	if parsed.Status != "partial" || parsed.Reason != toolsnap.ReasonTimeout || parsed.Window.CompletedAt != 500 {
		t.Fatalf("delta = status:%q reason:%q at:%d, want partial/%q/500",
			parsed.Status, parsed.Reason, parsed.Window.CompletedAt, toolsnap.ReasonTimeout)
	}
}

// A recovered partial remains retrievable by window identity after reopening.
func TestSweepObservePartialRetrievableByIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	// Missing post state must produce a post_snapshot_lost partial.
	strandObserveGroup(t, w, "s-obs", "toolu_obs", "evt-obs")

	if _, err := SweepToolWindows(ctx, w.repoPath); err != nil {
		t.Fatal(err)
	}
	if links := linksIn(t, w.semDir); len(links) != 0 {
		t.Fatalf("observe recovery published links: %+v", links)
	}

	obsKey := toolsnap.ToolKey{
		RepositoryID: w.repoID, Provider: "claude_code",
		SessionID: "s-obs", TurnID: "turn-1", ToolUseID: "toolu_obs", Mode: toolsnap.ToolModeObserve,
	}
	reopened, err := toolsnap.OpenRegistry(w.semDir)
	if err != nil {
		t.Fatal(err)
	}
	deltaHash, found, err := reopened.ClosureDelta(obsKey)
	if err != nil || !found || deltaHash == "" {
		t.Fatalf("partial observation not retrievable by identity: hash=%q found=%v err=%v", deltaHash, found, err)
	}
	repoBlobs, err := blobs.NewStore(filepath.Join(w.semDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := repoBlobs.Get(ctx, deltaHash)
	if err != nil {
		t.Fatalf("preserved partial blob missing: %v", err)
	}
	parsed, err := toolsnap.ParseDelta(raw)
	if err != nil {
		t.Fatalf("parse delta: %v", err)
	}
	if parsed.Status != "partial" || parsed.Reason != toolsnap.ReasonPostSnapshotLost || parsed.Window.CompletedAt != 500 {
		t.Fatalf("delta = status:%q reason:%q at:%d, want partial/%q/500",
			parsed.Status, parsed.Reason, parsed.Window.CompletedAt, toolsnap.ReasonPostSnapshotLost)
	}
}
