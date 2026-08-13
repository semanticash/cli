package toolsnap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/platform"
)

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func key(tool string) ToolKey {
	return ToolKey{
		RepositoryID: "repo-1", Provider: "claude_code",
		SessionID: "sess-1", TurnID: "turn-1", ToolUseID: tool,
	}
}

func entry(tool string, startedAt int64) PendingToolSnapshot {
	return PendingToolSnapshot{
		Key: key(tool), ToolName: "Bash",
		SnapshotRef: "refs/semantica/tool-windows/x", TreeHash: "t", HeadHash: "h",
		ObjectFormat: "sha1", StartedAt: startedAt,
	}
}

// noFinalize asserts finalize is never invoked.
func noFinalize(t *testing.T) func([]PendingToolSnapshot, *GroupFinal, bool, func() error) (FinalizeResult, error) {
	return func(m []PendingToolSnapshot, _ *GroupFinal, _ bool, _ func() error) (FinalizeResult, error) {
		t.Fatalf("finalize invoked unexpectedly with %d members", len(m))
		return FinalizeResult{}, nil
	}
}

// doneFinalize records members and reports durable completion.
func doneFinalize(members *[]PendingToolSnapshot) func([]PendingToolSnapshot, *GroupFinal, bool, func() error) (FinalizeResult, error) {
	return func(m []PendingToolSnapshot, _ *GroupFinal, _ bool, _ func() error) (FinalizeResult, error) {
		*members = m
		return FinalizeResult{Done: true}, nil
	}
}

func TestSingleWindowLifecycle(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()

	gid, err := r.Begin(ctx, entry("tu1", 100))
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	var got []PendingToolSnapshot
	closed, err := r.Complete(ctx, key("tu1"), CompletionInfo{EventID: "e", At: 200}, nil, doneFinalize(&got))
	if err != nil || !closed {
		t.Fatalf("complete: closed=%v err=%v", closed, err)
	}
	if len(got) != 1 || got[0].GroupID != gid || got[0].CompletedAt != 200 {
		t.Fatalf("members = %+v", got)
	}
	stale, err := r.Stale(ctx, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Errorf("windows remain after closure: %+v", stale)
	}
}

func TestTransitiveOverlapFormsOneGroup(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()

	// A starts; B overlaps A; A completes; C overlaps B only. All
	// three chain into one group even though A and C never overlapped.
	gidA, err := r.Begin(ctx, entry("tuA", 100))
	if err != nil {
		t.Fatal(err)
	}
	gidB, err := r.Begin(ctx, entry("tuB", 110))
	if err != nil {
		t.Fatal(err)
	}
	if gidB != gidA {
		t.Fatalf("B group %s, want union with A %s", gidB, gidA)
	}
	closed, err := r.Complete(ctx, key("tuA"), CompletionInfo{EventID: "e", At: 120}, nil, noFinalize(t))
	if err != nil || closed {
		t.Fatalf("A completion: closed=%v err=%v", closed, err)
	}
	gidC, err := r.Begin(ctx, entry("tuC", 130))
	if err != nil {
		t.Fatal(err)
	}
	if gidC != gidA {
		t.Fatalf("C group %s, want chained union %s", gidC, gidA)
	}
	if closed, err = r.Complete(ctx, key("tuB"), CompletionInfo{EventID: "e", At: 140}, nil, noFinalize(t)); err != nil || closed {
		t.Fatalf("B completion: closed=%v err=%v", closed, err)
	}
	var members []PendingToolSnapshot
	closed, err = r.Complete(ctx, key("tuC"), CompletionInfo{EventID: "e", At: 150}, nil, doneFinalize(&members))
	if err != nil || !closed {
		t.Fatalf("final closure: closed=%v err=%v", closed, err)
	}
	if len(members) != 3 {
		t.Fatalf("members = %d", len(members))
	}
	for i, want := range []string{"tuA", "tuB", "tuC"} {
		if members[i].Key.ToolUseID != want {
			t.Errorf("member %d = %s, want %s", i, members[i].Key.ToolUseID, want)
		}
	}
}

// Finalization holds the registry lock against new windows.
func TestFinalizeHoldsLockAgainstBegin(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	if _, err := r.Begin(ctx, entry("tu1", 100)); err != nil {
		t.Fatal(err)
	}

	inFinalize := make(chan struct{})
	releaseFinalize := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := r.Complete(ctx, key("tu1"), CompletionInfo{EventID: "e", At: 200}, nil, func([]PendingToolSnapshot, *GroupFinal, bool, func() error) (FinalizeResult, error) {
			close(inFinalize)
			<-releaseFinalize
			return FinalizeResult{Done: true}, nil
		})
		done <- err
	}()

	<-inFinalize
	// A registration cannot enter during finalization.
	beginCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := r.Begin(beginCtx, entry("tu2", 300))
	var pe *PartialError
	if !errors.As(err, &pe) || pe.Reason != ReasonLockTimeout {
		t.Fatalf("begin during finalize: err = %v, want %s", err, ReasonLockTimeout)
	}
	close(releaseFinalize)
	if err := <-done; err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := r.Begin(ctx, entry("tu2", 300)); err != nil {
		t.Fatalf("begin after closure: %v", err)
	}
}

