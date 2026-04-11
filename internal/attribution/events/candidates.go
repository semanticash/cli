package events

import "strings"

// BuildCandidatesFromRows extracts AI candidate data from event rows.
// It processes events in order, building the candidate maps and collecting
// diagnostic stats. Callers provide any pre-loaded payload bytes in each
// EventRow.
//
// When eligibleFiles is non-nil, only files in the set contribute to
// candidate maps (used by carry-forward gating).
func BuildCandidatesFromRows(rows []EventRow, repoRoot string, eligibleFiles map[string]bool) (Candidates, EventStats) {
	c := Candidates{
		AILines:              make(map[string]map[string]struct{}),
		ProviderTouchedFiles: make(map[string]string),
		FileProvider:         make(map[string]string),
		ProviderModel:        make(map[string]string),
	}

	var stats EventStats
	stats.EventsConsidered = len(rows)

	for _, ev := range rows {
		if ev.Model != "" {
			c.ProviderModel[ev.Provider] = ev.Model
		}

		// Provider file-touch events (Cursor, Copilot, Kiro, Gemini).
		if HasProviderFileEdit(ev.ToolUses) {
			stats.AIToolEvents++
			for _, fp := range ExtractProviderFileTouches(ev.ToolUses) {
				if eligibleFiles != nil && !eligibleFiles[fp] {
					continue
				}
				c.ProviderTouchedFiles[fp] = ev.Provider
			}
			continue
		}

		if ev.Role != "assistant" {
			continue
		}
		stats.EventsAssistant++

		if ev.PayloadHash == "" {
			continue
		}

		hasBash := strings.Contains(ev.ToolUses, `"Bash"`)
		if !HasEditOrWrite(ev.ToolUses) && !hasBash {
			continue
		}
		stats.AIToolEvents++

		if ev.Payload == nil {
			continue
		}
		stats.PayloadsLoaded++

		fileLines, bashCommands := ExtractClaudeActions(ev.Payload, repoRoot)
		for fp, lines := range fileLines {
			if eligibleFiles != nil && !eligibleFiles[fp] {
				continue
			}
			c.ProviderTouchedFiles[fp] = ev.Provider // touched for file-level tracking
			c.FileProvider[fp] = ev.Provider
			if c.AILines[fp] == nil {
				c.AILines[fp] = make(map[string]struct{})
			}
			for line := range lines {
				c.AILines[fp][line] = struct{}{}
			}
		}

		// Deleted paths from bash commands contribute to touched-files.
		for _, cmd := range bashCommands {
			for _, fp := range ExtractDeletedPaths(cmd, repoRoot) {
				if eligibleFiles != nil && !eligibleFiles[fp] {
					continue
				}
				c.ProviderTouchedFiles[fp] = ev.Provider
			}
		}
	}

	return c, stats
}
