package scoring

import "strings"

// DeltaClaimGroup contains one delta group's ordered claims and recency.
type DeltaClaimGroup struct {
	Provider  string
	Ts        int64
	InsertSeq int64
	EventID   string
	Lines     []string
	// Historical marks a carry-forward claim for a modified file.
	Historical bool
}

// LineStamp identifies one direct witness for a line.
type LineStamp struct {
	Provider  string
	Ts        int64
	InsertSeq int64
	EventID   string
}

// later compares recency by (ts, insert_seq, event_id).
func later(aTs, aSeq int64, aID string, bTs, bSeq int64, bID string) bool {
	if aTs != bTs {
		return aTs > bTs
	}
	if aSeq != bSeq {
		return aSeq > bSeq
	}
	return aID > bID
}

// ScoreFiles matches AI candidate maps against a parsed diff and returns
// per-file scores with match statistics. This is the v1 scorer and its
// behavior is frozen.
//
// Parameters are plain maps. Callers unpack candidate data into these maps.
//
// Matching is three-tier:
//   - Tier 1 (exact): trimmed line matches AI output exactly
//   - Tier 2 (formatted): matches after whitespace normalization
//   - Tier 3 (modified): in a contiguous group with tier 1 or 2 overlap
//
// lineProviders is the per-line ownership map (file -> line ->
// providers that emitted the line). When set, each matched diff line
// credits every provider that contributed it, so ProviderLines
// reflects per-line evidence rather than a per-file "last writer
// wins" assignment. When lineProviders is nil (older callers or
// candidates built without per-line tracking), the scorer falls back
// to the per-file fileProvider value for every matched line.
func ScoreFiles(
	diff DiffResult,
	aiLines map[string]map[string]struct{},
	providerTouchedFiles map[string]string,
	fileProvider map[string]string,
	lineProviders map[string]map[string]map[string]struct{},
) ([]FileScore, MatchStats) {
	aiLinesNorm := BuildNormalizedSet(aiLines)
	lineProvidersNorm := BuildNormalizedLineProviders(lineProviders)

	var scores []FileScore
	var stats MatchStats

	for _, fd := range diff.Files {
		fs := FileScore{
			Path:                        fd.Path,
			ProviderLines:               make(map[string]int),
			ProviderOnlyLinesByProvider: make(map[string]int),
		}

		provider, isProviderFile := providerTouchedFiles[fd.Path]
		isProviderOnly := isProviderFile && aiLines[fd.Path] == nil
		if isProviderOnly {
			// Keep file-touch evidence outside the headline line count.
			for _, group := range fd.Groups {
				for _, line := range group.Lines {
					trimmed := strings.TrimSpace(line)
					if trimmed == "" {
						continue
					}
					fs.TotalLines++
					fs.ProviderOnlyLines++
					// Keep touch-only counts separate from line evidence.
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

		for _, group := range fd.Groups {
			// classes carries the per-line decision plus the lookup
			// keys needed to attribute that line to its provider(s)
			// at credit time.
			type lc struct {
				tier    int
				trimmed string
				norm    string
			}
			var classes []lc
			hasOverlap := false

			// hunkProviders unions the providers that own every
			// tier-1 and tier-2 matched line in this group, crediting
			// tier-3 (modified) lines consistently with the matched
			// neighbours that anchored the overlap.
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
				trimmed := strings.TrimSpace(line)
				if trimmed == "" {
					continue
				}
				c := lc{trimmed: trimmed}
				if fileSet, ok := aiLines[fd.Path]; ok {
					if _, found := fileSet[trimmed]; found {
						c.tier = 1
						hasOverlap = true
						if perLine, ok := lineProviders[fd.Path]; ok {
							addHunkProviders(perLine[trimmed])
						}
					}
				}
				if c.tier == 0 {
					c.norm = NormalizeWhitespace(trimmed)
					if normSet, ok := aiLinesNorm[fd.Path]; ok {
						if _, found := normSet[c.norm]; found {
							c.tier = 2
							hasOverlap = true
							if perLine, ok := lineProvidersNorm[fd.Path]; ok {
								addHunkProviders(perLine[c.norm])
							}
						}
					}
				}
				classes = append(classes, c)
			}

			creditProviders := func(c lc) {
				switch c.tier {
				case 1:
					if perLine, ok := lineProviders[fd.Path]; ok {
						if provs, found := perLine[c.trimmed]; found && len(provs) > 0 {
							for p := range provs {
								fs.ProviderLines[p]++
							}
							return
						}
					}
				case 2:
					if perLine, ok := lineProvidersNorm[fd.Path]; ok {
						if provs, found := perLine[c.norm]; found && len(provs) > 0 {
							for p := range provs {
								fs.ProviderLines[p]++
							}
							return
						}
					}
				case 0:
					if len(hunkProviders) > 0 {
						for p := range hunkProviders {
							fs.ProviderLines[p]++
						}
						return
					}
				}
				if prov != "" {
					fs.ProviderLines[prov]++
				}
			}

			for _, c := range classes {
				fs.TotalLines++
				switch {
				case c.tier == 1:
					fs.ExactLines++
					creditProviders(c)
					stats.ExactMatches++
				case c.tier == 2:
					fs.FormattedLines++
					creditProviders(c)
					stats.NormalizedMatches++
				case c.tier == 0 && hasOverlap:
					fs.ModifiedLines++
					creditProviders(c)
					stats.ModifiedMatches++
				default:
					fs.HumanLines++
				}
			}
		}

		scores = append(scores, fs)
	}

	return scores, stats
}

// flattenGroups concatenates added lines in diff order.
func flattenGroups(groups []AddedGroup) []string {
	n := 0
	for _, g := range groups {
		n += len(g.Lines)
	}
	out := make([]string, 0, n)
	for _, g := range groups {
		out = append(out, g.Lines...)
	}
	return out
}
