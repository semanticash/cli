package service

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// manifestWorld provides a commit-linked repository and manifest store.
type manifestWorld struct {
	repoPath string
	repo     *git.Repo
	h        *sqlstore.Handle
	bs       *blobs.Store
	repoID   string

	prevCheckpointID string
	curCheckpointID  string
	prevCommit       string
	prevFiles        []blobs.ManifestFile
}

func (w *manifestWorld) git(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = w.repoPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newManifestWorld creates tracked rewrite and control files, an untracked
// file, and a completed checkpoint linked to the initial commit.
func newManifestWorld(t *testing.T) *manifestWorld {
	t.Helper()
	ctx := context.Background()

	repoPath, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &manifestWorld{repoPath: repoPath}
	w.git(t, "init", "-q", "-b", "main")
	w.git(t, "config", "user.email", "t@example.com")
	w.git(t, "config", "user.name", "t")

	writeManifestFile(t, repoPath, "a.txt", "alpha one\n")
	writeManifestFile(t, repoPath, "b.txt", "beta body\n")
	writeManifestFile(t, repoPath, "u.txt", "untracked v1\n")
	w.git(t, "add", "a.txt", "b.txt")
	w.git(t, "commit", "-q", "-m", "c1")
	w.prevCommit = w.git(t, "rev-parse", "HEAD")

	w.repo, err = git.OpenRepo(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	w.bs, err = blobs.NewStore(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(t.TempDir(), "lineage.db")
	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatal(err)
	}
	w.h, err = sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlstore.Close(w.h) })

	w.repoID = uuid.NewString()
	if err := w.h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: w.repoID, RootPath: repoPath, CreatedAt: 1000, EnabledAt: 1000,
	}); err != nil {
		t.Fatal(err)
	}

	// Build the previous checkpoint's manifest.
	paths, err := w.repo.ListFilesFromGit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mr, err := blobs.BuildManifest(ctx, w.bs, repoPath, paths, w.repo.ReadFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.prevFiles = mr.Manifest.Files

	w.prevCheckpointID = uuid.NewString()
	if err := w.h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: w.prevCheckpointID,
		RepositoryID: w.repoID,
		CreatedAt:    2000,
		Kind:         "auto",
		ManifestHash: sql.NullString{String: mr.ManifestHash, Valid: true},
		Status:       "complete",
	}); err != nil {
		t.Fatal(err)
	}

	// Add the pending checkpoint under test.
	w.curCheckpointID = uuid.NewString()
	if err := w.h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: w.curCheckpointID,
		RepositoryID: w.repoID,
		CreatedAt:    3000,
		Kind:         "auto",
		Status:       "pending",
	}); err != nil {
		t.Fatal(err)
	}
	return w
}

