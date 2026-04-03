package cursor

import (
	"testing"
)

func TestParseComposerData(t *testing.T) {
	input := `{
		"composerId": "abc-123",
		"title": "Fix login bug",
		"model": "claude-3.5-sonnet",
		"createdAt": 1700000000000,
		"lastUpdatedAt": 1700001000000
	}`
	cd := parseComposerData(input)

	if cd.ComposerID != "abc-123" {
		t.Errorf("composerId = %q, want abc-123", cd.ComposerID)
	}
	if cd.Title != "Fix login bug" {
		t.Errorf("title = %q, want Fix login bug", cd.Title)
	}
	if cd.Model != "claude-3.5-sonnet" {
		t.Errorf("model = %q, want claude-3.5-sonnet", cd.Model)
	}
	if cd.CreatedAt != 1700000000000 {
		t.Errorf("createdAt = %d, want 1700000000000", cd.CreatedAt)
	}
	if cd.LastUpdatedAt != 1700001000000 {
		t.Errorf("lastUpdatedAt = %d, want 1700001000000", cd.LastUpdatedAt)
	}
}

func TestParseComposerData_Seconds(t *testing.T) {
	input := `{"composerId": "x", "createdAt": 1700000000}`
	cd := parseComposerData(input)
	if cd.CreatedAt != 1700000000000 {
		t.Errorf("createdAt = %d, want 1700000000000 (seconds converted to ms)", cd.CreatedAt)
	}
}

func TestParseComposerData_Invalid(t *testing.T) {
	cd := parseComposerData("not json")
	if cd.ComposerID != "" {
		t.Errorf("expected empty composerData for invalid JSON")
	}
}

func TestParseBubble_User(t *testing.T) {
	input := `{"type": 1, "text": "Please fix the bug in auth.go"}`
	bd := parseBubble(input)

	if bd.Role != "user" {
		t.Errorf("role = %q, want user", bd.Role)
	}
	if bd.Summary != "Please fix the bug in auth.go" {
		t.Errorf("summary = %q, want 'Please fix the bug in auth.go'", bd.Summary)
	}
}

func TestParseBubble_Assistant(t *testing.T) {
	input := `{"type": 2, "text": "I'll fix the authentication issue by updating the token validation logic."}`
	bd := parseBubble(input)

	if bd.Role != "assistant" {
		t.Errorf("role = %q, want assistant", bd.Role)
	}
	if bd.Summary == "" {
		t.Error("expected non-empty summary")
	}
}

func TestParseBubble_UnknownType(t *testing.T) {
	input := `{"type": 99, "text": "something"}`
	bd := parseBubble(input)
	if bd.Role != "" {
		t.Errorf("expected empty role for unknown type, got %q", bd.Role)
	}
}

func TestParseBubble_WithToolFormerData(t *testing.T) {
	input := `{
		"type": 2,
		"text": "Let me edit the file",
		"toolFormerData": [
			{"name": "edit_file", "path": "/src/auth.go"},
			{"name": "run_terminal", "input": {"command": "go test"}}
		]
	}`
	bd := parseBubble(input)

	if len(bd.ToolUses) != 2 {
		t.Fatalf("expected 2 tool uses, got %d", len(bd.ToolUses))
	}
	if bd.ToolUses[0].Name != "edit_file" {
		t.Errorf("tool[0].name = %q, want edit_file", bd.ToolUses[0].Name)
	}
	if bd.ToolUses[0].FilePath != "/src/auth.go" {
		t.Errorf("tool[0].filepath = %q, want /src/auth.go", bd.ToolUses[0].FilePath)
	}
	if bd.ToolUses[0].FileOp != "edit" {
		t.Errorf("tool[0].fileop = %q, want edit", bd.ToolUses[0].FileOp)
	}
	if bd.ToolUses[1].Name != "run_terminal" {
		t.Errorf("tool[1].name = %q, want run_terminal", bd.ToolUses[1].Name)
	}
	if bd.ToolUses[1].FileOp != "exec" {
		t.Errorf("tool[1].fileop = %q, want exec", bd.ToolUses[1].FileOp)
	}
}

func TestExtractCursorToolUses_FilePathInInput(t *testing.T) {
	input := `[{"name": "read_file", "input": {"file_path": "/src/main.go"}}]`
	tools := extractCursorToolUses([]byte(input))

	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].FilePath != "/src/main.go" {
		t.Errorf("filepath = %q, want /src/main.go", tools[0].FilePath)
	}
	if tools[0].FileOp != "read" {
		t.Errorf("fileop = %q, want read", tools[0].FileOp)
	}
}

func TestExtractCursorToolUses_SingleObject(t *testing.T) {
	// Cursor sometimes emits a single tool object instead of an array.
	input := `{"name": "edit_file", "toolCallId": "tc-123", "rawArgs": "{\"file_path\":\"/src/auth.go\"}", "status": "completed"}`
	tools := extractCursorToolUses([]byte(input))

	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].Name != "edit_file" {
		t.Errorf("name = %q, want edit_file", tools[0].Name)
	}
	if tools[0].FilePath != "/src/auth.go" {
		t.Errorf("filepath = %q, want /src/auth.go (extracted from rawArgs)", tools[0].FilePath)
	}
}