// A retry uses the persisted closing identity, not the current workspace.
func TestFailedFinalizationRetriesFromPersistedIdentity(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	if _, err := r.Begin(ctx, entry("tu1", 100)); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("evidence persistence failed")
	closed, err := r.Complete(ctx, key("tu1"), CompletionInfo{EventID: "e", At: 200}, nil, func(_ []PendingToolSnapshot, prior *GroupFinal, _ bool, _ func() error) (FinalizeResult, error) {
		if prior != nil {
			t.Fatalf("first attempt received prior identity %+v", prior)
		}
		// Capture succeeded; persistence failed.
		return FinalizeResult{Final: GroupFinal{PostTreeHash: "tree-at-close", CapturedAt: 200}}, boom
	})
	if !errors.Is(err, boom) || closed {
		t.Fatalf("first attempt: closed=%v err=%v", closed, err)
	}

	// Register later activity before retrying the closed group.
	if _, err := r.Begin(ctx, entry("tu-later", 300)); err != nil {
		t.Fatal(err)
	}

	var members []PendingToolSnapshot
	closed, err = r.Complete(ctx, key("tu1"), CompletionInfo{EventID: "e", At: 999}, nil, func(m []PendingToolSnapshot, prior *GroupFinal, _ bool, _ func() error) (FinalizeResult, error) {
		if prior == nil || prior.PostTreeHash != "tree-at-close" {
			t.Fatalf("retry did not receive the persisted identity: %+v", prior)
		}
		members = m
		return FinalizeResult{Done: true}, nil
	})
	if err != nil || !closed {
		t.Fatalf("retry: closed=%v err=%v", closed, err)
	}
	// The original completion time is preserved; the retry timestamp
	// is not applied.
	if len(members) != 1 || members[0].CompletedAt != 200 {
		t.Fatalf("members = %+v", members)
	}

	stale, err := r.Stale(ctx, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].Key.ToolUseID != "tu-later" {
		t.Fatalf("windows after retry = %+v", stale)
	}
}

// A capture-unavailable finalization persists the partial reason so
// the retry converts to deterministic partial evidence.
func TestCaptureUnavailableFinalizationBecomesPartial(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	if _, err := r.Begin(ctx, entry("tu1", 100)); err != nil {
		t.Fatal(err)
	}
	closed, err := r.Complete(ctx, key("tu1"), CompletionInfo{EventID: "e", At: 200}, nil, func(_ []PendingToolSnapshot, _ *GroupFinal, _ bool, _ func() error) (FinalizeResult, error) {
		return FinalizeResult{Final: GroupFinal{PartialReason: ReasonLockTimeout}}, errors.New("post capture impossible")
	})
	if err == nil || closed {
		t.Fatalf("first attempt: closed=%v err=%v", closed, err)
	}
	closed, err = r.Complete(ctx, key("tu1"), CompletionInfo{EventID: "e", At: 999}, nil, func(_ []PendingToolSnapshot, prior *GroupFinal, _ bool, _ func() error) (FinalizeResult, error) {
		if prior == nil || prior.PartialReason != ReasonLockTimeout {
			t.Fatalf("retry missing partial reason: %+v", prior)
		}
		return FinalizeResult{Done: true}, nil
	})
	if err != nil || !closed {
		t.Fatalf("partial retry: closed=%v err=%v", closed, err)
	}
}

