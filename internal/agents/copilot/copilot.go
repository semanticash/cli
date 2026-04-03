package copilot

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
)

const ProviderName = "copilot"

// toolUse matches the structure used by other providers for interoperability.
type toolUse struct {
	Name      string `json:"name"`
	FilePath  string `json:"file_path,omitempty"`
	FileOp    string `json:"file_op,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
}

// parsedLine holds the extracted fields from a single Copilot JSONL event.
type parsedLine struct {
	Role         string
	Kind         string
	Summary      string
	ToolUses     []toolUse
	ContentTypes []string
	FilePaths    []string // absolute file paths from tool.execution_complete
	TokensIn     int64
	TokensOut    int64
}

// extractSessionID extracts the UUID directory name from a Copilot transcript
// path like ~/.copilot/session-state/<uuid>/events.jsonl.
func extractSessionID(transcriptRef string) string {
	dir := filepath.Dir(transcriptRef)
	base := filepath.Base(dir)
	if len(base) == 36 && strings.Count(base, "-") == 4 {
		return base
	}
	return ""
}

// extractCWDFromWorkspace reads the cwd field from workspace.yaml using
// simple line scanning (no YAML dependency). The file format is:
//
//	id: <uuid>
//	cwd: /path/to/project
//	...
func extractCWDFromWorkspace(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "cwd:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "cwd:"))
		}
	}
	return ""
}

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

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	if len(s) > max {
		return s[:max]
	}
	return s
}
