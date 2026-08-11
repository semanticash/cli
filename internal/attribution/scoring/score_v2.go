package scoring

import "strings"

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
// Match quality wins first, followed by recency in
// (ts, insert_seq, event_id) order. Evidence source does not affect rank.
// Delta groups align independently and cannot trigger modified-line
// inheritance. Missing direct stamps use zero recency and a deterministic
// provider fallback.
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

		// Refuse all delta line evidence for the file if any group cannot
		// be aligned within the shared budget.
		groups := deltaGroups[fd.Path]
		flat := flattenGroups(fd.Groups)
		aligned := make([][]AlignedLine, 0, len(groups))
		haveDelta := false
		if len(groups) > 0 {
			refused := !diff.Complete
			remaining := alignBudget
			if !refused {
				for _, g := range groups {
					// Check nc*na without overflowing.
					nc, na := len(g.Lines)+1, len(flat)+1
					if nc > remaining/na {
						refused = true
						break
					}
					remaining -= nc * na
					res, ok := AlignOrdered(NewClaimLines(g.Lines), flat)
					if !ok {
						refused = true
						break
					}
					aligned = append(aligned, res)
				}
			}
			if refused {
				aligned = nil
				fs.DeltaAlignmentRefused = true
				stats.DeltaAlignmentsRefused++
			} else {
				haveDelta = true
			}
		}

		provider, isProviderFile := providerTouchedFiles[fd.Path]
		isProviderOnly := isProviderFile && aiLines[fd.Path] == nil && !haveDelta
		if isProviderOnly {
			for _, group := range fd.Groups {
				for _, line := range group.Lines {
					trimmed := strings.TrimSpace(line)
					if trimmed == "" {
						continue
					}
					fs.TotalLines++
					fs.ProviderOnlyLines++
					fs.ProviderOnlyLinesByProvider[provider]++
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

		// Emit every direct witness. Without stamps, use one zero-recency
		// candidate from the line or file provider fallback.
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
						// Include whitespace-only competitors not already
						// represented by the exact witnesses.
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
				for gi, res := range aligned {
					if res == nil {
						continue
					}
					if a := res[lineFlat]; a.Tier != AlignNone {
						q := qualityExact
						if a.Tier == AlignNormalized {
							q = qualityNormalized
						}
						g := groups[gi]
						cands = append(cands, lineCandidate{
							quality: q, provider: g.Provider,
							ts: g.Ts, insertSeq: g.InsertSeq, eventID: g.EventID,
							fromDelta: true,
						})
					}
				}

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
					c.contested = len(cands) > 1
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