// Removing a group also removes its pending final identity.
func TestRemoveGroupClearsFinalIdentity(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	gid, err := r.Begin(ctx, entry("tu1", 100))
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("persist failed")
	if _, err := r.Complete(ctx, key("tu1"), CompletionInfo{EventID: "e", At: 200}, nil, func(_ []PendingToolSnapshot, _ *GroupFinal, _ bool, _ func() error) (FinalizeResult, error) {
		return FinalizeResult{Final: GroupFinal{PostTreeHash: "stale-tree", CapturedAt: 200}}, boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if err := r.RemoveGroup(ctx, gid); err != nil {
		t.Fatal(err)
	}

	gid2, err := r.Begin(ctx, entry("tu1", 300))
	if err != nil {
		t.Fatal(err)
	}
	if gid2 != gid {
		t.Fatalf("replayed group id %s, want deterministic %s", gid2, gid)
	}
	if _, err := r.Complete(ctx, key("tu1"), CompletionInfo{EventID: "e", At: 400}, nil, func(_ []PendingToolSnapshot, prior *GroupFinal, _ bool, _ func() error) (FinalizeResult, error) {
		if prior != nil {
			t.Fatalf("replayed group received stale final identity: %+v", prior)
		}
		return FinalizeResult{Done: true}, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// The persisted final identity is monotonic: a retry may enrich it
// with a delta hash but never replace the captured tree.
func TestFinalIdentityIsMonotonic(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	if _, err := r.Begin(ctx, entry("tu1", 100)); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("persist failed")
	if _, err := r.Complete(ctx, key("tu1"), CompletionInfo{EventID: "e", At: 200}, nil, func(_ []PendingToolSnapshot, _ *GroupFinal, _ bool, _ func() error) (FinalizeResult, error) {
		return FinalizeResult{Final: GroupFinal{PostTreeHash: "tree-1", CapturedAt: 200}}, boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	// A conflicting tree must be rejected and the original preserved.
	if _, err := r.Complete(ctx, key("tu1"), CompletionInfo{EventID: "e", At: 300}, nil, func(_ []PendingToolSnapshot, _ *GroupFinal, _ bool, _ func() error) (FinalizeResult, error) {
		return FinalizeResult{Final: GroupFinal{PostTreeHash: "tree-2", CapturedAt: 300}}, boom
	}); err == nil {
		t.Fatal("conflicting final tree accepted")
	}
	// Enrichment with a delta hash for the same tree is permitted.
	if _, err := r.Complete(ctx, key("tu1"), CompletionInfo{EventID: "e", At: 400}, nil, func(_ []PendingToolSnapshot, prior *GroupFinal, _ bool, _ func() error) (FinalizeResult, error) {
		if prior == nil || prior.PostTreeHash != "tree-1" {
			t.Fatalf("prior = %+v, want preserved tree-1", prior)
		}
		return FinalizeResult{Final: GroupFinal{PostTreeHash: "tree-1", DeltaHash: "delta-1", CapturedAt: 200}}, boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if _, err := r.Complete(ctx, key("tu1"), CompletionInfo{EventID: "e", At: 500}, nil, func(_ []PendingToolSnapshot, prior *GroupFinal, _ bool, _ func() error) (FinalizeResult, error) {
		if prior == nil || prior.PostTreeHash != "tree-1" || prior.DeltaHash != "delta-1" {
			t.Fatalf("prior = %+v, want enriched identity", prior)
		}
		return FinalizeResult{Done: true}, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// The strict-key validator applies at every boundary.
func TestStrictKeyValidatedEverywhere(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()

	partial := key("tu1")
	partial.TurnID = ""
	nulled := key("tu\x002")

	for name, k := range map[string]ToolKey{"incomplete": partial, "nul": nulled} {
		e := PendingToolSnapshot{Key: k, ToolName: "Bash", StartedAt: 100}
		if _, err := r.Begin(ctx, e); err == nil {
			t.Errorf("%s key accepted by Begin", name)
		}
		if err := r.WriteTombstone(k, 100); err == nil {
			t.Errorf("%s key accepted by WriteTombstone", name)
		}
		if _, err := r.HasTombstone(k); err == nil {
			t.Errorf("%s key accepted by HasTombstone", name)
		}
		if err := r.RemoveTombstone(k); err == nil {
			t.Errorf("%s key accepted by RemoveTombstone", name)
		}
	}
	if err := r.WriteTombstone(key("tu1"), 0); err == nil {
		t.Error("tombstone with zero timestamp accepted")
	}
}

// Delta enrichment must restate the captured tree.
func TestDeltaEnrichmentRequiresTreeRestatement(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	if _, err := r.Begin(ctx, entry("tu1", 100)); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("persist failed")
	if _, err := r.Complete(ctx, key("tu1"), CompletionInfo{EventID: "e", At: 200}, nil, func(_ []PendingToolSnapshot, _ *GroupFinal, _ bool, _ func() error) (FinalizeResult, error) {
		return FinalizeResult{Final: GroupFinal{PostTreeHash: "tree-1", CapturedAt: 200}}, boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	// A delta hash without the tree cannot be proven to describe it.
	if _, err := r.Complete(ctx, key("tu1"), CompletionInfo{EventID: "e", At: 300}, nil, func(_ []PendingToolSnapshot, _ *GroupFinal, _ bool, _ func() error) (FinalizeResult, error) {
		return FinalizeResult{Final: GroupFinal{DeltaHash: "delta-1"}}, boom
	}); err == nil || !strings.Contains(err.Error(), "restate") {
		t.Fatalf("delta without tree restatement accepted: %v", err)
	}
	// The identity is preserved.
	if _, err := r.Complete(ctx, key("tu1"), CompletionInfo{EventID: "e", At: 400}, nil, func(_ []PendingToolSnapshot, prior *GroupFinal, _ bool, _ func() error) (FinalizeResult, error) {
		if prior == nil || prior.PostTreeHash != "tree-1" || prior.DeltaHash != "" {
			t.Fatalf("prior = %+v, want unenriched tree-1", prior)
		}
		return FinalizeResult{Done: true}, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Invalid final states fail closed on load.
func TestCorruptFinalsFailClosed(t *testing.T) {
	completeWindow := `{"key":{"repository_id":"repo-1","provider":"p","session_id":"s","turn_id":"t","tool_use_id":"a"},"group_id":"g-1","status":"complete","started_at":1,"completed_at":2}`
	activeWindow := `{"key":{"repository_id":"repo-1","provider":"p","session_id":"s","turn_id":"t","tool_use_id":"a"},"group_id":"g-1","status":"active","started_at":1}`
	cases := map[string]string{
		"orphan group": `{"windows":[],"finals":{"g-ghost":{"post_tree_hash":"x"}}}`,
		"active group": `{"windows":[` + activeWindow + `],"finals":{"g-1":{"post_tree_hash":"x"}}}`,
		"both captured and partial": `{"windows":[` + completeWindow + `],` +
			`"finals":{"g-1":{"post_tree_hash":"x","partial_reason":"timeout"}}}`,
		"neither captured nor partial": `{"windows":[` + completeWindow + `],"finals":{"g-1":{"captured_at":5}}}`,
		"delta without tree": `{"windows":[` + completeWindow + `],` +
			`"finals":{"g-1":{"partial_reason":"timeout","delta_hash":"d"}}}`,
	}
	for name, state := range cases {
		t.Run(name, func(t *testing.T) {
			r := testRegistry(t)
			if err := os.WriteFile(r.statePath(), []byte(state), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Stale(context.Background(), 10); !errors.Is(err, ErrRegistryCorrupt) {
				t.Fatalf("err = %v, want ErrRegistryCorrupt", err)
			}
		})
	}
}

// Invalid callback state must not be published.
func TestInvalidPostCallbackStateNotPublished(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	if _, err := r.Begin(ctx, entry("tu1", 100)); err != nil {
		t.Fatal(err)
	}
	_, err := r.withLock(ctx, func(state *registryState) (bool, error) {
		state.Windows[0].Status = "bizarre"
		return true, nil
	})
	if !errors.Is(err, ErrRegistryCorrupt) {
		t.Fatalf("err = %v, want ErrRegistryCorrupt", err)
	}
	stale, err := r.Stale(ctx, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].Status != "active" {
		t.Fatalf("state after refused publication = %+v", stale)
	}
}

// CaptureAndBegin captures once and groups overlapping windows.
func TestCaptureAndBeginLifecycle(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	writeFile(t, root, "a.txt", "pre-hook content\n")
	k := key("tu-cab")
	w, err := reg.CaptureAndBegin(ctx, s, k, "Bash", 100)
	if err != nil {
		t.Fatalf("capture and begin: %v", err)
	}
	if w.Status != "active" || w.TreeHash == "" || w.SnapshotRef == "" || w.GroupID == "" {
		t.Fatalf("window = %+v", w)
	}
	refs, err := s.ListRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if refs[w.SnapshotRef] != w.TreeHash {
		t.Fatalf("refs = %v, want %s -> %s", refs, w.SnapshotRef, w.TreeHash)
	}
	content := run(t, root, "git", "--git-dir", s.Dir, "cat-file", "blob", w.TreeHash+":a.txt")
	if content != "pre-hook content\n" {
		t.Fatalf("captured tree = %q", content)
	}

	// Duplicate delivery: same registration, no new capture or ref.
	writeFile(t, root, "a.txt", "changed after first capture\n")
	dup, err := reg.CaptureAndBegin(ctx, s, k, "Bash", 105)
	if err != nil {
		t.Fatalf("duplicate: %v", err)
	}
	if dup != w {
		t.Fatalf("duplicate returned %+v, want original %+v", dup, w)
	}
	refsAfter, err := s.ListRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(refsAfter) != len(refs) {
		t.Fatalf("duplicate created refs: %v", refsAfter)
	}

	// An overlapping window joins the same group.
	w2, err := reg.CaptureAndBegin(ctx, s, key("tu-cab2"), "Bash", 110)
	if err != nil {
		t.Fatal(err)
	}
	if w2.GroupID != w.GroupID {
		t.Fatalf("overlap group %s, want %s", w2.GroupID, w.GroupID)
	}
}

// Mismatched registry and store directories fail before capture.
func TestCaptureAndBeginRejectsMismatchedStore(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	otherReg, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = otherReg.CaptureAndBegin(context.Background(), s, key("tu-mismatch"), "Bash", 100)
	if err == nil || !strings.Contains(err.Error(), "different directories") {
		t.Fatalf("mismatched pair accepted: %v", err)
	}
}

// Lock contention stops capture before a ref is created.
func TestCaptureAndBeginLockTimeoutLeavesNoState(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(reg.lockPath(), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	if err := platform.LockFile(holder); err != nil {
		t.Fatal(err)
	}

	writeFile(t, root, "a.txt", "never captured\n")
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err = reg.CaptureAndBegin(ctx, s, key("tu-lock"), "Bash", 100)
	var pe *PartialError
	if !errors.As(err, &pe) || pe.Reason != ReasonLockTimeout {
		t.Fatalf("err = %v, want %s", err, ReasonLockTimeout)
	}
	if err := platform.UnlockFile(holder); err != nil {
		t.Fatal(err)
	}
	refs, err := s.ListRefs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("refs created without registration: %v", refs)
	}
}

func TestCompleteWithoutBeginIsTyped(t *testing.T) {
	r := testRegistry(t)
	_, err := r.Complete(context.Background(), key("ghost"), CompletionInfo{EventID: "e", At: 10}, nil, noFinalize(t))
	if !errors.Is(err, ErrNoPendingSnapshot) {
		t.Fatalf("err = %v, want ErrNoPendingSnapshot", err)
	}
}

// Duplicate registration is an idempotent no-op for active and for
// completed-but-retained members alike.
func TestDuplicateBeginIsIdempotent(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	gid, err := r.Begin(ctx, entry("tu1", 100))
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.Begin(ctx, entry("tu1", 105))
	if err != nil || again != gid {
		t.Fatalf("active duplicate: gid=%s err=%v, want %s", again, err, gid)
	}

	// Overlap keeps the group open, then complete tu1: it stays
	// retained while tu2 is active.
	if _, err := r.Begin(ctx, entry("tu2", 110)); err != nil {
		t.Fatal(err)
	}
	if closed, err := r.Complete(ctx, key("tu1"), CompletionInfo{EventID: "e", At: 120}, nil, noFinalize(t)); err != nil || closed {
		t.Fatalf("completion: closed=%v err=%v", closed, err)
	}
	again, err = r.Begin(ctx, entry("tu1", 130))
	if err != nil || again != gid {
		t.Fatalf("retained duplicate: gid=%s err=%v, want no-op %s", again, err, gid)
	}
	// The no-op must not have added a second member.
	stale, err := r.Stale(ctx, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 2 {
		t.Fatalf("windows = %+v, want exactly tu1 and tu2", stale)
	}
}

func TestCorruptStateFailsClosedForEveryOperation(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	// Two active groups violate the invariant; write the state
	// directly, as corruption would.
	state := `{"windows":[` +
		`{"key":{"repository_id":"repo-1","provider":"p","session_id":"s","turn_id":"t","tool_use_id":"a"},"group_id":"g-1","status":"active","started_at":1},` +
		`{"key":{"repository_id":"repo-1","provider":"p","session_id":"s","turn_id":"t","tool_use_id":"b"},"group_id":"g-2","status":"active","started_at":2}]}`
	if err := os.WriteFile(r.statePath(), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Begin(ctx, entry("tu-new", 100)); !errors.Is(err, ErrRegistryCorrupt) {
		t.Errorf("Begin err = %v, want ErrRegistryCorrupt", err)
	}
	corruptKey := ToolKey{RepositoryID: "repo-1", Provider: "p", SessionID: "s", TurnID: "t", ToolUseID: "a"}
	if _, err := r.Complete(ctx, corruptKey, CompletionInfo{EventID: "e", At: 10}, nil, noFinalize(t)); !errors.Is(err, ErrRegistryCorrupt) {
		t.Errorf("Complete err = %v, want ErrRegistryCorrupt", err)
	}
	if _, err := r.Stale(ctx, 10); !errors.Is(err, ErrRegistryCorrupt) {
		t.Errorf("Stale err = %v, want ErrRegistryCorrupt", err)
	}
	if err := r.RemoveGroup(ctx, "g-1"); !errors.Is(err, ErrRegistryCorrupt) {
		t.Errorf("RemoveGroup err = %v, want ErrRegistryCorrupt", err)
	}
}

func TestLockContentionHonorsDeadline(t *testing.T) {
	r := testRegistry(t)

	holder, err := os.OpenFile(r.lockPath(), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	if err := platform.LockFile(holder); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err = r.Begin(ctx, entry("tu1", 100))
	var pe *PartialError
	if !errors.As(err, &pe) || pe.Reason != ReasonLockTimeout {
		t.Fatalf("err = %v, want PartialError %s", err, ReasonLockTimeout)
	}

	if err := platform.UnlockFile(holder); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Begin(context.Background(), entry("tu1", 100)); err != nil {
		t.Fatalf("begin after release: %v", err)
	}
}

// An expired context must not mutate an uncontended registry.
func TestExpiredContextNeverMutates(t *testing.T) {
	r := testRegistry(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.Begin(ctx, entry("tu1", 100))
	var pe *PartialError
	if !errors.As(err, &pe) || pe.Reason != ReasonLockTimeout {
		t.Fatalf("err = %v, want %s", err, ReasonLockTimeout)
	}
	stale, err := r.Stale(context.Background(), 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("expired context registered a window: %+v", stale)
	}
}

// Repeated mutations exercise state replacement on every platform.
func TestRepeatedMutationsReplaceState(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := r.Begin(ctx, entry(fmt.Sprintf("tu-%d", i), int64(100+i))); err != nil {
			t.Fatalf("begin %d: %v", i, err)
		}
	}
	stale, err := r.Stale(ctx, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 5 {
		t.Fatalf("windows = %d, want 5", len(stale))
	}
}

func TestConcurrentRegistrationsSerialize(t *testing.T) {
	r := testRegistry(t)
	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, errs[i] = r.Begin(ctx, entry(fmt.Sprintf("tu-%02d", i), int64(100+i)))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("begin %d: %v", i, err)
		}
	}

	ctx := context.Background()
	var members []PendingToolSnapshot
	closedCount := 0
	for i := 0; i < n; i++ {
		closed, err := r.Complete(ctx, key(fmt.Sprintf("tu-%02d", i)), CompletionInfo{EventID: "e", At: int64(200 + i)}, nil, doneFinalize(&members))
		if err != nil {
			t.Fatalf("complete %d: %v", i, err)
		}
		if closed {
			closedCount++
		}
	}
	if closedCount != 1 || len(members) != n {
		t.Fatalf("closures = %d, members = %d; want exactly one closure with all %d", closedCount, len(members), n)
	}
}

// Capture sequence orders members with equal timestamps.
func TestMemberOrderIsTotal(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()

	k1 := ToolKey{RepositoryID: "repo-1", Provider: "claude_code", SessionID: "sess-2", TurnID: "t", ToolUseID: "toolu_same"}
	k2 := ToolKey{RepositoryID: "repo-1", Provider: "claude_code", SessionID: "sess-1", TurnID: "t", ToolUseID: "toolu_same"}
	e1 := PendingToolSnapshot{Key: k1, ToolName: "Bash", StartedAt: 100}
	e2 := PendingToolSnapshot{Key: k2, ToolName: "Bash", StartedAt: 100}
	if _, err := r.Begin(ctx, e1); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Begin(ctx, e2); err != nil {
		t.Fatal(err)
	}
	if closed, err := r.Complete(ctx, k1, CompletionInfo{EventID: "e", At: 200}, nil, noFinalize(t)); err != nil || closed {
		t.Fatalf("k1 completion: closed=%v err=%v", closed, err)
	}
	var members []PendingToolSnapshot
	if _, err := r.Complete(ctx, k2, CompletionInfo{EventID: "e", At: 210}, nil, doneFinalize(&members)); err != nil {
		t.Fatal(err)
	}
	// sess-2 registered first despite sorting after sess-1.
	if len(members) != 2 || members[0].Key.SessionID != "sess-2" || members[1].Key.SessionID != "sess-1" {
		t.Fatalf("members = %+v", members)
	}
	if members[0].Seq >= members[1].Seq {
		t.Fatalf("capture order not reflected: seqs %d, %d", members[0].Seq, members[1].Seq)
	}
}

func TestTombstoneLifecycle(t *testing.T) {
	r := testRegistry(t)
	k := key("tu-tomb")

	if has, err := r.HasTombstone(k); err != nil || has {
		t.Fatalf("before write: has=%v err=%v", has, err)
	}
	if err := r.WriteTombstone(k, 100); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteTombstone(k, 105); err != nil {
		t.Fatal(err)
	}
	if has, err := r.HasTombstone(k); err != nil || !has {
		t.Fatalf("after write: has=%v err=%v", has, err)
	}
	tombs, malformed, err := r.ListTombstones()
	if err != nil {
		t.Fatal(err)
	}
	// The first write wins; the idempotent re-write does not update.
	if len(tombs) != 1 || tombs[0].Key != k || tombs[0].At != 100 || len(malformed) != 0 {
		t.Fatalf("tombstones = %+v malformed = %v", tombs, malformed)
	}
	if err := r.RemoveTombstone(k); err != nil {
		t.Fatal(err)
	}
	if has, err := r.HasTombstone(k); err != nil || has {
		t.Fatalf("after removal: has=%v err=%v", has, err)
	}
	if err := r.RemoveTombstone(k); err != nil {
		t.Fatal(err)
	}
}

// Crash remnants must not block publication or pollute listings, and
// malformed tombstones must be reported, not skipped.
func TestTombstoneCrashRemnantsAndMalformed(t *testing.T) {
	r := testRegistry(t)
	k := key("tu-crash")
	dir := filepath.Join(r.dir, "tombstones")

	if err := os.WriteFile(filepath.Join(dir, k.hash()+".tmp-9999"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "malformed-entry"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A valid record under the wrong filename would be stranded by
	// name-computed removal; it must be reported, not accepted.
	other := key("tu-other")
	misplaced, err := json.Marshal(Tombstone{Key: other, At: 50})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wrong-name"), misplaced, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := r.WriteTombstone(k, 100); err != nil {
		t.Fatalf("write with remnant present: %v", err)
	}
	tombs, malformed, err := r.ListTombstones()
	if err != nil {
		t.Fatal(err)
	}
	if len(tombs) != 1 || tombs[0].Key != k {
		t.Fatalf("tombs = %+v", tombs)
	}
	if len(malformed) != 2 {
		t.Fatalf("malformed = %v, want the non-JSON and misnamed entries reported", malformed)
	}
}

// Duplicate posts after closure are idempotent.
func TestDuplicatePostAfterClosureIsIdempotent(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	if _, err := r.Begin(ctx, entry("tu1", 100)); err != nil {
		t.Fatal(err)
	}
	var got []PendingToolSnapshot
	if closed, err := r.Complete(ctx, key("tu1"), CompletionInfo{EventID: "e", At: 200}, nil, doneFinalize(&got)); err != nil || !closed {
		t.Fatalf("closure: closed=%v err=%v", closed, err)
	}
	closed, err := r.Complete(ctx, key("tu1"), CompletionInfo{EventID: "e", At: 300}, nil, noFinalize(t))
	if err != nil || !closed {
		t.Fatalf("duplicate: closed=%v err=%v, want idempotent success", closed, err)
	}
}

// Receipt shapes and group bindings are enforced fail-closed.
func TestReceiptShapeAndBindingEnforced(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()

	// Invalid shape rejected at write time.
	if err := r.writeReceipt(context.Background(), completionReceipt{
		Key: key("tu-bad"), Info: CompletionInfo{EventID: "e", At: 1},
		GroupID: "g-x", Done: true, GroupFinal: &GroupFinal{PartialReason: "x"},
	}); err == nil {
		t.Error("done+final receipt shape accepted at write")
	}

	// A group-bound receipt naming the wrong group fails closed.
	gid, err := r.Begin(ctx, entry("tu1", 100))
	if err != nil {
		t.Fatal(err)
	}
	mismatched, err := json.Marshal(completionReceipt{
		Key: key("tu1"), Info: CompletionInfo{EventID: "e", At: 200},
		GroupID: "g-not-" + gid, Done: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.receiptsDir(), key("tu1").hash()), mismatched, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Stale(ctx, 10); !errors.Is(err, ErrRegistryCorrupt) {
		t.Fatalf("mismatched group receipt: err = %v, want corrupt", err)
	}
}

// A member receipt without its window is corrupt.
func TestUnknownMemberReceiptFailsClosed(t *testing.T) {
	r := testRegistry(t)
	orphan, err := json.Marshal(completionReceipt{
		Key: key("tu-ghost"), Info: CompletionInfo{EventID: "e", At: 200},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.receiptsDir(), key("tu-ghost").hash()), orphan, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Stale(context.Background(), 10); !errors.Is(err, ErrRegistryCorrupt) {
		t.Fatalf("unknown member receipt: err = %v, want corrupt", err)
	}
}

// Conflicting receipt bytes fail closed.
func TestConflictingReceiptFailsClosed(t *testing.T) {
	r := testRegistry(t)
	first := completionReceipt{Key: key("tu1"), Info: CompletionInfo{EventID: "evt-1", At: 100}}
	if err := r.writeReceipt(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	// Identical rewrite is idempotent.
	if err := r.writeReceipt(context.Background(), first); err != nil {
		t.Fatalf("identical rewrite: %v", err)
	}
	conflicting := completionReceipt{Key: key("tu1"), Info: CompletionInfo{EventID: "evt-2", At: 999}}
	if err := r.writeReceipt(context.Background(), conflicting); !errors.Is(err, ErrRegistryCorrupt) {
		t.Fatalf("conflicting receipt: err = %v, want corrupt", err)
	}
}

// Receipt upgrades are monotonic and reject equal-rank divergence.
func TestReceiptUpgradeIsMonotonic(t *testing.T) {
	info := CompletionInfo{EventID: "evt-1", At: 100}
	readReceipt := func(t *testing.T, r *Registry) completionReceipt {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(r.receiptsDir(), key("tu1").hash()))
		if err != nil {
			t.Fatal(err)
		}
		var rec completionReceipt
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatal(err)
		}
		return rec
	}

	t.Run("member to done", func(t *testing.T) {
		r := testRegistry(t)
		if err := r.writeReceipt(context.Background(), completionReceipt{Key: key("tu1"), Info: info}); err != nil {
			t.Fatal(err)
		}
		if err := r.writeReceipt(context.Background(), completionReceipt{Key: key("tu1"), Info: info, GroupID: "g1", Done: true}); err != nil {
			t.Fatalf("member->done upgrade: %v", err)
		}
		if rec := readReceipt(t, r); !rec.Done || rec.GroupID != "g1" {
			t.Fatalf("receipt after upgrade = %+v", rec)
		}
		// An intent cannot replace a newer outcome.
		if err := r.writeReceipt(context.Background(), completionReceipt{Key: key("tu1"), Info: info}); err != nil {
			t.Fatalf("superseded intent: %v", err)
		}
		if rec := readReceipt(t, r); !rec.Done {
			t.Fatalf("outcome demoted by intent: %+v", rec)
		}
	})

	t.Run("member to group final to done", func(t *testing.T) {
		r := testRegistry(t)
		if err := r.writeReceipt(context.Background(), completionReceipt{Key: key("tu1"), Info: info}); err != nil {
			t.Fatal(err)
		}
		final := &GroupFinal{PostTreeHash: "tree-x", CapturedAt: 100}
		if err := r.writeReceipt(context.Background(), completionReceipt{Key: key("tu1"), Info: info, GroupID: "g1", GroupFinal: final}); err != nil {
			t.Fatalf("member->final upgrade: %v", err)
		}
		if rec := readReceipt(t, r); rec.GroupFinal == nil || rec.GroupFinal.PostTreeHash != "tree-x" {
			t.Fatalf("receipt after upgrade = %+v", rec)
		}
		if err := r.writeReceipt(context.Background(), completionReceipt{Key: key("tu1"), Info: info, GroupID: "g1", Done: true}); err != nil {
			t.Fatalf("final->done upgrade: %v", err)
		}
		if rec := readReceipt(t, r); !rec.Done || rec.GroupFinal != nil {
			t.Fatalf("receipt after done = %+v", rec)
		}
	})

	t.Run("mismatched identity stays corrupt", func(t *testing.T) {
		r := testRegistry(t)
		if err := r.writeReceipt(context.Background(), completionReceipt{Key: key("tu1"), Info: info}); err != nil {
			t.Fatal(err)
		}
		other := CompletionInfo{EventID: "evt-2", At: 999}
		if err := r.writeReceipt(context.Background(), completionReceipt{Key: key("tu1"), Info: other, GroupID: "g1", Done: true}); !errors.Is(err, ErrRegistryCorrupt) {
			t.Fatalf("upgrade across identities: err = %v, want corrupt", err)
		}
	})

	t.Run("same rank divergence stays corrupt", func(t *testing.T) {
		r := testRegistry(t)
		a := &GroupFinal{PostTreeHash: "tree-a", CapturedAt: 100}
		b := &GroupFinal{PostTreeHash: "tree-b", CapturedAt: 100}
		if err := r.writeReceipt(context.Background(), completionReceipt{Key: key("tu1"), Info: info, GroupID: "g1", GroupFinal: a}); err != nil {
			t.Fatal(err)
		}
		if err := r.writeReceipt(context.Background(), completionReceipt{Key: key("tu1"), Info: info, GroupID: "g1", GroupFinal: b}); !errors.Is(err, ErrRegistryCorrupt) {
			t.Fatalf("diverging finals: err = %v, want corrupt", err)
		}
	})

	t.Run("concurrent divergent upgrades serialize", func(t *testing.T) {
		r := testRegistry(t)
		if err := r.writeReceipt(context.Background(), completionReceipt{Key: key("tu1"), Info: info}); err != nil {
			t.Fatal(err)
		}
		trees := []string{"tree-a", "tree-b"}
		errs := make([]error, len(trees))
		var wg sync.WaitGroup
		for i := range trees {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs[i] = r.writeReceipt(context.Background(), completionReceipt{
					Key: key("tu1"), Info: info, GroupID: "g1",
					GroupFinal: &GroupFinal{PostTreeHash: trees[i], CapturedAt: 100},
				})
			}(i)
		}
		wg.Wait()

		var winner int
		switch {
		case errs[0] == nil && errors.Is(errs[1], ErrRegistryCorrupt):
			winner = 0
		case errs[1] == nil && errors.Is(errs[0], ErrRegistryCorrupt):
			winner = 1
		default:
			t.Fatalf("errs = %v, want exactly one winner and one corruption", errs)
		}
		if rec := readReceipt(t, r); rec.GroupFinal == nil || rec.GroupFinal.PostTreeHash != trees[winner] {
			t.Fatalf("receipt = %+v, want the winner's tree %q", rec, trees[winner])
		}
	})

	t.Run("invalid existing shape stays corrupt", func(t *testing.T) {
		r := testRegistry(t)
		malformed, err := json.Marshal(completionReceipt{Key: key("tu1"), Info: info, GroupID: "g1"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.receiptsDir(), key("tu1").hash()), malformed, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := r.writeReceipt(context.Background(), completionReceipt{Key: key("tu1"), Info: info, GroupID: "g1", Done: true}); !errors.Is(err, ErrRegistryCorrupt) {
			t.Fatalf("upgrade over invalid shape: err = %v, want corrupt", err)
		}
	})
}

// Standalone recovery writes get one attempt after deadline expiry.
func TestReceiptLockDeadlineRules(t *testing.T) {
	r := testRegistry(t)
	expired, cancel := context.WithCancel(context.Background())
	cancel()

	// An uncontended recovery write still succeeds.
	rec := completionReceipt{Key: key("tu1"), Info: CompletionInfo{EventID: "evt-1", At: 100}}
	if err := r.writeReceipt(expired, rec); err != nil {
		t.Fatalf("uncontended safety write at deadline: %v", err)
	}

	// A contended registry pass respects its deadline.
	held, err := r.acquireReceiptLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = platform.UnlockFile(held)
		_ = held.Close()
	}()
	ctx, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	var pe *PartialError
	if _, err := r.Stale(ctx, 10); !errors.As(err, &pe) || pe.Reason != ReasonLockTimeout {
		t.Fatalf("contended pass past deadline: err = %v, want lock timeout", err)
	}
}

// Receipt consumption excludes concurrent upgrades through publication.
func TestReceiptConsumptionExcludesUpgraders(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	gid, err := r.Begin(ctx, entry("tu1", 100))
	if err != nil {
		t.Fatal(err)
	}
	// Seed an intent left by an interrupted finalizer.
	info := CompletionInfo{EventID: "evt-1", At: 200}
	if err := r.writeReceipt(ctx, completionReceipt{Key: key("tu1"), Info: info}); err != nil {
		t.Fatal(err)
	}

	// Pause after receipt consumption but before publication.
	entered := make(chan struct{})
	release := make(chan struct{})
	passDone := make(chan error, 1)
	go func() {
		_, err := r.withLock(ctx, func(*registryState) (bool, error) {
			close(entered)
			<-release
			return true, nil
		})
		passDone <- err
	}()
	<-entered

	// Attempt a concurrent outcome upgrade.
	upgradeDone := make(chan error, 1)
	go func() {
		upgradeDone <- r.writeReceipt(ctx, completionReceipt{
			Key: key("tu1"), Info: info, GroupID: gid,
			GroupFinal: &GroupFinal{PostTreeHash: "tree-x", CapturedAt: 200},
		})
	}()
	select {
	case err := <-upgradeDone:
		t.Fatalf("upgrade completed during the consuming pass: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	close(release)
	if err := <-passDone; err != nil {
		t.Fatal(err)
	}
	if err := <-upgradeDone; err != nil {
		t.Fatal(err)
	}

	// The upgrade remains available for the next pass.
	pending, err := r.PendingFinalizations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Final == nil || pending[0].Final.PostTreeHash != "tree-x" {
		t.Fatalf("pending = %+v, want the upgraded identity preserved", pending)
	}
}

// ResumeFinalization closes a complete group from durable state.
func TestResumeFinalizationRecoversStrandedGroup(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	gid, err := r.Begin(ctx, entry("tu1", 100))
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("persist failed")
	if _, err := r.Complete(ctx, key("tu1"), CompletionInfo{EventID: "evt-1", At: 200, CommandSummary: "cmd"}, nil,
		func(_ []PendingToolSnapshot, _ *GroupFinal, _ bool, _ func() error) (FinalizeResult, error) {
			return FinalizeResult{Final: GroupFinal{PostTreeHash: "tree-x", CapturedAt: 200}}, boom
		}); !errors.Is(err, boom) {
		t.Fatal(err)
	}

	pending, err := r.PendingFinalizations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].GroupID != gid || pending[0].Final == nil ||
		pending[0].Final.PostTreeHash != "tree-x" {
		t.Fatalf("pending = %+v", pending)
	}

	closed, err := r.ResumeFinalization(ctx, gid, func(members []PendingToolSnapshot, prior *GroupFinal, _ bool, _ func() error) (FinalizeResult, error) {
		if prior == nil || prior.PostTreeHash != "tree-x" {
			t.Fatalf("resume without persisted identity: %+v", prior)
		}
		if len(members) != 1 || members[0].EventID != "evt-1" || members[0].CommandSummary != "cmd" {
			t.Fatalf("members = %+v", members)
		}
		return FinalizeResult{Done: true}, nil
	})
	if err != nil || !closed {
		t.Fatalf("resume: closed=%v err=%v", closed, err)
	}
	// A late duplicate remains idempotent.
	if pending, err := r.PendingFinalizations(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("pending after resume = %+v err = %v", pending, err)
	}
	closed, err = r.Complete(ctx, key("tu1"), CompletionInfo{EventID: "evt-1", At: 999}, nil, noFinalize(t))
	if err != nil || !closed {
		t.Fatalf("late duplicate: closed=%v err=%v", closed, err)
	}
}

// Missing-member receipts may reference only groups that are also gone.
func TestGroupReceiptAbsentKeyRules(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()

	stale, err := json.Marshal(completionReceipt{
		Key: key("tu-gone"), Info: CompletionInfo{EventID: "e", At: 100},
		GroupID: "g-vanished", Done: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.receiptsDir(), key("tu-gone").hash()), stale, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Stale(ctx, 10); err != nil {
		t.Fatalf("stale receipt for vanished group: %v", err)
	}
	if entries, _ := os.ReadDir(r.receiptsDir()); len(entries) != 0 {
		t.Fatalf("stale receipt not consumed: %v", entries)
	}

	// Referencing a live group fails closed.
	gid, err := r.Begin(ctx, entry("tu-live", 100))
	if err != nil {
		t.Fatal(err)
	}
	hostile, err := json.Marshal(completionReceipt{
		Key: key("tu-foreign"), Info: CompletionInfo{EventID: "e", At: 100},
		GroupID: gid, Done: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.receiptsDir(), key("tu-foreign").hash()), hostile, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Stale(ctx, 10); !errors.Is(err, ErrRegistryCorrupt) {
		t.Fatalf("memberless receipt against live group: err = %v, want corrupt", err)
	}
}

// A malformed closure marker is corruption, never a signal.
func TestMalformedClosureMarkerFailsClosed(t *testing.T) {
	r := testRegistry(t)
	k := key("tu-marker")
	if err := os.WriteFile(filepath.Join(r.closuresDir(), k.hash()), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Complete(context.Background(), k, CompletionInfo{EventID: "e", At: 100}, nil, noFinalize(t)); !errors.Is(err, ErrRegistryCorrupt) {
		t.Fatalf("err = %v, want corrupt", err)
	}
	if _, err := r.CaptureAndBegin(context.Background(), nil, k, "Bash", 100); !errors.Is(err, ErrRegistryCorrupt) {
		// A nil store is never reached: the marker check precedes it.
		t.Fatalf("pre err = %v, want corrupt before any store use", err)
	}
}

// A persisted intent recovers an interrupted completion.
func TestFinalizationIntentRecoversDeadProcess(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	if _, err := r.Begin(ctx, entry("tu1", 100)); err != nil {
		t.Fatal(err)
	}
	// Seed the interrupted finalizer's intent.
	if err := r.writeReceipt(ctx, completionReceipt{Key: key("tu1"), Info: CompletionInfo{EventID: "evt-1", At: 200, CommandSummary: "cmd"}}); err != nil {
		t.Fatal(err)
	}

	// The next pass applies the completion.
	pending, err := r.PendingFinalizations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || len(pending[0].Members) != 1 {
		t.Fatalf("pending = %+v", pending)
	}
	m := pending[0].Members[0]
	if m.Status != "complete" || m.EventID != "evt-1" || m.CommandSummary != "cmd" || m.CompletedAt != 200 {
		t.Fatalf("member = %+v", m)
	}
	if entries, _ := os.ReadDir(r.receiptsDir()); len(entries) != 0 {
		t.Fatalf("intent not consumed: %v", entries)
	}
}

// A closure marker makes a leftover intent harmless.
func TestLeftoverIntentAfterClosureConsumed(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	if _, err := r.Begin(ctx, entry("tu1", 100)); err != nil {
		t.Fatal(err)
	}
	var got []PendingToolSnapshot
	if closed, err := r.Complete(ctx, key("tu1"), CompletionInfo{EventID: "evt-1", At: 200}, nil, doneFinalize(&got)); err != nil || !closed {
		t.Fatalf("closure: closed=%v err=%v", closed, err)
	}
	// Simulate an intent left after closure.
	if err := r.writeReceipt(ctx, completionReceipt{Key: key("tu1"), Info: CompletionInfo{EventID: "evt-1", At: 200}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Stale(ctx, 10); err != nil {
		t.Fatalf("leftover intent broke the registry: %v", err)
	}
	if entries, _ := os.ReadDir(r.receiptsDir()); len(entries) != 0 {
		t.Fatalf("leftover intent not consumed: %v", entries)
	}
}

// The locked closure check rejects a racing pre hook.
func TestClosedKeyRecheckedUnderLock(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	r, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	holder, err := os.OpenFile(r.lockPath(), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	if err := platform.LockFile(holder); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Block after the unlocked fast check.
		_, err := r.CaptureAndBegin(wctx, s, key("tu-race"), "Bash", 100)
		done <- err
	}()
	// Close the key while the pre hook waits.
	time.Sleep(150 * time.Millisecond)
	if err := r.writeClosureMarker(key("tu-race"), "g-x", 100); err != nil {
		t.Fatal(err)
	}
	if err := platform.UnlockFile(holder); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("racing pre: %v", err)
	}
	wins, err := r.Stale(ctx, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(wins) != 0 {
		t.Fatalf("closed key captured under the race: %+v", wins)
	}
}

func TestStaleWindowsListed(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	if _, err := r.Begin(ctx, entry("tu-old", 100)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Begin(ctx, entry("tu-new", 5000)); err != nil {
		t.Fatal(err)
	}
	stale, err := r.Stale(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].Key.ToolUseID != "tu-old" {
		t.Fatalf("stale = %+v", stale)
	}
}
