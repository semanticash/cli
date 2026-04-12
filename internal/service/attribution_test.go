package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	attrevents "github.com/semanticash/cli/internal/attribution/events"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// insertCommitCheckpoint creates a completed checkpoint and links it to a
// commit hash. Returns the checkpoint ID.
func insertCommitCheckpoint(t *testing.T, h *sqlstore.Handle, repoID, commitHash string, createdAt int64) string {
	t.Helper()
	cpID := insertCheckpoint(t, h, repoID, createdAt, "auto")
	ctx := context.Background()
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash:   commitHash,
		RepositoryID: repoID,
		CheckpointID: cpID,
		LinkedAt:     createdAt,
	}); err != nil {
		t.Fatalf("insert commit link: %v", err)
	}
	return cpID
}

// queryAttribution mirrors the attribution service's event query: find the
// previous commit-linked checkpoint for the lower bound, then query events
// directly by repository_id and time window via ListEventsInWindow.
func queryAttribution(t *testing.T, h *sqlstore.Handle, repoID, checkpointID string, cpCreatedAt int64) []string {
	t.Helper()
	ctx := context.Background()

	var afterTs int64
	prev, err := h.Queries.GetPreviousCommitLinkedCheckpoint(ctx, sqldb.GetPreviousCommitLinkedCheckpointParams{
		RepositoryID: repoID,
		CreatedAt:    cpCreatedAt,
	})
	if err == nil {
		afterTs = prev.CreatedAt
	}

	rows, err := h.Queries.ListEventsInWindow(ctx, sqldb.ListEventsInWindowParams{
		RepositoryID: repoID,
		AfterTs:      afterTs,
		UpToTs:       cpCreatedAt,
	})
	if err != nil {
		t.Fatalf("ListEventsInWindow: %v", err)
	}
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.EventID)
	}
	return ids
}

// TestAttribution_ManualCheckpointBetweenCommits verifies that creating a
// manual checkpoint between two commits does not shrink the attribution
// window. The AI events that happen between commit 1 and commit 2 must be
// visible in commit 2's attribution, regardless of any intermediate manual
// or baseline checkpoints.
func TestAttribution_ManualCheckpointBetweenCommits(t *testing.T) {
	h := testDB(t)

	// Timeline:
	//   T=100_000  enable (baseline)
	//   T=200_000  commit 1 (checkpoint linked to commit "aaa")
	//   T=300_000  event A (session 1) - AI works after commit 1
	//   T=400_000  event B (session 1)
	//   T=500_000  manual checkpoint (user runs `semantica checkpoint`)
	//   T=600_000  event C (session 1)
	//   T=700_000  commit 2 (checkpoint linked to commit "bbb")

	repoID := insertRepo(t, h, 100_000)
	_ = insertCheckpoint(t, h, repoID, 100_000, "baseline")

	srcID := insertSource(t, h, repoID, "/fake/source.jsonl")
	sessID := insertSession(t, h, repoID, srcID, "session-1")

	_ = insertCommitCheckpoint(t, h, repoID, "aaa", 200_000)

	eA := insertEvent(t, h, sessID, repoID, 300_000, "event A")
	eB := insertEvent(t, h, sessID, repoID, 400_000, "event B")

	// Manual checkpoint - must not affect attribution window.
	_ = insertCheckpoint(t, h, repoID, 500_000, "manual")

	eC := insertEvent(t, h, sessID, repoID, 600_000, "event C")

	cp2 := insertCommitCheckpoint(t, h, repoID, "bbb", 700_000)

	events := queryAttribution(t, h, repoID, cp2, 700_000)

	// All three events (A, B, C) must appear - the manual checkpoint at
	// T=500_000 must not hide events A and B.
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	assertContains(t, events, eA, "event A")
	assertContains(t, events, eB, "event B")
	assertContains(t, events, eC, "event C")
}

// TestAttribution_WorksWithoutSessionCheckpoints verifies that attribution
// returns the correct events even when session_checkpoints is completely
// empty. The query must not depend on that denormalized link table.
func TestAttribution_WorksWithoutSessionCheckpoints(t *testing.T) {
	h := testDB(t)

	// Timeline:
	//   T=100_000  enable (baseline)
	//   T=200_000  event A (session 1)
	//   T=300_000  event B (session 1)
	//   T=400_000  commit 1 (checkpoint linked to commit "aaa")
	//
	// Critically: NO linkSession() calls anywhere.

	repoID := insertRepo(t, h, 100_000)
	_ = insertCheckpoint(t, h, repoID, 100_000, "baseline")

	srcID := insertSource(t, h, repoID, "/fake/source.jsonl")
	sessID := insertSession(t, h, repoID, srcID, "session-1")

	eA := insertEvent(t, h, sessID, repoID, 200_000, "event A")
	eB := insertEvent(t, h, sessID, repoID, 300_000, "event B")

	cpID := insertCommitCheckpoint(t, h, repoID, "aaa", 400_000)

	events := queryAttribution(t, h, repoID, cpID, 400_000)

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	assertContains(t, events, eA, "event A")
	assertContains(t, events, eB, "event B")
}

// TestAttribution_FirstCommitUsesZeroLowerBound verifies that the very first
// commit (no previous commit-linked checkpoint) uses afterTs=0 as the lower
// bound, capturing all events since the repository was enabled.
func TestAttribution_FirstCommitUsesZeroLowerBound(t *testing.T) {
	h := testDB(t)

	// Timeline:
	//   T=100_000  enable (baseline checkpoint, NOT commit-linked)
	//   T=150_000  event A (session 1)
	//   T=200_000  event B (session 1)
	//   T=300_000  commit 1 (first ever commit checkpoint)
	//
	// There is no previous commit-linked checkpoint, so afterTs must be 0,
	// which means ALL events up to T=300_000 are included.

	repoID := insertRepo(t, h, 100_000)
	_ = insertCheckpoint(t, h, repoID, 100_000, "baseline")

	srcID := insertSource(t, h, repoID, "/fake/source.jsonl")
	sessID := insertSession(t, h, repoID, srcID, "session-1")

	eA := insertEvent(t, h, sessID, repoID, 150_000, "event A")
	eB := insertEvent(t, h, sessID, repoID, 200_000, "event B")

	cpID := insertCommitCheckpoint(t, h, repoID, "first-commit", 300_000)

	events := queryAttribution(t, h, repoID, cpID, 300_000)

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	assertContains(t, events, eA, "event A")
	assertContains(t, events, eB, "event B")
}

// TestAttribution_DeltaExcludesPreviousCommitEvents verifies that events
// belonging to a previous commit's window are excluded from the current
// commit's attribution.
func TestAttribution_DeltaExcludesPreviousCommitEvents(t *testing.T) {
	h := testDB(t)

	// Timeline:
	//   T=100_000  enable (baseline)
	//   T=200_000  event A (session 1)
	//   T=300_000  commit 1 -> checkpoint
	//   T=400_000  event B (session 1)
	//   T=500_000  commit 2 -> checkpoint
	//
	// Commit 2's attribution must include only event B, not event A.

	repoID := insertRepo(t, h, 100_000)
	_ = insertCheckpoint(t, h, repoID, 100_000, "baseline")

	srcID := insertSource(t, h, repoID, "/fake/source.jsonl")
	sessID := insertSession(t, h, repoID, srcID, "session-1")

	eA := insertEvent(t, h, sessID, repoID, 200_000, "event A")
	_ = insertCommitCheckpoint(t, h, repoID, "commit-1", 300_000)

	eB := insertEvent(t, h, sessID, repoID, 400_000, "event B")
	cp2 := insertCommitCheckpoint(t, h, repoID, "commit-2", 500_000)

	events := queryAttribution(t, h, repoID, cp2, 500_000)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	assertContains(t, events, eB, "event B")
	assertNotContains(t, events, eA, "event A should NOT be in commit 2")
}

