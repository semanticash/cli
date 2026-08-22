package service

import (
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// insertDirectWriteEvent records direct Write evidence without a tool delta.
func insertDirectWriteEvent(t *testing.T, h *sqlstore.Handle, bs *blobs.Store, repoID, repoRoot, sessID, relPath, content string, ts int64) {
	t.Helper()
	ctx := context.Background()
	eventID := uuid.NewString()
	escaped := strings.ReplaceAll(content, "\n", `\n`)
	payload := fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"%s/%s","content":"%s"}}]}}`, repoRoot, relPath, escaped)
	payloadHash, _, err := bs.Put(ctx, []byte(payload))
	if err != nil {
		t.Fatalf("store payload: %v", err)
	}
	toolUses := fmt.Sprintf(`{"content_types":["tool_use"],"tools":[{"name":"Write","file_path":"%s","file_op":"write"}]}`, relPath)
	if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID: eventID, SessionID: sessID, RepositoryID: repoID, Ts: ts,
		Kind: "assistant", Role: sqlstore.NullStr("assistant"),
		ToolUses:    sql.NullString{String: toolUses, Valid: true},
		PayloadHash: sqlstore.NullStr(payloadHash), Summary: sqlstore.NullStr("Wrote " + relPath),
	}); err != nil {
		t.Fatalf("insert agent event: %v", err)
	}
}

// setupDirectEvidenceRepo creates a commit with direct evidence from two providers.
func setupDirectEvidenceRepo(t *testing.T) (string, string) {
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
	claudeSess := insertSessionWithProvider(t, h, repoID, srcID, uuid.NewString(), "claude_code")
	codexSess := insertSessionWithProvider(t, h, repoID, srcID, uuid.NewString(), "codex")

	// Distinct files avoid the shared-line provider narrowing tested by scoring.
	insertDirectWriteEvent(t, h, bs, repoID, repoRoot, claudeSess, "feat.go", "package feat\nfunc Feat() {}\n", 200_000)
	insertDirectWriteEvent(t, h, bs, repoID, repoRoot, codexSess, "util.go", "package util\nfunc Util() {}\n", 210_000)

	mustWriteFile(t, filepath.Join(dir, "feat.go"), []byte("package feat\nfunc Feat() {}\n"))
	mustWriteFile(t, filepath.Join(dir, "util.go"), []byte("package util\nfunc Util() {}\n"))
	git("add", "feat.go", "util.go")
	git("commit", "-m", "add feat and util")
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

// TestAttributionV2_DirectOnlyFlagSelectsPath compares both scorers on the same
// mixed-provider commit without tool-delta evidence.
func TestAttributionV2_DirectOnlyFlagSelectsPath(t *testing.T) {
	dir, commitHash := setupDirectEvidenceRepo(t)

	run := func(flag string) *AttributionResult {
		t.Setenv("SEMANTICA_ATTRIBUTION_V2", flag)
		result, err := NewAttributionService().AttributeCommit(
			context.Background(), AttributionInput{RepoPath: dir, CommitHash: commitHash})
		if err != nil {
			t.Fatalf("AttributeCommit (flag %s): %v", flag, err)
		}
		return result
	}

	v1 := run("0")
	v2 := run("1")

	// The flag selects the scorer.
	if v1.AttrVersion != "v1" {
		t.Errorf("flag 0: AttrVersion = %q, want v1", v1.AttrVersion)
	}
	if v2.AttrVersion != "v2" {
		t.Errorf("flag 1: AttrVersion = %q, want v2", v2.AttrVersion)
	}

	// Direct evidence retains its provider.
	feat := fileByPath(t, v2.Files, "feat.go")
	util := fileByPath(t, v2.Files, "util.go")
	if feat.AIExactLines != 2 || len(feat.Providers) != 1 || feat.Providers[0] != "claude_code" {
		t.Fatalf("feat.go = %+v, want 2 exact AI lines by claude_code", feat)
	}
	if util.AIExactLines != 2 || len(util.Providers) != 1 || util.Providers[0] != "codex" {
		t.Fatalf("util.go = %+v, want 2 exact AI lines by codex", util)
	}
	if v2.AILines != 4 {
		t.Fatalf("v2 headline AILines = %d, want 4", v2.AILines)
	}
	// Direct-only evidence must not populate delta counters.
	if v2.AIDeltaExactLines != 0 || v2.AIDeltaFormattedLines != 0 {
		t.Errorf("v2 delta line counters non-zero on delta-free repo: exact=%d formatted=%d", v2.AIDeltaExactLines, v2.AIDeltaFormattedLines)
	}
	if v2.Diagnostics.DeltaGroupsEligible != 0 || v2.Diagnostics.DeltaExactMatches != 0 {
		t.Errorf("v2 populated delta diagnostics on delta-free repo: %+v", v2.Diagnostics)
	}

	// Results must match apart from the scorer version.
	v1.AttrVersion, v2.AttrVersion = "", ""
	if !reflect.DeepEqual(v1, v2) {
		t.Errorf("v1 and v2 results diverge on delta-free direct evidence:\n v1=%+v\n v2=%+v", v1, v2)
	}
}
