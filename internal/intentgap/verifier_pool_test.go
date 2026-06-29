package intentgap

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/llm"
)

// poolTestRunner is a controllable runner for pool tests.
type poolTestRunner struct {
	delay    time.Duration
	response string
	calls    int32

	mu     sync.Mutex
	starts []time.Time
	ends   []time.Time
}

func (r *poolTestRunner) GenerateText(ctx context.Context, _ string) (*llm.GenerateTextResult, error) {
	atomic.AddInt32(&r.calls, 1)
	r.mu.Lock()
	r.starts = append(r.starts, time.Now())
	r.mu.Unlock()
	select {
	case <-ctx.Done():
		r.mu.Lock()
		r.ends = append(r.ends, time.Now())
		r.mu.Unlock()
		return nil, ctx.Err()
	case <-time.After(r.delay):
	}
	r.mu.Lock()
	r.ends = append(r.ends, time.Now())
	r.mu.Unlock()
	return &llm.GenerateTextResult{Text: r.response}, nil
}

// poolCandidates builds n Track B candidates with stable IDs.
func poolCandidates(t *testing.T, n int) (VerifierPoolInput, []string) {
	t.Helper()
	intent := candIntent("i-shared", IntentRequest, 100, "add tests for the new handler")
	change := changeLedgerOf(
		struct {
			path     string
			category FileCategory
		}{"internal/service/handler.go", CatCode},
	)
	change.Files[0].Hunks = []ChangedHunk{{
		StartLine: 12, EndLine: 24,
		Direction: HunkAdded,
		Body:      "+func Handle(req *Request) error { return validate(req) }\n",
	}}
	change.ByPath["internal/service/handler.go"] = &change.Files[0]

	var ids []string
	var cands []Candidate
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("cand-%d", i)
		ids = append(ids, id)
		cands = append(cands, Candidate{
			ID:              id,
			Kind:            CandUnderImplPartialScope,
			IntentID:        intent.ID,
			Score:           0.5,
			Reason:          "intent matched files but no test category file was changed",
			DiffPointers:    []HunkRef{{File: "internal/service/handler.go", StartLine: 12, EndLine: 24}},
			MissingCategory: CatTest,
		})
	}
	return VerifierPoolInput{
		Candidates:  cands,
		IntentsByID: map[string]IntentItem{intent.ID: intent},
		Change:      change,
	}, ids
}

// Empty input returns without calling the runner.
func TestRunVerifierPool_EmptyShortCircuits(t *testing.T) {
	runner := &poolTestRunner{}
	got := RunVerifierPool(context.Background(), runner, VerifierPoolInput{})
	if len(got.Results) != 0 || got.SkippedOnDeadline != 0 || got.DeadlineExceeded {
		t.Errorf("empty input must produce empty result; got %+v", got)
	}
	if atomic.LoadInt32(&runner.calls) != 0 {
		t.Errorf("empty input must not call the runner; got %d calls", runner.calls)
	}
}

// Results preserve candidate order.
func TestRunVerifierPool_AllCandidatesProcessed(t *testing.T) {
	in, ids := poolCandidates(t, 6)
	runner := &poolTestRunner{
		delay:    5 * time.Millisecond,
		response: `{"verdict":"needs_more_context","rationale":"need more"}`,
	}

	got := RunVerifierPool(context.Background(), runner, in)
	if len(got.Results) != len(in.Candidates) {
		t.Fatalf("Results len = %d, want %d", len(got.Results), len(in.Candidates))
	}
	if got.SkippedOnDeadline != 0 {
		t.Errorf("SkippedOnDeadline = %d, want 0", got.SkippedOnDeadline)
	}
	for i, want := range ids {
		if got.Results[i].CandidateID != want {
			t.Errorf("Results[%d].CandidateID = %q, want %q (input order must be preserved)",
				i, got.Results[i].CandidateID, want)
		}
	}
}

// Deadline cancellation reports undispatched candidates as skipped.
func TestRunVerifierPool_DeadlineSkipsRemainingCandidates(t *testing.T) {
	in, _ := poolCandidates(t, 12)
	runner := &poolTestRunner{
		delay:    80 * time.Millisecond,
		response: `{"verdict":"needs_more_context","rationale":""}`,
	}
	// Tight enough that some queued candidates should remain unstarted.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	got := RunVerifierPool(ctx, runner, in)
	if !got.DeadlineExceeded {
		t.Errorf("DeadlineExceeded = false, want true; ctx.Err()=%v", ctx.Err())
	}
	if got.SkippedOnDeadline == 0 {
		t.Errorf("SkippedOnDeadline = 0; with 12 candidates and a tight deadline some should be skipped (results=%d)",
			len(got.Results))
	}
	total := len(got.Results) + got.SkippedOnDeadline
	if total != len(in.Candidates) {
		t.Errorf("Results(%d) + SkippedOnDeadline(%d) = %d, want %d",
			len(got.Results), got.SkippedOnDeadline, total, len(in.Candidates))
	}
}

