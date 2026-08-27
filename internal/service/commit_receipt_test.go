package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/git"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
)

// setupCommitRepo creates an enabled repository with a migrated database. It
// also stops post-commit from launching a detached worker, which would hold
// worker.log open and block TempDir cleanup on Windows.
func setupCommitRepo(t *testing.T) (dir, semDir, dbPath string) {
	t.Helper()
	origSpawn := spawnWorkerFn
	spawnWorkerFn = func(ctx context.Context, semDir, checkpointID, commitHash, repoRoot string) {}
	t.Cleanup(func() { spawnWorkerFn = origSpawn })

	dir = initGitRepo(t)
	semDir = filepath.Join(dir, ".semantica")
	if err := os.MkdirAll(filepath.Join(semDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath = filepath.Join(semDir, "lineage.db")
	if err := sqlstore.MigratePath(context.Background(), dbPath); err != nil {
		t.Fatal(err)
	}
	return dir, semDir, dbPath
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func headSHA(t *testing.T, dir string) string {
	t.Helper()
	repo, err := git.OpenRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	sha, err := repo.HeadCommitHash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

// A matching handoff is linked and removed after the commit.
func TestCommitCapture_LinksCurrentCommit(t *testing.T) {
	dir, semDir, dbPath := setupCommitRepo(t)
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "f.txt")
	if err := NewPreCommitService().HandlePreCommit(ctx, dir); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "commit", "-m", "c1")
	sha := headSHA(t, dir)

	res, err := NewPostCommitService().HandlePostCommit(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Linked || res.CommitHash != sha {
		t.Fatalf("post-commit result = %+v, want linked %s", res, sha)
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	if _, err := h.Queries.GetCommitLinkByCommitHash(ctx, sha); err != nil {
		t.Errorf("commit not linked: %v", err)
	}
	if r, _ := listCommitReceipts(semDir); len(r) != 0 {
		t.Errorf("receipts after success = %+v, want none", r)
	}
	if _, ok, err := readCommitHandoff(semDir); ok || err != nil {
		t.Errorf("handoff left behind after success: ok=%v err=%v", ok, err)
	}
}

// A mismatched handoff remains unlinked and intact.
func TestCommitCapture_MismatchLeavesHandoff(t *testing.T) {
	dir, semDir, _ := setupCommitRepo(t)
	ctx := context.Background()

	// Bind the handoff to a different tree.
	if err := writeCommitHandoff(semDir, commitHandoff{
		CheckpointID: uuid.NewString(),
		CreatedAt:    1000,
		Tree:         "0000000000000000000000000000000000000000",
		Head:         "",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "f.txt")
	runGit(t, dir, "commit", "-m", "unrelated")

	res, err := NewPostCommitService().HandlePostCommit(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Linked {
		t.Error("mismatched handoff was attached to an unrelated commit")
	}
	if _, ok, err := readCommitHandoff(semDir); !ok || err != nil {
		t.Errorf("mismatched handoff was consumed or corrupted: ok=%v err=%v", ok, err)
	}
	if r, _ := listCommitReceipts(semDir); len(r) != 0 {
		t.Errorf("receipts = %+v, want none for a mismatch", r)
	}
}

// Without a handoff, post-commit is a no-op.
func TestCommitCapture_NoHandoffNoLink(t *testing.T) {
	dir, semDir, _ := setupCommitRepo(t)
	res, err := NewPostCommitService().HandlePostCommit(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Linked {
		t.Error("post-commit linked without a handoff")
	}
	if r, _ := listCommitReceipts(semDir); len(r) != 0 {
		t.Errorf("receipts = %+v, want none", r)
	}
}

// A receipt preserves the checkpoint when SQLite is unavailable.
func TestCommitCapture_ReceiptSurvivesDBUnavailable(t *testing.T) {
	dir, semDir, dbPath := setupCommitRepo(t)
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "f.txt")
	if err := NewPreCommitService().HandlePreCommit(ctx, dir); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "commit", "-m", "c1")
	sha := headSHA(t, dir)

	// Prevent the inline SQLite write.
	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dbPath, 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := NewPostCommitService().HandlePostCommit(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Linked {
		t.Error("post-commit reported linked though the db was unavailable")
	}
	receipts, _ := listCommitReceipts(semDir)
	if len(receipts) != 1 || receipts[0].CommitSHA != sha {
		t.Fatalf("receipts = %+v, want one for %s", receipts, sha)
	}

	// Restore SQLite and drain the receipt.
	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}
	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatal(err)
	}
	if err := drainCommitReceipts(ctx, dir); err != nil {
		t.Fatalf("drain after db restore: %v", err)
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	cp, err := h.Queries.GetCheckpointByID(ctx, receipts[0].CheckpointID)
	if err != nil {
		t.Fatalf("checkpoint not created by drain: %v", err)
	}
	if cp.CreatedAt != receipts[0].CreatedAt {
		t.Errorf("checkpoint created_at = %d, want preserved %d", cp.CreatedAt, receipts[0].CreatedAt)
	}
	if _, err := h.Queries.GetCommitLinkByCommitHash(ctx, sha); err != nil {
		t.Errorf("commit not linked by drain: %v", err)
	}
	if r, _ := listCommitReceipts(semDir); len(r) != 0 {
		t.Errorf("receipts after drain = %+v, want none", r)
	}
}

// Repeated drains do not duplicate checkpoints or links.
func TestDrainCommitReceipts_Idempotent(t *testing.T) {
	dir, semDir, dbPath := setupCommitRepo(t)
	ctx := context.Background()

	ckpt := uuid.NewString()
	sha := "1111111111111111111111111111111111111111"
	if _, err := promoteToReceipt(semDir, sha, commitHandoff{CheckpointID: ckpt, CreatedAt: 5000}); err != nil {
		t.Fatal(err)
	}

	if err := drainCommitReceipts(ctx, dir); err != nil {
		t.Fatalf("first drain: %v", err)
	}
	if err := drainCommitReceipts(ctx, dir); err != nil {
		t.Fatalf("second drain: %v", err)
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	var checkpoints, links int
	if err := h.DB.QueryRowContext(ctx, "select count(*) from checkpoints where checkpoint_id = ?", ckpt).Scan(&checkpoints); err != nil {
		t.Fatal(err)
	}
	if err := h.DB.QueryRowContext(ctx, "select count(*) from commit_links where commit_hash = ?", sha).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if checkpoints != 1 || links != 1 {
		t.Errorf("checkpoints=%d links=%d, want exactly 1 each", checkpoints, links)
	}
}

// Receipt timestamps determine checkpoint sequence.
func TestDrainCommitReceipts_SequencesByCommitTime(t *testing.T) {
	dir, semDir, dbPath := setupCommitRepo(t)
	ctx := context.Background()

	// The earlier receipt has the SHA that sorts last.
	early := uuid.NewString()
	late := uuid.NewString()
	if _, err := promoteToReceipt(semDir, strings.Repeat("f", 40), commitHandoff{CheckpointID: early, CreatedAt: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := promoteToReceipt(semDir, strings.Repeat("0", 40), commitHandoff{CheckpointID: late, CreatedAt: 2000}); err != nil {
		t.Fatal(err)
	}

	if err := drainCommitReceipts(ctx, dir); err != nil {
		t.Fatal(err)
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	seq := func(ckpt string) int64 {
		var s int64
		if err := h.DB.QueryRowContext(ctx, "select repository_sequence from checkpoints where checkpoint_id = ?", ckpt).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if seq(early) >= seq(late) {
		t.Errorf("earlier commit(seq %d) must precede later(seq %d)", seq(early), seq(late))
	}
}

// Amended commits remain linked to their handoffs.
func TestCommitCapture_AmendIsLinked(t *testing.T) {
	dir, _, dbPath := setupCommitRepo(t)
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "f.txt")
	runGit(t, dir, "commit", "-m", "c1")

	// Record the original HEAD before amending it.
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "f.txt")
	if err := NewPreCommitService().HandlePreCommit(ctx, dir); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "commit", "--amend", "-m", "c1 amended")
	amend := headSHA(t, dir)

	res, err := NewPostCommitService().HandlePostCommit(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Linked || res.CommitHash != amend {
		t.Fatalf("amend not captured: %+v, want linked %s", res, amend)
	}
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	if _, err := h.Queries.GetCommitLinkByCommitHash(ctx, amend); err != nil {
		t.Errorf("amend commit not linked: %v", err)
	}
}

// An unprocessed receipt makes the drain fail.
func TestDrainCommitReceipts_ReturnsErrorWhenPending(t *testing.T) {
	dir, semDir, dbPath := setupCommitRepo(t)
	ctx := context.Background()

	if _, err := promoteToReceipt(semDir, strings.Repeat("a", 40), commitHandoff{CheckpointID: uuid.NewString(), CreatedAt: 1000}); err != nil {
		t.Fatal(err)
	}
	// Prevent the drain from opening SQLite.
	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dbPath, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := drainCommitReceipts(ctx, dir); err == nil {
		t.Error("drain should return an error when receipts remain unprocessed")
	}
	if r, _ := listCommitReceipts(semDir); len(r) != 1 {
		t.Errorf("receipts = %+v, want the pending one kept", r)
	}
}

// A new commit cannot overtake an existing receipt backlog.
func TestCommitCapture_DefersWhenReceiptsPending(t *testing.T) {
	dir, semDir, dbPath := setupCommitRepo(t)
	ctx := context.Background()

	// Seed an older pending receipt.
	older := uuid.NewString()
	if _, err := promoteToReceipt(semDir, strings.Repeat("a", 40), commitHandoff{CheckpointID: older, CreatedAt: 1000}); err != nil {
		t.Fatal(err)
	}

	// Capture a newer commit with SQLite available.
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "f.txt")
	if err := NewPreCommitService().HandlePreCommit(ctx, dir); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "commit", "-m", "c")
	cSHA := headSHA(t, dir)

	res, err := NewPostCommitService().HandlePostCommit(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Linked {
		t.Error("commit linked inline while an older receipt was pending")
	}

	// Both commits must remain queued as receipts.
	receipts, _ := listCommitReceipts(semDir)
	var newer string
	for _, r := range receipts {
		if r.CommitSHA == cSHA {
			newer = r.CheckpointID
		}
	}
	if len(receipts) != 2 || newer == "" {
		t.Fatalf("receipts = %+v, want the older one plus %s", receipts, cSHA)
	}

	if err := drainCommitReceipts(ctx, dir); err != nil {
		t.Fatal(err)
	}
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	seq := func(ckpt string) int64 {
		var s int64
		if err := h.DB.QueryRowContext(ctx, "select repository_sequence from checkpoints where checkpoint_id = ?", ckpt).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if seq(older) >= seq(newer) {
		t.Errorf("older receipt(seq %d) must precede the new commit(seq %d)", seq(older), seq(newer))
	}
}

// A corrupt receipt prevents a newer commit from linking.
func TestCommitCapture_DefersBehindCorruptReceipt(t *testing.T) {
	dir, semDir, dbPath := setupCommitRepo(t)
	ctx := context.Background()

	// Seed an older receipt with malformed JSON.
	if err := os.MkdirAll(commitReceiptsDir(semDir), 0o755); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(commitReceiptsDir(semDir), strings.Repeat("a", 40)+".json")
	if err := os.WriteFile(corrupt, []byte("{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "f.txt")
	if err := NewPreCommitService().HandlePreCommit(ctx, dir); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "commit", "-m", "c")
	cSHA := headSHA(t, dir)

	res, err := NewPostCommitService().HandlePostCommit(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Linked {
		t.Error("commit linked inline while a corrupt receipt was pending")
	}

	if err := drainCommitReceipts(ctx, dir); err == nil {
		t.Error("drainCommitReceipts = nil error; want the corrupt receipt to block the drain")
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	if _, err := h.Queries.GetCommitLinkByCommitHash(ctx, cSHA); err == nil {
		t.Error("newer commit was linked despite the corrupt older receipt")
	}
}

// A failed receipt blocks newer receipts.
func TestDrainCommitReceipts_StopsAtFirstFailure(t *testing.T) {
	dir, semDir, dbPath := setupCommitRepo(t)
	ctx := context.Background()

	olderSHA := strings.Repeat("a", 40)
	newerSHA := strings.Repeat("b", 40)
	if _, err := promoteToReceipt(semDir, olderSHA, commitHandoff{CheckpointID: uuid.NewString(), CreatedAt: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := promoteToReceipt(semDir, newerSHA, commitHandoff{CheckpointID: uuid.NewString(), CreatedAt: 2000}); err != nil {
		t.Fatal(err)
	}

	orig := linkReceiptFn
	defer func() { linkReceiptFn = orig }()
	linkReceiptFn = func(ctx context.Context, h *sqlstore.Handle, repoID string, r commitReceipt) error {
		if r.CommitSHA == olderSHA {
			return fmt.Errorf("injected failure")
		}
		return orig(ctx, h, repoID, r)
	}

	if err := drainCommitReceipts(ctx, dir); err == nil {
		t.Error("drain should return an error when a receipt cannot be linked")
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	if _, err := h.Queries.GetCommitLinkByCommitHash(ctx, newerSHA); err == nil {
		t.Error("newer receipt was linked despite the older one failing")
	}
	if r, _ := listCommitReceipts(semDir); len(r) != 2 {
		t.Errorf("receipts = %+v, want both kept after the failure", r)
	}
}

// Receipt deletion ignores path components in a supplied SHA.
func TestRemoveCommitReceipt_RejectsTraversal(t *testing.T) {
	semDir := t.TempDir()
	if err := os.MkdirAll(commitReceiptsDir(semDir), 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(semDir, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeCommitReceipt(semDir, "../../sentinel"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("traversal deleted a file outside the receipts dir: %v", err)
	}
}

// Invalid receipts block processing and are reported for repair.
func TestListCommitReceipts_StrictValidation(t *testing.T) {
	sha1 := strings.Repeat("a", 40)
	sha256 := strings.Repeat("b", 64)
	validJSON := func(sha string) string {
		return fmt.Sprintf(`{"checkpoint_id":"ck-1","created_at":100,"commit_sha":%q}`, sha)
	}
	writeReceipt := func(t *testing.T, name, content string) string {
		t.Helper()
		dir := t.TempDir()
		rdir := filepath.Join(dir, ".semantica", "commit-receipts")
		if err := os.MkdirAll(rdir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rdir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(dir, ".semantica")
	}

	for _, sha := range []string{sha1, sha256} {
		semDir := writeReceipt(t, sha+".json", validJSON(sha))
		got, err := listCommitReceipts(semDir)
		if err != nil || len(got) != 1 {
			t.Fatalf("valid %d-hex receipt: got %v err %v, want 1", len(sha), got, err)
		}
	}

	bad := []struct{ name, content, why string }{
		{sha1 + ".json", "{not json", "malformed JSON"},
		{sha1 + ".json", `{"created_at":100,"commit_sha":"` + sha1 + `"}`, "missing checkpoint_id"},
		{sha1 + ".json", `{"checkpoint_id":"ck","created_at":0,"commit_sha":"` + sha1 + `"}`, "non-positive created_at"},
		{sha1 + ".json", `{"checkpoint_id":"ck","created_at":1,"commit_sha":"short"}`, "bad commit_sha"},
		{"deadbeef.json", validJSON(sha1), "filename/sha mismatch"},
	}
	for _, b := range bad {
		t.Run(b.why, func(t *testing.T) {
			semDir := writeReceipt(t, b.name, b.content)
			if got, err := listCommitReceipts(semDir); err == nil {
				t.Errorf("listCommitReceipts = %v, nil error; want error", got)
			}
			if _, err := commitReceiptsPending(semDir); err == nil {
				t.Error("commitReceiptsPending = nil error; want error")
			}
			probs := InspectCommitReceipts(semDir)
			if len(probs) != 1 || probs[0].File != b.name || probs[0].Reason == "" {
				t.Errorf("InspectCommitReceipts = %+v, want one problem for %s", probs, b.name)
			}
		})
	}
}

// Non-regular receipt entries block processing.
func TestCommitReceipts_NonRegularEntryBlocks(t *testing.T) {
	dir := t.TempDir()
	semDir := filepath.Join(dir, ".semantica")
	if err := os.MkdirAll(filepath.Join(commitReceiptsDir(semDir), strings.Repeat("a", 40)+".json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := listCommitReceipts(semDir); err == nil {
		t.Errorf("listCommitReceipts = %v, nil error; want a block for a non-regular entry", got)
	}
	if probs := InspectCommitReceipts(semDir); len(probs) != 1 || probs[0].Reason != "not a regular file" {
		t.Errorf("InspectCommitReceipts = %+v, want one 'not a regular file' problem", probs)
	}
}

// Directory read failures are reported; an absent directory is healthy.
func TestInspectCommitReceipts_DirUnreadable(t *testing.T) {
	dir := t.TempDir()
	semDir := filepath.Join(dir, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Put a regular file where the receipt directory should be.
	if err := os.WriteFile(commitReceiptsDir(semDir), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := listCommitReceipts(semDir); err == nil {
		t.Error("listCommitReceipts = nil error; want a non-directory error")
	}
	if probs := InspectCommitReceipts(semDir); len(probs) != 1 || !strings.Contains(probs[0].Reason, "unreadable") {
		t.Errorf("InspectCommitReceipts = %+v, want one directory-unreadable problem", probs)
	}
	if probs := InspectCommitReceipts(filepath.Join(t.TempDir(), ".semantica")); probs != nil {
		t.Errorf("InspectCommitReceipts(absent) = %+v, want nil", probs)
	}
}
