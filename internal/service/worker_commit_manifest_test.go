package service

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// commitWorld provides an enabled Git repository for enrichment tests.
type commitWorld struct {
	dir    string
	repo   *git.Repo
	h      *sqlstore.Handle
	bs     *blobs.Store
	repoID string
	git    func(args ...string) string
}

func newCommitWorld(t *testing.T) *commitWorld {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	initCmd := exec.Command("git", "init", dir)
	initCmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	// Exclude Semantica state without adding a tracked .gitignore.
	if err := os.WriteFile(filepath.Join(dir, ".git", "info", "exclude"), []byte(".semantica/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd := func(args ...string) string {
		base := []string{"-C", dir, "-c", "user.email=t@t.t", "-c", "user.name=t", "-c", "core.autocrlf=false"}
		c := exec.Command("git", append(base, args...)...)
		c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	semDir := filepath.Join(dir, ".semantica")
	objectsDir := filepath.Join(semDir, "objects")
	if err := os.MkdirAll(objectsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(semDir, "lineage.db")
	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatal(err)
	}
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlstore.Close(h) })
	repo, err := git.OpenRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	repoID := uuid.NewString()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: repo.Root(), CreatedAt: 1, EnabledAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	bs, err := blobs.NewStore(objectsDir)
	if err != nil {
		t.Fatal(err)
	}
	return &commitWorld{dir: dir, repo: repo, h: h, bs: bs, repoID: repoID, git: gitCmd}
}