// Workers run in parallel.
func TestRunVerifierPool_WorkersRunInParallel(t *testing.T) {
	in, _ := poolCandidates(t, MaxVerifierWorkers)
	const callDelay = 60 * time.Millisecond
	runner := &poolTestRunner{
		delay:    callDelay,
		response: `{"verdict":"needs_more_context","rationale":""}`,
	}

	start := time.Now()
	got := RunVerifierPool(context.Background(), runner, in)
	elapsed := time.Since(start)

	if len(got.Results) != MaxVerifierWorkers {
		t.Fatalf("Results len = %d, want %d", len(got.Results), MaxVerifierWorkers)
	}
	// Use a loose wall-clock bound to avoid CI flakes.
	serial := time.Duration(MaxVerifierWorkers) * callDelay
	if elapsed >= serial/2 {
		t.Errorf("elapsed %v >= half of serial %v; workers do not appear to run in parallel",
			elapsed, serial)
	}
}

// gatedRunner blocks calls until release is closed.
type gatedRunner struct {
	release  chan struct{}
	started  int32
	response string
}

func (g *gatedRunner) GenerateText(ctx context.Context, _ string) (*llm.GenerateTextResult, error) {
	atomic.AddInt32(&g.started, 1)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-g.release:
	}
	return &llm.GenerateTextResult{Text: g.response}, nil
}

// Cancellation stops new dispatches even when work remains queued.
func TestRunVerifierPool_NoDispatchAfterDeadline(t *testing.T) {
	const candidates = MaxVerifierWorkers * 3
	in, _ := poolCandidates(t, candidates)

	gate := &gatedRunner{
		release:  make(chan struct{}),
		response: `{"verdict":"needs_more_context","rationale":""}`,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type poolOutcome struct {
		result VerifierPoolResult
	}
	done := make(chan poolOutcome, 1)
	go func() {
		done <- poolOutcome{result: RunVerifierPool(ctx, gate, in)}
	}()

	// Wait until the first worker wave is in flight.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&gate.started) < int32(MaxVerifierWorkers) {
		if time.Now().After(deadline) {
			t.Fatalf("worker pool never saturated; started=%d, want %d",
				gate.started, MaxVerifierWorkers)
		}
		time.Sleep(2 * time.Millisecond)
	}
	startedAtCancel := atomic.LoadInt32(&gate.started)

	// Cancel while queued work remains.
	cancel()

	// Give workers a chance to observe cancellation.
	time.Sleep(20 * time.Millisecond)

	// Release in-flight calls so the pool can drain.
	close(gate.release)

	select {
	case out := <-done:
		total := atomic.LoadInt32(&gate.started)
		if total > startedAtCancel {
			t.Errorf("pool dispatched %d additional candidate(s) after cancel; want 0 "+
				"(started_at_cancel=%d, started_total=%d)",
				total-startedAtCancel, startedAtCancel, total)
		}
		wantSkipped := candidates - int(startedAtCancel)
		if out.result.SkippedOnDeadline != wantSkipped {
			t.Errorf("SkippedOnDeadline = %d, want %d (started_at_cancel=%d)",
				out.result.SkippedOnDeadline, wantSkipped, startedAtCancel)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("pool did not return within 2s of cancellation + release")
	}
}

// Missing intents use the zero IntentItem and still run.
func TestRunVerifierPool_MissingIntentDoesNotPanic(t *testing.T) {
	intent := candIntent("i-known", IntentRequest, 100, "add tests for the new handler")
	change := changeLedgerOf(
		struct {
			path     string
			category FileCategory
		}{"f.go", CatCode},
	)
	cands := []Candidate{
		{ID: "c-1", Kind: CandUnderImplPartialScope, IntentID: "i-missing",
			DiffPointers: []HunkRef{{File: "f.go", StartLine: 1, EndLine: 2}}},
	}
	runner := &poolTestRunner{response: `{"verdict":"drop","drop_reason":"intent_too_vague","rationale":""}`}
	got := RunVerifierPool(context.Background(), runner, VerifierPoolInput{
		Candidates:  cands,
		IntentsByID: map[string]IntentItem{intent.ID: intent},
		Change:      change,
	})
	if len(got.Results) != 1 {
		t.Fatalf("Results len = %d, want 1", len(got.Results))
	}
	if got.Results[0].Verdict != VerdictDrop {
		t.Errorf("Verdict = %q, want drop", got.Results[0].Verdict)
	}
}

// Runner errors become one typed drop per candidate.
func TestRunVerifierPool_AllCallsFailingProducesCallFailedResults(t *testing.T) {
	in, ids := poolCandidates(t, 3)
	runner := &erroringRunner{}
	got := RunVerifierPool(context.Background(), runner, in)
	if len(got.Results) != 3 {
		t.Fatalf("Results len = %d, want 3", len(got.Results))
	}
	for i, r := range got.Results {
		if r.Verdict != VerdictDrop || r.DropReason != DropVerifierCallFailed {
			t.Errorf("Results[%d] = %+v, want drop with verifier_call_failed", i, r)
		}
		if r.CandidateID != ids[i] {
			t.Errorf("Results[%d].CandidateID = %q, want %q", i, r.CandidateID, ids[i])
		}
	}
}

// erroringRunner makes every verifier call fail.
type erroringRunner struct{}

func (erroringRunner) GenerateText(context.Context, string) (*llm.GenerateTextResult, error) {
	return nil, fmt.Errorf("simulated provider failure")
}
