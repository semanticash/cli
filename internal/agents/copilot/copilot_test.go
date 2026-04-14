package copilot

import (
	"path/filepath"
	"strings"
	"testing"
)

const (
	testCopilotProjectPath = "/workspace/demo-project"
	testCopilotSessionPath = "/workspace/.copilot/session-state/4de47255-3d43-4938-b8fa-b6e49f1d0aca/events.jsonl"
)

func TestParseLine_UserMessage(t *testing.T) {
	line := `{"type":"user.message","data":{"content":"create a hello world file"},"id":"msg-1","timestamp":"2026-03-12T10:00:00Z"}`

	pl := parseLine(line)

	if pl.Role != "user" {
		t.Errorf("role: got %q, want %q", pl.Role, "user")
	}
	if pl.Kind != "user" {
		t.Errorf("kind: got %q, want %q", pl.Kind, "user")
	}
	if pl.Summary != "create a hello world file" {
		t.Errorf("summary: got %q", pl.Summary)
	}
	if len(pl.ContentTypes) != 1 || pl.ContentTypes[0] != "text" {
		t.Errorf("content_types: got %v, want [text]", pl.ContentTypes)
	}
}

func TestParseLine_AssistantMessage(t *testing.T) {
	line := `{"type":"assistant.message","data":{"content":"I'll create that file for you.","inputTokens":12,"outputTokens":42,"toolRequests":[{"toolCallId":"call_report","name":"report_intent","arguments":{"intent":"Creating file"}},{"toolCallId":"call_edit","name":"edit","arguments":{"path":"/tmp/hello.txt","old_str":"a","new_str":"b"}}]}}`

	pl := parseLine(line)

	if pl.Role != "assistant" {
		t.Errorf("role: got %q, want %q", pl.Role, "assistant")
	}
	if pl.Kind != "assistant" {
		t.Errorf("kind: got %q, want %q", pl.Kind, "assistant")
	}
	if pl.TokensIn != 12 {
		t.Errorf("tokens_in: got %d, want 12", pl.TokensIn)
	}
	if pl.TokensOut != 42 {
		t.Errorf("tokens_out: got %d, want 42", pl.TokensOut)
	}
	if len(pl.ToolUses) != 1 {
		t.Fatalf("tool_uses: got %d, want 1", len(pl.ToolUses))
	}
	if pl.ToolUses[0].Name != "edit" {
		t.Errorf("tool name: got %q, want %q", pl.ToolUses[0].Name, "edit")
	}
	if pl.ToolUses[0].FilePath != "/tmp/hello.txt" {
		t.Errorf("tool file_path: got %q", pl.ToolUses[0].FilePath)
	}
	if pl.ToolUses[0].ToolUseID != "call_edit" {
		t.Errorf("tool tool_use_id: got %q, want %q", pl.ToolUses[0].ToolUseID, "call_edit")
	}
	if pl.ToolUses[0].FileOp != "edit" {
		t.Errorf("tool file_op: got %q, want %q", pl.ToolUses[0].FileOp, "edit")
	}
	if len(pl.ContentTypes) != 2 {
		t.Errorf("content_types: got %v, want [text tool_use]", pl.ContentTypes)
	}
}

func TestParseLine_ToolExecComplete_DoubleDeserialization(t *testing.T) {
	// filePaths is a JSON-encoded string inside a JSON field.
	line := `{"type":"tool.execution_complete","data":{"toolCallId":"call_123","toolTelemetry":{"properties":{"filePaths":"[\"/workspace/demo-project/hello.txt\",\"/workspace/demo-project/world.txt\"]"},"metrics":{"linesAdded":5,"linesRemoved":0}}}}`

	pl := parseLine(line)

	if pl.Role != "tool" {
		t.Errorf("role: got %q, want %q", pl.Role, "tool")
	}
	if pl.Kind != "tool_result" {
		t.Errorf("kind: got %q, want %q", pl.Kind, "tool_result")
	}
	if len(pl.FilePaths) != 2 {
		t.Fatalf("file_paths: got %d, want 2", len(pl.FilePaths))
	}
	if filepath.ToSlash(pl.FilePaths[0]) != testCopilotProjectPath+"/hello.txt" {
		t.Errorf("file_paths[0]: got %q", pl.FilePaths[0])
	}
	if len(pl.ToolUses) != 2 {
		t.Fatalf("tool_uses: got %d, want 2", len(pl.ToolUses))
	}
	if pl.ToolUses[0].Name != "copilot_file_edit" {
		t.Errorf("tool name: got %q", pl.ToolUses[0].Name)
	}
	if pl.ToolUses[0].ToolUseID != "call_123" {
		t.Errorf("tool_use_id: got %q, want %q", pl.ToolUses[0].ToolUseID, "call_123")
	}
}

