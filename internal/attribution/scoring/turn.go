package scoring

// alignTurnClaims matches snapshot lines in order.
// Conflicting providers and budget exhaustion leave lines unmatched.
func alignTurnClaims(groups []DeltaClaimGroup, added []string) []*lineCandidate {
	matched := make([]*lineCandidate, len(added))
	contested := make([]bool, len(added))
	remaining := alignBudget
	for _, group := range groups {
		if len(group.Lines)+1 > remaining/(len(added)+1) {
			return nil
		}
		remaining -= (len(group.Lines) + 1) * (len(added) + 1)
		alignment, ok := AlignOrdered(NewClaimLines(group.Lines), added)
		if !ok {
			return nil
		}
		for i, line := range alignment {
			if line.Tier == AlignNone || contested[i] {
				continue
			}
			if matched[i] != nil && matched[i].provider != group.Provider {
				matched[i], contested[i] = nil, true
				continue
			}
			quality := qualityExact
			if line.Tier == AlignNormalized {
				quality = qualityNormalized
			}
			matched[i] = &lineCandidate{provider: group.Provider, quality: quality}
		}
	}
	return matched
}
