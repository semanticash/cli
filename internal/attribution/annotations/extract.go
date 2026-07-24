package annotations

import (
	"encoding/json"
	"strings"
	"unicode"

	"github.com/semanticash/cli/internal/attribution/events"
)

// minSubstantiveLen is the shortest trimmed line length the overlap logic
// considers meaningful. It filters out bare braces, single operators, and
// other incidental lines that would otherwise produce coincidental overlaps.
const minSubstantiveLen = 4

// fileEdit holds the substantive lines a single agent event authored and
// replaced in one file. Lines are trimmed and de-duplicated into sets so
// overlap is a set intersection, matching how the attribution candidate
// path already keys AI lines.
type fileEdit struct {
	added    map[string]struct{} // substantive lines authored (new_string or content)
	replaced map[string]struct{} // substantive lines removed (Edit old_string)
}

// eventEdits is the per-event projection the detector works over: the file
// edits it carried, the files it touched (including provider-only touches),
// and any files it removed through a recognized deletion.
type eventEdits struct {
	ev      Event
	edits   map[string]*fileEdit // repo-relative path -> edit
	touched map[string]struct{}  // repo-relative paths the event touched
	removed map[string]struct{}  // repo-relative paths removed via recognized rm
}

// projectEvents turns raw events into per-event edits. Events without
// recognizable file activity contribute nothing and are dropped, which keeps
// unknown tool shapes unclassified by construction.
func projectEvents(in DetectInput) []eventEdits {
	out := make([]eventEdits, 0, len(in.Events))
	for _, ev := range in.Events {
		e := eventEdits{
			ev:      ev,
			edits:   make(map[string]*fileEdit),
			touched: make(map[string]struct{}),
			removed: make(map[string]struct{}),
		}

		// Provider file-touch events report paths without line-level payloads;
		// they count as touches only.
		if events.HasProviderFileEdit(ev.ToolUses) {
			for _, fp := range events.ExtractProviderFileTouches(ev.ToolUses, in.RepoRoot) {
				e.touched[fp] = struct{}{}
			}
		}

		// Claude-shaped assistant payloads carry line content, including the
		// Edit old_string needed for rework detection.
		if ev.Role == "assistant" && len(ev.Payload) > 0 {
			edits, bash := extractClaudeEdits(ev.Payload, in.RepoRoot)
			for fp, fe := range edits {
				e.edits[fp] = fe
				e.touched[fp] = struct{}{}
			}
			for _, cmd := range bash {
				for _, fp := range events.ExtractDeletedPaths(cmd, in.RepoRoot) {
					e.touched[fp] = struct{}{}
					e.removed[fp] = struct{}{}
				}
			}
		}

		// Some providers store replaced content in provenance instead of the
		// payload. Recognized provenance shapes add replaced lines only for
		// this event's own file.
		if len(ev.ProvenanceBlob) > 0 {
			mergeCanonicalProvenanceEdits(&e, ev, in.RepoRoot)
		}

		if len(e.touched) == 0 {
			continue
		}
		out = append(out, e)
	}
	return out
}

// canonicalProvenance is the Codex apply_patch canonical provenance shape:
// one shared multi-file blob per patch envelope, referenced by every
// per-file event the envelope produced.
type canonicalProvenance struct {
	Version int `json:"version"`
	Files   []struct {
		Path      string  `json:"path"`
		Operation string  `json:"operation"`
		OldText   *string `json:"old_text"`
		NewText   *string `json:"new_text"`
	} `json:"files"`
}

