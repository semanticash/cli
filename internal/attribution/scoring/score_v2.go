package scoring

import (
	"sort"
	"strings"
)

// Candidate qualities, ordered: higher wins.
const (
	qualityNormalized = 1
	qualityExact      = 2
)

// lineCandidate is one piece of evidence competing for a final line.
type lineCandidate struct {
	quality   int
	provider  string
	ts        int64
	insertSeq int64
	eventID   string
	fromDelta bool
}

// ScoreFilesWithDeltas scores direct and tool-delta evidence per line.
// Match quality wins before recency in (ts, insert_seq, event_id) order.
// Delta groups establish alignment before competing for shared lines and
// cannot trigger modified-line inheritance. Unstamped direct evidence uses
// zero recency and a deterministic provider fallback.
func ScoreFilesWithDeltas(
	diff DiffResult,
	aiLines map[string]map[string]struct{},
	providerTouchedFiles map[string]string,
	fileProvider map[string]string,
	lineProviders map[string]map[string]map[string]struct{},
	lineStamps map[string]map[string][]LineStamp,
	deltaGroups map[string][]DeltaClaimGroup,
) ([]FileScore, MatchStats) {
	aiLinesNorm := BuildNormalizedSet(aiLines)
	lineProvidersNorm := BuildNormalizedLineProviders(lineProviders)
	lineStampsNorm := buildNormalizedStamps(lineStamps)

	var scores []FileScore
	var stats MatchStats

	for _, fd := range diff.Files {
		fs := FileScore{
			Path:                        fd.Path,
			ProviderLines:               make(map[string]int),
			ProviderOnlyLinesByProvider: make(map[string]int),
		}

		// Exact anchors are discovered independently before recency assigns
		// shared occurrences. Lost anchors keep their alignment bounds, and
		// any budget failure drops all line-level delta evidence for the file.
		groups := deltaGroups[fd.Path]
		flat := flattenGroups(fd.Groups)
		deltaCand := make([]*lineCandidate, len(flat))
		deltaContention := make([]bool, len(flat))
		haveDelta := false
		if len(groups) > 0 {
			order := make([]int, len(groups))
			for i := range order {
				order[i] = i
			}
			sort.SliceStable(order, func(a, b int) bool {
				ga, gb := groups[order[a]], groups[order[b]]
				return later(ga.Ts, ga.InsertSeq, ga.EventID, gb.Ts, gb.InsertSeq, gb.EventID)
			})

			refused := !diff.Complete
			remaining := alignBudget
			consumed := make([]bool, len(flat))
			// charge reserves table cells without overflow; spend accounts
			// for linear scans.
			charge := func(claims, lines int) bool {
				nc, na := claims+1, lines+1
				if nc > remaining/na {
					return false
				}
				remaining -= nc * na
				return true
			}
			spend := func(n int) bool {
				if n > remaining {
					return false
				}
				remaining -= n
				return true
			}
			record := func(g DeltaClaimGroup, tier, idx int) {
				q := qualityExact
				if tier == AlignNormalized {
					q = qualityNormalized
				}
				consumed[idx] = true
				deltaCand[idx] = &lineCandidate{
					quality: q, provider: g.Provider,
					ts: g.Ts, insertSeq: g.InsertSeq, eventID: g.EventID,
					fromDelta: true,
				}
			}

			type anchorPair struct{ claim, flat int }
			anchorsBy := make([][]anchorPair, len(groups))
			remainingBy := make([][]int, len(groups))
			lostBy := make([][]int, len(groups))

			// Discover exact anchors for each group.
			for gi := range groups {
				g := groups[gi]
				if !charge(len(g.Lines), len(flat)) {
					refused = true
					break
				}
				res, ok := AlignOrdered(NewClaimLines(g.Lines), flat)
				if !ok {
					refused = true
					break
				}
				matched := make([]bool, len(g.Lines))
				for j, a := range res {
					if a.Tier != AlignExact {
						continue
					}
					anchorsBy[gi] = append(anchorsBy[gi], anchorPair{a.ClaimIdx, j})
					matched[a.ClaimIdx] = true
				}
				for ci := range g.Lines {
					if !matched[ci] {
						remainingBy[gi] = append(remainingBy[gi], ci)
					}
				}
			}

			// Assign shared anchors by recency.
			if !refused {
				for _, gi := range order {
					g := groups[gi]
					for ai, ap := range anchorsBy[gi] {
						if !consumed[ap.flat] {
							record(g, AlignExact, ap.flat)
						} else {
							deltaContention[ap.flat] = true
							lostBy[gi] = append(lostBy[gi], ai)
						}
					}
				}
			}

			// Rebind lost anchors to unclaimed duplicate occurrences.
			for _, gi := range order {
				if refused {
					break
				}
				g := groups[gi]
				for _, ai := range lostBy[gi] {
					if refused {
						break
					}
					ap := anchorsBy[gi][ai]
					tr := strings.TrimSpace(g.Lines[ap.claim])
					if tr == "" {
						continue
					}
					lo := -1
					if ai > 0 {
						lo = anchorsBy[gi][ai-1].flat
					}
					hi := len(flat)
					if ai+1 < len(anchorsBy[gi]) {
						hi = anchorsBy[gi][ai+1].flat
					}
					for p := lo + 1; p < hi; p++ {
						if !spend(1) {
							refused = true
							break
						}
						if !consumed[p] && strings.TrimSpace(flat[p]) == tr {
							record(g, AlignExact, p)
							anchorsBy[gi][ai] = anchorPair{ap.claim, p}
							break
						}
					}
				}
			}

			// Align remaining claims within the anchor bounds.
			for _, gi := range order {
				if refused {
					break
				}
				g := groups[gi]
				rem := remainingBy[gi]
				if len(rem) == 0 {
					continue
				}
				bounds := make([]anchorPair, 0, len(anchorsBy[gi])+2)
				bounds = append(bounds, anchorPair{-1, -1})
				bounds = append(bounds, anchorsBy[gi]...)
				bounds = append(bounds, anchorPair{len(g.Lines), len(flat)})
				left := rem[:0:0]
				ri := 0
				for bi := 0; bi+1 < len(bounds) && !refused; bi++ {
					cLo, pLo := bounds[bi].claim, bounds[bi].flat
					cHi, pHi := bounds[bi+1].claim, bounds[bi+1].flat
					// Both slices are ordered, so each claim is visited once.
					var segClaimIdx []int
					var segClaims []string
					for ri < len(rem) && rem[ri] > cLo && rem[ri] < cHi {
						segClaimIdx = append(segClaimIdx, rem[ri])
						segClaims = append(segClaims, g.Lines[rem[ri]])
						ri++
					}
					if len(segClaims) == 0 {
						continue
					}
					var segAvail []string
					var segMap []int
					for p := pLo + 1; p < pHi; p++ {
						if !spend(1) {
							refused = true
							break
						}
						if !consumed[p] {
							segAvail = append(segAvail, flat[p])
							segMap = append(segMap, p)
						}
					}
					if refused {
						break
					}
					if len(segAvail) == 0 {
						left = append(left, segClaimIdx...)
						continue
					}
					if !charge(len(segClaims), len(segAvail)) {
						refused = true
						break
					}
					res, ok := AlignOrdered(NewClaimLines(segClaims), segAvail)
					if !ok {
						refused = true
						break
					}
					segMatched := make([]bool, len(segClaims))
					for j, a := range res {
						if a.Tier == AlignNone {
							continue
						}
						record(g, a.Tier, segMap[j])
						segMatched[a.ClaimIdx] = true
					}
					for k, ci := range segClaimIdx {
						if !segMatched[k] {
							left = append(left, ci)
						}
					}
				}
				remainingBy[gi] = left
			}

			// Mark distinct contested occurrences within each anchor range.
			for gi := range groups {
				if refused {
					break
				}
				g := groups[gi]
				rem := remainingBy[gi]
				if len(rem) == 0 {
					continue
				}
				bounds := make([]anchorPair, 0, len(anchorsBy[gi])+2)
				bounds = append(bounds, anchorPair{-1, -1})
				bounds = append(bounds, anchorsBy[gi]...)
				bounds = append(bounds, anchorPair{len(g.Lines), len(flat)})
				ri := 0
				for bi := 0; bi+1 < len(bounds) && !refused; bi++ {
					cLo, pLo := bounds[bi].claim, bounds[bi].flat
					cHi, pHi := bounds[bi+1].claim, bounds[bi+1].flat
					cursor := pLo
					for ri < len(rem) && rem[ri] > cLo && rem[ri] < cHi && !refused {
						ci := rem[ri]
						ri++
						tr := strings.TrimSpace(g.Lines[ci])
						if tr == "" {
							continue
						}
						norm := NormalizeWhitespace(tr)
						found := -1
						normFound := -1
						for p := cursor + 1; p < pHi; p++ {
							if !spend(1) {
								refused = true
								break
							}
							if deltaCand[p] == nil || deltaContention[p] {
								continue
							}
							lineTr := strings.TrimSpace(flat[p])
							if lineTr == tr {
								found = p
								break
							}
							if normFound < 0 && NormalizeWhitespace(lineTr) == norm {
								normFound = p
							}
						}
						if found < 0 {
							found = normFound
						}
						if found >= 0 {
							deltaContention[found] = true
							cursor = found
						}
					}
				}
			}

			if refused {
				deltaCand = make([]*lineCandidate, len(flat))
				for i := range deltaContention {
					deltaContention[i] = false
				}
				fs.DeltaAlignmentRefused = true
				stats.DeltaAlignmentsRefused++
			} else {
				haveDelta = true
			}
		}
		// Refused alignments retain file-level evidence under the first
		// claim group's provider.
		refusedProvider := ""
		if fs.DeltaAlignmentRefused && len(groups) > 0 {
			refusedProvider = groups[0].Provider
		}

		provider, isProviderFile := providerTouchedFiles[fd.Path]
		isProviderOnly := isProviderFile && aiLines[fd.Path] == nil && !haveDelta
		if isProviderOnly {
			creditProvider := provider
			if refusedProvider != "" {
				creditProvider = refusedProvider
			}
			for _, group := range fd.Groups {
				for _, line := range group.Lines {
					trimmed := strings.TrimSpace(line)
					if trimmed == "" {
						continue
					}
					fs.TotalLines++
					fs.ProviderOnlyLines++
					fs.ProviderOnlyLinesByProvider[creditProvider]++
					stats.ProviderOnlyMatches++
				}
			}
			scores = append(scores, fs)
			continue
		}

		prov := fileProvider[fd.Path]
		if prov == "" && isProviderFile {
			prov = provider
		}

		// Emit every direct witness, or one deterministic unstamped fallback.
		directCandidates := func(cands []lineCandidate, key string, stamps map[string][]LineStamp, provs map[string]map[string]struct{}, quality int) []lineCandidate {
			if witnesses := stamps[key]; len(witnesses) > 0 {
				for _, s := range witnesses {
					cands = append(cands, lineCandidate{
						quality: quality, provider: s.Provider,
						ts: s.Ts, insertSeq: s.InsertSeq, eventID: s.EventID,
					})
				}
				return cands
			}
			best := ""
			for p := range provs[key] {
				if p > best {
					best = p
				}
			}
			if best == "" {
				best = prov
			}
			if best == "" {
				return cands
			}
			return append(cands, lineCandidate{quality: quality, provider: best})
		}

		flatIdx := 0
		for _, group := range fd.Groups {
			type lc struct {
				winner    lineCandidate
				hasWinner bool
				contested bool
				trimmed   string
				norm      string
			}
			var classes []lc
			hasDirectOverlap := false

			var hunkProviders map[string]struct{}
			addHunkProviders := func(provs map[string]struct{}) {
				if len(provs) == 0 {
					return
				}
				if hunkProviders == nil {
					hunkProviders = make(map[string]struct{}, len(provs))
				}
				for p := range provs {
					hunkProviders[p] = struct{}{}
				}
			}

			for _, line := range group.Lines {
				lineFlat := flatIdx
				flatIdx++
				trimmed := strings.TrimSpace(line)
				if trimmed == "" {
					continue
				}
				c := lc{trimmed: trimmed, norm: NormalizeWhitespace(trimmed)}

				var cands []lineCandidate
				if fileSet, ok := aiLines[fd.Path]; ok {
					if _, found := fileSet[trimmed]; found {
						cands = directCandidates(cands, trimmed, lineStamps[fd.Path], lineProviders[fd.Path], qualityExact)
						// Add normalized witnesses not represented by exact matches.
						exact := lineStamps[fd.Path][trimmed]
						for _, s := range lineStampsNorm[fd.Path][c.norm] {
							if containsStamp(exact, s) {
								continue
							}
							cands = append(cands, lineCandidate{
								quality: qualityNormalized, provider: s.Provider,
								ts: s.Ts, insertSeq: s.InsertSeq, eventID: s.EventID,
							})
						}
						hasDirectOverlap = true
						if perLine, ok := lineProviders[fd.Path]; ok {
							addHunkProviders(perLine[trimmed])
						}
					} else if normSet, ok := aiLinesNorm[fd.Path]; ok {
						if _, found := normSet[c.norm]; found {
							cands = directCandidates(cands, c.norm, lineStampsNorm[fd.Path], lineProvidersNorm[fd.Path], qualityNormalized)
							hasDirectOverlap = true
							if perLine, ok := lineProvidersNorm[fd.Path]; ok {
								addHunkProviders(perLine[c.norm])
							}
						}
					}
				}
				if dc := deltaCand[lineFlat]; dc != nil {
					cands = append(cands, *dc)
				}
				deltaContested := deltaContention[lineFlat]

				if len(cands) > 0 {
					c.winner = cands[0]
					for _, cand := range cands[1:] {
						if cand.quality > c.winner.quality ||
							(cand.quality == c.winner.quality &&
								later(cand.ts, cand.insertSeq, cand.eventID,
									c.winner.ts, c.winner.insertSeq, c.winner.eventID)) {
							c.winner = cand
						}
					}
					c.hasWinner = true
					c.contested = len(cands) > 1 || deltaContested
				}
				classes = append(classes, c)
			}

			for _, c := range classes {
				fs.TotalLines++
				switch {
				case c.hasWinner && c.winner.quality == qualityExact:
					fs.ExactLines++
					stats.ExactMatches++
					if c.winner.fromDelta {
						fs.DeltaExactLines++
						stats.DeltaExactMatches++
					}
					fs.ProviderLines[c.winner.provider]++
				case c.hasWinner:
					fs.FormattedLines++
					stats.NormalizedMatches++
					if c.winner.fromDelta {
						fs.DeltaFormattedLines++
						stats.DeltaNormalizedMatches++
					}
					fs.ProviderLines[c.winner.provider]++
				case hasDirectOverlap:
					// Only direct evidence can anchor modified-line fallback.
					fs.ModifiedLines++
					stats.ModifiedMatches++
					if len(hunkProviders) > 0 {
						for p := range hunkProviders {
							fs.ProviderLines[p]++
						}
					} else if prov != "" {
						fs.ProviderLines[prov]++
					}
				case refusedProvider != "":
					fs.ProviderOnlyLines++
					fs.ProviderOnlyLinesByProvider[refusedProvider]++
					stats.ProviderOnlyMatches++
				default:
					fs.HumanLines++
				}
				if c.contested {
					fs.ContestedLines++
					stats.ContestedLines++
				}
			}
		}

		scores = append(scores, fs)
	}

	return scores, stats
}

// containsStamp reports whether s is one of the witnesses.
func containsStamp(witnesses []LineStamp, s LineStamp) bool {
	for _, w := range witnesses {
		if w == s {
			return true
		}
	}
	return false
}

// buildNormalizedStamps groups witnesses by normalized line content.
// Winner selection is independent of merge order.
func buildNormalizedStamps(stamps map[string]map[string][]LineStamp) map[string]map[string][]LineStamp {
	if stamps == nil {
		return nil
	}
	out := make(map[string]map[string][]LineStamp, len(stamps))
	for file, byLine := range stamps {
		norm := make(map[string][]LineStamp, len(byLine))
		for line, witnesses := range byLine {
			key := NormalizeWhitespace(line)
			norm[key] = append(norm[key], witnesses...)
		}
		out[file] = norm
	}
	return out
}
