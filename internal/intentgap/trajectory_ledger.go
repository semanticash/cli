package intentgap

// TrajectoryLedger is a lookup view over the trajectory candidates
// detected from a bundle. All preserves the detector's output order
// (alphabetical by file, then line range); ByActionID lets a caller
// answer "is this action part of any detected trajectory?" in O(1),
// which is what the deferred-finding citation rule needs and what
// the candidate retrieval step uses to prefer trajectory-member
// actions when assembling verifier packets.
//
// The ledger holds pointers into its own All slice so callers reading
// from ByActionID see the same data as iterating All; rebuilding the
// ledger off the same bundle is idempotent.
type TrajectoryLedger struct {
	All        []TrajectoryCandidate
	ByActionID map[string]*TrajectoryCandidate
}

// BuildTrajectoryLedger runs the existing edit-revert detector over
// the bundle and indexes the result for downstream consumers. The
// detection logic stays in trajectory.go; this constructor only
// provides the index layer.
func BuildTrajectoryLedger(bundle Bundle) TrajectoryLedger {
	candidates := DetectEditRevertTrajectories(bundle)
	ledger := TrajectoryLedger{
		ByActionID: map[string]*TrajectoryCandidate{},
	}
	if len(candidates) == 0 {
		return ledger
	}
	ledger.All = make([]TrajectoryCandidate, len(candidates))
	copy(ledger.All, candidates)
	for i := range ledger.All {
		c := &ledger.All[i]
		for _, id := range c.ActionIDs {
			ledger.ByActionID[id] = c
		}
	}
	return ledger
}
