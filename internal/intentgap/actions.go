package intentgap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	attrevents "github.com/semanticash/cli/internal/attribution/events"
)

// ActionEventRow is one captured assistant event handed to the
// extractor. The loader fills it from agent_events and, when needed,
// from the payload CAS.
type ActionEventRow struct {
	EventID      string
	Provider     string
	TurnID       string
	CheckpointID string
	TS           int64
	ToolUses     string
}

// NeedsPayload reports whether ExtractActions can benefit from the
// event payload. Bash command text lives in the payload; Edit/Write
// and provider-native edit events usually carry file_path in tool_uses.
func NeedsPayload(toolUses string) bool {
	if toolUses == "" {
		return false
	}
	return strings.Contains(toolUses, `"Bash"`)
}

// ExtractActions returns the BundleAgentActions a single captured
// event represents. payload may be nil when the loader chose not to
// fetch it; the extractor then falls back to whatever tool_uses can
// give. repoRoot is required so paths can be normalized to the
// repo-relative form git diff produces.
//
// This is the owner of the BundleAgentAction contract. Any
// normalization and filtering required by the intent-gap evidence
// contract happens here, even when lower-level attribution parsers
// are reused.
//
// Read-only tools (Read, Grep, Glob, etc.) intentionally return nil.
// They do not anchor evidence about files the agent attempted to
// change.
func ExtractActions(row ActionEventRow, payload []byte, repoRoot string) []BundleAgentAction {
	if row.ToolUses == "" {
		return nil
	}

	// Provider-native edits and Claude Edit/Write share the same
	// tool_uses shape (name + file_path). The per-pair allowlist
	// below prevents read-only tools in mixed envelopes from becoming
	// actions.
	if attrevents.HasProviderFileEdit(row.ToolUses) || attrevents.HasEditOrWrite(row.ToolUses) {
		actions := extractFromToolUsesPairs(row, repoRoot)
		if len(actions) > 0 {
			return actions
		}
	}
	if NeedsPayload(row.ToolUses) {
		return extractFromBash(row, payload, repoRoot)
	}
	return nil
}

// mutatingTools is the per-pair allowlist for tool_uses entries. The
// set mirrors HasEditOrWrite plus HasProviderFileEdit so the event
// predicate and per-pair filter stay aligned.
var mutatingTools = map[string]bool{
	"Edit":              true,
	"Write":             true,
	"cursor_file_edit":  true,
	"cursor_edit":       true,
	"copilot_file_edit": true,
	"kiro_file_edit":    true,
	"codex_file_edit":   true,
	"editFile":          true,
	"createFile":        true,
	"write_file":        true,
	"edit_file":         true,
	"save_file":         true,
	"replace":           true,
}

// extractFromToolUsesPairs handles tool_uses payloads that already
// carry both the tool name and a file_path: Claude Edit/Write and
// provider-native file edits (Cursor, Copilot, Kiro, Gemini, Codex
// apply_patch normalized at capture time). Non-mutating tools and
// empty paths are dropped.
func extractFromToolUsesPairs(row ActionEventRow, repoRoot string) []BundleAgentAction {
	pairs := parseToolUsesPairs(row.ToolUses)
	if len(pairs) == 0 {
		return nil
	}
	out := make([]BundleAgentAction, 0, len(pairs))
	for i, p := range pairs {
		if p.Name == "" || p.FilePath == "" || !mutatingTools[p.Name] {
			continue
		}
		fp := attrevents.NormalizePath(p.FilePath, repoRoot)
		if fp == "" {
			fp = p.FilePath
		}
		out = append(out, BundleAgentAction{
			ActionID:     deriveActionID(row.EventID, i, row.TurnID, p.Name, fp, 0, 0),
			TurnID:       row.TurnID,
			CheckpointID: row.CheckpointID,
			TS:           row.TS,
			ToolName:     p.Name,
			FilePath:     fp,
			Sources: []string{
				"tool_name:tool_uses",
				"file_path:tool_uses",
			},
		})
	}
	return out
}

// extractFromBash handles Bash tool invocations. tool_uses for Bash
// is intentionally minimal ({name:"Bash",file_op:"exec"}), so paths
// have to be derived from the command string in the payload.
//
// For v1, the only mutating command we parse is `rm`; that matches
// the existing attribution-side derivation and keeps the surface
// narrow. Bash commands without a derivable path still produce one
// unknown-path action with FilePath empty and an explicit
// file_path:unknown source marker.
func extractFromBash(row ActionEventRow, payload []byte, repoRoot string) []BundleAgentAction {
	if len(payload) == 0 {
		return []BundleAgentAction{bashUnknownAction(row)}
	}
	_, bashCommands := attrevents.ExtractClaudeActions(payload, repoRoot)
	if len(bashCommands) == 0 {
		return []BundleAgentAction{bashUnknownAction(row)}
	}

	var out []BundleAgentAction
	idx := 0
	for _, cmd := range bashCommands {
		for _, target := range parseRMTargets(cmd) {
			fp := attrevents.NormalizePath(target, repoRoot)
			if fp == "" {
				continue
			}
			out = append(out, BundleAgentAction{
				ActionID:     deriveActionID(row.EventID, idx, row.TurnID, "Bash", fp, 0, 0),
				TurnID:       row.TurnID,
				CheckpointID: row.CheckpointID,
				TS:           row.TS,
				ToolName:     "Bash",
				FilePath:     fp,
				Sources: []string{
					"tool_name:tool_uses",
					"file_path:payload",
				},
			})
			idx++
		}
	}
	if len(out) == 0 {
		out = append(out, bashUnknownAction(row))
	}
	return out
}

