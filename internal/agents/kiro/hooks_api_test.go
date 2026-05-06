package kiro

import (
	"encoding/json"
	"testing"
)

// TestBuildToolUsesJSON_ToolNameRoutesScoring covers the scoring path encoded
// in tool_uses. Canonical Write/Edit names engage line-level attribution;
// kiro_file_edit uses file-touch attribution.
func TestBuildToolUsesJSON_ToolNameRoutesScoring(t *testing.T) {
	cases := []struct {
		name     string
		toolName string
		filePath string
		fileOp   string
	}{
		{"Write canonical", ToolNameWrite, "main.go", "write"},
		{"Edit canonical", ToolNameEdit, "main.go", "edit"},
		{"file-touch fallback for renames", ToolNameFileEdit, "main.go", "rename"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildToolUsesJSON(tc.toolName, tc.filePath, tc.fileOp)
			if !got.Valid {
				t.Fatalf("Valid = false, want true for non-empty filePath")
			}
			var payload toolUsesPayload
			if err := json.Unmarshal([]byte(got.String), &payload); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(payload.Tools) != 1 {
				t.Fatalf("tools = %d, want 1", len(payload.Tools))
			}
			if payload.Tools[0].Name != tc.toolName {
				t.Errorf("name = %q, want %q", payload.Tools[0].Name, tc.toolName)
			}
			if payload.Tools[0].FilePath != tc.filePath {
				t.Errorf("file_path = %q, want %q", payload.Tools[0].FilePath, tc.filePath)
			}
			if payload.Tools[0].FileOp != tc.fileOp {
				t.Errorf("file_op = %q, want %q", payload.Tools[0].FileOp, tc.fileOp)
			}
		})
	}
}

func TestBuildToolUsesJSON_EmptyFilePathInvalid(t *testing.T) {
	got := BuildToolUsesJSON(ToolNameWrite, "", "write")
	if got.Valid {
		t.Errorf("Valid = true, want false for empty file path")
	}
}
