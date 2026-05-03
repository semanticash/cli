package gemini

import (
	"database/sql"
	"io"
)

// GeminiTranscript aliases geminiTranscript for hook integration.
type GeminiTranscript = geminiTranscript

// GeminiMessage aliases geminiMessage for hook integration.
type GeminiMessage = geminiMessage

// ToolUse aliases toolUse for hook integration.
type ToolUse = toolUse

// ParseTranscript parses a Gemini session JSON file.
func ParseTranscript(data []byte) (*GeminiTranscript, error) {
	return parseTranscript(data)
}

// ExtractToolUses extracts tool use entries from a Gemini message.
func ExtractToolUses(msg GeminiMessage) []ToolUse {
	return extractToolUses(msg)
}

// SerializeToolUses returns tool uses and content types as a JSON NullString.
func SerializeToolUses(tus []ToolUse, contentTypes []string) sql.NullString {
	return serializeToolUses(tus, contentTypes)
}

// Truncate trims and limits a string to max characters.
func Truncate(s string, max int) string {
	return truncate(s, max)
}

// ExtractSessionID extracts a session ID from a Gemini session file path.
func ExtractSessionID(sourceKey string) string {
	return extractSessionID(sourceKey)
}

// SessionIDFromTranscript prefers a transcript-provided sessionId and
// falls back to the filename-derived ID.
func SessionIDFromTranscript(t *GeminiTranscript, sourceKey string) string {
	return sessionIDFromTranscript(t, sourceKey)
}

// ReadSessionIDFromHeader returns the JSONL header sessionId from the
// first non-empty line, or "" when no header is present.
func ReadSessionIDFromHeader(r io.Reader) string {
	return readSessionIDFromHeader(r)
}
