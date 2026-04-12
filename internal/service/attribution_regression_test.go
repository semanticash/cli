package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	attrreporting "github.com/semanticash/cli/internal/attribution/reporting"
	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// Regression tests for the public attribution entry points.
//
// Provider details are compared in canonical order (AI lines descending, then
// provider name). File-related slices are canonicalized by path before
// equality checks.

func insertProviderFileTouchEvent(t *testing.T, h *sqlstore.Handle, sessID, repoID, provider, toolName string, ts int64, filePath string) string {
	t.Helper()
	ctx := context.Background()
	eventID := uuid.NewString()
	tu := fmt.Sprintf(`{"content_types":["%s"],"tools":[{"name":"%s","file_path":"%s","file_op":"edit"}]}`,
		toolName, toolName, filePath)
	if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID:      eventID,
		SessionID:    sessID,
		RepositoryID: repoID,
		Ts:           ts,
		Kind:         toolName,
		Role:         sqlstore.NullStr("assistant"),
		ToolUses:     sql.NullString{String: tu, Valid: true},
		Summary:      sqlstore.NullStr("AI edited " + filePath),
	}); err != nil {
		t.Fatalf("insert provider file touch: %v", err)
	}
	return eventID
}

func insertSessionWithProvider(t *testing.T, h *sqlstore.Handle, repoID, srcID, sessID, provider string) string {
	t.Helper()
	ctx := context.Background()
	s, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: sessID, ProviderSessionID: sessID + "-prov",
		RepositoryID: repoID, Provider: provider, SourceID: srcID,
		StartedAt: 100_000, LastSeenAt: 500_000, MetadataJson: `{}`,
	})
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	return s.SessionID
}

// canonicalizeResult normalizes an AttributionResult for deterministic comparison.
func canonicalizeResult(r *AttributionResult) AttributionResult {
	c := *r
	sort.Slice(c.Files, func(i, j int) bool { return c.Files[i].Path < c.Files[j].Path })
	sort.Slice(c.ProviderDetails, func(i, j int) bool {
		if c.ProviderDetails[i].AILines != c.ProviderDetails[j].AILines {
			return c.ProviderDetails[i].AILines > c.ProviderDetails[j].AILines
		}
		return c.ProviderDetails[i].Provider < c.ProviderDetails[j].Provider
	})
	sort.Slice(c.FilesCreated, func(i, j int) bool { return c.FilesCreated[i].Path < c.FilesCreated[j].Path })
	sort.Slice(c.FilesEdited, func(i, j int) bool { return c.FilesEdited[i].Path < c.FilesEdited[j].Path })
	sort.Slice(c.FilesDeleted, func(i, j int) bool { return c.FilesDeleted[i].Path < c.FilesDeleted[j].Path })
	return c
}

func assertResultsEqual(t *testing.T, label string, got, want AttributionResult) {
	t.Helper()
	g := canonicalizeResult(&got)
	w := canonicalizeResult(&want)

	// Headline counts.
	if g.CommitHash != w.CommitHash { t.Errorf("%s CommitHash: got %q, want %q", label, g.CommitHash, w.CommitHash) }
	if g.CheckpointID != w.CheckpointID { t.Errorf("%s CheckpointID: got %q, want %q", label, g.CheckpointID, w.CheckpointID) }
	if g.AILines != w.AILines { t.Errorf("%s AILines: got %d, want %d", label, g.AILines, w.AILines) }
	if g.AIExactLines != w.AIExactLines { t.Errorf("%s AIExactLines: got %d, want %d", label, g.AIExactLines, w.AIExactLines) }
	if g.AIFormattedLines != w.AIFormattedLines { t.Errorf("%s AIFormattedLines: got %d, want %d", label, g.AIFormattedLines, w.AIFormattedLines) }
	if g.AIModifiedLines != w.AIModifiedLines { t.Errorf("%s AIModifiedLines: got %d, want %d", label, g.AIModifiedLines, w.AIModifiedLines) }
	if g.HumanLines != w.HumanLines { t.Errorf("%s HumanLines: got %d, want %d", label, g.HumanLines, w.HumanLines) }
	if g.TotalLines != w.TotalLines { t.Errorf("%s TotalLines: got %d, want %d", label, g.TotalLines, w.TotalLines) }
	if g.AIPercentage != w.AIPercentage { t.Errorf("%s AIPercentage: got %.1f, want %.1f", label, g.AIPercentage, w.AIPercentage) }
	if g.FilesTotal != w.FilesTotal { t.Errorf("%s FilesTotal: got %d, want %d", label, g.FilesTotal, w.FilesTotal) }
	if g.FilesAITouched != w.FilesAITouched { t.Errorf("%s FilesAITouched: got %d, want %d", label, g.FilesAITouched, w.FilesAITouched) }

	// Per-file attribution rows (canonicalized: sorted by path).
	if len(g.Files) != len(w.Files) {
		t.Errorf("%s Files count: got %d, want %d", label, len(g.Files), len(w.Files))
	} else {
		for i := range g.Files {
			if g.Files[i] != w.Files[i] {
				t.Errorf("%s Files[%d]: got %+v, want %+v", label, i, g.Files[i], w.Files[i])
			}
		}
	}

	// File change slices (canonicalized: sorted by path).
	assertFileChangesEqual(t, label+".FilesCreated", g.FilesCreated, w.FilesCreated)
	assertFileChangesEqual(t, label+".FilesEdited", g.FilesEdited, w.FilesEdited)
	assertFileChangesEqual(t, label+".FilesDeleted", g.FilesDeleted, w.FilesDeleted)

	// Provider details (canonicalized: sorted by AILines desc, Provider asc).
	if len(g.ProviderDetails) != len(w.ProviderDetails) {
		t.Errorf("%s ProviderDetails count: got %d, want %d", label, len(g.ProviderDetails), len(w.ProviderDetails))
	} else {
		for i := range g.ProviderDetails {
			if g.ProviderDetails[i] != w.ProviderDetails[i] {
				t.Errorf("%s ProviderDetails[%d]: got %+v, want %+v", label, i, g.ProviderDetails[i], w.ProviderDetails[i])
			}
		}
	}

	// Full diagnostics struct.
	if g.Diagnostics != w.Diagnostics {
		t.Errorf("%s Diagnostics: got %+v, want %+v", label, g.Diagnostics, w.Diagnostics)
	}
}

