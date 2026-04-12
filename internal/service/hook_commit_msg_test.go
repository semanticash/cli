package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

func TestFormatAttributionTrailers_Aggregate(t *testing.T) {
	r := &AIPercentResult{
		Percent:    40,
		TotalLines: 250,
		AILines:    100,
	}
	trailers := formatAttributionTrailers(r, 250)
	if len(trailers) != 1 {
		t.Fatalf("trailers: got %d, want 1", len(trailers))
	}
	want := "Semantica-Attribution: 40% (100/250 lines)"
	if trailers[0] != want {
		t.Errorf("trailer: got %q, want %q", trailers[0], want)
	}
}

func TestFormatAttributionTrailers_PerProvider(t *testing.T) {
	r := &AIPercentResult{
		Percent:    60,
		TotalLines: 200,
		AILines:    120,
		Providers: []ProviderAttribution{
			{Provider: "claude_code", Model: "opus 4.6", AILines: 80},
			{Provider: "cursor", AILines: 40},
		},
	}
	trailers := formatAttributionTrailers(r, 200)
	if len(trailers) != 2 {
		t.Fatalf("trailers: got %d, want 2", len(trailers))
	}
	want0 := "Semantica-Attribution: 40% claude_code (opus 4.6) (80/200 lines)"
	if trailers[0] != want0 {
		t.Errorf("trailer[0]: got %q, want %q", trailers[0], want0)
	}
	want1 := "Semantica-Attribution: 20% cursor (40/200 lines)"
	if trailers[1] != want1 {
		t.Errorf("trailer[1]: got %q, want %q", trailers[1], want1)
	}
}

func TestFormatAttributionTrailers_ZeroLines(t *testing.T) {
	r := &AIPercentResult{
		Percent:    0,
		TotalLines: 0,
		AILines:    0,
	}
	trailers := formatAttributionTrailers(r, 0)
	if trailers != nil {
		t.Errorf("expected nil trailers for zero lines, got %v", trailers)
	}
}

func TestFormatAttributionTrailers_NoEvents(t *testing.T) {
	trailers := formatAttributionTrailers(nil, 141)
	if len(trailers) != 1 {
		t.Fatalf("trailers: got %d, want 1", len(trailers))
	}
	want := "Semantica-Attribution: 0% AI detected (0/141 lines)"
	if trailers[0] != want {
		t.Errorf("trailer: got %q, want %q", trailers[0], want)
	}
}

func TestFormatAttributionTrailers_EventsNoMatch(t *testing.T) {
	r := &AIPercentResult{
		Percent:    0,
		TotalLines: 0,
		AILines:    0,
	}
	trailers := formatAttributionTrailers(r, 141)
	if len(trailers) != 1 {
		t.Fatalf("trailers: got %d, want 1", len(trailers))
	}
	want := "Semantica-Attribution: 0% AI detected (0/141 lines)"
	if trailers[0] != want {
		t.Errorf("trailer: got %q, want %q", trailers[0], want)
	}
}

func TestFormatDiagnosticsTrailer_CommitMsg(t *testing.T) {
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
		t.Errorf("diagnostics: got %q, want %q", got, want)
	}
}

func TestFormatDiagnosticsTrailer_NoEvents(t *testing.T) {
	cr := &commitAttrResult{totalLines: 141, noEvents: true}
	got := formatDiagnosticsTrailer(cr)
	want := "Semantica-Diagnostics: no AI events found in the checkpoint window"
	if got != want {
		t.Errorf("diagnostics: got %q, want %q", got, want)
	}
}

func TestFormatDiagnosticsTrailer_EventsNoMatch(t *testing.T) {
	cr := &commitAttrResult{
		result:     &AIPercentResult{TotalLines: 0, AILines: 0},
		totalLines: 141,
	}
	got := formatDiagnosticsTrailer(cr)
	want := "Semantica-Diagnostics: AI session events found, but no file-modifying changes matched this commit"
	if got != want {
		t.Errorf("diagnostics: got %q, want %q", got, want)
	}
}

func TestFormatDiagnosticsTrailer_EventsTouchedDifferentFiles(t *testing.T) {
	// ComputeAIPercentFromDiff returns TotalLines > 0 when the diff has
	// lines but the AI events touched different files - AILines stays 0.
	cr := &commitAttrResult{
		result: &AIPercentResult{
			TotalLines:   141,
			AILines:      0,
			FilesTouched: 3,
		},
		totalLines: 141,
	}
	got := formatDiagnosticsTrailer(cr)
	want := "Semantica-Diagnostics: AI session events found, but no file-modifying changes matched this commit"
	if got != want {
		t.Errorf("diagnostics: got %q, want %q", got, want)
	}
}

