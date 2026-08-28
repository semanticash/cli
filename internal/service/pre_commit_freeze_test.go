package service

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/toolsnap"
	"github.com/semanticash/cli/internal/util"
)

// stageFile writes and git-adds a file so pre-commit sees a staged change.
func stageFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", rel)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add %s: %v\n%s", rel, err, out)
	}
}

// checkpointByTrigger returns the newest checkpoint with a given trigger, or nil.
func checkpointByTrigger(t *testing.T, ctx context.Context, dir, trigger string) *sqldb.Checkpoint {
	t.Helper()
	h, err := sqlstore.Open(ctx, filepath.Join(dir, ".semantica", "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	cps, err := h.Queries.ListCheckpointsByRepository(ctx, sqldb.ListCheckpointsByRepositoryParams{RepositoryID: repoRow.RepositoryID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for i := range cps {
		if cps[i].Trigger.String == trigger {
			return &cps[i]
		}
	}
	return nil
}

// listFreezeRefs returns the workspace-freeze refs published in the repo's store.
func listFreezeRefs(t *testing.T, ctx context.Context, dir string) map[string]string {
	t.Helper()
	rc, err := toolsnap.ResolveRepoContext(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := toolsnap.OpenStore(ctx, rc, filepath.Join(dir, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	refs, err := store.ListWorkspaceFreezeRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return refs
}

// validFreezePairing builds a pairing whose ref matches the observation id and
// whose tree is a full object id, so it passes validateFreezePairing.
func validFreezePairing(obsID string) freezeHandoff {
	return freezeHandoff{
		ObservationID: obsID,
		Tree:          strings.Repeat("a", 40),
		Ref:           toolsnap.WorkspaceFreezeRef(obsID),
		EventCursor:   7,
	}
}

// commitLinkCount returns the number of commit links for a checkpoint.
func commitLinkCount(t *testing.T, ctx context.Context, h *sqlstore.Handle, checkpointID string) int {
	t.Helper()
	var n int
	if err := h.DB.QueryRowContext(ctx, "select count(*) from commit_links where checkpoint_id = ?", checkpointID).Scan(&n); err != nil {
		t.Fatalf("count commit links: %v", err)
	}
	return n
}

// The paired insert creates the observation before the commit checkpoint in one
// transaction, so the observation takes the lower sequence and carries the
// pinned window; the commit checkpoint keeps its existing shape.
func TestInsertPairedCheckpoints_OrderingAndFields(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()
	enableSemantica(t, ctx, dir)

	h, err := sqlstore.Open(ctx, filepath.Join(dir, ".semantica", "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	freeze := validFreezePairing("obs-1")
	now := time.Now().UnixMilli()
	if err := insertPairedCheckpoints(ctx, h, repoRow.RepositoryID, "commit-cp", now, freeze); err != nil {
		t.Fatalf("paired insert: %v", err)
	}

	obs, err := h.Queries.GetCheckpointByID(ctx, "obs-1")
	if err != nil {
		t.Fatal(err)
	}
	commit, err := h.Queries.GetCheckpointByID(ctx, "commit-cp")
	if err != nil {
		t.Fatal(err)
	}
	if obs.RepositorySequence >= commit.RepositorySequence {
		t.Errorf("observation seq %d not below commit seq %d", obs.RepositorySequence, commit.RepositorySequence)
	}
	if obs.Trigger.String != "workspace_freeze" || obs.Kind != "auto" || obs.Status != "pending" {
		t.Errorf("observation = kind %q trigger %q status %q", obs.Kind, obs.Trigger.String, obs.Status)
	}
	if obs.ManifestHash.Valid {
		t.Errorf("observation manifest_hash = %q, want NULL", obs.ManifestHash.String)
	}
	if !obs.EventCursor.Valid || obs.EventCursor.Int64 != 7 {
		t.Errorf("observation event_cursor = %+v, want pinned 7", obs.EventCursor)
	}
	if commit.Trigger.String != "commit" || commit.Status != "pending" {
		t.Errorf("commit checkpoint = trigger %q status %q", commit.Trigger.String, commit.Status)
	}
	// Neither is commit-linked by the insert; linking is the post-commit step.
	if n := commitLinkCount(t, ctx, h, "obs-1"); n != 0 {
		t.Errorf("observation commit links = %d, want 0 (stays unlinked)", n)
	}
}

// A failure inserting the commit checkpoint rolls back the observation too:
// the pairing is all-or-nothing.
func TestInsertPairedCheckpoints_Atomic(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()
	enableSemantica(t, ctx, dir)

	h, err := sqlstore.Open(ctx, filepath.Join(dir, ".semantica", "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	// Pre-create the commit checkpoint id so the second insert violates the PK.
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: "dupe-commit", RepositoryID: repoRow.RepositoryID,
		CreatedAt: time.Now().UnixMilli(), Kind: "auto", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}

	freeze := validFreezePairing("obs-atomic")
	if err := insertPairedCheckpoints(ctx, h, repoRow.RepositoryID, "dupe-commit", time.Now().UnixMilli(), freeze); err == nil {
		t.Fatal("expected paired insert to fail on the duplicate commit checkpoint")
	}
	if _, err := h.Queries.GetCheckpointByID(ctx, "obs-atomic"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("observation survived a rolled-back pairing: err=%v", err)
	}
}

// The handoff round-trips its optional freeze pairing, and the commit checkpoint
// id stays field[0] for the commit-msg hook.
func TestCommitHandoff_FreezeRoundTrip(t *testing.T) {
	semDir := filepath.Join(t.TempDir(), ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}

	fp := validFreezePairing("obs-1")
	fp.EventCursor = 42
	withFreeze := commitHandoff{
		CheckpointID: "commit-cp", CreatedAt: 123, Tree: "staged-tree", Head: "parent-head",
		Freeze: &fp,
	}
	if err := writeCommitHandoff(semDir, withFreeze); err != nil {
		t.Fatal(err)
	}
	got, ok, err := readCommitHandoff(semDir)
	if err != nil || !ok || got.CheckpointID != "commit-cp" || got.Tree != "staged-tree" || got.Head != "parent-head" {
		t.Fatalf("base fields = %+v, err=%v", got, err)
	}
	if got.Freeze == nil || *got.Freeze != *withFreeze.Freeze {
		t.Fatalf("freeze pairing = %+v, want %+v", got.Freeze, withFreeze.Freeze)
	}
	// The commit-msg hook reads only field[0].
	raw, err := os.ReadFile(util.PreCommitCheckpointPath(semDir))
	if err != nil {
		t.Fatal(err)
	}
	first := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(raw)), "|", 2)[0])
	if first != "commit-cp" {
		t.Errorf("handoff field[0] = %q, want commit-cp", first)
	}

	// A handoff without a freeze pairing reads back with no observation.
	if err := writeCommitHandoff(semDir, commitHandoff{CheckpointID: "c2", CreatedAt: 1, Tree: "t", Head: "h"}); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := readCommitHandoff(semDir); err != nil || !ok || got.Freeze != nil {
		t.Fatalf("legacy handoff produced freeze = %+v, err=%v", got.Freeze, err)
	}
}

// When the pre-commit paired insert never ran (crash/failure), the freeze
// pairing survives promotion into the receipt and the worker drain reconstructs
// both checkpoints, observation first, linking only the commit checkpoint.
func TestCommitReceipt_FreezeReconstructsPairOnDrain(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()
	enableSemantica(t, ctx, dir)
	semDir := filepath.Join(dir, ".semantica")

	freeze := validFreezePairing("obs-recover")
	sha := strings.Repeat("a", 40)
	handoff := commitHandoff{
		CheckpointID: "commit-recover", CreatedAt: time.Now().UnixMilli(),
		Tree: "stagedtree", Head: "", Freeze: &freeze,
	}
	// Promote the freeze-carrying receipt (no checkpoints exist yet).
	if _, err := promoteToReceipt(semDir, sha, handoff); err != nil {
		t.Fatal(err)
	}
	if err := drainCommitReceipts(ctx, dir); err != nil {
		t.Fatalf("drain: %v", err)
	}

	obs := checkpointByTrigger(t, ctx, dir, "workspace_freeze")
	commit := checkpointByTrigger(t, ctx, dir, "commit")
	if obs == nil || commit == nil {
		t.Fatalf("reconstructed pair missing: obs=%v commit=%v", obs, commit)
	}
	if obs.CheckpointID != "obs-recover" || commit.CheckpointID != "commit-recover" {
		t.Fatalf("ids: obs=%s commit=%s", obs.CheckpointID, commit.CheckpointID)
	}
	if obs.RepositorySequence >= commit.RepositorySequence {
		t.Errorf("observation seq %d not below commit seq %d", obs.RepositorySequence, commit.RepositorySequence)
	}

	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	link, err := h.Queries.GetCommitLinkByCommitHash(ctx, sha)
	if err != nil {
		t.Fatalf("commit link missing: %v", err)
	}
	if link.CheckpointID != "commit-recover" {
		t.Errorf("commit link points to %s, want commit-recover", link.CheckpointID)
	}
	if n := commitLinkCount(t, ctx, h, "obs-recover"); n != 0 {
		t.Errorf("observation has %d commit links, want 0", n)
	}
	if r, _ := listCommitReceipts(semDir); len(r) != 0 {
		t.Errorf("receipt not drained: %+v", r)
	}
}

// When the commit checkpoint exists but its paired observation is missing, a
// freeze-bearing receipt is not drained: the disagreement blocks and is left
// for repair.
func TestLinkReceipt_MismatchedPairingBlocksDrain(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()
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
	// Commit checkpoint exists, but its paired observation never landed.
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: "commit-mismatch", RepositoryID: repoRow.RepositoryID,
		CreatedAt: time.Now().UnixMilli(), Kind: "auto",
		Trigger: sql.NullString{String: "commit", Valid: true}, Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	_ = sqlstore.Close(h)

	freeze := validFreezePairing("obs-ghost")
	sha := strings.Repeat("b", 40)
	if _, err := promoteToReceipt(semDir, sha, commitHandoff{
		CheckpointID: "commit-mismatch", CreatedAt: time.Now().UnixMilli(),
		Tree: "t", Head: "", Freeze: &freeze,
	}); err != nil {
		t.Fatal(err)
	}

	if err := drainCommitReceipts(ctx, dir); err == nil {
		t.Fatal("expected drain to fail on the pairing disagreement")
	}
	if r, _ := listCommitReceipts(semDir); len(r) != 1 {
		t.Errorf("receipt drained despite disagreement: %+v", r)
	}
}

// A handoff with the wrong field count or an invalid field is a corruption:
// readCommitHandoff reports an error and leaves the artifact intact.
func TestReadCommitHandoff_MalformedIsCorruption(t *testing.T) {
	semDir := filepath.Join(t.TempDir(), ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := util.PreCommitCheckpointPath(semDir)
	corrupt := []string{
		"",                               // empty file
		"   ",                            // whitespace only
		"a|1|tree",                       // 3 fields
		"a|1|tree|head|x",                // 5 fields
		"a|1|tree|head|o|t|r",            // 7 fields
		"|1|tree|head",                   // empty checkpoint id
		"a|0|tree|head",                  // non-positive created_at
		"a|1||head",                      // empty staged tree
		"a|1|tree|head|obs|nothex|ref|3", // invalid freeze pairing
	}
	for _, c := range corrupt {
		if err := os.WriteFile(path, []byte(c+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := readCommitHandoff(semDir); ok || err == nil {
			t.Errorf("case %q: ok=%v err=%v, want corruption error", c, ok, err)
		}
		if _, serr := os.Stat(path); serr != nil {
			t.Errorf("case %q: corrupt handoff was removed", c)
		}
	}

	// A present-but-unreadable handoff is a corruption, not absence. Root
	// ignores permissions, so it cannot exercise this case.
	if os.Geteuid() > 0 {
		if err := os.WriteFile(path, []byte("a|1|tree|head\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
		if _, ok, err := readCommitHandoff(semDir); ok || err == nil {
			t.Errorf("unreadable handoff: ok=%v err=%v, want corruption error", ok, err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Valid four-field and eight-field handoffs parse.
	if err := writeCommitHandoff(semDir, commitHandoff{CheckpointID: "a", CreatedAt: 1, Tree: "t", Head: "h"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := readCommitHandoff(semDir); !ok || err != nil {
		t.Errorf("valid 4-field: ok=%v err=%v", ok, err)
	}
	fp := validFreezePairing("obs-x")
	if err := writeCommitHandoff(semDir, commitHandoff{CheckpointID: "a", CreatedAt: 1, Tree: "t", Head: "h", Freeze: &fp}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := readCommitHandoff(semDir); !ok || err != nil {
		t.Errorf("valid 8-field: ok=%v err=%v", ok, err)
	}
}

// With the gate off (default), pre-commit behaves exactly as before: one commit
// checkpoint, no observation, no freeze ref.
func TestHandlePreCommit_GateOff_CommitCheckpointOnly(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()
	enableSemantica(t, ctx, dir)
	stageFile(t, dir, "a.txt", "hello\n")

	if err := NewPreCommitService().HandlePreCommit(ctx, dir); err != nil {
		t.Fatalf("pre-commit: %v", err)
	}
	if commit := checkpointByTrigger(t, ctx, dir, "commit"); commit == nil {
		t.Fatal("commit checkpoint missing")
	}
	if obs := checkpointByTrigger(t, ctx, dir, "workspace_freeze"); obs != nil {
		t.Fatal("observation created while gate is off")
	}
	if refs := listFreezeRefs(t, ctx, dir); len(refs) != 0 {
		t.Fatalf("freeze refs = %d, want 0", len(refs))
	}
}

// With the gate on, pre-commit pairs an unlinked observation (lower sequence,
// pinned cursor) with the commit checkpoint, publishes a freeze ref, and the
// handoff carries the pairing.
func TestHandlePreCommit_GateOn_PairsObservation(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()
	enableSemantica(t, ctx, dir)
	t.Setenv("SEMANTICA_WORKSPACE_FREEZE", "1")
	stageFile(t, dir, "a.txt", "hello\n")

	if err := NewPreCommitService().HandlePreCommit(ctx, dir); err != nil {
		t.Fatalf("pre-commit: %v", err)
	}
	obs := checkpointByTrigger(t, ctx, dir, "workspace_freeze")
	commit := checkpointByTrigger(t, ctx, dir, "commit")
	if obs == nil || commit == nil {
		t.Fatalf("paired checkpoints missing: obs=%v commit=%v", obs, commit)
	}
	if obs.RepositorySequence >= commit.RepositorySequence {
		t.Errorf("observation seq %d not below commit seq %d", obs.RepositorySequence, commit.RepositorySequence)
	}
	if !obs.EventCursor.Valid {
		t.Errorf("observation event_cursor not pinned")
	}
	if refs := listFreezeRefs(t, ctx, dir); len(refs) != 1 {
		t.Fatalf("freeze refs = %d, want 1", len(refs))
	}
	h, ok, err := readCommitHandoff(filepath.Join(dir, ".semantica"))
	if err != nil || !ok || h.Freeze == nil || h.Freeze.ObservationID != obs.CheckpointID {
		t.Fatalf("handoff freeze pairing = %+v, err=%v, want observation %s", h.Freeze, err, obs.CheckpointID)
	}
}

// A failing/timed-out freeze falls back to a commit-only checkpoint; the commit
// proceeds and no observation or freeze ref is left behind.
func TestHandlePreCommit_GateOn_FreezeFailureFallsBack(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()
	enableSemantica(t, ctx, dir)
	t.Setenv("SEMANTICA_WORKSPACE_FREEZE", "1")

	orig := workspaceFreezeDeadline
	t.Cleanup(func() { workspaceFreezeDeadline = orig })
	workspaceFreezeDeadline = time.Nanosecond // expire the freeze before it captures

	stageFile(t, dir, "a.txt", "hello\n")
	if err := NewPreCommitService().HandlePreCommit(ctx, dir); err != nil {
		t.Fatalf("pre-commit: %v", err)
	}
	if obs := checkpointByTrigger(t, ctx, dir, "workspace_freeze"); obs != nil {
		t.Fatal("observation created despite freeze failure")
	}
	if commit := checkpointByTrigger(t, ctx, dir, "commit"); commit == nil {
		t.Fatal("commit checkpoint missing; commit path must proceed")
	}
	if refs := listFreezeRefs(t, ctx, dir); len(refs) != 0 {
		t.Fatalf("freeze refs = %d, want 0 after failed freeze", len(refs))
	}
	if h, ok, err := readCommitHandoff(filepath.Join(dir, ".semantica")); err != nil || !ok || h.Freeze != nil {
		t.Fatalf("handoff carried freeze after failure: %+v, err=%v", h.Freeze, err)
	}
}