// TestAttribution_MultipleManualCheckpointsBetweenCommits is a stress test
// with several manual and baseline checkpoints interleaved between two
// commits. None of them should affect attribution.
func TestAttribution_MultipleManualCheckpointsBetweenCommits(t *testing.T) {
	h := testDB(t)

	// Timeline:
	//   T=50_000   enable (baseline)
	//   T=100_000  commit 1
	//   T=200_000  event A
	//   T=250_000  manual checkpoint
	//   T=300_000  event B
	//   T=350_000  baseline checkpoint (e.g. from re-enable)
	//   T=400_000  event C
	//   T=450_000  manual checkpoint
	//   T=500_000  event D
	//   T=600_000  commit 2

	repoID := insertRepo(t, h, 50_000)
	_ = insertCheckpoint(t, h, repoID, 50_000, "baseline")

	srcID := insertSource(t, h, repoID, "/fake/source.jsonl")
	sessID := insertSession(t, h, repoID, srcID, "session-1")

	_ = insertCommitCheckpoint(t, h, repoID, "commit-1", 100_000)

	eA := insertEvent(t, h, sessID, repoID, 200_000, "event A")
	_ = insertCheckpoint(t, h, repoID, 250_000, "manual")
	eB := insertEvent(t, h, sessID, repoID, 300_000, "event B")
	_ = insertCheckpoint(t, h, repoID, 350_000, "baseline")
	eC := insertEvent(t, h, sessID, repoID, 400_000, "event C")
	_ = insertCheckpoint(t, h, repoID, 450_000, "manual")
	eD := insertEvent(t, h, sessID, repoID, 500_000, "event D")

	cp2 := insertCommitCheckpoint(t, h, repoID, "commit-2", 600_000)

	events := queryAttribution(t, h, repoID, cp2, 600_000)

	if len(events) != 4 {
		t.Fatalf("expected 4 events, got %d", len(events))
	}
	assertContains(t, events, eA, "event A")
	assertContains(t, events, eB, "event B")
	assertContains(t, events, eC, "event C")
	assertContains(t, events, eD, "event D")
}

// Matching helper tests.

// insertCursorSource creates a Cursor agent_source and returns its ID.
func insertCursorSource(t *testing.T, h *sqlstore.Handle, repoID, sourceKey string) string {
	t.Helper()
	ctx := context.Background()
	row, err := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID:     uuid.NewString(),
		RepositoryID: repoID,
		Provider:     "cursor",
		SourceKey:    sourceKey,
		LastSeenAt:   1700000000000,
		CreatedAt:    1700000000000,
	})
	if err != nil {
		t.Fatalf("insert cursor source: %v", err)
	}
	return row.SourceID
}

// insertCursorSession creates a Cursor session and returns its ID.
func insertCursorSession(t *testing.T, h *sqlstore.Handle, repoID, sourceID, providerSessionID string) string {
	t.Helper()
	ctx := context.Background()
	row, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID:         uuid.NewString(),
		ProviderSessionID: providerSessionID,
		RepositoryID:      repoID,
		Provider:          "cursor",
		SourceID:          sourceID,
		StartedAt:         1700000000000,
		LastSeenAt:        1700000000000,
		MetadataJson:      `{}`,
	})
	if err != nil {
		t.Fatalf("insert cursor session: %v", err)
	}
	return row.SessionID
}

// insertCursorEvent creates a cursor_file_edit event.
func insertCursorEvent(t *testing.T, h *sqlstore.Handle, sessionID, repoID string, ts int64, filePath string) string {
	t.Helper()
	ctx := context.Background()
	eventID := uuid.NewString()

	type tuPayload struct {
		ContentTypes []string `json:"content_types"`
		Tools        []struct {
			Name     string `json:"name"`
			FilePath string `json:"file_path"`
			FileOp   string `json:"file_op"`
		} `json:"tools"`
	}
	p := tuPayload{
		ContentTypes: []string{"cursor_file_edit"},
		Tools: []struct {
			Name     string `json:"name"`
			FilePath string `json:"file_path"`
			FileOp   string `json:"file_op"`
		}{{Name: "cursor_edit", FilePath: filePath, FileOp: "edit"}},
	}
	tuJSON, _ := json.Marshal(p)

	if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID:      eventID,
		SessionID:    sessionID,
		RepositoryID: repoID,
		Ts:           ts,
		Kind:         "cursor_file_edit",
		Role:         sqlstore.NullStr("assistant"),
		ToolUses:     sql.NullString{String: string(tuJSON), Valid: true},
		Summary:      sqlstore.NullStr("AI edited " + filePath),
	}); err != nil {
		t.Fatalf("insert cursor event: %v", err)
	}
	return eventID
}

func TestAttribution_CursorFileLevelAttribution(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()

	repoID := insertRepo(t, h, 100_000)
	srcID := insertCursorSource(t, h, repoID, "/fake/ai-code-tracking.db")
	sessID := insertCursorSession(t, h, repoID, srcID, "conv-123")

	// Cursor edited "src/handler.go" at T=200_000.
	_ = insertCursorEvent(t, h, sessID, repoID, 200_000, "src/handler.go")

	// Create checkpoint linked to a commit at T=300_000.
	cpID := insertCommitCheckpoint(t, h, repoID, "deadbeef1234", 300_000)
	_ = cpID

	// Query events in the window (0, 300_000].
	events, err := h.Queries.ListEventsInWindow(ctx, sqldb.ListEventsInWindowParams{
		RepositoryID: repoID,
		AfterTs:      0,
		UpToTs:       300_000,
	})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	ev := events[0]
	if ev.Provider != "cursor" {
		t.Errorf("provider = %q, want 'cursor'", ev.Provider)
	}
	if !attrevents.HasProviderFileEdit(ev.ToolUses.String) {
		t.Error("expected HasProviderFileEdit to be true")
	}

	// Paths are stored as repo-relative by the ingest pipeline.
	paths := attrevents.ExtractProviderFileTouches(ev.ToolUses.String)
	if len(paths) != 1 || paths[0] != "src/handler.go" {
		t.Errorf("ExtractProviderFileTouches = %v, want [src/handler.go]", paths)
	}
}

func TestComputeAIPercent_CursorFileLevel(t *testing.T) {
	h := testDB(t)

	repoID := insertRepo(t, h, 100_000)
	srcID := insertCursorSource(t, h, repoID, "/fake/ai-code-tracking.db")
	sessID := insertCursorSession(t, h, repoID, srcID, "conv-1")

	// Cursor touched "src/handler.go".
	_ = insertCursorEvent(t, h, sessID, repoID, 200_000, "src/handler.go")

	// Diff with "src/handler.go" having 3 non-blank added lines.
	diff := strings.Join([]string{
		"diff --git a/src/handler.go b/src/handler.go",
		"--- /dev/null",
		"+++ b/src/handler.go",
		"@@ -0,0 +1,4 @@",
		"+package main",
		"+",
		"+func handler() {",
		"+}",
	}, "\n")

	svc := NewAttributionService()
	// No blob store needed - Cursor events don't have payloads.
	bsDir := t.TempDir()
	bs, _ := blobs.NewStore(bsDir)

	result, err := svc.ComputeAIPercentFromDiff(context.Background(), h, bs, []byte(diff), ComputeAIPercentInput{
		RepoRoot: "/fake/repo",
		RepoID:   repoID,
		AfterTs:  0,
		UpToTs:   300_000,
	})
	if err != nil {
		t.Fatalf("ComputeAIPercentFromDiff: %v", err)
	}

	// 3 non-blank lines (package main, func handler() {, }) all from Cursor = 100%.
	if result.Percent != 100 {
		t.Errorf("AI%% = %.1f, want 100 (all lines from Cursor-touched file)", result.Percent)
	}
	if result.TotalLines != 3 {
		t.Errorf("TotalLines = %d, want 3", result.TotalLines)
	}
	if result.AILines != 3 {
		t.Errorf("AILines = %d, want 3", result.AILines)
	}
	if len(result.Providers) != 1 || result.Providers[0].Provider != "cursor" {
		t.Errorf("Providers = %+v, want [{cursor 3}]", result.Providers)
	}
}