func TestScanForTrailers(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		wantCP   bool
		wantAttr bool
		wantDiag bool
	}{
		{
			name:     "no trailers",
			text:     "Fix bug in parser\n\nThis fixes a segfault.\n",
			wantCP:   false,
			wantAttr: false,
			wantDiag: false,
		},
		{
			name:     "checkpoint only",
			text:     "Fix bug\n\nSemantica-Checkpoint: abc-123\n",
			wantCP:   true,
			wantAttr: false,
			wantDiag: false,
		},
		{
			name:     "attribution only",
			text:     "Fix bug\n\nSemantica-Attribution: 40% claude_code (100/250 lines)\n",
			wantCP:   false,
			wantAttr: true,
			wantDiag: false,
		},
		{
			name:     "diagnostics only",
			text:     "Fix bug\n\nSemantica-Diagnostics: 5 files, lines: 10 exact, 2 modified, 1 formatted\n",
			wantCP:   false,
			wantAttr: false,
			wantDiag: true,
		},
		{
			name:     "all three",
			text:     "Fix bug\n\nSemantica-Checkpoint: abc-123\nSemantica-Attribution: 40% (100/250 lines)\nSemantica-Diagnostics: 5 files, lines: 10 exact, 0 modified, 0 formatted\n",
			wantCP:   true,
			wantAttr: true,
			wantDiag: true,
		},
		{
			name:     "with leading whitespace",
			text:     "Fix bug\n\n  Semantica-Checkpoint: abc-123\n",
			wantCP:   true,
			wantAttr: false,
			wantDiag: false,
		},
		{
			name:     "with CRLF",
			text:     "Fix bug\r\n\r\nSemantica-Checkpoint: abc-123\r\n",
			wantCP:   true,
			wantAttr: false,
			wantDiag: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCP, gotAttr, gotDiag := scanForTrailers(tt.text)
			if gotCP != tt.wantCP {
				t.Errorf("checkpoint: got %v, want %v", gotCP, tt.wantCP)
			}
			if gotAttr != tt.wantAttr {
				t.Errorf("attribution: got %v, want %v", gotAttr, tt.wantAttr)
			}
			if gotDiag != tt.wantDiag {
				t.Errorf("diagnostics: got %v, want %v", gotDiag, tt.wantDiag)
			}
		})
	}
}

// TestRun_FallbackDiagnosticsTrailer verifies that when checkpointID is set
// but computeAttribution returns nil (e.g. no repository row in DB), the hook
// appends "Semantica-Diagnostics: attribution unavailable" instead of silently
// producing a checkpoint-only commit message.
func TestRun_FallbackDiagnosticsTrailer(t *testing.T) {
	dir := t.TempDir()

	// Minimal git repo structure so OpenRepo succeeds.
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	// Semantica enabled marker.
	semDir := filepath.Join(dir, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatalf("mkdir .semantica: %v", err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatalf("write enabled: %v", err)
	}

	// Create the DB so sqlstore.Open succeeds, but do NOT insert a
	// repository row - this forces GetRepositoryByRootPath to fail,
	// which makes computeAttribution return nil.
	ctx := context.Background()
	dbPath := filepath.Join(semDir, "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.OpenOptions{
		BusyTimeout: 200 * time.Millisecond,
		Synchronous: "NORMAL",
	})
	if err != nil {
		t.Fatalf("sqlstore.Open: %v", err)
	}
	if err := sqlstore.Close(h); err != nil {
		t.Fatalf("sqlstore.Close: %v", err)
	}

	// Write the handoff file with a fake checkpoint ID.
	handoffPath := filepath.Join(semDir, ".pre-commit-checkpoint")
	if err := os.WriteFile(handoffPath, []byte("fake-checkpoint-id"), 0o644); err != nil {
		t.Fatalf("write handoff: %v", err)
	}

	// Write the commit message file.
	msgFile := filepath.Join(dir, "COMMIT_EDITMSG")
	if err := os.WriteFile(msgFile, []byte("test commit\n"), 0o644); err != nil {
		t.Fatalf("write msg: %v", err)
	}

	svc := NewCommitMsgHookService(dir)
	if err := svc.Run(ctx, msgFile); err != nil {
		t.Fatalf("Run: %v", err)
	}

	result, err := os.ReadFile(msgFile)
	if err != nil {
		t.Fatalf("read msg: %v", err)
	}
	text := string(result)

	if !strings.Contains(text, "Semantica-Checkpoint: fake-checkpoint-id") {
		t.Errorf("missing checkpoint trailer in:\n%s", text)
	}
	if !strings.Contains(text, "Semantica-Diagnostics: attribution unavailable") {
		t.Errorf("missing fallback diagnostics trailer in:\n%s", text)
	}
	if strings.Contains(text, "Semantica-Attribution:") {
		t.Errorf("unexpected attribution trailer in:\n%s", text)
	}

	// Verify activity.log captured the bailout reason.
	logData, err := os.ReadFile(filepath.Join(semDir, "activity.log"))
	if err != nil {
		t.Fatalf("read activity.log: %v", err)
	}
	if !strings.Contains(string(logData), "commit-msg warning: get repository row failed:") {
		t.Errorf("activity.log missing bailout warning, got:\n%s", logData)
	}
}

