package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	attrevents "github.com/semanticash/cli/internal/attribution/events"
	attrreporting "github.com/semanticash/cli/internal/attribution/reporting"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/toolsnap"
)

// setupDeltaRepo creates line and touch-only tool-delta evidence.
func setupDeltaRepo(t *testing.T) (string, string) {
	t.Helper()

	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	t.Setenv("SEMANTICA_HOME", filepath.Join(dir, ".semantica-global"))
	t.Setenv("HOME", dir)

	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	git("config", "user.email", "test@test.com")
	git("config", "user.name", "Test")
	mustWriteFile(t, filepath.Join(dir, "README"), []byte("init\n"))
	git("add", "README")
	git("commit", "-m", "initial")

	semDir := filepath.Join(dir, ".semantica")
	mustMkdirAll(t, semDir)
	mustWriteFile(t, filepath.Join(semDir, "enabled"), nil)
	mustMkdirAll(t, filepath.Join(semDir, "objects"))

	ctx := context.Background()
	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}

	repo := mustOpenRepo(t, dir)
	repoRoot := repo.Root()
	repoID := uuid.NewString()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: repoRoot,
		CreatedAt: 100_000, EnabledAt: 100_000,
	}); err != nil {
		t.Fatalf("insert repository: %v", err)
	}
	baselineID := uuid.NewString()
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: baselineID, RepositoryID: repoID,
		CreatedAt: 100_000, Kind: "baseline", Status: "complete",
		CompletedAt: sql.NullInt64{Int64: 100_000, Valid: true},
	}); err != nil {
		t.Fatalf("insert baseline checkpoint: %v", err)
	}

	bs, err := blobs.NewStore(filepath.Join(semDir, "objects"))
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	srcID := insertSource(t, h, repoID, "/data/session.jsonl")
	sessID := insertSessionWithProvider(t, h, repoID, srcID, uuid.NewString(), "claude_code")

	// The Bash event has no direct Edit or Write evidence.
	eventID := uuid.NewString()
	payload := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"gofmt -w gen.go"}}]}}`
	payloadHash, _, err := bs.Put(ctx, []byte(payload))
	if err != nil {
		t.Fatalf("store payload: %v", err)
	}
	if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID: eventID, SessionID: sessID, RepositoryID: repoID, Ts: 200_000,
		Kind: "assistant", Role: sqlstore.NullStr("assistant"),
		ToolUses:    sql.NullString{String: `{"content_types":["tool_use"],"tools":[{"name":"Bash"}]}`, Valid: true},
		PayloadHash: sqlstore.NullStr(payloadHash), Summary: sqlstore.NullStr("Ran gofmt"),
	}); err != nil {
		t.Fatalf("insert agent event: %v", err)
	}

	// The delta claims gen.go and records notes.txt as touch-only.
	delta := &toolsnap.Delta{
		Scope: "tool", Status: "complete",
		Window: toolsnap.Window{StartedAt: 199_000, CompletedAt: 200_000, DurationMS: 1000},
		Actors: []toolsnap.Actor{{Provider: "claude_code", SessionID: sessID, TurnID: "t1"}},
		ToolUses: []toolsnap.ToolUse{{
			ToolUseID: "toolu_1", ToolName: "Bash", EventID: eventID, Actor: 0,
		}},
		Files: []toolsnap.FileDelta{
			{
				Path: "gen.go", Operation: "edit",
				BeforeHash: "a", AfterHash: "b",
				BeforeMode: "100644", AfterMode: "100644",
				Hunks: []toolsnap.Hunk{{
					OldStart: 1, OldCount: 0, NewStart: 1, NewCount: 2,
					NewLines: []string{"package gen", "func Generated() {}"},
				}},
			},
			{
				Path: "notes.txt", Operation: "edit",
				BeforeHash: "a", AfterHash: "b",
				BeforeMode: "100644", AfterMode: "100644", Truncated: true,
			},
			{
				// This content does not survive into the commit.
				Path: "rewritten.go", Operation: "edit",
				BeforeHash: "a", AfterHash: "b",
				BeforeMode: "100644", AfterMode: "100644",
				Hunks: []toolsnap.Hunk{{
					OldStart: 1, OldCount: 0, NewStart: 1, NewCount: 1,
					NewLines: []string{"tool wrote this line"},
				}},
			},
		},
		Limits: toolsnap.Limits{FilesObserved: 3, Truncated: true},
	}
	raw, err := delta.CanonicalBytes()
	if err != nil {
		t.Fatalf("canonical delta: %v", err)
	}
	deltaHash, _, err := bs.Put(ctx, raw)
	if err != nil {
		t.Fatalf("store delta: %v", err)
	}
	if err := h.Queries.InsertEvidenceLinkIfAbsent(ctx, sqldb.InsertEvidenceLinkIfAbsentParams{
		EventID: eventID, EvidenceKind: "tool_delta",
		EvidenceHash: deltaHash, GroupID: uuid.NewString(), CreatedAt: 200_000,
	}); err != nil {
		t.Fatalf("insert evidence link: %v", err)
	}

	// Commit the bash-written files.
	mustWriteFile(t, filepath.Join(dir, "gen.go"), []byte("package gen\nfunc Generated() {}\n"))
	mustWriteFile(t, filepath.Join(dir, "notes.txt"), []byte("first note\nsecond note\n"))
	mustWriteFile(t, filepath.Join(dir, "rewritten.go"), []byte("human rewrote everything\n"))
	git("add", "gen.go", "notes.txt", "rewritten.go")
	git("commit", "-m", "bash-mediated changes")
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	commitHash := strings.TrimSpace(string(out))

	cpID := uuid.NewString()
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpID, RepositoryID: repoID,
		CreatedAt: 300_000, Kind: "auto", Status: "complete",
		CompletedAt: sql.NullInt64{Int64: 300_000, Valid: true},
	}); err != nil {
		t.Fatalf("insert linked checkpoint: %v", err)
	}
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: commitHash, RepositoryID: repoID,
		CheckpointID: cpID, LinkedAt: 300_000,
	}); err != nil {
		t.Fatalf("insert commit link: %v", err)
	}

	_ = sqlstore.Close(h)
	return dir, commitHash
}

func fileByPath(t *testing.T, files []FileAttribution, path string) FileAttribution {
	t.Helper()
	for _, f := range files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("file %q not in result: %+v", path, files)
	return FileAttribution{}
}

// Disabled tool-delta scoring keeps Bash-written lines unclaimed.
func TestAttributionV2_FlagOff(t *testing.T) {
	dir, commitHash := setupDeltaRepo(t)
	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "0")

	svc := NewAttributionService()
	result, err := svc.AttributeCommit(context.Background(), AttributionInput{RepoPath: dir, CommitHash: commitHash})
	if err != nil {
		t.Fatalf("AttributeCommit: %v", err)
	}

	if result.AttrVersion != "v1" {
		t.Errorf("AttrVersion = %q, want v1", result.AttrVersion)
	}
	if result.AILines != 0 || result.AIDeltaExactLines != 0 {
		t.Errorf("v1 attributed delta evidence: AILines=%d delta=%d", result.AILines, result.AIDeltaExactLines)
	}
	gen := fileByPath(t, result.Files, "gen.go")
	if gen.HumanLines != 2 || gen.AIExactLines != 0 {
		t.Errorf("gen.go = %+v, want fully human under v1", gen)
	}
	d := result.Diagnostics
	if d.DeltaGroupsEligible != 0 || d.DeltaExactMatches != 0 {
		t.Errorf("v1 populated delta diagnostics: %+v", d)
	}
}

// Enabled tool-delta scoring attributes lines and touch-only files.
func TestAttributionV2_FlagOn(t *testing.T) {
	dir, commitHash := setupDeltaRepo(t)
	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "1")

	svc := NewAttributionService()
	result, err := svc.AttributeCommit(context.Background(), AttributionInput{RepoPath: dir, CommitHash: commitHash})
	if err != nil {
		t.Fatalf("AttributeCommit: %v", err)
	}

	if result.AttrVersion != "v2" {
		t.Errorf("AttrVersion = %q, want v2", result.AttrVersion)
	}

	gen := fileByPath(t, result.Files, "gen.go")
	if gen.AIExactLines != 2 || gen.HumanLines != 0 {
		t.Errorf("gen.go = %+v, want both delta-claimed lines exact", gen)
	}
	if gen.AIDeltaExactLines != 2 {
		t.Errorf("gen.go delta counter = %d, want 2", gen.AIDeltaExactLines)
	}
	if gen.EvidenceClass != string(attrreporting.EvidenceExact) {
		t.Errorf("gen.go evidence = %q, want exact", gen.EvidenceClass)
	}

	notes := fileByPath(t, result.Files, "notes.txt")
	if notes.AIProviderOnlyLines != 2 {
		t.Errorf("notes.txt = %+v, want touch-only lines in the provider-only sidecar", notes)
	}
	if notes.EvidenceClass != string(attrreporting.EvidenceToolDeltaTouch) {
		t.Errorf("notes.txt evidence = %q, want tool_delta_touch", notes.EvidenceClass)
	}

	// Unmatched claims do not attribute later content.
	rewritten := fileByPath(t, result.Files, "rewritten.go")
	if rewritten.HumanLines != 1 || rewritten.AIExactLines != 0 || rewritten.AIProviderOnlyLines != 0 {
		t.Errorf("rewritten.go = %+v, want fully human", rewritten)
	}
	if rewritten.EvidenceClass != string(attrreporting.EvidenceNone) {
		t.Errorf("rewritten.go evidence = %q, want none", rewritten.EvidenceClass)
	}
	if rewritten.Classification == "ai" {
		t.Error("rewritten.go marked AI despite no surviving claims")
	}

	if result.AILines != 2 || result.AIDeltaExactLines != 2 {
		t.Errorf("headline = AILines %d / delta %d, want 2/2", result.AILines, result.AIDeltaExactLines)
	}
	if result.AIProviderOnlyLines != 2 {
		t.Errorf("AIProviderOnlyLines = %d, want 2", result.AIProviderOnlyLines)
	}

	d := result.Diagnostics
	if d.DeltaGroupsEligible != 1 || d.DeltaGroupsRejected != 0 {
		t.Errorf("delta groups = %d/%d, want 1 eligible, 0 rejected", d.DeltaGroupsEligible, d.DeltaGroupsRejected)
	}
	if d.DeltaExactMatches != 2 {
		t.Errorf("DeltaExactMatches = %d, want 2 (per-line counter)", d.DeltaExactMatches)
	}

	for _, provs := range [][]FileChange{result.FilesCreated, result.FilesEdited} {
		for _, f := range provs {
			wantAI := f.Path != "rewritten.go"
			if f.AI != wantAI {
				t.Errorf("file %q AI = %v, want %v", f.Path, f.AI, wantAI)
			}
		}
	}
}

// Worker enrichment follows the selected scoring version.
func TestAttributeWithCarryForward_V2Parity(t *testing.T) {
	dir, _ := setupDeltaRepo(t)
	ctx := context.Background()
	semDir := filepath.Join(dir, ".semantica")
	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	bs, err := blobs.NewStore(filepath.Join(semDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	var repoID string
	if err := h.DB.QueryRowContext(ctx, "select repository_id from repositories").Scan(&repoID); err != nil {
		t.Fatalf("read repository id: %v", err)
	}

	diff := strings.Join([]string{
		"diff --git a/gen.go b/gen.go",
		"--- /dev/null",
		"+++ b/gen.go",
		"@@ -0,0 +1,2 @@",
		"+package gen",
		"+func Generated() {}",
		"",
	}, "\n")
	in := ComputeAIPercentInput{RepoRoot: dir, RepoID: repoID, Window: tsWindow(100_000, 300_000)}

	v1, err := attributeWithCarryForward(ctx, h, bs, []byte(diff), in, nil, semDir, false)
	if err != nil {
		t.Fatalf("v1: %v", err)
	}
	if v1.result.AILines != 0 {
		t.Errorf("v1 AILines = %d, want 0 (bash-mediated lines invisible)", v1.result.AILines)
	}

	v2, err := attributeWithCarryForward(ctx, h, bs, []byte(diff), in, nil, semDir, true)
	if err != nil {
		t.Fatalf("v2: %v", err)
	}
	if v2.result.AILines != 2 || v2.result.Percent != 100 {
		t.Errorf("v2 = %d lines / %.0f%%, want 2 / 100", v2.result.AILines, v2.result.Percent)
	}
}

// setupHistoricalDeltaRepo creates delta evidence carried across a checkpoint.
func setupHistoricalDeltaRepo(t *testing.T) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	t.Setenv("SEMANTICA_HOME", filepath.Join(dir, ".semantica-global"))
	t.Setenv("HOME", dir)

	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	git("config", "user.email", "test@test.com")
	git("config", "user.name", "Test")
	mustWriteFile(t, filepath.Join(dir, "README"), []byte("init\n"))
	git("add", "README")
	git("commit", "-m", "initial")
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	firstCommit := strings.TrimSpace(string(out))

	semDir := filepath.Join(dir, ".semantica")
	mustMkdirAll(t, semDir)
	mustWriteFile(t, filepath.Join(semDir, "enabled"), nil)
	mustMkdirAll(t, filepath.Join(semDir, "objects"))

	ctx := context.Background()
	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	repo := mustOpenRepo(t, dir)
	repoRoot := repo.Root()
	repoID := uuid.NewString()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: repoRoot,
		CreatedAt: 100_000, EnabledAt: 100_000,
	}); err != nil {
		t.Fatal(err)
	}
	bs, err := blobs.NewStore(filepath.Join(semDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	srcID := insertSource(t, h, repoID, "/data/session.jsonl")
	sessID := insertSessionWithProvider(t, h, repoID, srcID, uuid.NewString(), "claude_code")

	// The event precedes the checkpoint while the file remains uncommitted.
	eventID := uuid.NewString()
	payload := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"make generate"}}]}}`
	payloadHash, _, err := bs.Put(ctx, []byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID: eventID, SessionID: sessID, RepositoryID: repoID, Ts: 120_000,
		Kind: "assistant", Role: sqlstore.NullStr("assistant"),
		ToolUses:    sql.NullString{String: `{"content_types":["tool_use"],"tools":[{"name":"Bash"}]}`, Valid: true},
		PayloadHash: sqlstore.NullStr(payloadHash), Summary: sqlstore.NullStr("Ran make generate"),
	}); err != nil {
		t.Fatal(err)
	}
	delta := &toolsnap.Delta{
		Scope: "tool", Status: "complete",
		Window: toolsnap.Window{StartedAt: 119_000, CompletedAt: 120_000, DurationMS: 1000},
		Actors: []toolsnap.Actor{{Provider: "claude_code", SessionID: sessID, TurnID: "t1"}},
		ToolUses: []toolsnap.ToolUse{{
			ToolUseID: "toolu_h1", ToolName: "Bash", EventID: eventID, Actor: 0,
		}},
		Files: []toolsnap.FileDelta{{
			Path: "gen2.go", Operation: "create",
			BeforeHash: "", AfterHash: "b",
			BeforeMode: "", AfterMode: "100644",
			Hunks: []toolsnap.Hunk{{
				OldStart: 1, OldCount: 0, NewStart: 1, NewCount: 2,
				NewLines: []string{"package gen2", "func Generated2() {}"},
			}},
		}},
		Limits: toolsnap.Limits{FilesObserved: 1},
	}
	raw, err := delta.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	deltaHash, _, err := bs.Put(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertEvidenceLinkIfAbsent(ctx, sqldb.InsertEvidenceLinkIfAbsentParams{
		EventID: eventID, EvidenceKind: "tool_delta",
		EvidenceHash: deltaHash, GroupID: uuid.NewString(), CreatedAt: 120_000,
	}); err != nil {
		t.Fatal(err)
	}

	// The manifest makes gen2.go eligible for carry-forward.
	manifest, _ := json.Marshal(blobs.Manifest{
		Version: 1, CreatedAt: 150_000, RepoRoot: repoRoot,
		Files: []blobs.ManifestFile{{Path: "gen2.go", Blob: "x", Size: 40}},
	})
	manifestHash, _, err := bs.Put(ctx, manifest)
	if err != nil {
		t.Fatal(err)
	}
	cpA := uuid.NewString()
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpA, RepositoryID: repoID,
		CreatedAt: 150_000, Kind: "auto", Status: "complete",
		ManifestHash: sqlstore.NullStr(manifestHash),
		CompletedAt:  sql.NullInt64{Int64: 150_000, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: firstCommit, RepositoryID: repoID,
		CheckpointID: cpA, LinkedAt: 150_000,
	}); err != nil {
		t.Fatal(err)
	}

	// The next commit adds gen2.go without current-window events.
	mustWriteFile(t, filepath.Join(dir, "gen2.go"), []byte("package gen2\nfunc Generated2() {}\n"))
	git("add", "gen2.go")
	git("commit", "-m", "commit generated file")
	cmd = exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err = cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	secondCommit := strings.TrimSpace(string(out))
	cpB := uuid.NewString()
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpB, RepositoryID: repoID,
		CreatedAt: 400_000, Kind: "auto", Status: "complete",
		CompletedAt: sql.NullInt64{Int64: 400_000, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: secondCommit, RepositoryID: repoID,
		CheckpointID: cpB, LinkedAt: 400_000,
	}); err != nil {
		t.Fatal(err)
	}
	_ = sqlstore.Close(h)
	return dir, secondCommit, cpA
}