func TestComputeAIPercent_MixedClaudeAndCursor(t *testing.T) {
	h := testDB(t)

	repoID := insertRepo(t, h, 100_000)

	// Claude source + session + event with payload.
	claudeSrcID := insertSource(t, h, repoID, "/fake/transcript.jsonl")
	claudeSessID := insertSession(t, h, repoID, claudeSrcID, "claude-sess-1")

	// Claude event that edited "src/main.go" with known lines.
	bsDir := t.TempDir()
	bs, _ := blobs.NewStore(bsDir)

	payload := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"/fake/repo/src/main.go","content":"package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n"}}]}}`
	payloadHash, _, _ := bs.Put(context.Background(), []byte(payload))

	claudeEventID := uuid.NewString()
	tuJSON := `{"content_types":["tool_use"],"tools":[{"name":"Write","file_path":"src/main.go","file_op":"write"}]}`
	_ = h.Queries.InsertAgentEvent(context.Background(), sqldb.InsertAgentEventParams{
		EventID:      claudeEventID,
		SessionID:    claudeSessID,
		RepositoryID: repoID,
		Ts:           200_000,
		Kind:         "assistant",
		Role:         sqlstore.NullStr("assistant"),
		ToolUses:     sql.NullString{String: tuJSON, Valid: true},
		PayloadHash:  sqlstore.NullStr(payloadHash),
		Summary:      sqlstore.NullStr("Wrote main.go"),
	})

	// Cursor source + session + event touching "src/handler.go".
	cursorSrcID := insertCursorSource(t, h, repoID, "/fake/ai-code-tracking.db")
	cursorSessID := insertCursorSession(t, h, repoID, cursorSrcID, "conv-2")
	_ = insertCursorEvent(t, h, cursorSessID, repoID, 250_000, "src/handler.go")

	// Diff with both files.
	diff := strings.Join([]string{
		"diff --git a/src/main.go b/src/main.go",
		"--- /dev/null",
		"+++ b/src/main.go",
		"@@ -0,0 +1,5 @@",
		"+package main",
		"+",
		`+func main() {`,
		`+	fmt.Println("hello")`,
		"+}",
		"diff --git a/src/handler.go b/src/handler.go",
		"--- /dev/null",
		"+++ b/src/handler.go",
		"@@ -0,0 +1,3 @@",
		"+package main",
		"+",
		"+func handler() {}",
	}, "\n")

	svc := NewAttributionService()
	result, err := svc.ComputeAIPercentFromDiff(context.Background(), h, bs, []byte(diff), ComputeAIPercentInput{
		RepoRoot: "/fake/repo",
		RepoID:   repoID,
		AfterTs:  0,
		UpToTs:   300_000,
	})
	if err != nil {
		t.Fatalf("ComputeAIPercentFromDiff: %v", err)
	}

	// All lines from both files should be AI-attributed.
	// main.go: 4 non-blank lines (Claude exact), handler.go: 2 non-blank lines (Cursor file-level).
	// Total: 6 non-blank lines, all AI -> 100%.
	if result.Percent != 100 {
		t.Errorf("AI%% = %.1f, want 100 (mixed Claude+Cursor, all AI)", result.Percent)
	}

	// Verify per-provider breakdown.
	if len(result.Providers) != 2 {
		t.Fatalf("Providers count = %d, want 2", len(result.Providers))
	}
	// Providers sorted by AILines desc: Claude (4) > Cursor (2).
	if result.Providers[0].Provider != "claude_code" {
		t.Errorf("Providers[0].Provider = %q, want claude_code", result.Providers[0].Provider)
	}
	if result.Providers[0].AILines != 4 {
		t.Errorf("Providers[0].AILines = %d, want 4", result.Providers[0].AILines)
	}
	if result.Providers[1].Provider != "cursor" {
		t.Errorf("Providers[1].Provider = %q, want cursor", result.Providers[1].Provider)
	}
	if result.Providers[1].AILines != 2 {
		t.Errorf("Providers[1].AILines = %d, want 2", result.Providers[1].AILines)
	}

	// Verify tier breakdown.
	// Claude: 4 exact lines (content-matched). Cursor: 2 modified lines
	// (file-level touch, no content to exact-match).
	if result.ExactLines != 4 {
		t.Errorf("ExactLines = %d, want 4 (Claude content-matched only)", result.ExactLines)
	}
	if result.ModifiedLines != 2 {
		t.Errorf("ModifiedLines = %d, want 2 (Cursor file-level)", result.ModifiedLines)
	}
	if result.FilesTouched != 2 {
		t.Errorf("FilesTouched = %d, want 2", result.FilesTouched)
	}
}

func TestFormatAttributionTrailers_SingleProvider(t *testing.T) {
	r := &AIPercentResult{
		Percent:    40,
		TotalLines: 250,
		AILines:    100,
		Providers: []ProviderAttribution{
			{Provider: "claude_code", AILines: 100},
		},
	}
	trailers := formatAttributionTrailers(r, 250)
	if len(trailers) != 1 {
		t.Fatalf("got %d trailers, want 1", len(trailers))
	}
	want := "Semantica-Attribution: 40% claude_code (100/250 lines)"
	if trailers[0] != want {
		t.Errorf("trailer = %q, want %q", trailers[0], want)
	}
}

func TestFormatAttributionTrailers_MultiProvider(t *testing.T) {
	r := &AIPercentResult{
		Percent:    60,
		TotalLines: 250,
		AILines:    150,
		Providers: []ProviderAttribution{
			{Provider: "claude_code", AILines: 100},
			{Provider: "cursor", AILines: 50},
		},
	}
	trailers := formatAttributionTrailers(r, 250)
	if len(trailers) != 2 {
		t.Fatalf("got %d trailers, want 2", len(trailers))
	}
	want0 := "Semantica-Attribution: 40% claude_code (100/250 lines)"
	want1 := "Semantica-Attribution: 20% cursor (50/250 lines)"
	if trailers[0] != want0 {
		t.Errorf("trailers[0] = %q, want %q", trailers[0], want0)
	}
	if trailers[1] != want1 {
		t.Errorf("trailers[1] = %q, want %q", trailers[1], want1)
	}
}

func TestFormatAttributionTrailers_WithModel(t *testing.T) {
	r := &AIPercentResult{
		Percent:    75,
		TotalLines: 200,
		AILines:    150,
		Providers: []ProviderAttribution{
			{Provider: "claude_code", Model: "opus 4.6", AILines: 150},
		},
	}
	trailers := formatAttributionTrailers(r, 200)
	if len(trailers) != 1 {
		t.Fatalf("got %d trailers, want 1", len(trailers))
	}
	want := "Semantica-Attribution: 75% claude_code (opus 4.6) (150/200 lines)"
	if trailers[0] != want {
		t.Errorf("trailer = %q, want %q", trailers[0], want)
	}
}

func TestFormatAttributionTrailers_NoProviders(t *testing.T) {
	r := &AIPercentResult{
		Percent:    50,
		TotalLines: 100,
		AILines:    50,
	}
	trailers := formatAttributionTrailers(r, 100)
	if len(trailers) != 1 {
		t.Fatalf("got %d trailers, want 1", len(trailers))
	}
	want := "Semantica-Attribution: 50% (50/100 lines)"
	if trailers[0] != want {
		t.Errorf("trailer = %q, want %q", trailers[0], want)
	}
}

func TestFormatDiagnosticsTrailer(t *testing.T) {
	cr := &commitAttrResult{
		result: &AIPercentResult{
			FilesTouched:   15,
			ExactLines:     120,
			ModifiedLines:  20,
			FormattedLines: 10,
			TotalLines:     150,
			AILines:        150,
		},
		totalLines: 150,
	}
	got := formatDiagnosticsTrailer(cr)
	want := "Semantica-Diagnostics: 15 files, lines: 120 exact, 20 modified, 10 formatted"
	if got != want {
		t.Errorf("diagnostics = %q, want %q", got, want)
	}
}

func TestScanForTrailers_NewFormat(t *testing.T) {
	msg := "Fix bug\n\nSemantica-Checkpoint: abc123\nSemantica-Attribution: 40% claude_code (100/250 lines)\nSemantica-Diagnostics: 5 files, lines: 80 exact, 15 modified, 5 formatted\n"
	hasCP, hasAttr, hasDiag := scanForTrailers(msg)
	if !hasCP {
		t.Error("expected hasCheckpoint=true")
	}
	if !hasAttr {
		t.Error("expected hasAttribution=true")
	}
	if !hasDiag {
		t.Error("expected hasDiagnostics=true")
	}
}

func TestScanForTrailers_OldFormatNotDetected(t *testing.T) {
	msg := "Fix bug\n\nSemantica-Session: sess-123\nSemantica-AI: 73%\n"
	hasCP, hasAttr, hasDiag := scanForTrailers(msg)
	if hasCP {
		t.Error("expected hasCheckpoint=false")
	}
	if hasAttr {
		t.Error("expected hasAttribution=false (old AI trailer is not Attribution)")
	}
	if hasDiag {
		t.Error("expected hasDiagnostics=false")
	}
}

// carryForwardCandidates tests.

func TestCarryForwardCandidates(t *testing.T) {
	tests := []struct {
		name         string
		filesCreated []string
		manifest     []blobs.ManifestFile
		want         map[string]bool
	}{
		{
			name:         "created file in manifest",
			filesCreated: []string{"a.go"},
			manifest:     []blobs.ManifestFile{{Path: "a.go"}, {Path: "b.go"}},
			want:         map[string]bool{"a.go": true},
		},
		{
			name:         "created file NOT in manifest",
			filesCreated: []string{"new.go"},
			manifest:     []blobs.ManifestFile{{Path: "b.go"}},
			want:         nil,
		},
		{
			name:         "no created files",
			filesCreated: nil,
			manifest:     []blobs.ManifestFile{{Path: "a.go"}},
			want:         nil,
		},
		{
			name:         "mix of in and not in manifest",
			filesCreated: []string{"a.go", "new.go"},
			manifest:     []blobs.ManifestFile{{Path: "a.go"}, {Path: "b.go"}},
			want:         map[string]bool{"a.go": true},
		},
		{
			name:         "empty manifest",
			filesCreated: []string{"a.go"},
			manifest:     nil,
			want:         nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dr := diffResult{filesCreated: tt.filesCreated}
			got := carryForwardCandidates(dr, tt.manifest)
			if tt.want == nil {
				if got != nil {
					t.Errorf("expected nil, got %v", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for k := range tt.want {
				if !got[k] {
					t.Errorf("missing key %q", k)
				}
			}
		})
	}
}

// scoreDiffPerFile tests.

func TestScoreDiffPerFile_ReturnsPerFileResults(t *testing.T) {
	cands := aiCandidates{
		aiLines: map[string]map[string]struct{}{
			"a.go": {"func main() {": {}, "return nil": {}},
			"b.go": {"package b": {}},
		},
		providerTouchedFiles: make(map[string]string),
		fileProvider:         map[string]string{"a.go": "claude_code", "b.go": "claude_code"},
		providerModel:        map[string]string{"claude_code": "opus 4.6"},
	}

	diff := strings.Join([]string{
		"diff --git a/a.go b/a.go",
		"--- /dev/null",
		"+++ b/a.go",
		"@@ -0,0 +1,3 @@",
		"+func main() {",
		"+\treturn nil",
		"+}",
		"diff --git a/b.go b/b.go",
		"--- /dev/null",
		"+++ b/b.go",
		"@@ -0,0 +1,2 @@",
		"+package b",
		"+",
	}, "\n")

	dr := parseDiff([]byte(diff))
	scores, _ := scoreDiffPerFile(dr, cands)

	if len(scores) != 2 {
		t.Fatalf("expected 2 file scores, got %d", len(scores))
	}

	// Find scores by path.
	byPath := map[string]fileScore{}
	for _, fs := range scores {
		byPath[fs.path] = fs
	}

	// a.go: 2 exact (func main, return nil), 1 modified (})
	aScore := byPath["a.go"]
	if aScore.exactLines != 2 {
		t.Errorf("a.go exactLines = %d, want 2", aScore.exactLines)
	}
	if aScore.totalLines != 3 {
		t.Errorf("a.go totalLines = %d, want 3", aScore.totalLines)
	}

	// b.go: 1 exact (package b)
	bScore := byPath["b.go"]
	if bScore.exactLines != 1 {
		t.Errorf("b.go exactLines = %d, want 1", bScore.exactLines)
	}
	if bScore.totalLines != 1 {
		t.Errorf("b.go totalLines = %d, want 1", bScore.totalLines)
	}
}

// aggregateFileScores tests.

func TestAggregateFileScores_SumsCorrectly(t *testing.T) {
	scores := []fileScore{
		{
			path:           "a.go",
			totalLines:     10,
			exactLines:     5,
			formattedLines: 2,
			modifiedLines:  1,
			humanLines:     2,
			providerLines:  map[string]int{"claude_code": 8},
		},
		{
			path:           "b.go",
			totalLines:     5,
			exactLines:     3,
			formattedLines: 0,
			modifiedLines:  0,
			humanLines:     2,
			providerLines:  map[string]int{"cursor": 3},
		},
	}
	provModel := map[string]string{
		"claude_code": "opus 4.6",
		"cursor":      "",
	}

	result := aggregateFileScores(scores, provModel, 2)

	if result.TotalLines != 15 {
		t.Errorf("TotalLines = %d, want 15", result.TotalLines)
	}
	if result.AILines != 11 { // 5+2+1 + 3+0+0
		t.Errorf("AILines = %d, want 11", result.AILines)
	}
	if result.ExactLines != 8 {
		t.Errorf("ExactLines = %d, want 8", result.ExactLines)
	}
	if result.FormattedLines != 2 {
		t.Errorf("FormattedLines = %d, want 2", result.FormattedLines)
	}
	if result.FilesTouched != 2 {
		t.Errorf("FilesTouched = %d, want 2", result.FilesTouched)
	}
	if len(result.Providers) != 2 {
		t.Fatalf("Providers count = %d, want 2", len(result.Providers))
	}
	// claude_code (8) > cursor (3)
	if result.Providers[0].Provider != "claude_code" {
		t.Errorf("Providers[0].Provider = %q, want claude_code", result.Providers[0].Provider)
	}
	if result.Providers[0].Model != "opus 4.6" {
		t.Errorf("Providers[0].Model = %q, want opus 4.6", result.Providers[0].Model)
	}
}

// Integration test for eligible-file gating through toEventRows and
// BuildCandidatesFromRows.

func TestBuildAICandidates_EligibleFileGating(t *testing.T) {
	h := testDB(t)

	repoID := insertRepo(t, h, 100_000)
	srcID := insertSource(t, h, repoID, "/fake/source.jsonl")
	sessID := insertSession(t, h, repoID, srcID, "session-1")

	bsDir := t.TempDir()
	bs, _ := blobs.NewStore(bsDir)

	// Create an event with a payload that touches both a.go and b.go.
	payload := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"/test/repo/` + repoID + `/a.go","content":"package a\n"}},{"type":"tool_use","name":"Write","input":{"file_path":"/test/repo/` + repoID + `/b.go","content":"package b\n"}}]}}`
	payloadHash, _, _ := bs.Put(context.Background(), []byte(payload))

	eventID := "event-gating-test"
	tuJSON := `{"content_types":["tool_use"],"tools":[{"name":"Write","file_path":"a.go","file_op":"write"},{"name":"Write","file_path":"b.go","file_op":"write"}]}`
	_ = h.Queries.InsertAgentEvent(context.Background(), sqldb.InsertAgentEventParams{
		EventID:      eventID,
		SessionID:    sessID,
		RepositoryID: repoID,
		Ts:           200_000,
		Kind:         "assistant",
		Role:         sqlstore.NullStr("assistant"),
		ToolUses:     sql.NullString{String: tuJSON, Valid: true},
		PayloadHash:  sqlstore.NullStr(payloadHash),
		Summary:      sqlstore.NullStr("Wrote a.go and b.go"),
	})

	events, err := h.Queries.ListEventsInWindow(context.Background(), sqldb.ListEventsInWindowParams{
		RepositoryID: repoID,
		AfterTs:      0,
		UpToTs:       300_000,
	})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}

	repoRoot := "/test/repo/" + repoID
	eventRows := toEventRows(context.Background(), bs, events)

	// Without gating: both files should appear.
	candsAll, _ := attrevents.BuildCandidatesFromRows(eventRows, repoRoot, nil)
	if len(candsAll.AILines) != 2 {
		t.Errorf("ungated: expected 2 files in AILines, got %d", len(candsAll.AILines))
	}

	// With gating to a.go only: only a.go should appear.
	candsGated, _ := attrevents.BuildCandidatesFromRows(eventRows, repoRoot, map[string]bool{"a.go": true})
	if len(candsGated.AILines) != 1 {
		t.Fatalf("gated: expected 1 file in AILines, got %d", len(candsGated.AILines))
	}
	if _, ok := candsGated.AILines["a.go"]; !ok {
		t.Error("gated: expected a.go in AILines")
	}
	if _, ok := candsGated.AILines["b.go"]; ok {
		t.Error("gated: b.go should NOT be in AILines")
	}
}

