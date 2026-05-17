package scoring

import "strings"

// ScoreFiles matches AI candidate maps against a parsed diff and returns
// per-file scores with match statistics.
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
			Path:                       fd.Path,
			ProviderLines:              make(map[string]int),
			ProviderOnlyLinesByProvider: make(map[string]int),
		}

		provider, isProviderFile := providerTouchedFiles[fd.Path]
		isProviderOnly := isProviderFile && aiLines[fd.Path] == nil
		if isProviderOnly {
			// Provider-only: the AI session touched the file but we
			// have no line-level payload to match against. Counted
			// in ProviderOnlyLines so the headline AI% (which sums
			// exact + formatted + modified) does not inflate on
			// thin evidence. The file still carries a provider
			// attribution for the breakdown.
			for _, group := range fd.Groups {
				for _, line := range group.Lines {
					trimmed := strings.TrimSpace(line)
					if trimmed == "" {
						continue
					}
					fs.TotalLines++
					fs.ProviderOnlyLines++
					// Per-provider sidecar: kept separate from
					// ProviderLines so consumers can render the
					// breakdown as "claude: 4 line-level, cursor:
					// 2 provider-only" without conflating the two.
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
			// at credit time. trimmed feeds the exact (tier 1) and
			// modified (tier 3) per-line provider lookup; norm feeds
			// the tier-2 lookup against the whitespace-normalized
			// projection.
			type lc struct {
				tier    int
				trimmed string
				norm    string
			}
			var classes []lc
			hasOverlap := false

			// hunkProviders unions the providers that own every
			// tier-1 and tier-2 matched line in this group. Tier-3
			// (modified) lines have no direct line-set match, so
			// they get credited to whichever provider(s) own the
			// matched-tier neighbours that anchored the overlap.
			// Without this, a mixed-provider file collapses
			// modified-tier lines onto the per-file fileProvider -
			// which is last-writer-wins and disagrees with the rest
			// of the per-line attribution model.
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
				// Tier 1/2: credit the matched line's providers
				// directly. Multiple providers that emitted the
				// same line each get +1 (involvement, not
				// exclusive ownership).
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
					// Modified-tier: credit the hunk's matched-tier
					// providers so this line's attribution aligns
					// with the neighbours that earned the overlap.
					if len(hunkProviders) > 0 {
						for p := range hunkProviders {
							fs.ProviderLines[p]++
						}
						return
					}
				}
				// Fallback for: tier 1/2 without per-line provenance,
				// or tier-3 lines in a hunk that has none either
				// (older candidates built before LineProviders, or
				// provider-touch fallback paths). Credit the per-
				// file provider so existing callers keep working.
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