func assertFileChangesEqual(t *testing.T, label string, got, want []FileChange) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s count: got %d, want %d", label, len(got), len(want))
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s[%d]: got %+v, want %+v", label, i, got[i], want[i])
		}
	}
}

// setupGoldenRepo creates a git repo with matching Semantica state for a single
// AI-authored commit. The setup is written directly to disk rather than going
// through enableSemantica so the tests are isolated from installed git hooks.
func setupGoldenRepo(t *testing.T) (string, string) {
	t.Helper()

	// Use /tmp directly so the test paths are stable across macOS temp-dir aliases.
	dir, err := os.MkdirTemp("/tmp", "golden-repo-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("SEMANTICA_HOME", filepath.Join(dir, ".semantica-global"))
	t.Setenv("HOME", dir)

	// Init git repo.
	for _, args := range [][]string{
		{"init"}, {"config", "user.email", "test@test.com"}, {"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "README"), []byte("init\n"), 0o644)
	for _, args := range [][]string{{"add", "README"}, {"commit", "-m", "initial"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Create the minimal .semantica state required by the attribution service.
	semDir := filepath.Join(dir, ".semantica")
	_ = os.MkdirAll(semDir, 0o755)
	_ = os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644)
	_ = os.MkdirAll(filepath.Join(semDir, "objects"), 0o755)

	ctx := context.Background()
	dbPath := filepath.Join(semDir, "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}

	repo, _ := git.OpenRepo(dir)
	repoRoot := repo.Root()
	repoID := uuid.NewString()
	_ = h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: repoRoot,
		CreatedAt: 100_000, EnabledAt: 100_000,
	})

	// Baseline checkpoint without a commit link.
	baselineID := uuid.NewString()
	_ = h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: baselineID, RepositoryID: repoID,
		CreatedAt: 100_000, Kind: "baseline", Status: "complete",
		CompletedAt: sql.NullInt64{Int64: 100_000, Valid: true},
	})

	bs, _ := blobs.NewStore(filepath.Join(semDir, "objects"))
	srcID := insertSource(t, h, repoID, "/data/session.jsonl")
	sessID := insertSession(t, h, repoID, srcID, "golden-sess")

	// Insert the AI event before the linked checkpoint.
	_ = insertEventWithPayload(t, h, bs, sessID, repoID, repoRoot,
		200_000, "handler.go", "package api\nfunc Handle() {}\n")

	// Create and commit the file in git.
	_ = os.WriteFile(filepath.Join(dir, "handler.go"), []byte("package api\nfunc Handle() {}\n"), 0o644)
	gitCmd := exec.Command("git", "add", "handler.go")
	gitCmd.Dir = dir
	if out, err := gitCmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	gitCmd = exec.Command("git", "commit", "-m", "add handler")
	gitCmd.Dir = dir
	if out, err := gitCmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	gitCmd = exec.Command("git", "rev-parse", "HEAD")
	gitCmd.Dir = dir
	out, err := gitCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	commitHash := strings.TrimSpace(string(out))

	// Link the commit to a later checkpoint.
	cpID := uuid.NewString()
	_ = h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpID, RepositoryID: repoID,
		CreatedAt: 300_000, Kind: "auto", Status: "complete",
		CompletedAt: sql.NullInt64{Int64: 300_000, Valid: true},
	})
	_ = h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: commitHash, RepositoryID: repoID,
		CheckpointID: cpID, LinkedAt: 300_000,
	})

	_ = sqlstore.Close(h)
	return dir, commitHash
}

// Claude line-level payload with exact matches only.

func TestRegression_ComputeAIPercent_ClaudeLineLevel(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()
	bsDir := t.TempDir()
	bs, _ := blobs.NewStore(bsDir)

	repoID := insertRepo(t, h, 100_000)
	repoRoot := "/test/repo/" + repoID
	srcID := insertSource(t, h, repoID, "/data/session.jsonl")
	sessID := insertSession(t, h, repoID, srcID, "sess-claude")

	insertCommitCheckpoint(t, h, repoID, "baseline000", 100_000)

	_ = insertEventWithPayload(t, h, bs, sessID, repoID, repoRoot,
		200_000, "main.go", "package main\nfunc main() {\nfmt.Println()\n}\n")

	cpID := insertCommitCheckpoint(t, h, repoID, "commit1abc", 300_000)

	diff := strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"--- /dev/null",
		"+++ b/main.go",
		"@@ -0,0 +1,4 @@",
		"+package main",
		"+func main() {",
		"+fmt.Println()",
		"+}",
		"",
	}, "\n")

	input := ComputeAIPercentInput{
		RepoRoot: repoRoot, RepoID: repoID, AfterTs: 100_000, UpToTs: 300_000,
	}

	svc := NewAttributionService()
	result, err := svc.ComputeAIPercentFromDiff(ctx, h, bs, []byte(diff), input)
	if err != nil {
		t.Fatalf("ComputeAIPercentFromDiff: %v", err)
	}

	if result.TotalLines != 4 { t.Errorf("TotalLines = %d, want 4", result.TotalLines) }
	if result.AILines != 4 { t.Errorf("AILines = %d, want 4", result.AILines) }
	if result.ExactLines != 4 { t.Errorf("ExactLines = %d, want 4", result.ExactLines) }
	if result.FormattedLines != 0 { t.Errorf("FormattedLines = %d, want 0", result.FormattedLines) }
	if result.ModifiedLines != 0 { t.Errorf("ModifiedLines = %d, want 0", result.ModifiedLines) }
	if result.Percent != 100 { t.Errorf("Percent = %.1f, want 100", result.Percent) }
	if result.FilesTouched != 1 { t.Errorf("FilesTouched = %d, want 1", result.FilesTouched) }
	if len(result.Providers) != 1 { t.Fatalf("Providers count = %d, want 1", len(result.Providers)) }
	if result.Providers[0].Provider != "claude_code" { t.Errorf("Provider = %q, want claude_code", result.Providers[0].Provider) }
	if result.Providers[0].AILines != 4 { t.Errorf("Provider AILines = %d, want 4", result.Providers[0].AILines) }
	_ = cpID
}

