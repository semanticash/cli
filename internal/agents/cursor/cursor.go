package cursor

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

const ProviderName = "cursor"

// serializeToolUses converts tool uses and content types to the same JSON
// format used by the Claude provider:
// {"content_types":["text","tool_use"],"tools":[{"name":"..."}]}
func serializeToolUses(tus []toolUse, contentTypes []string) sql.NullString {
	contentTypes = normalizeContentTypes(contentTypes)
	if len(tus) == 0 && len(contentTypes) == 0 {
		return sql.NullString{}
	}
	type payload struct {
		ContentTypes []string  `json:"content_types,omitempty"`
		Tools        []toolUse `json:"tools,omitempty"`
	}
	b, err := json.Marshal(payload{ContentTypes: contentTypes, Tools: tus})
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

// DecodeProjectPath decodes the project path from a Cursor transcript path.
// Cursor stores project data under ~/.cursor/projects/<encoded-path>/, where
// the encoded path replaces "/" with "-".
// Unlike Claude (which uses a leading "-" to encode the root "/"), Cursor
// omits the leading separator, so the result needs a "/" prefix.
// Example: tmp-demo-project -> /tmp/demo/project
//
// Returns "" when sourceKey is not actually under the projects base.
func DecodeProjectPath(sourceKey string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	base := filepath.Join(home, ".cursor", "projects")
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
	decoded := "/" + strings.ReplaceAll(parts[0], "-", "/")
	return filepath.Clean(decoded)
}
