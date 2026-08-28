package service

import "sort"

// selectModifiedCarryForwardPaths returns approved paths with historical AI lines
// and no current AI lines. Invalid scores and provider-only evidence are ignored.
func selectModifiedCarryForwardPaths(approved map[string]bool, current, historical map[string]*fileScore) []string {
	var out []string
	for path, ok := range approved {
		if !ok || path == "" {
			continue
		}
		if cur, seen := current[path]; seen {
			// A malformed current score cannot establish an attribution gap.
			if cur == nil || cur.path != path {
				continue
			}
			if fileScoreAILines(cur) > 0 {
				continue
			}
		}
		hist, seen := historical[path]
		if !seen || hist == nil || hist.path != path || fileScoreAILines(hist) == 0 {
			continue
		}
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}
