package service

import (
	"context"
	"database/sql"
	"encoding/json"
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
	payload, err := json.Marshal(map[string]any{
		"type": "assistant",
		"message": map[string]any{"content": []any{map[string]any{
			"type": "tool_use",
			"name": "Write",
			"input": map[string]any{
				"file_path": filepath.Join(repoRoot, relPath),
				"content":   content,
			},
		}}},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	payloadHash, _, err := bs.Put(ctx, payload)
	if err != nil {
		t.Fatalf("store payload: %v", err)
	}
	toolUsesJSON, err := json.Marshal(map[string]any{
		"content_types": []string{"tool_use"},
		"tools": []any{map[string]any{
			"name":      "Write",
			"file_path": relPath,
			"file_op":   "write",
		}},
	})
	if err != nil {
		t.Fatalf("marshal tool uses: %v", err)
	}
	if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID: eventID, SessionID: sessID, RepositoryID: repoID, Ts: ts,
		Kind: "assistant", Role: sqlstore.NullStr("assistant"),
		ToolUses:    sql.NullString{String: string(toolUsesJSON), Valid: true},
		PayloadHash: sqlstore.NullStr(payloadHash), Summary: sqlstore.NullStr("Wrote " + relPath),
	}); err != nil {
		t.Fatalf("insert agent event: %v", err)
	}
}

// directEvidenceProviders lists identities covered by the direct-evidence test.
var directEvidenceProviders = []struct{ provider, file string }{
	{"claude_code", "probe_claude.txt"},
	{"codex", "probe_codex.txt"},
	{"copilot", "probe_copilot.txt"},
	{"gemini_cli", "probe_gemini.txt"},
	{"kiro-cli", "probe_kirocli.txt"},
	{"kiro-ide", "probe_kiroide.txt"},
}

// insertProviderSource creates a source for one provider.
func insertProviderSource(t *testing.T, h *sqlstore.Handle, repoID, sourceKey, provider string) string {
	t.Helper()
	row, err := h.Queries.UpsertAgentSource(context.Background(), sqldb.UpsertAgentSourceParams{
		SourceID: uuid.NewString(), RepositoryID: repoID, Provider: provider,
		SourceKey: sourceKey, LastSeenAt: 100_000, CreatedAt: 100_000,
	})
	if err != nil {
		t.Fatalf("insert %s source: %v", provider, err)
	}
	return row.SourceID
}

// setupDirectEvidenceRepo commits one direct-evidence file per provider.
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
	// Use distinct content so evidence cannot overlap across providers.
	ts := int64(200_000)
	files := make([]string, 0, len(directEvidenceProviders))
	for _, s := range directEvidenceProviders {
		src := insertProviderSource(t, h, repoID, "/data/"+s.provider+".jsonl", s.provider)
		sess := insertSessionWithProvider(t, h, repoID, src, uuid.NewString(), s.provider)
		content := s.provider + " alpha\n" + s.provider + " beta\n"
		insertDirectWriteEvent(t, h, bs, repoID, repoRoot, sess, s.file, content, ts)
		mustWriteFile(t, filepath.Join(dir, s.file), []byte(content))
		files = append(files, s.file)
		ts += 10_000
	}
	git(append([]string{"add"}, files...)...)
	git("commit", "-m", "add direct-evidence files")
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

// TestAttributionV2_DirectOnlyFlagSelectsPath compares both scorers without tool deltas.
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

	// Each file retains its provider and two exact lines.
	for _, s := range directEvidenceProviders {
		f := fileByPath(t, v2.Files, s.file)
		if f.AIExactLines != 2 || len(f.Providers) != 1 || f.Providers[0] != s.provider {
			t.Errorf("%s = %+v, want 2 exact AI lines by %s", s.file, f, s.provider)
		}
	}
	if want := 2 * len(directEvidenceProviders); v2.AILines != want {
		t.Fatalf("v2 headline AILines = %d, want %d", v2.AILines, want)
	}
	// Direct evidence must not populate delta counters.
	if v2.AIDeltaExactLines != 0 || v2.AIDeltaFormattedLines != 0 {
		t.Errorf("v2 delta line counters non-zero on delta-free repo: exact=%d formatted=%d", v2.AIDeltaExactLines, v2.AIDeltaFormattedLines)
	}
	if v2.Diagnostics.DeltaGroupsEligible != 0 || v2.Diagnostics.DeltaExactMatches != 0 {
		t.Errorf("v2 populated delta diagnostics on delta-free repo: %+v", v2.Diagnostics)
	}

	// Results must match except for the scorer version.
	v1.AttrVersion, v2.AttrVersion = "", ""
	if !reflect.DeepEqual(v1, v2) {
		t.Errorf("v1 and v2 results diverge on delta-free direct evidence:\n v1=%+v\n v2=%+v", v1, v2)
	}
}