// Cursor file-touch attribution with no line-level payload.

func TestRegression_ComputeAIPercent_CursorFileTouchOnly(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()
	bsDir := t.TempDir()
	bs, _ := blobs.NewStore(bsDir)

	repoID := insertRepo(t, h, 100_000)
	srcID := insertSource(t, h, repoID, "/data/cursor.jsonl")
	sessID := insertSessionWithProvider(t, h, repoID, srcID, "sess-cursor", "cursor")
	insertCommitCheckpoint(t, h, repoID, "baseline000", 100_000)
	insertProviderFileTouchEvent(t, h, sessID, repoID, "cursor", "cursor_edit", 200_000, "handler.go")
	insertCommitCheckpoint(t, h, repoID, "commit2abc", 300_000)

	diff := strings.Join([]string{
		"diff --git a/handler.go b/handler.go", "--- /dev/null", "+++ b/handler.go",
		"@@ -0,0 +1,3 @@", "+package api", "+func Handle() {}", "+func Process() {}", "",
	}, "\n")

	svc := NewAttributionService()
	result, err := svc.ComputeAIPercentFromDiff(ctx, h, bs, []byte(diff), ComputeAIPercentInput{
		RepoRoot: "/test/repo/" + repoID, RepoID: repoID, AfterTs: 100_000, UpToTs: 300_000,
	})
	if err != nil { t.Fatalf("ComputeAIPercentFromDiff: %v", err) }

	if result.TotalLines != 3 { t.Errorf("TotalLines = %d, want 3", result.TotalLines) }
	if result.AILines != 3 { t.Errorf("AILines = %d, want 3", result.AILines) }
	if result.Percent != 100 { t.Errorf("Percent = %.1f, want 100", result.Percent) }
	if len(result.Providers) != 1 || result.Providers[0].Provider != "cursor" {
		t.Errorf("Providers = %v, want [cursor]", result.Providers)
	}
}

// Error and empty-input cases.

func TestRegression_ComputeAIPercent_NoEventsInWindow(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()
	bs, _ := blobs.NewStore(t.TempDir())
	repoID := insertRepo(t, h, 100_000)
	insertCommitCheckpoint(t, h, repoID, "baseline000", 100_000)
	insertCommitCheckpoint(t, h, repoID, "commit1abc", 300_000)

	diff := "diff --git a/main.go b/main.go\n--- /dev/null\n+++ b/main.go\n@@ -0,0 +1,1 @@\n+package main\n"
	svc := NewAttributionService()
	_, err := svc.ComputeAIPercentFromDiff(ctx, h, bs, []byte(diff), ComputeAIPercentInput{
		RepoRoot: "/test/repo/" + repoID, RepoID: repoID, AfterTs: 100_000, UpToTs: 300_000,
	})
	if !errors.Is(err, ErrNoEventsInWindow) { t.Errorf("expected ErrNoEventsInWindow, got %v", err) }
}

func TestRegression_ComputeAIPercent_EmptyDiff(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()
	bs, _ := blobs.NewStore(t.TempDir())
	repoID := insertRepo(t, h, 100_000)
	svc := NewAttributionService()
	result, err := svc.ComputeAIPercentFromDiff(ctx, h, bs, nil, ComputeAIPercentInput{
		RepoRoot: "/test/repo/" + repoID, RepoID: repoID, AfterTs: 100_000, UpToTs: 300_000,
	})
	if err != nil { t.Fatalf("expected no error, got %v", err) }
	if result.TotalLines != 0 { t.Errorf("TotalLines = %d, want 0", result.TotalLines) }
	if result.Percent != 0 { t.Errorf("Percent = %.1f, want 0", result.Percent) }
}

// Carry-forward behavior.

func TestRegression_CarryForward_HistoricalLookback(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()
	bs, _ := blobs.NewStore(t.TempDir())
	semDir := t.TempDir()

	repoID := insertRepo(t, h, 100_000)
	repoRoot := "/test/repo/" + repoID
	srcID := insertSource(t, h, repoID, "/data/session.jsonl")
	sessID := insertSession(t, h, repoID, srcID, "sess-1")

	_ = insertEventWithPayload(t, h, bs, sessID, repoID, repoRoot, 150_000, "utils.go", "package utils\nfunc Helper() {}\n")
	cp1ID := insertCheckpointWithManifest(t, h, bs, repoID, 200_000, []string{"utils.go"})
	_ = h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: "commit1", RepositoryID: repoID, CheckpointID: cp1ID, LinkedAt: 200_000,
	})
	_ = insertCheckpointWithManifest(t, h, bs, repoID, 300_000, []string{"utils.go"})

	diff := "diff --git a/utils.go b/utils.go\n--- /dev/null\n+++ b/utils.go\n@@ -0,0 +1,2 @@\n+package utils\n+func Helper() {}\n"
	cp1, _ := h.Queries.GetCheckpointByID(ctx, cp1ID)
	cfr, err := attributeWithCarryForward(ctx, h, bs, []byte(diff), ComputeAIPercentInput{
		RepoRoot: repoRoot, RepoID: repoID, AfterTs: 200_000, UpToTs: 300_000,
	}, &cp1, semDir)
	if err != nil { t.Fatalf("attributeWithCarryForward: %v", err) }

	if cfr.noEvents { t.Error("noEvents should be false") }
	if cfr.result.TotalLines != 2 { t.Errorf("TotalLines = %d, want 2", cfr.result.TotalLines) }
	if cfr.result.AILines != 2 { t.Errorf("AILines = %d, want 2", cfr.result.AILines) }
	if cfr.result.Percent != 100 { t.Errorf("Percent = %.1f, want 100", cfr.result.Percent) }
}