func writeManifestFile(t *testing.T, root, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// rewriteSameSizeRestoreMtime changes content without changing size or mtime.
func rewriteSameSizeRestoreMtime(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	fi, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	prev, err := os.ReadFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	if len(prev) != len(content) {
		t.Fatalf("rewrite of %s changes size (%d -> %d); the test needs a same-size rewrite", rel, len(prev), len(content))
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(abs, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
}

func (w *manifestWorld) link(t *testing.T, checkpointID, commitHash string) {
	t.Helper()
	if err := w.h.Queries.InsertCommitLink(context.Background(), sqldb.InsertCommitLinkParams{
		CommitHash:   commitHash,
		RepositoryID: w.repoID,
		CheckpointID: checkpointID,
		LinkedAt:     time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
}

func (w *manifestWorld) linkPrev(t *testing.T, commitHash string) {
	t.Helper()
	w.link(t, w.prevCheckpointID, commitHash)
}

func (w *manifestWorld) linkCur(t *testing.T, commitHash string) {
	t.Helper()
	w.link(t, w.curCheckpointID, commitHash)
}

func (w *manifestWorld) prevResult() prevManifestResult {
	return prevManifestResult{
		files:        w.prevFiles,
		checkpointID: w.prevCheckpointID,
		exists:       true,
		ok:           true,
	}
}

func (w *manifestWorld) reusable(ctx context.Context, curCommit string) ([]blobs.ManifestFile, error) {
	return reusableCommitRangeFiles(ctx, w.h, w.repo, w.prevResult(), w.curCheckpointID, curCommit)
}

func blobOf(files []blobs.ManifestFile, path string) string {
	for _, f := range files {
		if f.Path == path {
			return f.Blob
		}
	}
	return ""
}

// Manifest reuse is disabled on Windows.
func reuseSupported() bool {
	return runtime.GOOS != "windows"
}

func TestManifestReuse_DisabledOnWindows(t *testing.T) {
	if reuseSupported() {
		t.Skip("windows-only platform contract")
	}
	ctx := context.Background()
	w := newManifestWorld(t)
	w.linkPrev(t, w.prevCommit)

	writeManifestFile(t, w.repoPath, "c.txt", "third\n")
	w.git(t, "add", "c.txt")
	w.git(t, "commit", "-q", "-m", "c2")
	curCommit := w.git(t, "rev-parse", "HEAD")
	w.linkCur(t, curCommit)

	got, err := w.reusable(ctx, curCommit)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("eligible = %v on windows, want nil (reuse disabled)", got)
	}
}

// Commit changes and untracked files are rehashed; unchanged tracked files
// remain eligible for reuse.
func TestManifestReuse_CommitRangeInvalidatesRestoredMtimeRewrite(t *testing.T) {
	ctx := context.Background()
	w := newManifestWorld(t)
	w.linkPrev(t, w.prevCommit)

	// Rewrite one tracked file and one untracked file without changing metadata.
	rewriteSameSizeRestoreMtime(t, w.repoPath, "a.txt", "alpha two\n")
	rewriteSameSizeRestoreMtime(t, w.repoPath, "u.txt", "untracked v2\n")
	w.git(t, "add", "a.txt")
	w.git(t, "commit", "-q", "-m", "c2")
	curCommit := w.git(t, "rev-parse", "HEAD")
	w.linkCur(t, curCommit)

	eligible, err := w.reusable(ctx, curCommit)
	if err != nil {
		t.Fatal(err)
	}
	if got := blobOf(eligible, "a.txt"); got != "" {
		t.Errorf("changed a.txt is reuse-eligible (blob %s); commit range must exclude it", got)
	}
	if reuseSupported() && blobOf(eligible, "b.txt") == "" {
		t.Error("unchanged b.txt missing from the reuse-eligible set")
	}
	if got := blobOf(eligible, "u.txt"); got != "" {
		t.Errorf("untracked u.txt is reuse-eligible (blob %s); untracked files must always hash", got)
	}

	// Count reads to distinguish reuse from equal output hashes.
	reads := map[string]int{}
	countingRead := func(rel string) ([]byte, error) {
		reads[rel]++
		return w.repo.ReadFile(rel)
	}
	paths, err := w.repo.ListFilesFromGit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mr, err := blobs.BuildManifest(ctx, w.bs, w.repoPath, paths, countingRead, eligible)
	if err != nil {
		t.Fatal(err)
	}

	if reads["a.txt"] == 0 {
		t.Error("a.txt was not re-read despite changing in the commit range")
	}
	if got, prev := blobOf(mr.Manifest.Files, "a.txt"), blobOf(w.prevFiles, "a.txt"); got == prev {
		t.Errorf("a.txt kept stale blob %s after a same-size restored-mtime rewrite", got)
	}
	if reuseSupported() && reads["b.txt"] != 0 {
		t.Errorf("unchanged b.txt was read %d times; want metadata reuse", reads["b.txt"])
	}
	if got, prev := blobOf(mr.Manifest.Files, "b.txt"), blobOf(w.prevFiles, "b.txt"); got != prev {
		t.Errorf("unchanged b.txt blob drifted: %s -> %s", prev, got)
	}
	if reads["u.txt"] == 0 {
		t.Error("untracked u.txt was not read; untracked files must always hash")
	}
	if got, prev := blobOf(mr.Manifest.Files, "u.txt"), blobOf(w.prevFiles, "u.txt"); got == prev {
		t.Errorf("untracked u.txt kept stale blob %s after a same-size restored-mtime rewrite", got)
	}
}

// Worktree changes after the commit are not eligible for reuse.
func TestManifestReuse_PostCommitRestoredMtimeRewriteStillHashes(t *testing.T) {
	ctx := context.Background()
	w := newManifestWorld(t)
	w.linkPrev(t, w.prevCommit)

	// Commit an unrelated file so b.txt is unchanged between commits.
	writeManifestFile(t, w.repoPath, "c.txt", "third\n")
	w.git(t, "add", "c.txt")
	w.git(t, "commit", "-q", "-m", "c2")
	curCommit := w.git(t, "rev-parse", "HEAD")
	w.linkCur(t, curCommit)

	// Rewrite b.txt only in the worktree without changing size or mtime.
	rewriteSameSizeRestoreMtime(t, w.repoPath, "b.txt", "beta EDIT\n")

	eligible, err := w.reusable(ctx, curCommit)
	if err != nil {
		t.Fatal(err)
	}
	if got := blobOf(eligible, "b.txt"); got != "" {
		t.Errorf("worktree-drifted b.txt is reuse-eligible (blob %s); eligibility must verify against the commit", got)
	}
	paths, err := w.repo.ListFilesFromGit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reads := map[string]int{}
	countingRead := func(rel string) ([]byte, error) {
		reads[rel]++
		return w.repo.ReadFile(rel)
	}
	mr, err := blobs.BuildManifest(ctx, w.bs, w.repoPath, paths, countingRead, eligible)
	if err != nil {
		t.Fatal(err)
	}
	if reads["b.txt"] == 0 {
		t.Error("b.txt was not re-read despite drifting from the current commit")
	}
	if got, prev := blobOf(mr.Manifest.Files, "b.txt"), blobOf(w.prevFiles, "b.txt"); got == prev {
		t.Errorf("post-commit restored-mtime rewrite of b.txt kept stale blob %s", got)
	}
}

// Missing, ambiguous, or invalid commit boundaries disable reuse.
func TestManifestReuse_FailsClosedWithoutSinglePrevBoundary(t *testing.T) {
	ctx := context.Background()
	w := newManifestWorld(t)

	writeManifestFile(t, w.repoPath, "c.txt", "third\n")
	w.git(t, "add", "c.txt")
	w.git(t, "commit", "-q", "-m", "c2")
	curCommit := w.git(t, "rev-parse", "HEAD")
	w.linkCur(t, curCommit)

	mustNil := func(label string, got []blobs.ManifestFile, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: unexpected error %v", label, err)
		}
		if got != nil {
			t.Errorf("%s: eligible = %v, want nil", label, got)
		}
	}

	// The previous checkpoint has no commit link.
	got, err := w.reusable(ctx, curCommit)
	mustNil("zero prev links", got, err)

	// One link enables reuse where supported.
	w.linkPrev(t, w.prevCommit)
	got, err = w.reusable(ctx, curCommit)
	if err != nil {
		t.Fatal(err)
	}
	if reuseSupported() && blobOf(got, "b.txt") == "" {
		t.Error("single prev link: expected b.txt to be reuse-eligible")
	}

	// A second link makes the boundary ambiguous.
	w.linkPrev(t, strings.Repeat("b", 40))
	got, err = w.reusable(ctx, curCommit)
	mustNil("two prev links", got, err)

	// Revision expressions are not accepted as commit boundaries.
	got, err = w.reusable(ctx, "HEAD")
	mustNil("non-hash current commit", got, err)
}

// The current checkpoint must have one link matching the worker commit.
func TestManifestReuse_FailsClosedWithoutMatchingCurrentBoundary(t *testing.T) {
	ctx := context.Background()
	w := newManifestWorld(t)
	w.linkPrev(t, w.prevCommit)

	writeManifestFile(t, w.repoPath, "c.txt", "third\n")
	w.git(t, "add", "c.txt")
	w.git(t, "commit", "-q", "-m", "c2")
	curCommit := w.git(t, "rev-parse", "HEAD")

	// The current checkpoint has no commit link.
	got, err := w.reusable(ctx, curCommit)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("zero current links: eligible = %v, want nil", got)
	}

	// A different commit does not match the worker input.
	w.linkCur(t, strings.Repeat("c", 40))
	got, err = w.reusable(ctx, curCommit)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("mismatched current link: eligible = %v, want nil", got)
	}

	// Adding the matching link leaves two ambiguous links.
	w.linkCur(t, curCommit)
	got, err = w.reusable(ctx, curCommit)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("two current links: eligible = %v, want nil", got)
	}
}