// TestRun_CarryForwardTrailer verifies carry-forward through the commit-msg
// hook path.
func TestRun_CarryForwardTrailer(t *testing.T) {
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

	gitCmd := func(args ...string) {
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
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, "edit.go"), []byte("package edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd("add", "edit.go")
	gitCmd("commit", "-m", "initial")

	semDir := filepath.Join(dir, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// Blob store at .semantica/objects (where the hook looks for it).
	objectsDir := filepath.Join(semDir, "objects")
	bs, err := blobs.NewStore(objectsDir)
	if err != nil {
		t.Fatal(err)
	}

	// Open DB.
	dbPath := filepath.Join(semDir, "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.OpenOptions{
		BusyTimeout: 200 * time.Millisecond,
		Synchronous: "NORMAL",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	// Insert repository row with the actual repo path.
	repoID := uuid.NewString()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID,
		RootPath:     dir,
		CreatedAt:    50_000,
		EnabledAt:    50_000,
	}); err != nil {
		t.Fatal(err)
	}

	// Insert source and session.
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

	// Helper to insert an AI event with a Write payload.
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

	// Helper to insert a checkpoint with manifest.
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

	// T=200_000: CP1 with manifest containing both files, linked to first commit.
	cp1ID := insertCP(200_000, []string{"edit.go", "create.go"})
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash:   "commit1hash",
		RepositoryID: repoID,
		CheckpointID: cp1ID,
		LinkedAt:     200_000,
	}); err != nil {
		t.Fatal(err)
	}

	// T=250_000: New AI event touching only edit.go (current-window activity).
	insertEvt(250_000, "edit.go", "package edit\nfunc Handle() {}\nfunc Process() {}\n")

	// T=300_000: CP2 (current).
	cp2ID := insertCP(300_000, []string{"edit.go", "create.go"})

	// Write the handoff file with CP2's ID.
	handoffPath := filepath.Join(semDir, ".pre-commit-checkpoint")
	if err := os.WriteFile(handoffPath, []byte(cp2ID), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "edit.go"),
		[]byte("package edit\nfunc Handle() {}\nfunc Process() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "create.go"),
		[]byte("package create\nfunc New() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd("add", "edit.go", "create.go")

	// Write the commit message file.
	msgFile := filepath.Join(dir, "COMMIT_EDITMSG")
	if err := os.WriteFile(msgFile, []byte("second commit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := NewCommitMsgHookService(dir)
	if err := svc.Run(ctx, msgFile); err != nil {
		t.Fatalf("Run: %v", err)
	}

	result, err := os.ReadFile(msgFile)
	if err != nil {
		t.Fatal(err)
	}
	text := string(result)

	if !strings.Contains(text, "Semantica-Checkpoint: "+cp2ID) {
		t.Errorf("missing checkpoint trailer in:\n%s", text)
	}

	if !strings.Contains(text, "Semantica-Attribution:") {
		t.Errorf("missing attribution trailer in:\n%s", text)
	}
	// The trailer should not fall back to the zero-AI form.
	if strings.Contains(text, "0% AI detected") {
		t.Errorf("unexpected 0%% AI attribution (carry-forward should have matched):\n%s", text)
	}

	if strings.Contains(text, "no AI events") {
		t.Errorf("unexpected 'no AI events' diagnostics:\n%s", text)
	}
	if strings.Contains(text, "attribution unavailable") {
		t.Errorf("unexpected 'attribution unavailable' diagnostics:\n%s", text)
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		startMs int64
		endMs   int64
		want    string
	}{
		{0, 0, "0s"},
		{0, 30000, "30s"},
		{0, 90000, "1m"},
		{0, 3600000, "1h"},
		{0, 5400000, "1h30m"},
		{100, 50, "0s"}, // negative
	}
	for _, tt := range tests {
		got := FormatDuration(tt.startMs, tt.endMs)
		if got != tt.want {
			t.Errorf("FormatDuration(%d, %d) = %q, want %q", tt.startMs, tt.endMs, got, tt.want)
		}
	}
}