// mergeCanonicalProvenanceEdits reads replaced-content evidence from a Codex
// apply_patch canonical provenance blob. It is intentionally narrow:
//
//   - unknown shapes are ignored;
//   - only operation "edit" entries contribute old_text;
//   - entries are filtered to this event's file path, because one patch blob
//     can be shared by several per-file events.
func mergeCanonicalProvenanceEdits(e *eventEdits, ev Event, repoRoot string) {
	var cp canonicalProvenance
	if err := json.Unmarshal(ev.ProvenanceBlob, &cp); err != nil {
		return
	}
	if cp.Version != 1 || len(cp.Files) == 0 {
		return
	}

	// The event's own file paths, from its tool_uses record.
	eventPaths := make(map[string]struct{})
	for _, fp := range events.ExtractProviderFileTouches(ev.ToolUses, repoRoot) {
		eventPaths[fp] = struct{}{}
	}
	if len(eventPaths) == 0 {
		return
	}

	for _, f := range cp.Files {
		if f.Operation != "edit" || f.OldText == nil || *f.OldText == "" {
			continue
		}
		rel := events.NormalizePath(f.Path, repoRoot)
		if _, ok := eventPaths[rel]; !ok {
			continue
		}
		fe := e.edits[rel]
		if fe == nil {
			fe = &fileEdit{added: make(map[string]struct{}), replaced: make(map[string]struct{})}
			e.edits[rel] = fe
		}
		for _, line := range strings.Split(*f.OldText, "\n") {
			t := strings.TrimSpace(line)
			if substantive(t) {
				fe.replaced[t] = struct{}{}
			}
		}
		e.touched[rel] = struct{}{}
	}
}

// extractClaudeEdits parses a Claude-shaped assistant payload into per-file
// authored and replaced substantive line sets, plus raw bash commands. It
// mirrors events.ExtractClaudeActions but additionally captures the Edit
// old_string used for rework detection.
func extractClaudeEdits(raw []byte, repoRoot string) (map[string]*fileEdit, []string) {
	edits := make(map[string]*fileEdit)
	var bash []string

	var payload struct {
		Type    string `json:"type"`
		Message struct {
			Content []json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return edits, bash
	}
	if payload.Type != "assistant" {
		return edits, bash
	}

	for _, blockRaw := range payload.Message.Content {
		var block struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(blockRaw, &block); err != nil {
			continue
		}
		if block.Type != "tool_use" {
			continue
		}

		switch block.Name {
		case "Edit":
			var inp struct {
				FilePath  string `json:"file_path"`
				OldString string `json:"old_string"`
				NewString string `json:"new_string"`
			}
			if err := json.Unmarshal(block.Input, &inp); err != nil {
				continue
			}
			rel := events.NormalizePath(inp.FilePath, repoRoot)
			addSubstantive(edits, rel, inp.NewString, false)
			addSubstantive(edits, rel, inp.OldString, true)

		case "Write":
			var inp struct {
				FilePath string `json:"file_path"`
				Content  string `json:"content"`
			}
			if err := json.Unmarshal(block.Input, &inp); err != nil || inp.Content == "" {
				continue
			}
			rel := events.NormalizePath(inp.FilePath, repoRoot)
			addSubstantive(edits, rel, inp.Content, false)

		case "Bash":
			var inp struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(block.Input, &inp); err == nil && inp.Command != "" {
				bash = append(bash, inp.Command)
			}
		}
	}

	return edits, bash
}

// addSubstantive splits text into trimmed lines and inserts the substantive
// ones into the added or replaced set for a file. Empty file paths and
// non-substantive lines are skipped.
func addSubstantive(m map[string]*fileEdit, filePath, text string, replaced bool) {
	if filePath == "" || text == "" {
		return
	}
	fe := m[filePath]
	if fe == nil {
		fe = &fileEdit{added: make(map[string]struct{}), replaced: make(map[string]struct{})}
		m[filePath] = fe
	}
	target := fe.added
	if replaced {
		target = fe.replaced
	}
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if !substantive(t) {
			continue
		}
		target[t] = struct{}{}
	}
}

// substantive reports whether a trimmed line is meaningful enough to anchor
// an overlap on. It rejects blank, too-short, and punctuation-only lines
// (braces, closers) that recur across unrelated edits.
func substantive(trimmed string) bool {
	if len(trimmed) < minSubstantiveLen {
		return false
	}
	for _, r := range trimmed {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}
