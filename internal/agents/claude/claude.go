package claude

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

const ProviderName = "claude_code"

// extractSessionIDFromPath extracts the UUID from a JSONL filename like
// "141807d2-c9fe-49c4-81f0-17d816bd42e8.jsonl". Returns "" if the filename
// is not a valid UUID.
func extractSessionIDFromPath(sourceKey string) string {
	base := filepath.Base(sourceKey)
	ext := filepath.Ext(base)
	if ext != "" {
		base = base[:len(base)-len(ext)]
	}
	// Validate it looks like a UUID (36 chars with hyphens)
	if len(base) == 36 && strings.Count(base, "-") == 4 {
		return base
	}
	return ""
}

// extractBasename returns the filename without extension, used as a fallback
// provider_session_id for non-UUID filenames (e.g. subagent files like
// "agent-acompact-166797be1b9155b0").
func extractBasename(sourceKey string) string {
	base := filepath.Base(sourceKey)
	ext := filepath.Ext(base)
	if ext != "" {
		base = base[:len(base)-len(ext)]
	}
	return base
}

// extractParentSessionID extracts the parent session UUID from a subagent
// source key. Claude Code stores subagent JSONL files under
// <parent_uuid>/subagents/<subagent>.jsonl. Returns "" if the path does not
// follow this convention.
func extractParentSessionID(sourceKey string) string {
	dir := filepath.Dir(sourceKey)
	if filepath.Base(dir) != "subagents" {
		return ""
	}
	parentDir := filepath.Base(filepath.Dir(dir))
	if len(parentDir) == 36 && strings.Count(parentDir, "-") == 4 {
		return parentDir
	}
	return ""
}

// DecodeProjectPath decodes the project path from a Claude source key.
// Claude stores projects under ~/.claude/projects/<encoded-path>/, where
// the encoded path replaces "/" with "-".
//
// Returns "" when sourceKey is not actually under the projects base.
func DecodeProjectPath(sourceKey string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	base := filepath.Join(home, ".claude", "projects")
	rel, err := filepath.Rel(base, sourceKey)
	if err != nil {
		return ""
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return ""
	}
	parts := strings.SplitN(rel, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		return ""
	}
	projectDir := parts[0]
	decoded := strings.ReplaceAll(projectDir, "-", "/")
	return filepath.Clean(decoded)
}

type toolUsesPayload struct {
	ContentTypes []string  `json:"content_types,omitempty"`
	Tools        []toolUse `json:"tools,omitempty"`
}

func serializeToolUses(tus []toolUse, contentTypes []string) sql.NullString {
	contentTypes = normalizeContentTypes(contentTypes)
	if len(tus) == 0 && len(contentTypes) == 0 {
		return sql.NullString{}
	}
	p := toolUsesPayload{
		ContentTypes: contentTypes,
		Tools:        tus,
	}
	b, err := json.Marshal(p)
	if err != nil {
		return sql.NullString{}
	}
	return sql.NullString{String: string(b), Valid: true}
}

func normalizeContentTypes(contentTypes []string) []string {
	if len(contentTypes) == 0 {
		return nil
	}
	keep := make([]string, 0, len(contentTypes))
	seen := make(map[string]bool, len(contentTypes))
	for _, ct := range contentTypes {
		if ct != "thinking" && ct != "tool_use" {
			continue
		}
		if seen[ct] {
			continue
		}
		seen[ct] = true
		keep = append(keep, ct)
	}
	if len(keep) == 0 {
		return nil
	}
	return keep
}