func TestParseLine_ToolExecComplete_EmptyFilePaths(t *testing.T) {
	line := `{"type":"tool.execution_complete","data":{"toolTelemetry":{"properties":{},"metrics":{}}}}`

	pl := parseLine(line)

	if pl.Role != "tool" {
		t.Errorf("role: got %q, want %q", pl.Role, "tool")
	}
	if len(pl.FilePaths) != 0 {
		t.Errorf("file_paths should be empty, got %v", pl.FilePaths)
	}
}

func TestParseLine_ToolExecComplete_DeduplicatesFilePaths(t *testing.T) {
	line := `{"type":"tool.execution_complete","data":{"toolTelemetry":{"properties":{"filePaths":"[\"/a/b.go\",\"/a/b.go\"]"}}}}`

	pl := parseLine(line)

	if len(pl.FilePaths) != 1 {
		t.Errorf("file_paths should be deduped, got %v", pl.FilePaths)
	}
}

func TestParseLine_UnknownType(t *testing.T) {
	line := `{"type":"session.model_change","data":{"newModel":"gpt-4o"}}`

	pl := parseLine(line)

	if pl.Role != "" {
		t.Errorf("unknown type should return empty role, got %q", pl.Role)
	}
}

func TestParseLine_Malformed(t *testing.T) {
	pl := parseLine("not json")
	if pl.Role != "" {
		t.Errorf("malformed line should return empty role, got %q", pl.Role)
	}
}

func TestExtractSessionID(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{
			"standard path",
			testCopilotSessionPath,
			"4de47255-3d43-4938-b8fa-b6e49f1d0aca",
		},
		{
			"no uuid",
			"/tmp/events.jsonl",
			"",
		},
		{
			"empty",
			"",
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractSessionID(tt.path)
			if got != tt.want {
				t.Errorf("extractSessionID(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestExtractCWDFromWorkspace(t *testing.T) {
	yaml := `id: 4de47255-3d43-4938-b8fa-b6e49f1d0aca
cwd: /workspace/demo-project
summary_count: 0
created_at: 2026-03-12T17:37:43.471Z`

	got := extractCWDFromWorkspace([]byte(yaml))
	want := testCopilotProjectPath
	if got != want {
		t.Errorf("extractCWDFromWorkspace: got %q, want %q", got, want)
	}
}

func TestExtractCWDFromWorkspace_Empty(t *testing.T) {
	got := extractCWDFromWorkspace([]byte("id: abc\nsummary_count: 0\n"))
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestMapToolOp(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"edit", "edit"},
		{"editFile", "edit"},
		{"edit_file", "edit"},
		{"create", "create"},
		{"createFile", "create"},
		{"create_file", "create"},
		{"read", "read"},
		{"readFile", "read"},
		{"read_file", "read"},
		{"runCommand", "exec"},
		{"run_command", "exec"},
		{"bash", "exec"},
		{"unknownTool", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapToolOp(tt.name)
			if got != tt.want {
				t.Errorf("mapToolOp(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestShouldSkipToolUse(t *testing.T) {
	if !shouldSkipToolUse("report_intent") {
		t.Fatal("expected report_intent to be skipped")
	}
	if shouldSkipToolUse("bash") {
		t.Fatal("did not expect bash to be skipped")
	}
}

func TestSerializeToolUses(t *testing.T) {
	tus := []toolUse{
		{Name: "editFile", FilePath: "/a/b.go", FileOp: "edit"},
	}
	result := serializeToolUses(tus, []string{"text", "tool_use"})

	if !result.Valid {
		t.Fatal("expected valid NullString")
	}
	if result.String == "" {
		t.Fatal("expected non-empty string")
	}
	if strings.Contains(result.String, `"text"`) {
		t.Fatalf("did not expect text content type in serialized payload: %q", result.String)
	}
}

func TestSerializeToolUses_Empty(t *testing.T) {
	result := serializeToolUses(nil, nil)
	if result.Valid {
		t.Error("expected invalid NullString for empty input")
	}
}

func TestSerializeToolUses_TextOnlyDropsPayload(t *testing.T) {
	result := serializeToolUses(nil, []string{"text"})
	if result.Valid {
		t.Fatalf("expected invalid NullString for text-only content types, got %q", result.String)
	}
}

func TestSerializeToolUses_ThinkingOnlyPersists(t *testing.T) {
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
