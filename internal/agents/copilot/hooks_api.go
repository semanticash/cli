package copilot

import "database/sql"

// ParsedLine aliases parsedLine for hook integration.
type ParsedLine = parsedLine

// ToolUse aliases toolUse for hook integration.
type ToolUse = toolUse

// ParseLine parses a single Copilot JSONL line and returns structured fields.
func ParseLine(line string) ParsedLine {
	return parseLine(line)
}

// SerializeToolUses returns tool uses and content types as a JSON NullString.
func SerializeToolUses(tus []ToolUse, contentTypes []string) sql.NullString {
	return serializeToolUses(tus, contentTypes)
}

// ExtractSessionID extracts the UUID session ID from a Copilot transcript path.
func ExtractSessionID(transcriptRef string) string {
	return extractSessionID(transcriptRef)
}

// ExtractCWDFromWorkspace reads the cwd field from workspace.yaml data.
func ExtractCWDFromWorkspace(data []byte) string {
	return extractCWDFromWorkspace(data)
}

// Truncate trims and limits a string to max characters.
func Truncate(s string, max int) string {
	return truncate(s, max)
}

// ExtractModelFromLine extracts the LLM model name from a JSONL line.
func ExtractModelFromLine(line string) string {
	return extractModelFromLine(line)
}

// ExtractSessionShutdownTokens extracts aggregate token usage from a
// session.shutdown JSONL line. Returns nil if not a shutdown event.
func ExtractSessionShutdownTokens(line string) *SessionTokens {
	return extractSessionShutdownTokens(line)
}