// Historical tool deltas participate in carry-forward.
func TestAttributionV2_HistoricalDeltaCarryForward(t *testing.T) {
	dir, secondCommit, _ := setupHistoricalDeltaRepo(t)
	ctx := context.Background()

	// V1 ignores the historical delta.
	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "0")
	svc := NewAttributionService()
	result, err := svc.AttributeCommit(ctx, AttributionInput{RepoPath: dir, CommitHash: secondCommit})
	if err != nil {
		t.Fatalf("v1 AttributeCommit: %v", err)
	}
	if result.AILines != 0 {
		t.Errorf("v1 AILines = %d, want 0", result.AILines)
	}

	// V2 carries the historical delta forward.
	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "1")
	result, err = svc.AttributeCommit(ctx, AttributionInput{RepoPath: dir, CommitHash: secondCommit})
	if err != nil {
		t.Fatalf("v2 AttributeCommit: %v", err)
	}
	gen := fileByPath(t, result.Files, "gen2.go")
	if gen.AIExactLines != 2 || gen.AIDeltaExactLines != 2 || gen.HumanLines != 0 {
		t.Fatalf("gen2.go = %+v, want both historical delta lines attributed", gen)
	}
	hasCF := false
	for _, c := range gen.EvidenceClasses {
		if c == string(attrreporting.EvidenceCarryForward) {
			hasCF = true
		}
	}
	if !hasCF {
		t.Errorf("gen2.go classes = %v, want carry_forward recorded", gen.EvidenceClasses)
	}
	if result.AttrVersion != "v2" {
		t.Errorf("AttrVersion = %q, want v2", result.AttrVersion)
	}
}

