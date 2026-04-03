package cursor

import "database/sql"

// BubbleData aliases bubbleData for hook integration.
type BubbleData = bubbleData

// ToolUse aliases toolUse for hook integration.
type ToolUse = toolUse

// ParseCursorJSONLLine parses a Cursor agent-transcripts JSONL line.
func ParseCursorJSONLLine(line string) BubbleData {
	return parseCursorJSONLLine(line)
}

// SerializeToolUses returns tool uses and content types as a JSON NullString.
func SerializeToolUses(tus []ToolUse, contentTypes []string) sql.NullString {
	return serializeToolUses(tus, contentTypes)
}

// DecodeProjectPathFromSourceKey decodes the project path from a Cursor source key.
func DecodeProjectPathFromSourceKey(sourceKey string) string {
	return DecodeProjectPath(sourceKey)
}