// parseRMTargets returns concrete file-path arguments from simple
// `rm` statements. This parser is owned by intent-gap because every
// emitted path becomes visible evidence in BundleAgentAction.
//
// The parser splits on common chaining operators (`&&`, `||`, `;`,
// `|`, newline) so compound commands like `rm a.go && ls a*` yield
// only the rm target. If a segment contains any shell metacharacter
// (redirection, command substitution, globs, quoting, etc.), the
// entire segment is discarded rather than partially interpreted. The
// caller emits an unknown-path action when no concrete target remains.
func parseRMTargets(cmd string) []string {
	if !strings.Contains(cmd, "rm") {
		return nil
	}
	var targets []string
	for _, seg := range splitShellSegments(cmd) {
		seg = strings.TrimSpace(seg)
		tokens := strings.Fields(seg)
		if len(tokens) == 0 || tokens[0] != "rm" {
			continue
		}
		// Drop ambiguous segments as a whole. The per-token check
		// below remains as a defensive backstop.
		if containsShellMetacharacters(seg) {
			continue
		}
		for _, tok := range tokens[1:] {
			if strings.HasPrefix(tok, "-") {
				continue
			}
			if containsShellMetacharacters(tok) {
				continue
			}
			targets = append(targets, tok)
		}
	}
	return targets
}

// splitShellSegments splits a command on the boundaries between
// chained shell statements. The split is naive - it treats the
// listed operators as separators regardless of quoting context. This
// intentionally favors dropping ambiguous segments over emitting
// false concrete paths.
func splitShellSegments(cmd string) []string {
	for _, op := range []string{"&&", "||", ";", "|", "\n"} {
		cmd = strings.ReplaceAll(cmd, op, "\x00")
	}
	return strings.Split(cmd, "\x00")
}

// containsShellMetacharacters reports whether a token carries any
// character that would require shell evaluation to resolve. Globs
// (`*`, `?`, `[`, `]`), brace expansion (`{`, `}`), redirections
// (`<`, `>`), subshells/groups (`(`, `)`), command substitution
// (backtick, `$`), and quoting (`"`, `'`, `\\`) all signal that the
// token is not a concrete path for this parser.
func containsShellMetacharacters(tok string) bool {
	return strings.ContainsAny(tok, "*?[]{}<>()`$&|;\\\"'")
}

// bashUnknownAction records Bash activity when no concrete path can
// be derived.
func bashUnknownAction(row ActionEventRow) BundleAgentAction {
	return BundleAgentAction{
		ActionID:     deriveActionID(row.EventID, 0, row.TurnID, "Bash", "", 0, 0),
		TurnID:       row.TurnID,
		CheckpointID: row.CheckpointID,
		TS:           row.TS,
		ToolName:     "Bash",
		FilePath:     "",
		Sources: []string{
			"tool_name:tool_uses",
			"file_path:unknown",
		},
	}
}

// toolUsePair is one (name, file_path) entry parsed from tool_uses
// JSON. Many provider payloads carry several tools in one event.
type toolUsePair struct {
	Name     string `json:"name"`
	FilePath string `json:"file_path"`
}

// parseToolUsesPairs reads the (name, file_path) pairs out of the
// tool_uses JSON. Returns nil when the JSON is missing, malformed,
// or contains no tools array. The wider JSON shape is intentionally
// ignored - only the two fields the action contract needs are read.
func parseToolUsesPairs(toolUses string) []toolUsePair {
	if toolUses == "" {
		return nil
	}
	var envelope struct {
		Tools []toolUsePair `json:"tools"`
	}
	if err := json.Unmarshal([]byte(toolUses), &envelope); err != nil {
		return nil
	}
	return envelope.Tools
}

// deriveActionID produces a stable identifier for an action by
// hashing the citation anchors. Used by findings and why output to
// reference actions without inlining their content.
//
// eventID and index disambiguate multiple actions extracted from a
// single event, including repeated edits to the same file.
func deriveActionID(eventID string, index int, turnID, toolName, filePath string, lineStart, lineEnd int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s|%s|%s|%d-%d",
		eventID, index, turnID, toolName, filePath, lineStart, lineEnd)))
	return "a_" + hex.EncodeToString(h[:])[:16]
}
