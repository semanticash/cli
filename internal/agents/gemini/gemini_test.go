package gemini

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseTranscript(t *testing.T) {
	data := []byte(`{"messages":[
		{"type":"user","content":[{"text":"Hello"}]},
		{"type":"gemini","content":"Hi there!"}
	]}`)
	tr, err := parseTranscript(data)
	if err != nil {
		t.Fatalf("parseTranscript: %v", err)
	}
	if len(tr.Messages) != 2 {
		t.Fatalf("messages count = %d, want 2", len(tr.Messages))
	}
	if tr.Messages[0].Type != "user" {
		t.Errorf("msg[0] type = %q, want user", tr.Messages[0].Type)
	}
	if tr.Messages[0].Content != "Hello" {
		t.Errorf("msg[0] content = %q, want Hello", tr.Messages[0].Content)
	}
	if tr.Messages[1].Content != "Hi there!" {
		t.Errorf("msg[1] content = %q, want 'Hi there!'", tr.Messages[1].Content)
	}
}

func TestUnmarshalContent_UserArray(t *testing.T) {
	data := []byte(`{"type":"user","content":[{"text":"First"},{"text":"Second"}]}`)
	var msg geminiMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Content != "First\nSecond" {
		t.Errorf("content = %q, want 'First\\nSecond'", msg.Content)
	}
}

func TestUnmarshalContent_GeminiString(t *testing.T) {
	data := []byte(`{"type":"gemini","content":"Response text"}`)
	var msg geminiMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Content != "Response text" {
		t.Errorf("content = %q, want 'Response text'", msg.Content)
	}
}

func TestUnmarshalContent_Null(t *testing.T) {
	data := []byte(`{"type":"gemini","content":null}`)
	var msg geminiMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Content != "" {
		t.Errorf("content = %q, want empty", msg.Content)
	}
}

func TestExtractToolUses(t *testing.T) {
	msg := geminiMessage{
		Type: "gemini",
		ToolCalls: []geminiToolCall{
			{Name: "edit_file", Args: map[string]any{"file_path": "auth.go"}},
			{Name: "write_file", Args: map[string]any{"path": "new.go"}},
			{Name: "read_file", Args: map[string]any{"file_path": "main.go"}},
		},
	}
	tus := extractToolUses(msg)
	if len(tus) != 3 {
		t.Fatalf("tool uses count = %d, want 3", len(tus))
	}
	if tus[0].FilePath != "auth.go" || tus[0].FileOp != "edit" {
		t.Errorf("tus[0] = %+v, want edit auth.go", tus[0])
	}
	if tus[1].FilePath != "new.go" || tus[1].FileOp != "write" {
		t.Errorf("tus[1] = %+v, want write new.go", tus[1])
	}
	if tus[2].FileOp != "read" {
		t.Errorf("tus[2] fileOp = %q, want read", tus[2].FileOp)
	}
}

func TestMapGeminiToolOp(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"write_file", "write"},
		{"save_file", "write"},
		{"edit_file", "edit"},
		{"replace", "edit"},
		{"read_file", "read"},
		{"search_files", "read"},
		{"shell", "exec"},
		{"bash", "exec"},
		{"unknown_tool", ""},
	}
	for _, tt := range tests {
		got := mapGeminiToolOp(tt.name)
		if got != tt.want {
			t.Errorf("mapGeminiToolOp(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestExtractSessionID(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/workspace/.gemini/tmp/abc123/chats/session-2026-03-05T10-00-abcd1234.json", "session-2026-03-05T10-00-abcd1234"},
		{"/foo/bar/session-2026-01-01T00-00-99887766.json", "session-2026-01-01T00-00-99887766"},
	}
	for _, tt := range tests {
		got := extractSessionID(tt.path)
		if got != tt.want {
			t.Errorf("extractSessionID(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
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