// Refused claims retain their provider and other file-touch providers.
func TestAttributionV2_RefusedDeltaKeepsTouchProvider(t *testing.T) {
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	t.Setenv("SEMANTICA_HOME", filepath.Join(dir, ".semantica-global"))
	t.Setenv("HOME", dir)
	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "1")

	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	git("config", "user.email", "test@test.com")
	git("config", "user.name", "Test")
	mustWriteFile(t, filepath.Join(dir, "README"), []byte("init\n"))
	git("add", "README")
	git("commit", "-m", "initial")

	semDir := filepath.Join(dir, ".semantica")
	mustMkdirAll(t, semDir)
	mustWriteFile(t, filepath.Join(semDir, "enabled"), nil)
	mustMkdirAll(t, filepath.Join(semDir, "objects"))

	ctx := context.Background()
	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	repo := mustOpenRepo(t, dir)
	repoID := uuid.NewString()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: repo.Root(),
		CreatedAt: 100_000, EnabledAt: 100_000,
	}); err != nil {
		t.Fatal(err)
	}
	baselineID := uuid.NewString()
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: baselineID, RepositoryID: repoID,
		CreatedAt: 100_000, Kind: "baseline", Status: "complete",
		CompletedAt: sql.NullInt64{Int64: 100_000, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	bs, err := blobs.NewStore(filepath.Join(semDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	srcID := insertSource(t, h, repoID, "/data/session.jsonl")
	claudeSess := insertSessionWithProvider(t, h, repoID, srcID, uuid.NewString(), "claude_code")
	cursorSess := insertSessionWithProvider(t, h, repoID, srcID, uuid.NewString(), "cursor")

	// Cursor touches the file via a provider file-edit event.
	if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID: uuid.NewString(), SessionID: cursorSess, RepositoryID: repoID, Ts: 190_000,
		Kind: "assistant", Role: sqlstore.NullStr("assistant"),
		ToolUses: sql.NullString{String: `{"content_types":["cursor_file_edit"],"tools":[{"name":"cursor_edit","file_path":"big.go","file_op":"edit"}]}`, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	// A 2101 by 2101 alignment exceeds the work limit.
	const n = 2100
	claims := make([]string, n)
	var committed strings.Builder
	for i := 0; i < n; i++ {
		claims[i] = fmt.Sprintf("claim %d", i)
		fmt.Fprintf(&committed, "line %d\n", i)
	}
	eventID := uuid.NewString()
	payload := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"make generate"}}]}}`
	payloadHash, _, err := bs.Put(ctx, []byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID: eventID, SessionID: claudeSess, RepositoryID: repoID, Ts: 200_000,
		Kind: "assistant", Role: sqlstore.NullStr("assistant"),
		ToolUses:    sql.NullString{String: `{"content_types":["tool_use"],"tools":[{"name":"Bash"}]}`, Valid: true},
		PayloadHash: sqlstore.NullStr(payloadHash),
	}); err != nil {
		t.Fatal(err)
	}
	delta := &toolsnap.Delta{
		Scope: "tool", Status: "complete",
		Window: toolsnap.Window{StartedAt: 199_000, CompletedAt: 200_000, DurationMS: 1000},
		Actors: []toolsnap.Actor{{Provider: "claude_code", SessionID: claudeSess, TurnID: "t1"}},
		ToolUses: []toolsnap.ToolUse{{
			ToolUseID: "toolu_big", ToolName: "Bash", EventID: eventID, Actor: 0,
		}},
		Files: []toolsnap.FileDelta{{
			Path: "big.go", Operation: "create",
			BeforeHash: "", AfterHash: "b",
			BeforeMode: "", AfterMode: "100644",
			Hunks: []toolsnap.Hunk{{
				OldStart: 1, OldCount: 0, NewStart: 1, NewCount: n, NewLines: claims,
			}},
		}},
		Limits: toolsnap.Limits{FilesObserved: 1},
	}
	raw, err := delta.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	deltaHash, _, err := bs.Put(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertEvidenceLinkIfAbsent(ctx, sqldb.InsertEvidenceLinkIfAbsentParams{
		EventID: eventID, EvidenceKind: "tool_delta",
		EvidenceHash: deltaHash, GroupID: uuid.NewString(), CreatedAt: 200_000,
	}); err != nil {
		t.Fatal(err)
	}

	mustWriteFile(t, filepath.Join(dir, "big.go"), []byte(committed.String()))
	git("add", "big.go")
	git("commit", "-m", "big generated file")
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	commitHash := strings.TrimSpace(string(out))
	cpID := uuid.NewString()
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpID, RepositoryID: repoID,
		CreatedAt: 300_000, Kind: "auto", Status: "complete",
		CompletedAt: sql.NullInt64{Int64: 300_000, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: commitHash, RepositoryID: repoID,
		CheckpointID: cpID, LinkedAt: 300_000,
	}); err != nil {
		t.Fatal(err)
	}
	_ = sqlstore.Close(h)

	svc := NewAttributionService()
	result, err := svc.AttributeCommit(ctx, AttributionInput{RepoPath: dir, CommitHash: commitHash})
	if err != nil {
		t.Fatalf("AttributeCommit: %v", err)
	}
	big := fileByPath(t, result.Files, "big.go")
	if result.Diagnostics.DeltaAlignmentsRefused != 1 {
		t.Fatalf("DeltaAlignmentsRefused = %d, want 1", result.Diagnostics.DeltaAlignmentsRefused)
	}
	if big.EvidenceClass != string(attrreporting.EvidenceToolDeltaTouch) {
		t.Errorf("big.go evidence = %q, want tool_delta_touch", big.EvidenceClass)
	}
	if big.AIProviderOnlyLines != n || big.HumanLines != 0 {
		t.Errorf("big.go = %+v, want all lines in the sidecar", big)
	}
	if len(big.Providers) != 2 || big.Providers[0] != "claude_code" || big.Providers[1] != "cursor" {
		t.Errorf("big.go providers = %v, want [claude_code cursor]", big.Providers)
	}
}

// Worker enrichment carries historical deltas forward.
func TestAttributeWithCarryForward_HistoricalDelta(t *testing.T) {
	dir, _, cpA := setupHistoricalDeltaRepo(t)
	ctx := context.Background()
	semDir := filepath.Join(dir, ".semantica")
	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	bs, err := blobs.NewStore(filepath.Join(semDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	var repoID string
	if err := h.DB.QueryRowContext(ctx, "select repository_id from repositories").Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	prevCP, err := h.Queries.GetCheckpointByID(ctx, cpA)
	if err != nil {
		t.Fatal(err)
	}

	diff := strings.Join([]string{
		"diff --git a/gen2.go b/gen2.go",
		"--- /dev/null",
		"+++ b/gen2.go",
		"@@ -0,0 +1,2 @@",
		"+package gen2",
		"+func Generated2() {}",
		"",
	}, "\n")
	in := ComputeAIPercentInput{RepoRoot: dir, RepoID: repoID, Window: tsWindow(150_000, 400_000)}

	v1, err := attributeWithCarryForward(ctx, h, bs, []byte(diff), in, &prevCP, semDir, false)
	if err != nil {
		t.Fatalf("v1: %v", err)
	}
	if v1.result.AILines != 0 {
		t.Errorf("v1 AILines = %d, want 0", v1.result.AILines)
	}

	v2, err := attributeWithCarryForward(ctx, h, bs, []byte(diff), in, &prevCP, semDir, true)
	if err != nil {
		t.Fatalf("v2: %v", err)
	}
	if v2.result.AILines != 2 || v2.result.Percent != 100 {
		t.Errorf("v2 = %d lines / %.0f%%, want the historical delta carried forward (2 / 100)",
			v2.result.AILines, v2.result.Percent)
	}
}

// Deletions retain deletion semantics; touches preserve line evidence.
func TestApplyDeltaTouchOrigins(t *testing.T) {
	origins := map[string]attrreporting.TouchOrigin{
		"edited-then-deleted.go": attrreporting.TouchOriginLineLevel,
		"touched-and-edited.go":  attrreporting.TouchOriginLineLevel,
		"touched-only.bin":       attrreporting.TouchOriginProviderEdit,
	}
	deltas := &attrevents.DeltaCandidates{
		Touched: map[string][]string{
			"touched-and-edited.go": {"claude_code"},
			"touched-only.bin":      {"claude_code"},
		},
		Deleted: map[string][]string{
			"edited-then-deleted.go": {"claude_code"},
		},
	}
	applyDeltaTouchOrigins(origins, deltas)

	if origins["edited-then-deleted.go"] != attrreporting.TouchOriginDeletion {
		t.Errorf("deleted origin = %q, want unconditional deletion", origins["edited-then-deleted.go"])
	}
	if origins["touched-and-edited.go"] != attrreporting.TouchOriginLineLevel {
		t.Errorf("touched origin = %q, want line-level retained", origins["touched-and-edited.go"])
	}
	if origins["touched-only.bin"] != attrreporting.TouchOriginToolDelta {
		t.Errorf("touch-only origin = %q, want tool_delta", origins["touched-only.bin"])
	}
}

// Refused alignments retain tool-delta touch evidence.
func TestApplyDeltaRefusals(t *testing.T) {
	scores := []fileScore{
		{path: "refused.go", totalLines: 3, humanLines: 3, deltaAlignmentRefused: true},
		{path: "matched.go", totalLines: 2, exactLines: 2, deltaExactLines: 2},
	}
	deltas := &attrevents.DeltaCandidates{
		Claims: map[string][]attrevents.DeltaClaimGroup{
			"refused.go": {
				{GroupID: "g1", Provider: "claude_code"},
				{GroupID: "g2", Provider: "codex"},
			},
			"matched.go": {{GroupID: "g3", Provider: "claude_code"}},
		},
		Touched: map[string][]string{},
	}
	aiTouched := map[string]bool{}
	providerTouched := map[string]string{}
	origins := map[string]attrreporting.TouchOrigin{}

	applyDeltaRefusals(scores, deltas, aiTouched, providerTouched, origins)

	if !aiTouched["refused.go"] || providerTouched["refused.go"] != "claude_code" {
		t.Errorf("refused.go touch = %v/%q, want AI-touched with claim provider",
			aiTouched["refused.go"], providerTouched["refused.go"])
	}
	if origins["refused.go"] != attrreporting.TouchOriginToolDelta {
		t.Errorf("refused.go origin = %q, want tool_delta", origins["refused.go"])
	}
	// Keep every provider involved in the refused claims.
	touched := deltas.Touched["refused.go"]
	if len(touched) != 2 || touched[0] != "claude_code" || touched[1] != "codex" {
		t.Errorf("refused.go touched providers = %v, want both groups' providers", touched)
	}
	if aiTouched["matched.go"] || origins["matched.go"] != "" {
		t.Error("matched claims must not receive touch fallback")
	}
}

// Touched and deleted files retain all involved providers.
func TestOverlayDeltaProviders(t *testing.T) {
	fileProviders := map[string][]string{
		"shared.bin": {"cursor"}, // historical fallback collapsed to one
	}
	deltas := &attrevents.DeltaCandidates{
		Touched: map[string][]string{
			"shared.bin": {"claude_code", "codex"},
		},
		Deleted: map[string][]string{
			"gone.go": {"codex"},
		},
	}
	overlayDeltaProviders(fileProviders, deltas, nil)

	want := []string{"claude_code", "codex", "cursor"}
	got := fileProviders["shared.bin"]
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("shared.bin providers = %v, want %v", got, want)
	}
	if len(fileProviders["gone.go"]) != 1 || fileProviders["gone.go"][0] != "codex" {
		t.Errorf("gone.go providers = %v, want [codex]", fileProviders["gone.go"])
	}

	// Matched-line providers remain first.
	fileProviders = map[string][]string{"mixed.go": {"cursor"}}
	deltas = &attrevents.DeltaCandidates{
		Touched: map[string][]string{"mixed.go": {"claude_code"}},
	}
	overlayDeltaProviders(fileProviders, deltas, map[string]bool{"mixed.go": true})
	got = fileProviders["mixed.go"]
	if len(got) != 2 || got[0] != "cursor" || got[1] != "claude_code" {
		t.Errorf("mixed.go providers = %v, want scored cursor leading", got)
	}
}

// Repository settings can enable tool-delta scoring.
func TestAttributionV2_SettingsFlag(t *testing.T) {
	dir, commitHash := setupDeltaRepo(t)
	semDir := filepath.Join(dir, ".semantica")
	mustWriteFile(t, filepath.Join(semDir, "settings.json"),
		[]byte(fmt.Sprintf("{%q: true, %q: 1, %q: true}\n", "enabled", "version", "attribution_v2")))

	svc := NewAttributionService()
	result, err := svc.AttributeCommit(context.Background(), AttributionInput{RepoPath: dir, CommitHash: commitHash})
	if err != nil {
		t.Fatalf("AttributeCommit: %v", err)
	}
	if result.AttrVersion != "v2" {
		t.Errorf("AttrVersion = %q, want v2 from settings", result.AttrVersion)
	}
}