func TestExtractCursorToolUses_WrappedInToolsKey(t *testing.T) {
	// Wrapper object with "tools" array.
	input := `{"tools": [{"name": "read_file", "path": "/foo.go"}, {"name": "write_file", "path": "/bar.go"}]}`
	tools := extractCursorToolUses([]byte(input))

	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	if tools[0].Name != "read_file" {
		t.Errorf("tool[0].name = %q, want read_file", tools[0].Name)
	}
	if tools[1].FilePath != "/bar.go" {
		t.Errorf("tool[1].filepath = %q, want /bar.go", tools[1].FilePath)
	}
}

func TestExtractCursorToolUses_RawArgsFilePath(t *testing.T) {
	// rawArgs is stringified JSON containing file_path.
	input := `[{"name": "create_file", "rawArgs": "{\"file_path\":\"/new/file.ts\",\"content\":\"export {}\"}"}]`
	tools := extractCursorToolUses([]byte(input))

	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].FilePath != "/new/file.ts" {
		t.Errorf("filepath = %q, want /new/file.ts", tools[0].FilePath)
	}
	if tools[0].FileOp != "write" {
		t.Errorf("fileop = %q, want write", tools[0].FileOp)
	}
}

func TestExtractCursorToolUses_InvalidJSON(t *testing.T) {
	tools := extractCursorToolUses([]byte("not json"))
	if len(tools) != 0 {
		t.Errorf("expected no tools for invalid JSON, got %d", len(tools))
	}
}

func TestMapCursorToolOp(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"write_file", "write"},
		{"create_file", "write"},
		{"edit_file", "edit"},
		{"apply_diff", "edit"},
		{"replace_in_file", "edit"},
		{"read_file", "read"},
		{"search_files", "read"},
		{"grep_search", "read"},
		{"run_terminal", "exec"},
		{"execute_command", "exec"},
		{"bash_command", "exec"},
		{"unknown_tool", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapCursorToolOp(tt.name)
			if got != tt.want {
				t.Errorf("mapCursorToolOp(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestNormalizeTimestamp(t *testing.T) {
	tests := []struct {
		name  string
		input float64
		want  int64
	}{
		{"zero", 0, 0},
		{"seconds", 1700000000, 1700000000000},
		{"milliseconds", 1700000000000, 1700000000000},
		{"nanoseconds", 1700000000000000000, 1700000000000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeTimestamp(tt.input)
			if got != tt.want {
				t.Errorf("normalizeTimestamp(%f) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

// Comment #7: Kind vs Role - parseBubble should set Kind and ContentTypes correctly.
func TestParseBubble_KindAndContentTypes(t *testing.T) {
	// Assistant with text + tools should have kind="assistant", content_types=["text","tool_use"].
	input := `{"type":2,"text":"Editing file","toolFormerData":[{"name":"edit_file","path":"/foo.go"}]}`
	bd := parseBubble(input)

	if bd.Kind != "assistant" {
		t.Errorf("kind = %q, want assistant", bd.Kind)
	}
	if bd.Role != "assistant" {
		t.Errorf("role = %q, want assistant", bd.Role)
	}
	if len(bd.ContentTypes) != 2 {
		t.Fatalf("content_types length = %d, want 2", len(bd.ContentTypes))
	}
	if bd.ContentTypes[0] != "text" {
		t.Errorf("content_types[0] = %q, want text", bd.ContentTypes[0])
	}
	if bd.ContentTypes[1] != "tool_use" {
		t.Errorf("content_types[1] = %q, want tool_use", bd.ContentTypes[1])
	}
}

func TestParseBubble_UserKindAndContentTypes(t *testing.T) {
	// User with text only should have kind="user", content_types=["text"].
	input := `{"type":1,"text":"Please fix this"}`
	bd := parseBubble(input)

	if bd.Kind != "user" {
		t.Errorf("kind = %q, want user", bd.Kind)
	}
	if bd.Role != "user" {
		t.Errorf("role = %q, want user", bd.Role)
	}
	if len(bd.ContentTypes) != 1 || bd.ContentTypes[0] != "text" {
		t.Errorf("content_types = %v, want [text]", bd.ContentTypes)
	}
}

func TestParseBubble_AssistantNoToolsContentTypes(t *testing.T) {
	// Assistant with text but no tools should have content_types=["text"] only.
	input := `{"type":2,"text":"Sure, I can help"}`
	bd := parseBubble(input)

	if len(bd.ContentTypes) != 1 || bd.ContentTypes[0] != "text" {
		t.Errorf("content_types = %v, want [text]", bd.ContentTypes)
	}
}

func TestParseBubble_EmptyTextNoContentTypes(t *testing.T) {
	// No text, no tools - content_types should be empty.
	input := `{"type":2}`
	bd := parseBubble(input)

	if len(bd.ContentTypes) != 0 {
		t.Errorf("content_types = %v, want empty", bd.ContentTypes)
	}
}

func TestTruncate(t *testing.T) {
	long := "This is a really long string that should be truncated at some point because it exceeds the maximum allowed length for summaries in our system."
	got := truncate(long, 50)
	if len(got) != 50 {
		t.Errorf("expected length 50, got %d", len(got))
	}

	short := "Short"
	got = truncate(short, 50)
	if got != "Short" {
		t.Errorf("truncate(%q, 50) = %q, want %q", short, got, "Short")
	}

	withNewlines := "Line one\nLine two\nLine three"
	got = truncate(withNewlines, 200)
	if got != "Line one Line two Line three" {
		t.Errorf("expected newlines replaced, got %q", got)
	}
}