func (w *commitWorld) write(t *testing.T, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(w.dir, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// checkpoint inserts an auto checkpoint and links it to commit (when non-empty).
func (w *commitWorld) checkpoint(t *testing.T, commits ...string) string {
	return w.checkpointKind(t, "auto", commits...)
}

// checkpointKind inserts a checkpoint of the given kind and links commits.
func (w *commitWorld) checkpointKind(t *testing.T, kind string, commits ...string) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()
	if err := w.h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: id, RepositoryID: w.repoID, CreatedAt: 1, Kind: kind, Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range commits {
		if c == "" {
			continue
		}
		if err := w.h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
			CommitHash: c, RepositoryID: w.repoID, CheckpointID: id, LinkedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func (w *commitWorld) enrich(t *testing.T, cpID, commitHint string) (enrichResult, error) {
	t.Helper()
	ctx := context.Background()
	cp, err := w.h.Queries.GetCheckpointByID(ctx, cpID)
	if err != nil {
		t.Fatal(err)
	}
	wctx := &workerContext{h: w.h, blobStore: w.bs, repo: w.repo, cp: cp, semDir: filepath.Join(w.dir, ".semantica")}
	return enrichCheckpoint(ctx, wctx, WorkerInput{CheckpointID: cpID, CommitHash: commitHint, RepoRoot: w.dir})
}

func (w *commitWorld) parseManifest(t *testing.T, hash string) blobs.Manifest {
	t.Helper()
	raw, err := w.bs.Get(context.Background(), hash)
	if err != nil {
		t.Fatal(err)
	}
	m, err := blobs.ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func manifestFile(m blobs.Manifest, path string) (blobs.ManifestFile, bool) {
	for _, f := range m.Files {
		if f.Path == path {
			return f, true
		}
	}
	return blobs.ManifestFile{}, false
}

// A commit manifest remains anchored when the worktree changes before enrichment.
func TestEnrichCheckpoint_CommitManifestAnchorsToCommit(t *testing.T) {
	ctx := context.Background()
	w := newCommitWorld(t)

	w.write(t, "a.txt", "committed A\n")
	w.git("add", "-A")
	w.git("commit", "-m", "A")
	commitA := w.git("rev-parse", "HEAD")

	cpID := w.checkpoint(t, commitA)

	// Change the worktree and create commit B after checkpoint A.
	w.write(t, "a.txt", "DRIFTED worktree content\n")
	w.write(t, "untracked.txt", "never committed\n")
	w.git("add", "a.txt")
	w.git("commit", "-m", "B")

	res, err := w.enrich(t, cpID, commitA)
	if err != nil {
		t.Fatalf("enrichCheckpoint: %v", err)
	}

	m := w.parseManifest(t, res.manifestHash)
	if !m.IsCommitScoped() || m.CommitHash != commitA {
		t.Fatalf("manifest = commitScoped=%v hash=%s, want commit-scoped for %s", m.IsCommitScoped(), m.CommitHash, commitA)
	}
	fa, ok := manifestFile(m, "a.txt")
	if !ok {
		t.Fatal("a.txt missing from commit manifest")
	}
	content, err := w.bs.Get(ctx, fa.Blob)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "committed A\n" {
		t.Errorf("a.txt content = %q, want the committed bytes, not the drifted worktree", content)
	}
	if _, ok := manifestFile(m, "untracked.txt"); ok {
		t.Error("untracked file must be excluded from a commit manifest")
	}
	// Root commit A: one changed path.
	if res.filesChanged != 1 || res.fileCount != 1 {
		t.Errorf("stats = filesChanged=%d fileCount=%d, want 1/1", res.filesChanged, res.fileCount)
	}
}

func TestEnrichCheckpoint_RejectsMultipleLinks(t *testing.T) {
	w := newCommitWorld(t)
	w.write(t, "a.txt", "a\n")
	w.git("add", "-A")
	w.git("commit", "-m", "A")
	commitA := w.git("rev-parse", "HEAD")
	w.write(t, "b.txt", "b\n")
	w.git("add", "-A")
	w.git("commit", "-m", "B")
	commitB := w.git("rev-parse", "HEAD")

	cpID := w.checkpoint(t, commitA, commitB) // two links: ambiguous
	if _, err := w.enrich(t, cpID, commitA); err == nil {
		t.Error("enrichCheckpoint = nil error for a checkpoint with multiple commit links")
	}
}

func TestEnrichCheckpoint_RejectsWorkerLinkMismatch(t *testing.T) {
	w := newCommitWorld(t)
	w.write(t, "a.txt", "a\n")
	w.git("add", "-A")
	w.git("commit", "-m", "A")
	commitA := w.git("rev-parse", "HEAD")

	cpID := w.checkpoint(t, commitA)
	other := strings.Repeat("b", 40)
	if _, err := w.enrich(t, cpID, other); err == nil {
		t.Error("enrichCheckpoint = nil error when the worker commit does not match the persisted link")
	}
}

func TestEnrichCheckpoint_NoLinkBuildsWorkspace(t *testing.T) {
	w := newCommitWorld(t)
	w.write(t, "a.txt", "a\n")
	w.git("add", "-A")
	w.git("commit", "-m", "A")

	cpID := w.checkpoint(t) // no commit link -> workspace
	res, err := w.enrich(t, cpID, "")
	if err != nil {
		t.Fatalf("enrichCheckpoint: %v", err)
	}
	m := w.parseManifest(t, res.manifestHash)
	if m.IsCommitScoped() {
		t.Error("a checkpoint with no commit link must produce a workspace manifest")
	}
}

func TestEnrichCheckpoint_CommitStatsDiffAgainstParent(t *testing.T) {
	w := newCommitWorld(t)
	w.write(t, "a.txt", "a\n")
	w.write(t, "keep.txt", "keep\n")
	w.git("add", "-A")
	w.git("commit", "-m", "root")

	// Change one file and add another; keep.txt remains unchanged.
	w.write(t, "a.txt", "a changed\n")
	w.write(t, "b.txt", "b\n")
	w.git("add", "-A")
	w.git("commit", "-m", "second")
	commit2 := w.git("rev-parse", "HEAD")

	cpID := w.checkpoint(t, commit2)
	res, err := w.enrich(t, cpID, commit2)
	if err != nil {
		t.Fatalf("enrichCheckpoint: %v", err)
	}
	if res.filesChanged != 2 {
		t.Errorf("filesChanged = %d, want 2 (a.txt changed, b.txt added)", res.filesChanged)
	}
	if res.fileCount != 3 {
		t.Errorf("fileCount = %d, want 3 tracked tree entries", res.fileCount)
	}
}

func TestEnrichCheckpoint_EmptyHintUsesLink(t *testing.T) {
	w := newCommitWorld(t)
	w.write(t, "a.txt", "a\n")
	w.git("add", "-A")
	w.git("commit", "-m", "A")
	commitA := w.git("rev-parse", "HEAD")

	cpID := w.checkpoint(t, commitA)
	// The persisted link determines commit scope when the hint is empty.
	res, err := w.enrich(t, cpID, "")
	if err != nil {
		t.Fatalf("enrichCheckpoint: %v", err)
	}
	m := w.parseManifest(t, res.manifestHash)
	if !m.IsCommitScoped() || m.CommitHash != commitA {
		t.Errorf("manifest commitScoped=%v hash=%s, want commit-scoped for %s", m.IsCommitScoped(), m.CommitHash, commitA)
	}
}

func TestEnrichCheckpoint_RejectsHintWithoutLink(t *testing.T) {
	w := newCommitWorld(t)
	w.write(t, "a.txt", "a\n")
	w.git("add", "-A")
	w.git("commit", "-m", "A")
	commitA := w.git("rev-parse", "HEAD")

	cpID := w.checkpoint(t) // no link
	if _, err := w.enrich(t, cpID, commitA); err == nil {
		t.Error("enrichCheckpoint = nil error for a commit hint with no persisted link")
	}
}

func TestEnrichCheckpoint_RejectsWorkspaceKindWithLink(t *testing.T) {
	w := newCommitWorld(t)
	w.write(t, "a.txt", "a\n")
	w.git("add", "-A")
	w.git("commit", "-m", "A")
	commitA := w.git("rev-parse", "HEAD")

	// Manual checkpoints cannot carry commit links.
	cpID := w.checkpointKind(t, "manual", commitA)
	if _, err := w.enrich(t, cpID, commitA); err == nil {
		t.Error("enrichCheckpoint = nil error for a manual checkpoint carrying a commit link")
	}
}

func TestEnrichCheckpoint_MergeUsesFirstParent(t *testing.T) {
	w := newCommitWorld(t)
	w.write(t, "base.txt", "base\n")
	w.git("add", "-A")
	w.git("commit", "-m", "base")
	mainBranch := w.git("rev-parse", "--abbrev-ref", "HEAD")

	w.git("checkout", "-b", "feature")
	w.write(t, "feature.txt", "f\n")
	w.git("add", "-A")
	w.git("commit", "-m", "feature")

	w.git("checkout", mainBranch)
	w.write(t, "main.txt", "m\n")
	w.git("add", "-A")
	w.git("commit", "-m", "main")
	w.git("merge", "--no-ff", "feature", "-m", "merge")
	mergeCommit := w.git("rev-parse", "HEAD")

	cpID := w.checkpoint(t, mergeCommit)
	res, err := w.enrich(t, cpID, mergeCommit)
	if err != nil {
		t.Fatalf("enrichCheckpoint: %v", err)
	}
	// Only feature.txt differs from the first parent.
	if res.filesChanged != 1 {
		t.Errorf("filesChanged = %d, want 1 (merge diffed against first parent)", res.filesChanged)
	}
}

func TestEnrichCheckpoint_RenameCountsAsTwo(t *testing.T) {
	w := newCommitWorld(t)
	w.write(t, "old.txt", "content\n")
	w.git("add", "-A")
	w.git("commit", "-m", "add")

	w.git("mv", "old.txt", "new.txt")
	w.git("commit", "-m", "rename")
	renameCommit := w.git("rev-parse", "HEAD")

	cpID := w.checkpoint(t, renameCommit)
	res, err := w.enrich(t, cpID, renameCommit)
	if err != nil {
		t.Fatalf("enrichCheckpoint: %v", err)
	}
	// Rename detection is disabled, so this counts as delete plus add.
	if res.filesChanged != 2 {
		t.Errorf("filesChanged = %d, want 2 (rename as delete + add)", res.filesChanged)
	}
}

// Post-completion work receives the resolved commit anchor.
func TestWorkerRun_PostCompletionUsesResolvedAnchor(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	t.Setenv("SEMANTICA_HOME", filepath.Join(dir, ".semantica-global"))
	t.Setenv("HOME", dir)

	gitRun(t, dir, "init")
	gitRun(t, dir, "config", "user.email", "t@t.t")
	gitRun(t, dir, "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "main.go")
	gitRun(t, dir, "commit", "-m", "init")
	commit := gitRun(t, dir, "rev-parse", "HEAD")

	enableSemantica(t, ctx, dir)

	semDir := filepath.Join(dir, ".semantica")
	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: "ck-anchor", RepositoryID: repoRow.RepositoryID, CreatedAt: 1, Kind: "auto", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: commit, RepositoryID: repoRow.RepositoryID, CheckpointID: "ck-anchor", LinkedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	_ = sqlstore.Close(h)

	var called bool
	var gotCommit string
	orig := runPostCompletionFn
	runPostCompletionFn = func(_ context.Context, _ *workerContext, in WorkerInput) {
		called, gotCommit = true, in.CommitHash
	}
	defer func() { runPostCompletionFn = orig }()

	// Call processOne directly so the queue cannot populate the hint.
	if err := NewWorkerService(nil).processOne(ctx, WorkerInput{CheckpointID: "ck-anchor", CommitHash: "", RepoRoot: dir}); err != nil {
		t.Fatalf("processOne: %v", err)
	}
	if !called {
		t.Fatal("post-completion was not invoked")
	}
	if gotCommit != commit {
		t.Errorf("post-completion commit = %q, want the resolved anchor %q", gotCommit, commit)
	}
}

// A workspace checkpoint does not use a commit manifest as its baseline.
func TestEnrichCheckpoint_WorkspaceIgnoresCommitPredecessor(t *testing.T) {
	ctx := context.Background()
	w := newCommitWorld(t)
	w.write(t, "a.txt", "a\n")
	w.write(t, "b.txt", "b\n")
	w.git("add", "-A")
	w.git("commit", "-m", "A")
	commitA := w.git("rev-parse", "HEAD")

	cpA := w.checkpoint(t, commitA)
	resA, err := w.enrich(t, cpA, commitA)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.h.Queries.CompleteCheckpoint(ctx, sqldb.CompleteCheckpointParams{
		CheckpointID: cpA, ManifestHash: sqlstore.NullStr(resA.manifestHash),
		SizeBytes: sqlNullInt(resA.totalBytes), CompletedAt: sqlNullInt(2),
	}); err != nil {
		t.Fatal(err)
	}

	// Add an untracked file before the workspace checkpoint.
	w.write(t, "c.txt", "c\n")
	cpB := w.checkpoint(t) // no commit link -> workspace
	resB, err := w.enrich(t, cpB, "")
	if err != nil {
		t.Fatal(err)
	}
	if m := w.parseManifest(t, resB.manifestHash); m.IsCommitScoped() {
		t.Error("workspace checkpoint produced a commit manifest")
	}
	// Without a workspace baseline, all three files count as changed.
	if resB.filesChanged != 3 {
		t.Errorf("filesChanged = %d, want 3 (no comparable workspace baseline)", resB.filesChanged)
	}
}

func TestEnrichCheckpoint_CommitReusesPreviousBlobs(t *testing.T) {
	ctx := context.Background()
	w := newCommitWorld(t)

	w.write(t, "a.txt", "a\n")
	w.git("add", "-A")
	w.git("commit", "-m", "A")
	commitA := w.git("rev-parse", "HEAD")
	cpA := w.checkpoint(t, commitA)
	resA, err := w.enrich(t, cpA, commitA)
	if err != nil {
		t.Fatal(err)
	}
	// Complete checkpoint A so it can seed reuse.
	if err := w.h.Queries.CompleteCheckpoint(ctx, sqldb.CompleteCheckpointParams{
		CheckpointID: cpA, ManifestHash: sqlstore.NullStr(resA.manifestHash),
		SizeBytes: sqlNullInt(resA.totalBytes), CompletedAt: sqlNullInt(2),
	}); err != nil {
		t.Fatal(err)
	}
	mA := w.parseManifest(t, resA.manifestHash)
	blobA, _ := manifestFile(mA, "a.txt")

	// Commit B adds b.txt while a.txt remains unchanged.
	w.write(t, "b.txt", "b\n")
	w.git("add", "-A")
	w.git("commit", "-m", "B")
	commitB := w.git("rev-parse", "HEAD")
	cpB := w.checkpoint(t, commitB)
	resB, err := w.enrich(t, cpB, commitB)
	if err != nil {
		t.Fatal(err)
	}
	mB := w.parseManifest(t, resB.manifestHash)
	blobB, ok := manifestFile(mB, "a.txt")
	if !ok {
		t.Fatal("a.txt missing from commit B manifest")
	}
	if blobB.Blob != blobA.Blob {
		t.Errorf("a.txt blob B=%q A=%q, want the reused blob", blobB.Blob, blobA.Blob)
	}
}

func sqlNullInt(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }
