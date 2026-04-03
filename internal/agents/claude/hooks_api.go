package claude

import "database/sql"

// ExtractedFields aliases extractedFields for hook integration.
type ExtractedFields = extractedFields

// ToolUse aliases toolUse for hook integration.
type ToolUse = toolUse

// ExtractFields parses a single Claude JSONL line and returns structured fields.
func ExtractFields(line string) ExtractedFields {
	return extractFields(line)
}

// SerializeToolUses returns tool uses and content types as a JSON NullString.
func SerializeToolUses(tus []ToolUse, contentTypes []string) sql.NullString {
	return serializeToolUses(tus, contentTypes)
}

// HasOnlyReadToolUses reports whether every extracted tool_use is Read.
func HasOnlyReadToolUses(tus []ToolUse) bool {
	return hasOnlyReadToolUses(tus)
}

// ExtractSessionIDFromPath extracts the UUID session ID from a JSONL filename.
func ExtractSessionIDFromPath(sourceKey string) string {
	return extractSessionIDFromPath(sourceKey)
}

// ExtractBasename returns the filename without extension.
func ExtractBasename(sourceKey string) string {
	return extractBasename(sourceKey)
}

// ExtractParentSessionID extracts the parent session UUID from a subagent path.
func ExtractParentSessionID(sourceKey string) string {
	return extractParentSessionID(sourceKey)
}

// DecodeProjectPathFromSourceKey decodes the project path from a Claude source key.
func DecodeProjectPathFromSourceKey(sourceKey string) string {
	return DecodeProjectPath(sourceKey)
}
