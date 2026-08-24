package events

import "strings"

// BuildCandidatesFromRows extracts attribution candidates from event rows.
// A non-nil eligibleFiles map limits which files contribute evidence.
func BuildCandidatesFromRows(rows []EventRow, repoRoot string, eligibleFiles map[string]bool) (Candidates, EventStats) {
	c := Candidates{
		AILines:              make(map[string]map[string]struct{}),
		LineProviders:        make(map[string]map[string]map[string]struct{}),
		LineStamps:           make(map[string]map[string][]LineStamp),
		ProviderTouchedFiles: make(map[string]string),
		FileProvider:         make(map[string]string),
		ProviderModel:        make(map[string]string),
		InferredDeletions:    make(map[string]string),
		ExplicitTouches:      make(map[string]LineStamp),
	}

	var stats EventStats
	stats.EventsConsidered = len(rows)

	// Keep the latest explicit touch available if an inferred deletion
	// temporarily replaces the file-level provider.
	touch := func(fp string, s LineStamp) {
		if cur, ok := c.ExplicitTouches[fp]; !ok || stampLater(s, cur) {
			c.ExplicitTouches[fp] = s
			c.ProviderTouchedFiles[fp] = s.Provider
		}
	}

	for _, ev := range rows {
		if ev.Model != "" {
			c.ProviderModel[ev.Provider] = ev.Model
		}

		// Provider file-touch events (Cursor, Copilot, Kiro, Gemini).
		if HasProviderFileEdit(ev.ToolUses) {
			stats.AIToolEvents++
			for _, fp := range ExtractProviderFileTouches(ev.ToolUses, repoRoot) {
				if eligibleFiles != nil && !eligibleFiles[fp] {
					continue
				}
				touch(fp, LineStamp{Provider: ev.Provider, Ts: ev.Ts, InsertSeq: ev.InsertSeq, EventID: ev.EventID})
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

		actions := extractClaudeActions(ev.Payload, repoRoot)
		stamp := LineStamp{Provider: ev.Provider, Ts: ev.Ts, InsertSeq: ev.InsertSeq, EventID: ev.EventID}
		for fp := range actions.fileTouches {
			if eligibleFiles != nil && !eligibleFiles[fp] {
				continue
			}
			touch(fp, stamp)
			c.FileProvider[fp] = ev.Provider
		}
		for fp, lines := range actions.fileLines {
			if eligibleFiles != nil && !eligibleFiles[fp] {
				continue
			}
			if c.AILines[fp] == nil {
				c.AILines[fp] = make(map[string]struct{})
			}
			if c.LineProviders[fp] == nil {
				c.LineProviders[fp] = make(map[string]map[string]struct{})
			}
			if c.LineStamps[fp] == nil {
				c.LineStamps[fp] = make(map[string][]LineStamp)
			}
			for line := range lines {
				c.AILines[fp][line] = struct{}{}
				if c.LineProviders[fp][line] == nil {
					c.LineProviders[fp][line] = make(map[string]struct{})
				}
				c.LineProviders[fp][line][ev.Provider] = struct{}{}
				// Preserve every witness for deterministic winner selection.
				c.LineStamps[fp][line] = append(c.LineStamps[fp][line], stamp)
			}
		}

		// A verified delta deletion can supersede this command inference.
		for _, cmd := range actions.bashCommands {
			for _, fp := range ExtractDeletedPaths(cmd, repoRoot) {
				if eligibleFiles != nil && !eligibleFiles[fp] {
					continue
				}
				c.ProviderTouchedFiles[fp] = ev.Provider
				c.InferredDeletions[fp] = ev.Provider
			}
		}
	}

	return c, stats
}

// stampLater reports whether a beats b by (Ts, InsertSeq, EventID).
func stampLater(a, b LineStamp) bool {
	if a.Ts != b.Ts {
		return a.Ts > b.Ts
	}
	if a.InsertSeq != b.InsertSeq {
		return a.InsertSeq > b.InsertSeq
	}
	return a.EventID > b.EventID
}
