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
func noFinalize(t *testing.T) func([]PendingToolSnapshot, *GroupFinal) (FinalizeResult, error) {
	return func(m []PendingToolSnapshot, _ *GroupFinal) (FinalizeResult, error) {
		t.Fatalf("finalize invoked unexpectedly with %d members", len(m))
		return FinalizeResult{}, nil
	}
}

// doneFinalize records members and reports durable completion.
func doneFinalize(members *[]PendingToolSnapshot) func([]PendingToolSnapshot, *GroupFinal) (FinalizeResult, error) {
	return func(m []PendingToolSnapshot, _ *GroupFinal) (FinalizeResult, error) {
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
	closed, err := r.Complete(ctx, key("tu1"), 200, doneFinalize(&got))
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
	closed, err := r.Complete(ctx, key("tuA"), 120, noFinalize(t))
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
	if closed, err = r.Complete(ctx, key("tuB"), 140, noFinalize(t)); err != nil || closed {
		t.Fatalf("B completion: closed=%v err=%v", closed, err)
	}
	var members []PendingToolSnapshot
	closed, err = r.Complete(ctx, key("tuC"), 150, doneFinalize(&members))
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
		_, err := r.Complete(ctx, key("tu1"), 200, func([]PendingToolSnapshot, *GroupFinal) (FinalizeResult, error) {
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
	closed, err := r.Complete(ctx, key("tu1"), 200, func(_ []PendingToolSnapshot, prior *GroupFinal) (FinalizeResult, error) {
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
	closed, err = r.Complete(ctx, key("tu1"), 999, func(m []PendingToolSnapshot, prior *GroupFinal) (FinalizeResult, error) {
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
	closed, err := r.Complete(ctx, key("tu1"), 200, func(_ []PendingToolSnapshot, _ *GroupFinal) (FinalizeResult, error) {
		return FinalizeResult{Final: GroupFinal{PartialReason: ReasonLockTimeout}}, errors.New("post capture impossible")
	})
	if err == nil || closed {
		t.Fatalf("first attempt: closed=%v err=%v", closed, err)
	}
	closed, err = r.Complete(ctx, key("tu1"), 999, func(_ []PendingToolSnapshot, prior *GroupFinal) (FinalizeResult, error) {
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
	if _, err := r.Complete(ctx, key("tu1"), 200, func(_ []PendingToolSnapshot, _ *GroupFinal) (FinalizeResult, error) {
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
	if _, err := r.Complete(ctx, key("tu1"), 400, func(_ []PendingToolSnapshot, prior *GroupFinal) (FinalizeResult, error) {
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
	if _, err := r.Complete(ctx, key("tu1"), 200, func(_ []PendingToolSnapshot, _ *GroupFinal) (FinalizeResult, error) {
		return FinalizeResult{Final: GroupFinal{PostTreeHash: "tree-1", CapturedAt: 200}}, boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	// A conflicting tree must be rejected and the original preserved.
	if _, err := r.Complete(ctx, key("tu1"), 300, func(_ []PendingToolSnapshot, _ *GroupFinal) (FinalizeResult, error) {
		return FinalizeResult{Final: GroupFinal{PostTreeHash: "tree-2", CapturedAt: 300}}, boom
	}); err == nil {
		t.Fatal("conflicting final tree accepted")
	}
	// Enrichment with a delta hash for the same tree is permitted.
	if _, err := r.Complete(ctx, key("tu1"), 400, func(_ []PendingToolSnapshot, prior *GroupFinal) (FinalizeResult, error) {
		if prior == nil || prior.PostTreeHash != "tree-1" {
			t.Fatalf("prior = %+v, want preserved tree-1", prior)
		}
		return FinalizeResult{Final: GroupFinal{PostTreeHash: "tree-1", DeltaHash: "delta-1", CapturedAt: 200}}, boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if _, err := r.Complete(ctx, key("tu1"), 500, func(_ []PendingToolSnapshot, prior *GroupFinal) (FinalizeResult, error) {
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
	if _, err := r.Complete(ctx, key("tu1"), 200, func(_ []PendingToolSnapshot, _ *GroupFinal) (FinalizeResult, error) {
		return FinalizeResult{Final: GroupFinal{PostTreeHash: "tree-1", CapturedAt: 200}}, boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	// A delta hash without the tree cannot be proven to describe it.
	if _, err := r.Complete(ctx, key("tu1"), 300, func(_ []PendingToolSnapshot, _ *GroupFinal) (FinalizeResult, error) {
		return FinalizeResult{Final: GroupFinal{DeltaHash: "delta-1"}}, boom
	}); err == nil || !strings.Contains(err.Error(), "restate") {
		t.Fatalf("delta without tree restatement accepted: %v", err)
	}
	// The identity is preserved.
	if _, err := r.Complete(ctx, key("tu1"), 400, func(_ []PendingToolSnapshot, prior *GroupFinal) (FinalizeResult, error) {
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
	err := r.withLock(ctx, func(state *registryState) (bool, error) {
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

func TestCompleteWithoutBeginIsTyped(t *testing.T) {
	r := testRegistry(t)
	_, err := r.Complete(context.Background(), key("ghost"), 10, noFinalize(t))
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
	if closed, err := r.Complete(ctx, key("tu1"), 120, noFinalize(t)); err != nil || closed {
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
	if _, err := r.Complete(ctx, corruptKey, 10, noFinalize(t)); !errors.Is(err, ErrRegistryCorrupt) {
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
		closed, err := r.Complete(ctx, key(fmt.Sprintf("tu-%02d", i)), int64(200+i), doneFinalize(&members))
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

// Equal start times and colliding tool-use IDs across sessions must
// still order totally, never by insertion order.
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
	if closed, err := r.Complete(ctx, k1, 200, noFinalize(t)); err != nil || closed {
		t.Fatalf("k1 completion: closed=%v err=%v", closed, err)
	}
	var members []PendingToolSnapshot
	if _, err := r.Complete(ctx, k2, 210, doneFinalize(&members)); err != nil {
		t.Fatal(err)
	}
	// sess-1 sorts before sess-2 despite later registration.
	if len(members) != 2 || members[0].Key.SessionID != "sess-1" || members[1].Key.SessionID != "sess-2" {
		t.Fatalf("members = %+v", members)
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
