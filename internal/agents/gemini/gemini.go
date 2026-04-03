package gemini

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
)

const ProviderName = "gemini_cli"

// extractSessionID returns a stable session identifier from a Gemini transcript
// filename. The format is session-<date>-<shortid>.json - we use the full
// basename (without extension) as the provider session ID.
func extractSessionID(sourceKey string) string {
	base := filepath.Base(sourceKey)
	ext := filepath.Ext(base)
	if ext != "" {
		base = base[:len(base)-len(ext)]
	}
	return base
}

// toolUsesPayload matches the JSON structure used by other providers.
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
