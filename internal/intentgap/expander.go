package intentgap

import (
	"context"
	"fmt"
	"sort"
)

// Expansion limits. The orchestrator owns the overall deadline.
const (
	MaxExpansionsPerRun         = 3
	HunkNeighborWindowLines     = 50
	MaxSiblingFilesPerExpansion = 3
	MaxExpandedDiffPointers     = 6

	// DropExpansionInconclusive marks a second needs_more_context result.
	DropExpansionInconclusive = "expansion_inconclusive"
)

// ExpansionInput contains the verifier results and ledgers used for
// one expansion pass.
type ExpansionInput struct {
	VerifierResults []VerifierResult
	CandidatesByID  map[string]Candidate
	IntentsByID     map[string]IntentItem
	Change          ChangeLedger
	Action          ActionLedger
	Bundle          Bundle
}

// ExpansionResult contains updated verifier results and counters for
// coverage_summary.
type ExpansionResult struct {
	UpdatedResults    []VerifierResult
	ExpansionAttempts int
	Inconclusive      int
}

// RunExpander re-verifies the highest-scoring needs_more_context
// results once, then writes results back in original order.
func RunExpander(ctx context.Context, runner ScopedVerifierRunner, in ExpansionInput) ExpansionResult {
	if len(in.VerifierResults) == 0 {
		return ExpansionResult{}
	}

	type scored struct {
		index int
		score float64
	}
	var nmc []scored
	for i, r := range in.VerifierResults {
		if r.Verdict != VerdictNeedsMoreContext {
			continue
		}
		// Skip orphaned results; expansion needs candidate metadata.
		cand, ok := in.CandidatesByID[r.CandidateID]
		if !ok {
			continue
		}
		nmc = append(nmc, scored{index: i, score: cand.Score})
	}
	// Highest score first; tie-break by index so the order is
	// stable across runs.
	sort.SliceStable(nmc, func(i, j int) bool {
		if nmc[i].score == nmc[j].score {
			return nmc[i].index < nmc[j].index
		}
		return nmc[i].score > nmc[j].score
	})
	if len(nmc) > MaxExpansionsPerRun {
		nmc = nmc[:MaxExpansionsPerRun]
	}

	// Key replacements by result index so duplicate CandidateIDs do
	// not share an output slot.
	replacements := make(map[int]VerifierResult, len(nmc))
	out := ExpansionResult{}
	for _, n := range nmc {
		original := in.VerifierResults[n.index]
		// Candidate metadata was verified during scoring.
		cand := in.CandidatesByID[original.CandidateID]
		intent := in.IntentsByID[cand.IntentID]
		expandedCand := expandCandidate(cand, in.Change)
		neighbors := neighboringTurns(intent.TurnID, in.Bundle.Turns)

		input := VerifierInput{
			Candidate:        expandedCand,
			Intent:           intent,
			Change:           in.Change,
			Action:           in.Action,
			Bundle:           in.Bundle,
			NeighboringTurns: neighbors,
		}
		result := RunScopedVerifier(ctx, runner, input)
		out.ExpansionAttempts++

		if result.Verdict == VerdictNeedsMoreContext {
			result = VerifierResult{
				CandidateID: result.CandidateID,
				Verdict:     VerdictDrop,
				DropReason:  DropExpansionInconclusive,
				Rationale:   result.Rationale,
			}
			out.Inconclusive++
		}
		replacements[n.index] = result
	}

	out.UpdatedResults = make([]VerifierResult, len(in.VerifierResults))
	for i, r := range in.VerifierResults {
		if rep, ok := replacements[i]; ok {
			out.UpdatedResults[i] = rep
		} else {
			out.UpdatedResults[i] = r
		}
	}
	return out
}

// expandCandidate adds nearby hunks and same-directory files to a
// candidate, capped by MaxExpandedDiffPointers. V1 does not expand
// ActionIDs.
func expandCandidate(c Candidate, change ChangeLedger) Candidate {
	out := c
	existing := map[string]bool{}
	for _, p := range out.DiffPointers {
		existing[hunkRefKey(p)] = true
	}

	for _, p := range c.DiffPointers {
		if len(out.DiffPointers) >= MaxExpandedDiffPointers {
			break
		}
		f := change.ByPath[p.File]
		if f == nil {
			continue
		}
		for _, h := range f.Hunks {
			if len(out.DiffPointers) >= MaxExpandedDiffPointers {
				break
			}
			ref := HunkRef{File: p.File, StartLine: h.StartLine, EndLine: h.EndLine}
			if existing[hunkRefKey(ref)] {
				continue
			}
			if !hunkWithinNeighborWindow(p, h) {
				continue
			}
			out.DiffPointers = append(out.DiffPointers, ref)
			existing[hunkRefKey(ref)] = true
		}
	}

	if len(c.DiffPointers) > 0 {
		dir := parentDir(c.DiffPointers[0].File)
		filesOnDir := map[string]bool{}
		for _, p := range out.DiffPointers {
			filesOnDir[p.File] = true
		}
		added := 0
		for _, f := range change.Files {
			if added >= MaxSiblingFilesPerExpansion {
				break
			}
			if len(out.DiffPointers) >= MaxExpandedDiffPointers {
				break
			}
			if parentDir(f.Path) != dir {
				continue
			}
			if filesOnDir[f.Path] {
				continue
			}
			if len(f.Hunks) == 0 {
				continue
			}
			h := f.Hunks[0]
			ref := HunkRef{File: f.Path, StartLine: h.StartLine, EndLine: h.EndLine}
			if existing[hunkRefKey(ref)] {
				continue
			}
			out.DiffPointers = append(out.DiffPointers, ref)
			existing[hunkRefKey(ref)] = true
			filesOnDir[f.Path] = true
			added++
		}
	}

	return out
}

// hunkWithinNeighborWindow reports whether h is near p.
func hunkWithinNeighborWindow(p HunkRef, h ChangedHunk) bool {
	if h.EndLine < p.StartLine-HunkNeighborWindowLines {
		return false
	}
	if h.StartLine > p.EndLine+HunkNeighborWindowLines {
		return false
	}
	return true
}

// hunkRefKey returns a stable HunkRef identity.
func hunkRefKey(r HunkRef) string {
	return fmt.Sprintf("%s:%d-%d", r.File, r.StartLine, r.EndLine)
}

// neighboringTurns returns the adjacent turns around turnID.
func neighboringTurns(turnID string, turns []BundleTurn) []BundleTurn {
	idx := -1
	for i, t := range turns {
		if t.TurnID == turnID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	var out []BundleTurn
	if idx > 0 {
		out = append(out, turns[idx-1])
	}
	if idx+1 < len(turns) {
		out = append(out, turns[idx+1])
	}
	return out
}
