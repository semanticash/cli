package builder

import (
	"strings"

	"github.com/semanticash/cli/internal/redact"
)

// TruncateWithEllipsis returns s unchanged when its length is at most
// max; otherwise it returns the first max bytes followed by "...".
// Used by the Claude, Cursor, Gemini, and Kiro CLI prompt paths.
// No whitespace normalization is applied; the input appears in the
// summary exactly as it was received.
func TruncateWithEllipsis(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// TruncateClean normalizes whitespace and truncates without an
// ellipsis. Used by the Copilot prompt and subagent-prompt paths.
// The steps are:
//  1. Trim leading and trailing whitespace.
//  2. Replace embedded newlines with single spaces.
//  3. Strip carriage returns entirely (no substitution, which means
//     "a\rb" becomes "ab" rather than "a b").
//  4. Truncate to at most max bytes, with no ellipsis appended.
//
// The asymmetry between newline handling (substituted) and carriage
// return handling (dropped) is preserved from the original
// agentcopilot.Truncate implementation and is asserted by the
// Copilot direct-emit tests.
func TruncateClean(s string, max int) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	if len(s) > max {
		return s[:max]
	}
	return s
}

// Redact wraps redact.String with the error policy every direct-emit
// helper already uses: on redaction failure, return the input string
// unchanged rather than surfacing the error. Returns the input
// untouched when it is empty so call sites do not have to guard
// individually.
func Redact(s string) string {
	if s == "" {
		return ""
	}
	r, err := redact.String(s)
	if err != nil {
		return s
	}
	return r
}
