package cursor

import (
	"strings"
	"testing"
)

const testCursorProjectPath = "/workspace/demo-project"

func TestParseCursorJSONLLine_User(t *testing.T) {
	line := `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nPlease fix the bug\n</user_query>"}]}}`
	bd := parseCursorJSONLLine(line)
	if bd.Role != "user" {
		t.Errorf("role = %q, want user", bd.Role)
	}
	if bd.Kind != "user" {
		t.Errorf("kind = %q, want user", bd.Kind)
	}
	if bd.Summary != "Please fix the bug" {
		t.Errorf("summary = %q, want %q", bd.Summary, "Please fix the bug")
	}
}

func TestParseCursorJSONLLine_Assistant(t *testing.T) {
	line := `{"role":"assistant","message":{"content":[{"type":"text","text":"I'll fix the bug by updating the handler."}]}}`
	bd := parseCursorJSONLLine(line)
	if bd.Role != "assistant" {
		t.Errorf("role = %q, want assistant", bd.Role)
	}
	if bd.Summary != "I'll fix the bug by updating the handler." {
		t.Errorf("summary = %q", bd.Summary)
	}
}

func TestParseCursorJSONLLine_ToolUse(t *testing.T) {
	line := `{"role":"assistant","message":{"content":[{"type":"text","text":"I'll list the files."},{"type":"tool_use","name":"Shell","input":{"command":"ls","working_directory":"/workspace/demo-project"}},{"type":"tool_use","name":"Glob","input":{"target_directory":"/workspace/demo-project","glob_pattern":"README*"}}]}}`
	bd := parseCursorJSONLLine(line)
	if bd.Role != "assistant" {
		t.Fatalf("role = %q, want assistant", bd.Role)
	}
	if bd.Summary != "I'll list the files." {
		t.Errorf("summary = %q", bd.Summary)
	}
	if len(bd.ToolUses) != 2 {
		t.Fatalf("tool uses = %d, want 2", len(bd.ToolUses))
	}
	if bd.ToolUses[0].Name != "Shell" {
		t.Errorf("tool[0].name = %q, want Shell", bd.ToolUses[0].Name)
	}
	if bd.ToolUses[0].FilePath != testCursorProjectPath {
		t.Errorf("tool[0].file_path = %q", bd.ToolUses[0].FilePath)
	}
	if bd.ToolUses[0].FileOp != "exec" {
		t.Errorf("tool[0].file_op = %q, want exec", bd.ToolUses[0].FileOp)
	}
	if bd.ToolUses[1].Name != "Glob" {
		t.Errorf("tool[1].name = %q, want Glob", bd.ToolUses[1].Name)
	}
	if bd.ToolUses[1].FilePath != testCursorProjectPath {
		t.Errorf("tool[1].file_path = %q", bd.ToolUses[1].FilePath)
	}
	// Verify content types.
	wantTypes := map[string]bool{"text": true, "tool_use": true}
	for _, ct := range bd.ContentTypes {
		delete(wantTypes, ct)
	}
	if len(wantTypes) > 0 {
		t.Errorf("missing content types: %v", wantTypes)
	}
}

func TestParseCursorJSONLLine_FilePathTool(t *testing.T) {
	line := `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/workspace/demo-project/go.mod"}}]}}`
	bd := parseCursorJSONLLine(line)
	if len(bd.ToolUses) != 1 {
		t.Fatalf("tool uses = %d, want 1", len(bd.ToolUses))
	}
	if bd.ToolUses[0].FilePath != testCursorProjectPath+"/go.mod" {
		t.Errorf("file_path = %q", bd.ToolUses[0].FilePath)
	}
	if bd.ToolUses[0].FileOp != "read" {
		t.Errorf("file_op = %q, want read", bd.ToolUses[0].FileOp)
	}
}

func TestDecodeProjectPath(t *testing.T) {
	path := DecodeProjectPath("/not/.cursor/projects/foo/bar")
	// This path is outside Cursor's project base, so DecodeProjectPath should
	// return an empty path and avoid panicking.
	_ = path
}

func TestSerializeToolUses_DropsTextOnlyContentTypes(t *testing.T) {
	result := serializeToolUses(nil, []string{"text"})
	if result.Valid {
		t.Fatalf("expected invalid NullString for text-only content types, got %q", result.String)
	}
}

func TestSerializeToolUses_KeepsThinking(t *testing.T) {
	result := serializeToolUses(nil, []string{"text", "thinking"})
	if !result.Valid {
		t.Fatal("expected valid NullString when thinking is present")
	}
	if !strings.Contains(result.String, `"thinking"`) {
		t.Fatalf("expected thinking content type, got %q", result.String)
	}
	if strings.Contains(result.String, `"text"`) {
		t.Fatalf("did not expect text content type, got %q", result.String)
	}
}

func TestStripUserQueryTags(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"<user_query>\nFix the bug\n</user_query>", "Fix the bug"},
		{"<user_query>Go ahead</user_query>", "Go ahead"},
		{"No tags here", "No tags here"},
		{"  <user_query>\n  Hello  \n</user_query>  ", "Hello"},
	}
	for _, tt := range tests {
		got := stripUserQueryTags(tt.input)
		if got != tt.want {
			t.Errorf("stripUserQueryTags(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