// End-to-end carry-forward tests.

// insertEventWithPayload creates a claude_code assistant event with a Write
// payload for the specified file content. Returns the event ID.
func insertEventWithPayload(t *testing.T, h *sqlstore.Handle, bs *blobs.Store, sessID, repoID, repoRoot string, ts int64, filePath, fileContent string) string {
	t.Helper()
	ctx := context.Background()
	eventID := uuid.NewString()

	payload := fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"%s/%s","content":"%s"}}]}}`,
		repoRoot, filePath, strings.ReplaceAll(fileContent, "\n", "\\n"))
	payloadHash, _, _ := bs.Put(ctx, []byte(payload))

	tuJSON := fmt.Sprintf(`{"content_types":["tool_use"],"tools":[{"name":"Write","file_path":"%s","file_op":"write"}]}`, filePath)
	if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID:      eventID,
		SessionID:    sessID,
		RepositoryID: repoID,
		Ts:           ts,
		Kind:         "assistant",
		Role:         sqlstore.NullStr("assistant"),
		ToolUses:     sql.NullString{String: tuJSON, Valid: true},
		PayloadHash:  sqlstore.NullStr(payloadHash),
		Summary:      sqlstore.NullStr("Wrote " + filePath),
	}); err != nil {
		t.Fatalf("insert event with payload: %v", err)
	}
	return eventID
}

// insertCheckpointWithManifest creates a completed checkpoint and stores
// a manifest blob containing the given file paths. Returns the checkpoint ID.
func insertCheckpointWithManifest(t *testing.T, h *sqlstore.Handle, bs *blobs.Store, repoID string, createdAt int64, filePaths []string) string {
	t.Helper()
	ctx := context.Background()

	var files []blobs.ManifestFile
	for _, p := range filePaths {
		files = append(files, blobs.ManifestFile{Path: p, Blob: "fakehash-" + p, Size: 100})
	}
	manifest := blobs.Manifest{Version: 1, CreatedAt: createdAt, Files: files}
	raw, _ := json.Marshal(manifest)
	manifestHash, _, _ := bs.Put(ctx, raw)

	cpID := uuid.NewString()
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpID,
		RepositoryID: repoID,
		CreatedAt:    createdAt,
		Kind:         "auto",
		Trigger:      sqlstore.NullStr("test"),
		Message:      sqlstore.NullStr(fmt.Sprintf("checkpoint at %d", createdAt)),
		ManifestHash: sqlstore.NullStr(manifestHash),
		SizeBytes:    sql.NullInt64{Int64: 100, Valid: true},
		Status:       "complete",
		CompletedAt:  sql.NullInt64{Int64: createdAt, Valid: true},
	}); err != nil {
		t.Fatalf("insert checkpoint with manifest: %v", err)
	}
	return cpID
}

func TestCarryForward_MixedWindow(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()

	bsDir := t.TempDir()
	bs, _ := blobs.NewStore(bsDir)
	semDir := t.TempDir()

	repoID := insertRepo(t, h, 100_000)
	repoRoot := "/test/repo/" + repoID
	srcID := insertSource(t, h, repoID, "/fake/source.jsonl")
	sessID := insertSession(t, h, repoID, srcID, "session-1")

	// T=100_000: AI creates both create.go and edit.go
	_ = insertEventWithPayload(t, h, bs, sessID, repoID, repoRoot,
		100_000, "create.go", "package create\nfunc New() {}\n")
	_ = insertEventWithPayload(t, h, bs, sessID, repoID, repoRoot,
		100_000, "edit.go", "package edit\nfunc Handle() {}\n")

	// T=200_000: CP1 with manifest containing both files, linked to commit
	cp1ID := insertCheckpointWithManifest(t, h, bs, repoID, 200_000, []string{"edit.go", "create.go"})
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash:   "commit1hash",
		RepositoryID: repoID,
		CheckpointID: cp1ID,
		LinkedAt:     200_000,
	}); err != nil {
		t.Fatalf("insert commit link: %v", err)
	}

	// T=250_000: New event touching only edit.go (current-window activity)
	_ = insertEventWithPayload(t, h, bs, sessID, repoID, repoRoot,
		250_000, "edit.go", "package edit\nfunc Handle() {}\nfunc Process() {}\n")

	// T=300_000: CP2 (current)
	_ = insertCheckpointWithManifest(t, h, bs, repoID, 300_000, []string{"edit.go", "create.go"})

	// Build diff with edit.go as modified and create.go as new file.
	diff := strings.Join([]string{
		"diff --git a/edit.go b/edit.go",
		"--- a/edit.go",
		"+++ b/edit.go",
		"@@ -1,2 +1,3 @@",
		" package edit",
		"+func Handle() {}",
		"+func Process() {}",
		"diff --git a/create.go b/create.go",
		"--- /dev/null",
		"+++ b/create.go",
		"@@ -0,0 +1,2 @@",
		"+package create",
		"+func New() {}",
	}, "\n")

	// Get CP1 for prevCP
	cp1, err := h.Queries.GetCheckpointByID(ctx, cp1ID)
	if err != nil {
		t.Fatalf("get cp1: %v", err)
	}

	input := ComputeAIPercentInput{
		RepoRoot: repoRoot,
		RepoID:   repoID,
		AfterTs:  cp1.CreatedAt, // 200_000
		UpToTs:   300_000,
	}

	cfr, err := attributeWithCarryForward(ctx, h, bs, []byte(diff), input, &cp1, semDir)
	if err != nil {
		t.Fatalf("attributeWithCarryForward: %v", err)
	}

	if cfr.noEvents {
		t.Error("expected noEvents=false")
	}

	// Both files should have AI attribution.
	if cfr.result.AILines == 0 {
		t.Fatal("expected AILines > 0")
	}
	// edit.go has current-window AI, create.go has historical carry-forward AI.
	// Total: 4 lines (2 from edit.go + 2 from create.go), all AI.
	if cfr.result.TotalLines != 4 {
		t.Errorf("TotalLines = %d, want 4", cfr.result.TotalLines)
	}
	if cfr.result.AILines < 3 {
		t.Errorf("AILines = %d, want >= 3 (both files should be attributed)", cfr.result.AILines)
	}
}

// TestCarryForward_HistoricalBoundUsesCP1CreatedAt verifies the window bounds
// used for historical carry-forward.
func TestCarryForward_HistoricalBoundUsesCP1CreatedAt(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()

	bsDir := t.TempDir()
	bs, _ := blobs.NewStore(bsDir)

	repoID := insertRepo(t, h, 100_000)
	repoRoot := "/test/repo/" + repoID
	srcID := insertSource(t, h, repoID, "/fake/source.jsonl")
	sessID := insertSession(t, h, repoID, srcID, "session-1")

	// T=100_000: AI writes create.go (within historical window).
	ev1ID := insertEventWithPayload(t, h, bs, sessID, repoID, repoRoot,
		100_000, "create.go", "package create\nfunc New() {}\n")

	// T=200_000 is included at the upper bound.
	evBoundaryID := insertEventWithPayload(t, h, bs, sessID, repoID, repoRoot,
		200_000, "create.go", "package create\nfunc Extra() {}\n")

	// T=200_001 is excluded from the historical window and included in the
	// current window.
	evPostCP1ID := insertEventWithPayload(t, h, bs, sessID, repoID, repoRoot,
		200_001, "create.go", "package create\nfunc PostCP1() {}\n")

	// Historical window: (0, 200_000].
	histEvents, err := loadWindowEvents(ctx, h, ComputeAIPercentInput{
		RepoRoot: repoRoot,
		RepoID:   repoID,
		AfterTs:  0,
		UpToTs:   200_000,
	})
	if err != nil {
		t.Fatalf("loadWindowEvents (historical): %v", err)
	}

	foundEv1, foundBoundary, foundPostCP1 := false, false, false
	for _, ev := range histEvents {
		switch ev.EventID {
		case ev1ID:
			foundEv1 = true
		case evBoundaryID:
			foundBoundary = true
		case evPostCP1ID:
			foundPostCP1 = true
		}
	}
	if !foundEv1 {
		t.Error("historical window should include T=100_000 event")
	}
	if !foundBoundary {
		t.Error("historical window should include T=200_000 event (at boundary, ts <= UpToTs)")
	}
	if foundPostCP1 {
		t.Error("historical window must NOT include T=200_001 event (ts > UpToTs=200_000)")
	}

	// Current window: (200_000, 300_000].
	currentEvents, err := loadWindowEvents(ctx, h, ComputeAIPercentInput{
		RepoRoot: repoRoot,
		RepoID:   repoID,
		AfterTs:  200_000,
		UpToTs:   300_000,
	})
	if err != nil {
		t.Fatalf("loadWindowEvents (current): %v", err)
	}

	foundPostCP1 = false
	for _, ev := range currentEvents {
		if ev.EventID == evPostCP1ID {
			foundPostCP1 = true
		}
		if ev.EventID == ev1ID || ev.EventID == evBoundaryID {
			t.Errorf("current window should NOT include event %s (ts <= AfterTs=200_000)", ev.EventID)
		}
	}
	if !foundPostCP1 {
		t.Error("current window should include T=200_001 event")
	}
}

func TestCarryForward_NoOverride(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()

	bsDir := t.TempDir()
	bs, _ := blobs.NewStore(bsDir)
	semDir := t.TempDir()

	repoID := insertRepo(t, h, 100_000)
	repoRoot := "/test/repo/" + repoID
	srcID := insertSource(t, h, repoID, "/fake/source.jsonl")
	sessID := insertSession(t, h, repoID, srcID, "session-1")

	// T=100_000: AI creates foo.go
	_ = insertEventWithPayload(t, h, bs, sessID, repoID, repoRoot,
		100_000, "foo.go", "package foo\nfunc Old() {}\n")

	// T=200_000: CP1 with manifest, linked to commit
	cp1ID := insertCheckpointWithManifest(t, h, bs, repoID, 200_000, []string{"foo.go"})
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash:   "commit1hash",
		RepositoryID: repoID,
		CheckpointID: cp1ID,
		LinkedAt:     200_000,
	}); err != nil {
		t.Fatalf("insert commit link: %v", err)
	}

	// T=250_000: AI also edits foo.go in the current window
	_ = insertEventWithPayload(t, h, bs, sessID, repoID, repoRoot,
		250_000, "foo.go", "package foo\nfunc New() {}\n")

	// T=300_000: CP2
	_ = insertCheckpointWithManifest(t, h, bs, repoID, 300_000, []string{"foo.go"})

	// Diff: foo.go as new file.
	diff := strings.Join([]string{
		"diff --git a/foo.go b/foo.go",
		"--- /dev/null",
		"+++ b/foo.go",
		"@@ -0,0 +1,2 @@",
		"+package foo",
		"+func New() {}",
	}, "\n")

	cp1, err := h.Queries.GetCheckpointByID(ctx, cp1ID)
	if err != nil {
		t.Fatalf("get cp1: %v", err)
	}

	input := ComputeAIPercentInput{
		RepoRoot: repoRoot,
		RepoID:   repoID,
		AfterTs:  cp1.CreatedAt,
		UpToTs:   300_000,
	}

	cfr, err := attributeWithCarryForward(ctx, h, bs, []byte(diff), input, &cp1, semDir)
	if err != nil {
		t.Fatalf("attributeWithCarryForward: %v", err)
	}

	// Current-window attribution should remain authoritative for foo.go.
	if cfr.result.AILines == 0 {
		t.Error("expected AILines > 0 from current window")
	}
	// The result should use current-window AI, not historical.
	// "func New() {}" is in the current window, so it should match.
	if cfr.result.ExactLines == 0 {
		t.Error("expected ExactLines > 0 from current window match")
	}
}

func TestCarryForward_PerFileMerge(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()

	bsDir := t.TempDir()
	bs, _ := blobs.NewStore(bsDir)
	semDir := t.TempDir()

	repoID := insertRepo(t, h, 100_000)
	repoRoot := "/test/repo/" + repoID
	srcID := insertSource(t, h, repoID, "/fake/source.jsonl")
	sessID := insertSession(t, h, repoID, srcID, "session-1")

	// T=100_000: AI creates fileB.go
	_ = insertEventWithPayload(t, h, bs, sessID, repoID, repoRoot,
		100_000, "fileB.go", "package b\nfunc B() {}\n")

	// T=200_000: CP1 with manifest, linked to commit
	cp1ID := insertCheckpointWithManifest(t, h, bs, repoID, 200_000, []string{"fileB.go"})
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash:   "commit1hash",
		RepositoryID: repoID,
		CheckpointID: cp1ID,
		LinkedAt:     200_000,
	}); err != nil {
		t.Fatalf("insert commit link: %v", err)
	}

	// T=250_000: AI creates fileA.go (current window)
	_ = insertEventWithPayload(t, h, bs, sessID, repoID, repoRoot,
		250_000, "fileA.go", "package a\nfunc A() {}\n")

	// T=300_000: CP2
	_ = insertCheckpointWithManifest(t, h, bs, repoID, 300_000, []string{"fileA.go", "fileB.go"})

	// Diff: fileA.go edited, fileB.go created (deferred)
	diff := strings.Join([]string{
		"diff --git a/fileA.go b/fileA.go",
		"--- /dev/null",
		"+++ b/fileA.go",
		"@@ -0,0 +1,2 @@",
		"+package a",
		"+func A() {}",
		"diff --git a/fileB.go b/fileB.go",
		"--- /dev/null",
		"+++ b/fileB.go",
		"@@ -0,0 +1,2 @@",
		"+package b",
		"+func B() {}",
	}, "\n")

	cp1, err := h.Queries.GetCheckpointByID(ctx, cp1ID)
	if err != nil {
		t.Fatalf("get cp1: %v", err)
	}

	input := ComputeAIPercentInput{
		RepoRoot: repoRoot,
		RepoID:   repoID,
		AfterTs:  cp1.CreatedAt,
		UpToTs:   300_000,
	}

	cfr, err := attributeWithCarryForward(ctx, h, bs, []byte(diff), input, &cp1, semDir)
	if err != nil {
		t.Fatalf("attributeWithCarryForward: %v", err)
	}

	// Both files contribute: fileA from current window, fileB from historical.
	// Total: 4 lines, all AI.
	if cfr.result.TotalLines != 4 {
		t.Errorf("TotalLines = %d, want 4", cfr.result.TotalLines)
	}
	if cfr.result.AILines != 4 {
		t.Errorf("AILines = %d, want 4 (both files fully AI)", cfr.result.AILines)
	}
}

func TestCarryForward_ProviderMerge(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()

	bsDir := t.TempDir()
	bs, _ := blobs.NewStore(bsDir)
	semDir := t.TempDir()

	repoID := insertRepo(t, h, 100_000)
	repoRoot := "/test/repo/" + repoID

	// Historical window: cursor touches fileB.go
	cursorSrcID := insertCursorSource(t, h, repoID, "/fake/cursor.db")
	cursorSessID := insertCursorSession(t, h, repoID, cursorSrcID, "cursor-1")
	_ = insertCursorEvent(t, h, cursorSessID, repoID, 100_000, "fileB.go")

	// T=200_000: CP1 with manifest, linked to commit
	cp1ID := insertCheckpointWithManifest(t, h, bs, repoID, 200_000, []string{"fileB.go"})
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash:   "commit1hash",
		RepositoryID: repoID,
		CheckpointID: cp1ID,
		LinkedAt:     200_000,
	}); err != nil {
		t.Fatalf("insert commit link: %v", err)
	}

	// Current window: claude touches fileA.go
	claudeSrcID := insertSource(t, h, repoID, "/fake/source.jsonl")
	claudeSessID := insertSession(t, h, repoID, claudeSrcID, "claude-1")
	_ = insertEventWithPayload(t, h, bs, claudeSessID, repoID, repoRoot,
		250_000, "fileA.go", "package a\nfunc A() {}\n")

	// T=300_000: CP2
	_ = insertCheckpointWithManifest(t, h, bs, repoID, 300_000, []string{"fileA.go", "fileB.go"})

	// Diff: both files as new
	diff := strings.Join([]string{
		"diff --git a/fileA.go b/fileA.go",
		"--- /dev/null",
		"+++ b/fileA.go",
		"@@ -0,0 +1,2 @@",
		"+package a",
		"+func A() {}",
		"diff --git a/fileB.go b/fileB.go",
		"--- /dev/null",
		"+++ b/fileB.go",
		"@@ -0,0 +1,2 @@",
		"+package b",
		"+func B() {}",
	}, "\n")

	cp1, err := h.Queries.GetCheckpointByID(ctx, cp1ID)
	if err != nil {
		t.Fatalf("get cp1: %v", err)
	}

	input := ComputeAIPercentInput{
		RepoRoot: repoRoot,
		RepoID:   repoID,
		AfterTs:  cp1.CreatedAt,
		UpToTs:   300_000,
	}

	cfr, err := attributeWithCarryForward(ctx, h, bs, []byte(diff), input, &cp1, semDir)
	if err != nil {
		t.Fatalf("attributeWithCarryForward: %v", err)
	}

	// Both providers should appear: claude_code from current, cursor from historical.
	if len(cfr.result.Providers) < 2 {
		t.Errorf("Providers count = %d, want >= 2 (claude_code + cursor)", len(cfr.result.Providers))
	}

	provMap := map[string]int{}
	for _, p := range cfr.result.Providers {
		provMap[p.Provider] = p.AILines
	}
	if provMap["claude_code"] == 0 {
		t.Error("expected claude_code provider with AILines > 0")
	}
	if provMap["cursor"] == 0 {
		t.Error("expected cursor provider with AILines > 0")
	}
}

func TestCarryForward_NilPrevCP(t *testing.T) {
	h := testDB(t)

	bsDir := t.TempDir()
	bs, _ := blobs.NewStore(bsDir)
	semDir := t.TempDir()

	repoID := insertRepo(t, h, 100_000)
	repoRoot := "/test/repo/" + repoID
	srcID := insertSource(t, h, repoID, "/fake/source.jsonl")
	sessID := insertSession(t, h, repoID, srcID, "session-1")

	// AI event in the window
	_ = insertEventWithPayload(t, h, bs, sessID, repoID, repoRoot,
		200_000, "main.go", "package main\nfunc main() {}\n")

	diff := strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"--- /dev/null",
		"+++ b/main.go",
		"@@ -0,0 +1,2 @@",
		"+package main",
		"+func main() {}",
	}, "\n")

	// nil prevCP = no carry-forward, current-window only
	cfr, err := attributeWithCarryForward(context.Background(), h, bs, []byte(diff), ComputeAIPercentInput{
		RepoRoot: repoRoot,
		RepoID:   repoID,
		AfterTs:  0,
		UpToTs:   300_000,
	}, nil, semDir)
	if err != nil {
		t.Fatalf("attributeWithCarryForward: %v", err)
	}

	if cfr.result.AILines != 2 {
		t.Errorf("AILines = %d, want 2", cfr.result.AILines)
	}
	if cfr.noEvents {
		t.Error("expected noEvents=false")
	}
}

// TestCarryForward_EventsWithNoCandidates verifies that carry-forward still
// runs when the current window contains events but produces no candidates.
func TestCarryForward_EventsWithNoCandidates(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()

	bsDir := t.TempDir()
	bs, _ := blobs.NewStore(bsDir)
	semDir := t.TempDir()

	repoID := insertRepo(t, h, 100_000)
	repoRoot := "/test/repo/" + repoID
	srcID := insertSource(t, h, repoID, "/fake/source.jsonl")
	sessID := insertSession(t, h, repoID, srcID, "session-1")

	// T=100_000: historical write for create.go.
	_ = insertEventWithPayload(t, h, bs, sessID, repoID, repoRoot,
		100_000, "create.go", "package create\nfunc New() {}\n")

	// T=200_000: CP1 with manifest, linked to commit.
	cp1ID := insertCheckpointWithManifest(t, h, bs, repoID, 200_000, []string{"create.go"})
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash:   "commit1hash",
		RepositoryID: repoID,
		CheckpointID: cp1ID,
		LinkedAt:     200_000,
	}); err != nil {
		t.Fatalf("insert commit link: %v", err)
	}

	// T=250_000: current-window event with a missing payload blob.
	eventID := uuid.NewString()
	tuJSON := `{"content_types":["tool_use"],"tools":[{"name":"Write","file_path":"whatever.go","file_op":"write"}]}`
	if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID:      eventID,
		SessionID:    sessID,
		RepositoryID: repoID,
		Ts:           250_000,
		Kind:         "assistant",
		Role:         sqlstore.NullStr("assistant"),
		ToolUses:     sql.NullString{String: tuJSON, Valid: true},
		PayloadHash:  sqlstore.NullStr("nonexistent-blob-hash"),
		Summary:      sqlstore.NullStr("broken event"),
	}); err != nil {
		t.Fatalf("insert broken event: %v", err)
	}

	// T=300_000: CP2
	_ = insertCheckpointWithManifest(t, h, bs, repoID, 300_000, []string{"create.go"})

	// Diff: create.go as new file.
	diff := strings.Join([]string{
		"diff --git a/create.go b/create.go",
		"--- /dev/null",
		"+++ b/create.go",
		"@@ -0,0 +1,2 @@",
		"+package create",
		"+func New() {}",
	}, "\n")

	cp1, err := h.Queries.GetCheckpointByID(ctx, cp1ID)
	if err != nil {
		t.Fatalf("get cp1: %v", err)
	}

	input := ComputeAIPercentInput{
		RepoRoot: repoRoot,
		RepoID:   repoID,
		AfterTs:  cp1.CreatedAt,
		UpToTs:   300_000,
	}

	cfr, err := attributeWithCarryForward(ctx, h, bs, []byte(diff), input, &cp1, semDir)
	if err != nil {
		t.Fatalf("attributeWithCarryForward: %v", err)
	}

	// create.go should still be attributed via historical carry-forward.
	if cfr.result.AILines != 2 {
		t.Errorf("AILines = %d, want 2 (carry-forward despite broken current-window events)", cfr.result.AILines)
	}
	if cfr.noEvents {
		t.Error("expected noEvents=false (current window had events, even if broken)")
	}
}

func TestCarryForward_NoEventsInBothWindows(t *testing.T) {
	h := testDB(t)

	bsDir := t.TempDir()
	bs, _ := blobs.NewStore(bsDir)
	semDir := t.TempDir()

	repoID := insertRepo(t, h, 100_000)
	repoRoot := "/test/repo/" + repoID

	// CP1 with manifest, linked to commit - but no events at all
	cp1ID := insertCheckpointWithManifest(t, h, bs, repoID, 200_000, []string{"create.go"})
	if err := h.Queries.InsertCommitLink(context.Background(), sqldb.InsertCommitLinkParams{
		CommitHash:   "commit1hash",
		RepositoryID: repoID,
		CheckpointID: cp1ID,
		LinkedAt:     200_000,
	}); err != nil {
		t.Fatalf("insert commit link: %v", err)
	}

	diff := strings.Join([]string{
		"diff --git a/create.go b/create.go",
		"--- /dev/null",
		"+++ b/create.go",
		"@@ -0,0 +1,1 @@",
		"+package create",
	}, "\n")

	cp1, _ := h.Queries.GetCheckpointByID(context.Background(), cp1ID)

	_, err := attributeWithCarryForward(context.Background(), h, bs, []byte(diff), ComputeAIPercentInput{
		RepoRoot: repoRoot,
		RepoID:   repoID,
		AfterTs:  cp1.CreatedAt,
		UpToTs:   300_000,
	}, &cp1, semDir)

	// Should return ErrNoEventsInWindow with noEvents=true
	if !errors.Is(err, ErrNoEventsInWindow) {
		t.Errorf("expected ErrNoEventsInWindow, got %v", err)
	}
}

// TestAttributeCommit_CarryForward verifies carry-forward through the public
// AttributeCommit API.
func TestAttributeCommit_CarryForward(t *testing.T) {
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	ctx := context.Background()

	gitInit := exec.Command("git", "init", dir)
	gitInit.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := gitInit.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	gitCmd := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@test.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	if err := os.WriteFile(filepath.Join(dir, "edit.go"), []byte("package edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd("add", "edit.go")
	gitCmd("commit", "-m", "initial")

	if err := os.WriteFile(filepath.Join(dir, "edit.go"),
		[]byte("package edit\nfunc Handle() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd("add", "edit.go")
	gitCmd("commit", "-m", "first: edit only")
	commit1Hash := gitCmd("rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(dir, "edit.go"),
		[]byte("package edit\nfunc Handle() {}\nfunc Process() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "create.go"),
		[]byte("package create\nfunc New() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd("add", "edit.go", "create.go")
	gitCmd("commit", "-m", "second: edit + create")
	commit2Hash := gitCmd("rev-parse", "HEAD")

	semDir := filepath.Join(dir, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	objectsDir := filepath.Join(semDir, "objects")
	bs, err := blobs.NewStore(objectsDir)
	if err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(semDir, "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.OpenOptions{
		BusyTimeout: 200,
		Synchronous: "NORMAL",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	repoID := uuid.NewString()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID,
		RootPath:     dir,
		CreatedAt:    50_000,
		EnabledAt:    50_000,
	}); err != nil {
		t.Fatal(err)
	}

	srcRow, err := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID:     uuid.NewString(),
		RepositoryID: repoID,
		SourceKey:    "/fake/source.jsonl",
		Provider:     "claude_code",
		LastSeenAt:   50_000,
		CreatedAt:    50_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessRow, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID:         uuid.NewString(),
		ProviderSessionID: "test-session",
		RepositoryID:      repoID,
		Provider:          "claude_code",
		SourceID:          srcRow.SourceID,
		StartedAt:         50_000,
		LastSeenAt:        50_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessID := sessRow.SessionID

	insertEvt := func(ts int64, filePath, content string) {
		t.Helper()
		eventID := uuid.NewString()
		payload := fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"%s/%s","content":"%s"}}]}}`,
			dir, filePath, strings.ReplaceAll(content, "\n", "\\n"))
		payloadHash, _, _ := bs.Put(ctx, []byte(payload))
		tuJSON := fmt.Sprintf(`{"content_types":["tool_use"],"tools":[{"name":"Write","file_path":"%s","file_op":"write"}]}`, filePath)
		if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
			EventID:      eventID,
			SessionID:    sessID,
			RepositoryID: repoID,
			Ts:           ts,
			Kind:         "assistant",
			Role:         sqlstore.NullStr("assistant"),
			ToolUses:     sql.NullString{String: tuJSON, Valid: true},
			PayloadHash:  sqlstore.NullStr(payloadHash),
			Summary:      sqlstore.NullStr("Wrote " + filePath),
		}); err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}

	insertCP := func(createdAt int64, manifestFiles []string) string {
		t.Helper()
		var files []blobs.ManifestFile
		for _, p := range manifestFiles {
			files = append(files, blobs.ManifestFile{Path: p, Blob: "fakehash-" + p, Size: 100})
		}
		manifest := blobs.Manifest{Version: 1, CreatedAt: createdAt, Files: files}
		raw, _ := json.Marshal(manifest)
		manifestHash, _, _ := bs.Put(ctx, raw)
		cpID := uuid.NewString()
		if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
			CheckpointID: cpID,
			RepositoryID: repoID,
			CreatedAt:    createdAt,
			Kind:         "auto",
			Trigger:      sqlstore.NullStr("test"),
			Message:      sqlstore.NullStr(fmt.Sprintf("cp at %d", createdAt)),
			ManifestHash: sqlstore.NullStr(manifestHash),
			SizeBytes:    sql.NullInt64{Int64: 100, Valid: true},
			Status:       "complete",
			CompletedAt:  sql.NullInt64{Int64: createdAt, Valid: true},
		}); err != nil {
			t.Fatalf("insert checkpoint: %v", err)
		}
		return cpID
	}

	// T=100_000: AI creates both edit.go and create.go
	insertEvt(100_000, "edit.go", "package edit\nfunc Handle() {}\n")
	insertEvt(100_000, "create.go", "package create\nfunc New() {}\n")

	// T=200_000: CP1 with manifest (both files), linked to first commit.
	cp1ID := insertCP(200_000, []string{"edit.go", "create.go"})
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash:   commit1Hash,
		RepositoryID: repoID,
		CheckpointID: cp1ID,
		LinkedAt:     200_000,
	}); err != nil {
		t.Fatal(err)
	}

	// T=250_000: AI edits edit.go (current-window activity).
	insertEvt(250_000, "edit.go", "package edit\nfunc Handle() {}\nfunc Process() {}\n")

	// T=300_000: CP2, linked to second commit.
	cp2ID := insertCP(300_000, []string{"edit.go", "create.go"})
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash:   commit2Hash,
		RepositoryID: repoID,
		CheckpointID: cp2ID,
		LinkedAt:     300_000,
	}); err != nil {
		t.Fatal(err)
	}

	svc := NewAttributionService()
	result, err := svc.AttributeCommit(ctx, AttributionInput{
		RepoPath:   dir,
		CommitHash: commit2Hash,
	})
	if err != nil {
		t.Fatalf("AttributeCommit: %v", err)
	}

	// Both files should have AI attribution.
	if result.AILines == 0 {
		t.Fatal("expected AILines > 0")
	}
	if result.AIPercentage == 0 {
		t.Fatal("expected AIPercentage > 0")
	}

	// Verify create.go is attributed (carry-forward from historical window).
	foundCreateAI := false
	for _, f := range result.Files {
		if f.Path == "create.go" {
			aiLines := f.AIExactLines + f.AIFormattedLines + f.AIModifiedLines
			if aiLines > 0 {
				foundCreateAI = true
			}
		}
	}
	if !foundCreateAI {
		t.Error("create.go should have AI attribution via carry-forward, got 0")
	}

	// Verify edit.go is attributed (current window).
	foundEditAI := false
	for _, f := range result.Files {
		if f.Path == "edit.go" {
			aiLines := f.AIExactLines + f.AIFormattedLines + f.AIModifiedLines
			if aiLines > 0 {
				foundEditAI = true
			}
		}
	}
	if !foundEditAI {
		t.Error("edit.go should have AI attribution from current window, got 0")
	}

	// create.go should appear in FilesCreated with AI=true.
	foundInCreated := false
	for _, fc := range result.FilesCreated {
		if fc.Path == "create.go" {
			foundInCreated = true
			if !fc.AI {
				t.Error("create.go in FilesCreated should have AI=true")
			}
		}
	}
	if !foundInCreated {
		t.Error("create.go should appear in FilesCreated")
	}
}

