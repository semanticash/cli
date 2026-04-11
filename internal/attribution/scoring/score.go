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
func ScoreFiles(
	diff DiffResult,
	aiLines map[string]map[string]struct{},
	providerTouchedFiles map[string]string,
	fileProvider map[string]string,
) ([]FileScore, MatchStats) {
	aiLinesNorm := BuildNormalizedSet(aiLines)

	var scores []FileScore
	var stats MatchStats

	for _, fd := range diff.Files {
		fs := FileScore{
			Path:          fd.Path,
			ProviderLines: make(map[string]int),
		}

		provider, isProviderFile := providerTouchedFiles[fd.Path]
		isProviderOnly := isProviderFile && aiLines[fd.Path] == nil
		if isProviderOnly {
			for _, group := range fd.Groups {
				for _, line := range group.Lines {
					trimmed := strings.TrimSpace(line)
					if trimmed == "" {
						continue
					}
					fs.TotalLines++
					fs.ModifiedLines++
					fs.ProviderLines[provider]++
					stats.ModifiedMatches++
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
			type lc struct{ tier int }
			var classes []lc
			hasOverlap := false

			for _, line := range group.Lines {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" {
					continue
				}
				c := lc{}
				if fileSet, ok := aiLines[fd.Path]; ok {
					if _, found := fileSet[trimmed]; found {
						c.tier = 1
						hasOverlap = true
					}
				}
				if c.tier == 0 {
					norm := NormalizeWhitespace(trimmed)
					if normSet, ok := aiLinesNorm[fd.Path]; ok {
						if _, found := normSet[norm]; found {
							c.tier = 2
							hasOverlap = true
						}
					}
				}
				classes = append(classes, c)
			}

			for _, c := range classes {
				fs.TotalLines++
				switch {
				case c.tier == 1:
					fs.ExactLines++
					fs.ProviderLines[prov]++
					stats.ExactMatches++
				case c.tier == 2:
					fs.FormattedLines++
					fs.ProviderLines[prov]++
					stats.NormalizedMatches++
				case c.tier == 0 && hasOverlap:
					fs.ModifiedLines++
					fs.ProviderLines[prov]++
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