func TestRegression_CarryForward_BothWindowsEmpty(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()
	bs, _ := blobs.NewStore(t.TempDir())
	semDir := t.TempDir()
	repoID := insertRepo(t, h, 100_000)
	cp1ID := insertCheckpointWithManifest(t, h, bs, repoID, 200_000, []string{"utils.go"})
	_ = h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: "commit1", RepositoryID: repoID, CheckpointID: cp1ID, LinkedAt: 200_000,
	})
	_ = insertCheckpointWithManifest(t, h, bs, repoID, 300_000, []string{"utils.go"})
	diff := "diff --git a/utils.go b/utils.go\n--- /dev/null\n+++ b/utils.go\n@@ -0,0 +1,1 @@\n+package utils\n"
	cp1, _ := h.Queries.GetCheckpointByID(ctx, cp1ID)
	cfr, err := attributeWithCarryForward(ctx, h, bs, []byte(diff), ComputeAIPercentInput{
		RepoRoot: "/test/repo/" + repoID, RepoID: repoID, AfterTs: 200_000, UpToTs: 300_000,
	}, &cp1, semDir)
	if !errors.Is(err, ErrNoEventsInWindow) { t.Errorf("expected ErrNoEventsInWindow, got %v", err) }
	if !cfr.noEvents { t.Error("noEvents should be true") }
}

func TestRegression_CarryForward_NilPrevCP(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()
	bs, _ := blobs.NewStore(t.TempDir())
	semDir := t.TempDir()
	repoID := insertRepo(t, h, 100_000)
	diff := "diff --git a/main.go b/main.go\n--- /dev/null\n+++ b/main.go\n@@ -0,0 +1,1 @@\n+package main\n"
	cfr, err := attributeWithCarryForward(ctx, h, bs, []byte(diff), ComputeAIPercentInput{
		RepoRoot: "/test/repo/" + repoID, RepoID: repoID, AfterTs: 0, UpToTs: 300_000,
	}, nil, semDir)
	if !errors.Is(err, ErrNoEventsInWindow) { t.Errorf("expected ErrNoEventsInWindow, got %v", err) }
	if !cfr.noEvents { t.Error("noEvents should be true") }
}

func TestRegression_CarryForward_HistoricalEmpty_PreservesCurrent(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()
	bs, _ := blobs.NewStore(t.TempDir())
	semDir := t.TempDir()
	repoID := insertRepo(t, h, 100_000)
	repoRoot := "/test/repo/" + repoID
	srcID := insertSource(t, h, repoID, "/data/session.jsonl")
	sessID := insertSession(t, h, repoID, srcID, "sess-1")

	cp1ID := insertCheckpointWithManifest(t, h, bs, repoID, 200_000, []string{"edit.go"})
	_ = h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: "commit1", RepositoryID: repoID, CheckpointID: cp1ID, LinkedAt: 200_000,
	})
	_ = insertEventWithPayload(t, h, bs, sessID, repoID, repoRoot, 250_000, "edit.go", "package edit\nfunc Handle() {}\n")
	_ = insertCheckpointWithManifest(t, h, bs, repoID, 300_000, []string{"edit.go", "new.go"})

	diff := "diff --git a/edit.go b/edit.go\n--- a/edit.go\n+++ b/edit.go\n@@ -1 +1,2 @@\n+package edit\n+func Handle() {}\ndiff --git a/new.go b/new.go\n--- /dev/null\n+++ b/new.go\n@@ -0,0 +1,1 @@\n+package new\n"
	cp1, _ := h.Queries.GetCheckpointByID(ctx, cp1ID)
	cfr, err := attributeWithCarryForward(ctx, h, bs, []byte(diff), ComputeAIPercentInput{
		RepoRoot: repoRoot, RepoID: repoID, AfterTs: 200_000, UpToTs: 300_000,
	}, &cp1, semDir)
	if err != nil { t.Fatalf("attributeWithCarryForward: %v", err) }
	if cfr.noEvents { t.Error("noEvents should be false") }
	if cfr.result.AILines == 0 { t.Error("expected AILines > 0") }
	if cfr.result.TotalLines < 2 { t.Errorf("TotalLines = %d, want >= 2", cfr.result.TotalLines) }
}

// Mixed-provider attribution.

func TestRegression_ComputeAIPercent_MixedProviders(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()
	bs, _ := blobs.NewStore(t.TempDir())
	repoID := insertRepo(t, h, 100_000)
	repoRoot := "/test/repo/" + repoID
	srcID := insertSource(t, h, repoID, "/data/session.jsonl")
	claudeSess := insertSessionWithProvider(t, h, repoID, srcID, "sess-claude", "claude_code")
	cursorSess := insertSessionWithProvider(t, h, repoID, srcID, "sess-cursor", "cursor")
	insertCommitCheckpoint(t, h, repoID, "baseline000", 100_000)
	_ = insertEventWithPayload(t, h, bs, claudeSess, repoID, repoRoot, 200_000, "main.go", "package main\nfunc main() {}\n")
	insertProviderFileTouchEvent(t, h, cursorSess, repoID, "cursor", "cursor_edit", 250_000, "handler.go")
	insertCommitCheckpoint(t, h, repoID, "commit1abc", 300_000)

	diff := "diff --git a/main.go b/main.go\n--- /dev/null\n+++ b/main.go\n@@ -0,0 +1,2 @@\n+package main\n+func main() {}\ndiff --git a/handler.go b/handler.go\n--- /dev/null\n+++ b/handler.go\n@@ -0,0 +1,2 @@\n+package api\n+func Handle() {}\n"

	svc := NewAttributionService()
	result, err := svc.ComputeAIPercentFromDiff(ctx, h, bs, []byte(diff), ComputeAIPercentInput{
		RepoRoot: repoRoot, RepoID: repoID, AfterTs: 100_000, UpToTs: 300_000,
	})
	if err != nil { t.Fatalf("ComputeAIPercentFromDiff: %v", err) }

	if result.TotalLines != 4 { t.Errorf("TotalLines = %d, want 4", result.TotalLines) }
	if result.AILines != 4 { t.Errorf("AILines = %d, want 4", result.AILines) }
	if result.Percent != 100 { t.Errorf("Percent = %.1f, want 100", result.Percent) }
	if len(result.Providers) != 2 { t.Fatalf("Providers count = %d, want 2", len(result.Providers)) }
	if result.Providers[0].Provider != "claude_code" || result.Providers[1].Provider != "cursor" {
		t.Errorf("provider order: got [%s, %s]", result.Providers[0].Provider, result.Providers[1].Provider)
	}
	if result.Providers[0].AILines != 2 { t.Errorf("claude AILines = %d, want 2", result.Providers[0].AILines) }
	if result.Providers[1].AILines != 2 { t.Errorf("cursor AILines = %d, want 2", result.Providers[1].AILines) }
	if result.ExactLines != 2 { t.Errorf("ExactLines = %d, want 2", result.ExactLines) }
	if result.ModifiedLines != 2 { t.Errorf("ModifiedLines = %d, want 2", result.ModifiedLines) }
}