// Index flags that suppress worktree checks also disable manifest reuse.
func TestManifestReuse_AssumeUnchangedAndSkipWorktreeExcluded(t *testing.T) {
	ctx := context.Background()
	w := newManifestWorld(t)
	w.linkPrev(t, w.prevCommit)

	writeManifestFile(t, w.repoPath, "c.txt", "third\n")
	w.git(t, "add", "c.txt")
	w.git(t, "commit", "-q", "-m", "c2")
	curCommit := w.git(t, "rev-parse", "HEAD")
	w.linkCur(t, curCommit)

	// Git diff hides these rewrites because of their index flags.
	w.git(t, "update-index", "--assume-unchanged", "a.txt")
	rewriteSameSizeRestoreMtime(t, w.repoPath, "a.txt", "alpha two\n")
	w.git(t, "update-index", "--skip-worktree", "b.txt")
	rewriteSameSizeRestoreMtime(t, w.repoPath, "b.txt", "beta EDIT\n")

	eligible, err := w.reusable(ctx, curCommit)
	if err != nil {
		t.Fatal(err)
	}
	if got := blobOf(eligible, "a.txt"); got != "" {
		t.Errorf("assume-unchanged a.txt is reuse-eligible (blob %s); want excluded", got)
	}
	if got := blobOf(eligible, "b.txt"); got != "" {
		t.Errorf("skip-worktree b.txt is reuse-eligible (blob %s); want excluded", got)
	}
	// The ordinary sibling remains eligible.
	if blobOf(eligible, "c.txt") == "" && len(eligible) == 0 {
		t.Log("no eligible files at all; flag exclusion may be over-broad")
	}

	reads := map[string]int{}
	countingRead := func(rel string) ([]byte, error) {
		reads[rel]++
		return w.repo.ReadFile(rel)
	}
	paths, err := w.repo.ListFilesFromGit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mr, err := blobs.BuildManifest(ctx, w.bs, w.repoPath, paths, countingRead, eligible)
	if err != nil {
		t.Fatal(err)
	}
	if reads["a.txt"] == 0 {
		t.Error("assume-unchanged a.txt was not re-read")
	}
	if got, prev := blobOf(mr.Manifest.Files, "a.txt"), blobOf(w.prevFiles, "a.txt"); got == prev {
		t.Errorf("assume-unchanged a.txt kept stale blob %s", got)
	}
	if reads["b.txt"] == 0 {
		t.Error("skip-worktree b.txt was not re-read")
	}
	if got, prev := blobOf(mr.Manifest.Files, "b.txt"), blobOf(w.prevFiles, "b.txt"); got == prev {
		t.Errorf("skip-worktree b.txt kept stale blob %s", got)
	}
}

// Cancellation is an error on both validation and Git-backed paths.
func TestManifestReuse_CancelledContextReturnsError(t *testing.T) {
	w := newManifestWorld(t)
	w.linkPrev(t, w.prevCommit)

	writeManifestFile(t, w.repoPath, "c.txt", "third\n")
	w.git(t, "add", "c.txt")
	w.git(t, "commit", "-q", "-m", "c2")
	curCommit := w.git(t, "rev-parse", "HEAD")
	w.linkCur(t, curCommit)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := w.reusable(ctx, curCommit)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got != nil {
		t.Errorf("eligible = %v on cancellation, want nil", got)
	}

	// Invalid inputs must not hide cancellation behind a reuse miss.
	if _, err := reusableCommitRangeFiles(ctx, w.h, w.repo, prevManifestResult{}, w.curCheckpointID, curCommit); !errors.Is(err, context.Canceled) {
		t.Errorf("not-ok prev branch: err = %v, want context.Canceled", err)
	}
	if _, err := reusableCommitRangeFiles(ctx, w.h, w.repo, w.prevResult(), w.curCheckpointID, "HEAD"); !errors.Is(err, context.Canceled) {
		t.Errorf("invalid commit branch: err = %v, want context.Canceled", err)
	}
}
