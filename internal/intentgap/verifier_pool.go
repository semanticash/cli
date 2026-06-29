package intentgap

import (
	"context"
	"sync"
)

// MaxVerifierWorkers caps verifier parallelism.
const MaxVerifierWorkers = 4

// VerifierPoolInput contains data shared by pool workers.
type VerifierPoolInput struct {
	Candidates  []Candidate
	IntentsByID map[string]IntentItem
	Change      ChangeLedger
	Action      ActionLedger
	Bundle      Bundle
}

// VerifierPoolResult reports completed results and deadline skips.
type VerifierPoolResult struct {
	Results           []VerifierResult
	SkippedOnDeadline int
	DeadlineExceeded  bool
}

// RunVerifierPool verifies candidates in parallel and returns results
// in candidate order. The caller owns the context deadline.
func RunVerifierPool(ctx context.Context, runner ScopedVerifierRunner, in VerifierPoolInput) VerifierPoolResult {
	if len(in.Candidates) == 0 {
		return VerifierPoolResult{}
	}

	workQueue := make(chan int, len(in.Candidates))
	for i := range in.Candidates {
		workQueue <- i
	}
	close(workQueue)

	// Keep output ordered by candidate index.
	results := make([]VerifierResult, len(in.Candidates))
	processed := make([]bool, len(in.Candidates))
	var resultsMu sync.Mutex

	workers := MaxVerifierWorkers
	if workers > len(in.Candidates) {
		workers = len(in.Candidates)
	}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case idx, ok := <-workQueue:
					if !ok {
						return
					}
					// Re-check after receive. If ctx.Done() and the
					// buffered queue are both ready, select may choose
					// the queue even after cancellation.
					select {
					case <-ctx.Done():
						return
					default:
					}
					result := runOneCandidate(ctx, runner, in, idx)
					resultsMu.Lock()
					results[idx] = result
					processed[idx] = true
					resultsMu.Unlock()
				}
			}
		}()
	}

	wg.Wait()

	out := VerifierPoolResult{}
	for i := range in.Candidates {
		if processed[i] {
			out.Results = append(out.Results, results[i])
		} else {
			out.SkippedOnDeadline++
		}
	}
	if ctx.Err() != nil {
		out.DeadlineExceeded = true
	}
	return out
}

// runOneCandidate builds the scoped verifier input for one candidate.
func runOneCandidate(ctx context.Context, runner ScopedVerifierRunner, in VerifierPoolInput, idx int) VerifierResult {
	candidate := in.Candidates[idx]
	intent := in.IntentsByID[candidate.IntentID]
	input := VerifierInput{
		Candidate: candidate,
		Intent:    intent,
		Change:    in.Change,
		Action:    in.Action,
		Bundle:    in.Bundle,
	}
	return RunScopedVerifier(ctx, runner, input)
}
