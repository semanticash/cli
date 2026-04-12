package reporting

import (
	"fmt"
	"strings"
)

// RenderDiagnosticNote produces a human-readable diagnostic note explaining
// why a particular AI percentage was computed. When AI% is 0 it identifies
// which pipeline stage had no data. When AI% > 0 and non-exact matches
// contributed, it breaks down the match tiers.
func RenderDiagnosticNote(in DiagnosticsInput) string {
	if in.AIPercent == 0 {
		switch {
		case in.EventStats.EventsConsidered == 0:
			return "No agent events found in the delta window between checkpoints."
		case in.EventStats.AIToolEvents == 0:
			return "Agent events found but none contained file-modifying tool calls (Edit/Write)."
		case in.EventStats.PayloadsLoaded == 0:
			return "Agent tool calls found but payloads could not be loaded from blob store."
		default:
			return "Agent tool calls found but no added lines in the commit matched AI-produced output."
		}
	}

	if in.MatchStats.NormalizedMatches > 0 || in.MatchStats.ModifiedMatches > 0 {
		parts := []string{fmt.Sprintf("%d exact", in.MatchStats.ExactMatches)}
		if in.MatchStats.NormalizedMatches > 0 {
			parts = append(parts, fmt.Sprintf("%d normalized", in.MatchStats.NormalizedMatches))
		}
		if in.MatchStats.ModifiedMatches > 0 {
			parts = append(parts, fmt.Sprintf("%d modified", in.MatchStats.ModifiedMatches))
		}
		return fmt.Sprintf("AI matches: %s.", strings.Join(parts, ", "))
	}

	return ""
}
