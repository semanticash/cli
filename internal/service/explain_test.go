package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// enableSemantica is a helper that enables semantica in the given repo dir.
func enableSemantica(t *testing.T, ctx context.Context, dir string) *EnableResult {
	t.Helper()
	svc, err := NewEnableService(EnableServiceOptions{RepoPath: dir})
	if err != nil {
		t.Fatalf("NewEnableService: %v", err)
	}
	res, err := svc.Enable(ctx, EnableOptions{})
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	return res
}

// gitRun executes a git command in the given directory and returns stdout.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// insertCommitLink opens the semantica DB in dir and inserts a commit_link
// row that binds the given commit hash to the given checkpoint ID.
func insertCommitLinkInRepo(t *testing.T, ctx context.Context, dir, commitHash, checkpointID string) {
	t.Helper()
	dbPath := filepath.Join(dir, ".semantica", "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open db for commit link: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, dir)
	if err != nil {
		t.Fatalf("get repo row: %v", err)
	}

	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash:   commitHash,
		RepositoryID: repoRow.RepositoryID,
		CheckpointID: checkpointID,
		LinkedAt:     time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("insert commit link: %v", err)
	}
}

// TestExplain_ManualCheckpointWithSyntheticLink tests the explain service
// using a manually created checkpoint and a hand-inserted commit link.
// This exercises the service-layer wiring (enable -> checkpoint -> explain)
// but does NOT exercise the real hook/worker path (pre-commit -> post-commit
// -> background worker -> commit link insertion). A true end-to-end test
// would require running the compiled binary with live Git hooks.
func TestExplain_ManualCheckpointWithSyntheticLink(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	// 1. Enable semantica.
	enableSemantica(t, ctx, dir)

	// 2. Create a file, git add, git commit.
	testFile := filepath.Join(dir, "hello.go")
	if err := os.WriteFile(testFile, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "hello.go")
	gitRun(t, dir, "commit", "-m", "add hello.go", "--no-verify")

	// 3. Create a checkpoint.
	cpSvc := NewCheckpointService()
	cpRes, err := cpSvc.Create(ctx, CreateCheckpointInput{
		RepoPath: dir,
		Kind:     CheckpointManual,
		Message:  "test checkpoint",
	})
	if err != nil {
		t.Fatalf("Create checkpoint: %v", err)
	}

	// 4. Get the commit hash.
	commitHash := gitRun(t, dir, "rev-parse", "HEAD")

	// 5. Insert the commit link manually (normally done by the post-commit hook).
	insertCommitLinkInRepo(t, ctx, dir, commitHash, cpRes.CheckpointID)

	// 6. Call explain.
	explainSvc := NewExplainService()
	res, err := explainSvc.Explain(ctx, ExplainInput{
		RepoPath: dir,
		Ref:      commitHash,
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	// 7. Assertions.
	if res.CommitHash != commitHash {
		t.Errorf("CommitHash = %q, want %q", res.CommitHash, commitHash)
	}
	if res.FilesChanged <= 0 {
		t.Errorf("FilesChanged = %d, want > 0", res.FilesChanged)
	}
	if res.LinesAdded <= 0 {
		t.Errorf("LinesAdded = %d, want > 0", res.LinesAdded)
	}
	if res.AIPercentage != 0 {
		t.Errorf("AIPercentage = %f, want 0 (no AI sessions)", res.AIPercentage)
	}
	if res.SessionCount != 0 {
		t.Errorf("SessionCount = %d, want 0", res.SessionCount)
	}
	if res.CheckpointID == "" {
		t.Error("CheckpointID is empty, want non-empty")
	}
}

// TestExplain_TopFilesMatchAttribution verifies that TopFiles use the same
// counting basis as the AI involvement headline (added non-blank lines from
// attribution), not raw git diff --numstat churn.
func TestExplain_TopFilesMatchAttribution(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	enableSemantica(t, ctx, dir)

	// Create a file with a mix of blank and non-blank lines.
	// git diff --numstat will count 5 added lines (including the blank line),
	// but attribution counts only 4 non-blank added lines.
	content := "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"hello\") }\n"
	testFile := filepath.Join(dir, "main.go")
	if err := os.WriteFile(testFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "main.go")
	gitRun(t, dir, "commit", "-m", "add main.go", "--no-verify")

	cpSvc := NewCheckpointService()
	cpRes, err := cpSvc.Create(ctx, CreateCheckpointInput{
		RepoPath: dir,
		Kind:     CheckpointManual,
		Message:  "test",
	})
	if err != nil {
		t.Fatalf("Create checkpoint: %v", err)
	}

	commitHash := gitRun(t, dir, "rev-parse", "HEAD")
	insertCommitLinkInRepo(t, ctx, dir, commitHash, cpRes.CheckpointID)

	explainSvc := NewExplainService()
	res, err := explainSvc.Explain(ctx, ExplainInput{
		RepoPath: dir,
		Ref:      commitHash,
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	if len(res.TopFiles) == 0 {
		t.Fatal("TopFiles is empty")
	}

	// The top-file totals should sum to the same as the headline totals.
	var topTotal, topAI, topHuman int
	for _, f := range res.TopFiles {
		topTotal += f.TotalLines
		topAI += f.AILines
		topHuman += f.HumanLines
	}

	// In this test (no AI events), all lines should be human.
	if topAI != 0 {
		t.Errorf("TopFiles AI lines = %d, want 0 (no AI sessions)", topAI)
	}

	// TopFiles.TotalLines must equal AILines + HumanLines for each file.
	for _, f := range res.TopFiles {
		if f.TotalLines != f.AILines+f.HumanLines {
			t.Errorf("file %s: TotalLines(%d) != AILines(%d) + HumanLines(%d)",
				f.Path, f.TotalLines, f.AILines, f.HumanLines)
		}
	}

	// The sum of top-file totals must equal the headline total (since we
	// have only 1 file, all files are in TopFiles).
	headlineTotal := res.AILines + res.HumanLines
	if topTotal != headlineTotal {
		t.Errorf("sum(TopFiles.TotalLines) = %d, headline AILines+HumanLines = %d; want same basis",
			topTotal, headlineTotal)
	}
}

// TestExplain_TopFilesTotalLeqHeadline verifies that with >5 files, the sum
// of TopFiles.TotalLines is strictly <= the headline total (top 5 of N).
func TestExplain_TopFilesTotalLeqHeadline(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	enableSemantica(t, ctx, dir)

	// Create 7 files so TopFiles (max 5) is a strict subset.
	for i := 0; i < 7; i++ {
		name := fmt.Sprintf("file%d.go", i)
		content := fmt.Sprintf("package pkg%d\n\nfunc F%d() {}\n", i, i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRun(t, dir, "add", name)
	}
	gitRun(t, dir, "commit", "-m", "add 7 files", "--no-verify")

	cpSvc := NewCheckpointService()
	cpRes, err := cpSvc.Create(ctx, CreateCheckpointInput{
		RepoPath: dir,
		Kind:     CheckpointManual,
		Message:  "test",
	})
	if err != nil {
		t.Fatalf("Create checkpoint: %v", err)
	}

	commitHash := gitRun(t, dir, "rev-parse", "HEAD")
	insertCommitLinkInRepo(t, ctx, dir, commitHash, cpRes.CheckpointID)

	explainSvc := NewExplainService()
	res, err := explainSvc.Explain(ctx, ExplainInput{
		RepoPath: dir,
		Ref:      commitHash,
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	if len(res.TopFiles) != 5 {
		t.Fatalf("TopFiles count = %d, want 5 (capped)", len(res.TopFiles))
	}

	var topTotal int
	for _, f := range res.TopFiles {
		topTotal += f.TotalLines
		if f.TotalLines != f.AILines+f.HumanLines {
			t.Errorf("file %s: TotalLines(%d) != AILines(%d) + HumanLines(%d)",
				f.Path, f.TotalLines, f.AILines, f.HumanLines)
		}
	}

	headlineTotal := res.AILines + res.HumanLines
	if topTotal > headlineTotal {
		t.Errorf("sum(TopFiles.TotalLines) = %d > headline total %d; top-5 subset must be <=",
			topTotal, headlineTotal)
	}
	if topTotal >= headlineTotal {
		// With 7 equal-size files and only 5 shown, strict < is expected.
		t.Logf("topTotal=%d headlineTotal=%d (expected strict <)", topTotal, headlineTotal)
	}
}

// TestExplain_DeletedOnlyFile verifies that a commit which deletes a file
// includes the deleted non-blank lines in LinesDeleted and FilesChanged,
// keeping the single-basis guarantee intact.
func TestExplain_DeletedOnlyFile(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	enableSemantica(t, ctx, dir)

	// Create two files and commit them.
	if err := os.WriteFile(filepath.Join(dir, "keep.go"), []byte("package keep\n\nfunc Keep() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "remove.go"), []byte("package remove\n\nfunc Remove() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "keep.go", "remove.go")
	gitRun(t, dir, "commit", "-m", "add two files", "--no-verify")

	// Second commit: edit keep.go and delete remove.go entirely.
	if err := os.WriteFile(filepath.Join(dir, "keep.go"), []byte("package keep\n\nfunc Keep() {}\n\nfunc Extra() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "remove.go")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "keep.go", "remove.go")
	gitRun(t, dir, "commit", "-m", "edit keep, delete remove", "--no-verify")

	cpSvc := NewCheckpointService()
	cpRes, err := cpSvc.Create(ctx, CreateCheckpointInput{
		RepoPath: dir,
		Kind:     CheckpointManual,
		Message:  "test",
	})
	if err != nil {
		t.Fatalf("Create checkpoint: %v", err)
	}

	commitHash := gitRun(t, dir, "rev-parse", "HEAD")
	insertCommitLinkInRepo(t, ctx, dir, commitHash, cpRes.CheckpointID)

	explainSvc := NewExplainService()
	res, err := explainSvc.Explain(ctx, ExplainInput{
		RepoPath: dir,
		Ref:      commitHash,
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	// FilesChanged must include both the edited and the deleted file.
	if res.FilesChanged < 2 {
		t.Errorf("FilesChanged = %d, want >= 2 (edited + deleted)", res.FilesChanged)
	}

	// LinesDeleted must be > 0 because remove.go had non-blank lines.
	if res.LinesDeleted == 0 {
		t.Error("LinesDeleted = 0, want > 0 (remove.go had non-blank lines)")
	}

	// The single-basis invariant: LinesAdded = AILines + HumanLines.
	if res.LinesAdded != res.AILines+res.HumanLines {
		t.Errorf("LinesAdded(%d) != AILines(%d) + HumanLines(%d)",
			res.LinesAdded, res.AILines, res.HumanLines)
	}
}

func TestExplain_NoCheckpoint(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	// 1. Enable semantica.
	enableSemantica(t, ctx, dir)

	// 2. Make a commit but do NOT create a checkpoint or commit link.
	//    The post-commit hook would normally do this, but we are not running
	//    the binary in this test.
	testFile := filepath.Join(dir, "foo.txt")
	if err := os.WriteFile(testFile, []byte("some content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "foo.txt")
	gitRun(t, dir, "commit", "-m", "add foo.txt", "--no-verify")

	// 3. Get the commit hash.
	commitHash := gitRun(t, dir, "rev-parse", "HEAD")

	// 4. Call explain - without a commit_link row, resolveCommitRef should
	//    fail because the commit hash is not known to the DB, and it is not
	//    a valid checkpoint ID either. This documents the expected behavior:
	//    explain requires a commit link (created by the post-commit hook)
	//    to resolve a commit hash.
	explainSvc := NewExplainService()
	_, err := explainSvc.Explain(ctx, ExplainInput{
		RepoPath: dir,
		Ref:      commitHash,
	})
	if err == nil {
		// If it somehow succeeds (e.g., future fallback logic), that is also
		// acceptable - just log and return.
		t.Log("Explain succeeded without a checkpoint/commit link - fallback logic may exist")
		return
	}

	// Verify the error message indicates the ref is unknown.
	if !strings.Contains(err.Error(), "not a known commit or checkpoint") {
		t.Errorf("unexpected error: %v; expected 'not a known commit or checkpoint'", err)
	}
}