// Deletion path through bash-derived file touches.

func TestRegression_ComputeAIPercent_DeletionPath(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()
	bs, _ := blobs.NewStore(t.TempDir())
	repoID := insertRepo(t, h, 100_000)
	repoRoot := "/test/repo/" + repoID
	srcID := insertSource(t, h, repoID, "/data/session.jsonl")
	sessID := insertSession(t, h, repoID, srcID, "sess-1")
	insertCommitCheckpoint(t, h, repoID, "baseline000", 100_000)

	eventID := uuid.NewString()
	payload := fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"rm %s/old.go && echo done"}}]}}`, repoRoot)
	payloadHash, _, _ := bs.Put(ctx, []byte(payload))
	_ = h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID: eventID, SessionID: sessID, RepositoryID: repoID, Ts: 200_000,
		Kind: "assistant", Role: sqlstore.NullStr("assistant"),
		ToolUses: sql.NullString{String: `{"content_types":["tool_use"],"tools":[{"name":"Bash"}]}`, Valid: true},
		PayloadHash: sqlstore.NullStr(payloadHash), Summary: sqlstore.NullStr("Ran bash"),
	})
	insertCommitCheckpoint(t, h, repoID, "commit1abc", 300_000)

	diff := "diff --git a/old.go b/old.go\n--- a/old.go\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-package old\n-func Legacy() {}\n"
	svc := NewAttributionService()
	result, err := svc.ComputeAIPercentFromDiff(ctx, h, bs, []byte(diff), ComputeAIPercentInput{
		RepoRoot: repoRoot, RepoID: repoID, AfterTs: 100_000, UpToTs: 300_000,
	})
	if err != nil { t.Fatalf("ComputeAIPercentFromDiff: %v", err) }
	if result.TotalLines != 0 { t.Errorf("TotalLines = %d, want 0", result.TotalLines) }
}

// AttributeCommit full result.

func TestRegression_AttributeCommit_FullResult(t *testing.T) {
	dir, commitHash := setupGoldenRepo(t)
	ctx := context.Background()

	svc := NewAttributionService()
	result, err := svc.AttributeCommit(ctx, AttributionInput{RepoPath: dir, CommitHash: commitHash})
	if err != nil { t.Fatalf("AttributeCommit: %v", err) }

	if result.CommitHash != commitHash { t.Errorf("CommitHash = %q, want %q", result.CommitHash, commitHash) }
	if result.CheckpointID == "" { t.Error("expected non-empty CheckpointID") }
	if result.FilesTotal != 1 { t.Errorf("FilesTotal = %d, want 1", result.FilesTotal) }
	if len(result.Files) != 1 { t.Fatalf("Files count = %d, want 1", len(result.Files)) }
	if result.Files[0].Path != "handler.go" { t.Errorf("File = %q, want handler.go", result.Files[0].Path) }
	if result.Files[0].TotalLines != 2 { t.Errorf("handler.go TotalLines = %d, want 2", result.Files[0].TotalLines) }
	if result.TotalLines != 2 { t.Errorf("TotalLines = %d, want 2", result.TotalLines) }
	if result.AILines != 2 { t.Errorf("AILines = %d, want 2", result.AILines) }
	if result.AIPercentage != 100 { t.Errorf("AIPercentage = %.1f, want 100", result.AIPercentage) }
	if len(result.FilesCreated) != 1 || result.FilesCreated[0].Path != "handler.go" {
		t.Errorf("FilesCreated = %v, want [handler.go]", result.FilesCreated)
	}
	if len(result.ProviderDetails) != 1 || result.ProviderDetails[0].Provider != "claude_code" {
		t.Errorf("ProviderDetails = %v, want [claude_code]", result.ProviderDetails)
	}
	if result.Diagnostics.EventsConsidered != 1 { t.Errorf("EventsConsidered = %d, want 1", result.Diagnostics.EventsConsidered) }
	if result.Diagnostics.PayloadsLoaded != 1 { t.Errorf("PayloadsLoaded = %d, want 1", result.Diagnostics.PayloadsLoaded) }
}

// Evidence integration: verify evidence fields reach the public result.

func TestRegression_AttributeCommit_EvidenceFields(t *testing.T) {
	dir, commitHash := setupGoldenRepo(t)
	ctx := context.Background()

	svc := NewAttributionService()
	result, err := svc.AttributeCommit(ctx, AttributionInput{RepoPath: dir, CommitHash: commitHash})
	if err != nil { t.Fatalf("AttributeCommit: %v", err) }

	// Commit-level evidence label should be present (all files exact).
	if result.EvidenceLabel == "" {
		t.Error("EvidenceLabel is empty, expected a label for commits with AI lines")
	}
	if result.EvidenceLabel != "Strong evidence" {
		t.Errorf("EvidenceLabel = %q, want 'Strong evidence' (all exact)", result.EvidenceLabel)
	}
	if result.FallbackCount != 0 {
		t.Errorf("FallbackCount = %d, want 0", result.FallbackCount)
	}

	// Per-file evidence class should be set.
	if len(result.Files) != 1 {
		t.Fatalf("Files count = %d, want 1", len(result.Files))
	}
	if result.Files[0].EvidenceClass == "" {
		t.Error("EvidenceClass is empty on file row")
	}
	if result.Files[0].EvidenceClass != "exact" {
		t.Errorf("EvidenceClass = %q, want 'exact'", result.Files[0].EvidenceClass)
	}
}

func TestRegression_CarryForward_EvidenceOnlyWhenScored(t *testing.T) {
	// A file goes through carry-forward but the historical window has no
	// matching events either. The file should NOT get carry_forward evidence
	// because it scored zero AI lines.
	h := testDB(t)
	ctx := context.Background()
	bs, _ := blobs.NewStore(t.TempDir())
	semDir := t.TempDir()

	repoID := insertRepo(t, h, 100_000)
	repoRoot := "/test/repo/" + repoID

	// CP1 has a manifest with utils.go but NO events with matching content.
	cp1ID := insertCheckpointWithManifest(t, h, bs, repoID, 200_000, []string{"utils.go"})
	_ = h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: "commit1", RepositoryID: repoID, CheckpointID: cp1ID, LinkedAt: 200_000,
	})
	// CP2: current window, also no matching events.
	_ = insertCheckpointWithManifest(t, h, bs, repoID, 300_000, []string{"utils.go"})

	// Diff creates utils.go. Carry-forward will attempt historical lookup
	// but find nothing, so the file should remain human-only.
	diff := "diff --git a/utils.go b/utils.go\n--- /dev/null\n+++ b/utils.go\n@@ -0,0 +1,1 @@\n+package utils\n"
	cp1, _ := h.Queries.GetCheckpointByID(ctx, cp1ID)
	cfr, _ := attributeWithCarryForward(ctx, h, bs, []byte(diff), ComputeAIPercentInput{
		RepoRoot: repoRoot, RepoID: repoID, AfterTs: 200_000, UpToTs: 300_000,
	}, &cp1, semDir)

	// The file should have 0 AI lines and no carry-forward evidence.
	if cfr.result.AILines != 0 {
		t.Errorf("AILines = %d, want 0 (no matching historical events)", cfr.result.AILines)
	}
	// Verify via the reporting path: build the commit result with the same
	// inputs the orchestrator would use, and check evidence.
	scores := []fileScore{{
		path: "utils.go", totalLines: 1, humanLines: 1,
	}}
	dr := parseDiff([]byte(diff))
	// actualCF should be empty because the file scored 0 AI lines.
	actualCF := make(map[string]bool)
	for _, fs := range scores {
		if fs.path == "utils.go" && fileScoreAILines(&fs) > 0 {
			actualCF["utils.go"] = true
		}
	}
	input := buildCommitResultInput(scores, dr, commitResultContext{
		aiTouchedFiles:    map[string]bool{},
		providerModel:     map[string]string{},
		fileTouchOrigins:  map[string]attrreporting.TouchOrigin{},
		carryForwardFiles: actualCF,
	})
	cr := attrreporting.BuildCommitResult(input)
	if len(cr.Files) != 1 {
		t.Fatalf("Files = %d, want 1", len(cr.Files))
	}
	if string(cr.Files[0].PrimaryEvidence) == "carry_forward" {
		t.Error("file with 0 AI lines should NOT have carry_forward evidence")
	}
	if cr.Files[0].PrimaryEvidence != attrreporting.EvidenceNone {
		t.Errorf("PrimaryEvidence = %q, want 'none'", cr.Files[0].PrimaryEvidence)
	}
}

// Blame entry points should converge to AttributeCommit for linked commits.

func TestRegression_Blame_CommitRef(t *testing.T) {
	dir, commitHash := setupGoldenRepo(t)
	ctx := context.Background()
	svc := NewAttributionService()
	direct, err := svc.AttributeCommit(ctx, AttributionInput{RepoPath: dir, CommitHash: commitHash})
	if err != nil { t.Fatalf("AttributeCommit: %v", err) }
	blame, err := svc.Blame(ctx, BlameInput{RepoPath: dir, Ref: commitHash})
	if err != nil { t.Fatalf("Blame(commit): %v", err) }
	assertResultsEqual(t, "Blame(commitHash)", *blame, *direct)
}

func TestRegression_Blame_CommitPrefix(t *testing.T) {
	dir, commitHash := setupGoldenRepo(t)
	ctx := context.Background()
	svc := NewAttributionService()
	direct, err := svc.AttributeCommit(ctx, AttributionInput{RepoPath: dir, CommitHash: commitHash})
	if err != nil { t.Fatalf("AttributeCommit: %v", err) }
	blame, err := svc.Blame(ctx, BlameInput{RepoPath: dir, Ref: commitHash[:8]})
	if err != nil { t.Fatalf("Blame(prefix): %v", err) }
	assertResultsEqual(t, "Blame(prefix)", *blame, *direct)
}

func TestRegression_Blame_CheckpointRef(t *testing.T) {
	dir, commitHash := setupGoldenRepo(t)
	ctx := context.Background()
	svc := NewAttributionService()
	direct, err := svc.AttributeCommit(ctx, AttributionInput{RepoPath: dir, CommitHash: commitHash})
	if err != nil { t.Fatalf("AttributeCommit: %v", err) }

	repo, _ := git.OpenRepo(dir)
	dbPath := filepath.Join(repo.Root(), ".semantica", "lineage.db")
	h, _ := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	link, _ := h.Queries.GetCommitLinkByCommitHash(ctx, commitHash)
	cpID := link.CheckpointID
	_ = sqlstore.Close(h)

	blame, err := svc.Blame(ctx, BlameInput{RepoPath: dir, Ref: cpID})
	if err != nil { t.Fatalf("Blame(checkpoint): %v", err) }
	assertResultsEqual(t, "Blame(checkpointRef)", *blame, *direct)
}

// Unknown blame ref.

func TestRegression_Blame_UnknownRef(t *testing.T) {
	dir, _ := setupGoldenRepo(t)
	ctx := context.Background()
	svc := NewAttributionService()
	_, err := svc.Blame(ctx, BlameInput{RepoPath: dir, Ref: "nonexistent-ref-xyz"})
	if err == nil { t.Fatal("expected error for unknown ref") }
	if !strings.Contains(err.Error(), "not a known commit or checkpoint") {
		t.Errorf("expected 'not a known commit or checkpoint', got: %v", err)
	}
}

// Checkpoint blame without a linked commit.

func TestRegression_Blame_CheckpointWithoutCommit(t *testing.T) {
	dir, _ := setupGoldenRepo(t)
	ctx := context.Background()

	repo, _ := git.OpenRepo(dir)
	repoRoot := repo.Root()
	dbPath := filepath.Join(repoRoot, ".semantica", "lineage.db")
	objectsDir := filepath.Join(repoRoot, ".semantica", "objects")
	h, _ := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	repoRow, _ := h.Queries.GetRepositoryByRootPath(ctx, repoRoot)
	bs, _ := blobs.NewStore(objectsDir)

	srcID := insertSource(t, h, repoRow.RepositoryID, "/data/session2.jsonl")
	sessID := insertSession(t, h, repoRow.RepositoryID, srcID, "unlinked-sess")
	_ = insertEventWithPayload(t, h, bs, sessID, repoRow.RepositoryID, repoRoot,
		400_000, "utils.go", "package utils\nfunc Help() {}\n")

	cpID := uuid.NewString()
	_ = h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpID, RepositoryID: repoRow.RepositoryID,
		CreatedAt: 500_000, Kind: "manual", Status: "complete",
		CompletedAt: sql.NullInt64{Int64: 500_000, Valid: true},
	})
	_ = sqlstore.Close(h)

	svc := NewAttributionService()
	result, err := svc.Blame(ctx, BlameInput{RepoPath: dir, Ref: cpID})
	if err != nil { t.Fatalf("Blame(unlinked): %v", err) }

	if result.CommitHash != "" { t.Errorf("CommitHash = %q, want empty", result.CommitHash) }
	if result.CheckpointID != cpID { t.Errorf("CheckpointID = %q, want %q", result.CheckpointID, cpID) }
	if result.Diagnostics.EventsConsidered != 1 { t.Errorf("EventsConsidered = %d, want 1", result.Diagnostics.EventsConsidered) }
	if result.Diagnostics.AIToolEvents != 1 { t.Errorf("AIToolEvents = %d, want 1", result.Diagnostics.AIToolEvents) }
}

// AttributeCommit preserves deleted-file reporting.

func TestRegression_AttributeCommit_DeletionTouchedFiles(t *testing.T) {
	dir, _ := setupGoldenRepo(t)
	ctx := context.Background()

	// Create old.go, commit it.
	_ = os.WriteFile(filepath.Join(dir, "old.go"), []byte("package old\n\nfunc Legacy() {}\n"), 0o644)
	gitCmd := exec.Command("git", "add", "old.go")
	gitCmd.Dir = dir
	_, _ = gitCmd.CombinedOutput()
	gitCmd = exec.Command("git", "commit", "-m", "add old.go")
	gitCmd.Dir = dir
	_, _ = gitCmd.CombinedOutput()

	// Delete old.go, commit.
	_ = os.Remove(filepath.Join(dir, "old.go"))
	gitCmd = exec.Command("git", "add", "old.go")
	gitCmd.Dir = dir
	_, _ = gitCmd.CombinedOutput()
	gitCmd = exec.Command("git", "commit", "-m", "remove old.go")
	gitCmd.Dir = dir
	_, _ = gitCmd.CombinedOutput()
	gitCmd = exec.Command("git", "rev-parse", "HEAD")
	gitCmd.Dir = dir
	out, _ := gitCmd.Output()
	deleteCommit := strings.TrimSpace(string(out))

	// Link the delete commit to a checkpoint.
	repo, _ := git.OpenRepo(dir)
	dbPath := filepath.Join(repo.Root(), ".semantica", "lineage.db")
	h, _ := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	repoRow, _ := h.Queries.GetRepositoryByRootPath(ctx, repo.Root())
	cpID := uuid.NewString()
	_ = h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpID, RepositoryID: repoRow.RepositoryID,
		CreatedAt: 600_000, Kind: "auto", Status: "complete",
		CompletedAt: sql.NullInt64{Int64: 600_000, Valid: true},
	})
	_ = h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: deleteCommit, RepositoryID: repoRow.RepositoryID,
		CheckpointID: cpID, LinkedAt: 600_000,
	})
	_ = sqlstore.Close(h)

	svc := NewAttributionService()
	result, err := svc.AttributeCommit(ctx, AttributionInput{RepoPath: dir, CommitHash: deleteCommit})
	if err != nil { t.Fatalf("AttributeCommit(delete): %v", err) }

	deletedFound := false
	for _, fc := range result.FilesDeleted {
		if fc.Path == "old.go" { deletedFound = true }
	}
	if !deletedFound { t.Error("old.go not found in FilesDeleted") }
	if result.TotalLines != 0 { t.Errorf("TotalLines = %d, want 0", result.TotalLines) }
}

// Provider file-touch events should still mark FilesCreated as AI even when
// there is no line-level payload content.

func TestRegression_FileChangeAI_ProviderTouchNoLineLevelEvidence(t *testing.T) {
	dir, _ := setupGoldenRepo(t)
	ctx := context.Background()

	// Add a new file, commit it.
	_ = os.WriteFile(filepath.Join(dir, "touched.go"), []byte("package touched\nfunc F() {}\n"), 0o644)
	gitCmd := exec.Command("git", "add", "touched.go")
	gitCmd.Dir = dir
	_, _ = gitCmd.CombinedOutput()
	gitCmd = exec.Command("git", "commit", "-m", "add touched.go")
	gitCmd.Dir = dir
	_, _ = gitCmd.CombinedOutput()
	gitCmd = exec.Command("git", "rev-parse", "HEAD")
	gitCmd.Dir = dir
	out, _ := gitCmd.Output()
	commitHash := strings.TrimSpace(string(out))

	// Insert a provider file-touch event (no payload, no line-level data).
	repo, _ := git.OpenRepo(dir)
	dbPath := filepath.Join(repo.Root(), ".semantica", "lineage.db")
	h, _ := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	repoRow, _ := h.Queries.GetRepositoryByRootPath(ctx, repo.Root())

	srcID := insertSource(t, h, repoRow.RepositoryID, "/data/cursor.jsonl")
	sessID := insertSessionWithProvider(t, h, repoRow.RepositoryID, srcID, "sess-cursor-touch", "cursor")
	insertProviderFileTouchEvent(t, h, sessID, repoRow.RepositoryID, "cursor", "cursor_edit", 350_000, "touched.go")

	cpID := uuid.NewString()
	_ = h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpID, RepositoryID: repoRow.RepositoryID,
		CreatedAt: 400_000, Kind: "auto", Status: "complete",
		CompletedAt: sql.NullInt64{Int64: 400_000, Valid: true},
	})
	_ = h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: commitHash, RepositoryID: repoRow.RepositoryID,
		CheckpointID: cpID, LinkedAt: 400_000,
	})
	_ = sqlstore.Close(h)

	svc := NewAttributionService()
	result, err := svc.AttributeCommit(ctx, AttributionInput{RepoPath: dir, CommitHash: commitHash})
	if err != nil {
		t.Fatalf("AttributeCommit: %v", err)
	}

	// The file has no exact/formatted/modified scored lines (no payload),
	// but it should be marked AI=true in FilesCreated because of the
	// provider file-touch event.
	foundInCreated := false
	for _, fc := range result.FilesCreated {
		if fc.Path == "touched.go" {
			foundInCreated = true
			if !fc.AI {
				t.Error("touched.go FilesCreated.AI should be true (provider file-touch, zero scored lines)")
			}
		}
	}
	if !foundInCreated {
		t.Error("touched.go not found in FilesCreated")
	}

	// AI percentage is 100% because all lines are AI-Modified via provider touch.
	if result.AIPercentage != 100 {
		t.Errorf("AIPercentage = %.1f, want 100", result.AIPercentage)
	}
}

// Bash deletion events should still mark FilesDeleted entries as AI even when
// the commit adds no lines.

func TestRegression_FileChangeAI_BashDeletionZeroScored(t *testing.T) {
	dir, _ := setupGoldenRepo(t)
	ctx := context.Background()

	// Create a file, commit, then delete via AI bash command.
	_ = os.WriteFile(filepath.Join(dir, "doomed.go"), []byte("package doomed\n"), 0o644)
	gitCmd := exec.Command("git", "add", "doomed.go")
	gitCmd.Dir = dir
	_, _ = gitCmd.CombinedOutput()
	gitCmd = exec.Command("git", "commit", "-m", "add doomed.go")
	gitCmd.Dir = dir
	_, _ = gitCmd.CombinedOutput()

	// Delete and commit.
	_ = os.Remove(filepath.Join(dir, "doomed.go"))
	gitCmd = exec.Command("git", "add", "doomed.go")
	gitCmd.Dir = dir
	_, _ = gitCmd.CombinedOutput()
	gitCmd = exec.Command("git", "commit", "-m", "remove doomed.go")
	gitCmd.Dir = dir
	_, _ = gitCmd.CombinedOutput()
	gitCmd = exec.Command("git", "rev-parse", "HEAD")
	gitCmd.Dir = dir
	out, _ := gitCmd.Output()
	deleteCommit := strings.TrimSpace(string(out))

	// Insert AI bash rm event.
	repo, _ := git.OpenRepo(dir)
	repoRoot := repo.Root()
	dbPath := filepath.Join(repoRoot, ".semantica", "lineage.db")
	objectsDir := filepath.Join(repoRoot, ".semantica", "objects")
	h, _ := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	repoRow, _ := h.Queries.GetRepositoryByRootPath(ctx, repoRoot)
	bs, _ := blobs.NewStore(objectsDir)

	srcID := insertSource(t, h, repoRow.RepositoryID, "/data/session.jsonl")
	sessID := insertSession(t, h, repoRow.RepositoryID, srcID, "sess-rm")

	payload := fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"rm %s/doomed.go"}}]}}`, repoRoot)
	payloadHash, _, _ := bs.Put(ctx, []byte(payload))
	_ = h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID: uuid.NewString(), SessionID: sessID, RepositoryID: repoRow.RepositoryID,
		Ts: 450_000, Kind: "assistant", Role: sqlstore.NullStr("assistant"),
		ToolUses:    sql.NullString{String: `{"content_types":["tool_use"],"tools":[{"name":"Bash"}]}`, Valid: true},
		PayloadHash: sqlstore.NullStr(payloadHash),
	})

	cpID := uuid.NewString()
	_ = h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpID, RepositoryID: repoRow.RepositoryID,
		CreatedAt: 500_000, Kind: "auto", Status: "complete",
		CompletedAt: sql.NullInt64{Int64: 500_000, Valid: true},
	})
	_ = h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: deleteCommit, RepositoryID: repoRow.RepositoryID,
		CheckpointID: cpID, LinkedAt: 500_000,
	})
	_ = sqlstore.Close(h)

	svc := NewAttributionService()
	result, err := svc.AttributeCommit(ctx, AttributionInput{RepoPath: dir, CommitHash: deleteCommit})
	if err != nil {
		t.Fatalf("AttributeCommit: %v", err)
	}

	// Deleted file has zero added lines (TotalLines=0, AIPercentage=0).
	if result.TotalLines != 0 {
		t.Errorf("TotalLines = %d, want 0", result.TotalLines)
	}

	// But FilesDeleted should mark doomed.go with AI=true because
	// the AI ran the rm command.
	foundDeleted := false
	for _, fc := range result.FilesDeleted {
		if fc.Path == "doomed.go" {
			foundDeleted = true
			if !fc.AI {
				t.Error("doomed.go FilesDeleted.AI should be true (bash rm, zero scored lines)")
			}
		}
	}
	if !foundDeleted {
		t.Error("doomed.go not found in FilesDeleted")
	}

	// Headline AI percentage unchanged (0% because no added lines).
	if result.AIPercentage != 0 {
		t.Errorf("AIPercentage = %.1f, want 0 (deleted file only)", result.AIPercentage)
	}
}
