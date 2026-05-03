package gemini

import (
	"bytes"
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

func TestParseTranscript_JSONL_040(t *testing.T) {
	// 0.40 JSONL: header record then per-message records.
	data := []byte(`{"sessionId":"d6b9c1e2-1234-4abc-9def-0123456789ab","projectHash":"abc123","startTime":"2026-04-01T10:00:00Z","kind":"main"}
{"type":"user","content":[{"text":"Hi"}]}
{"type":"gemini","content":"Hello back"}`)
	tr, err := parseTranscript(data)
	if err != nil {
		t.Fatalf("parseTranscript: %v", err)
	}
	if tr.SessionID != "d6b9c1e2-1234-4abc-9def-0123456789ab" {
		t.Errorf("SessionID = %q, want UUID from header", tr.SessionID)
	}
	if len(tr.Messages) != 2 {
		t.Fatalf("messages count = %d, want 2", len(tr.Messages))
	}
	if tr.Messages[0].Content != "Hi" {
		t.Errorf("msg[0] content = %q, want Hi", tr.Messages[0].Content)
	}
	if tr.Messages[1].Content != "Hello back" {
		t.Errorf("msg[1] content = %q, want 'Hello back'", tr.Messages[1].Content)
	}
}

func TestParseTranscript_JSONL_SkipsNonMessageRecords(t *testing.T) {
	// A non-header, non-message metadata record (no "type" field)
	// must not enter t.Messages, otherwise downstream consumers walk
	// past blank entries.
	data := []byte(`{"sessionId":"s1","kind":"main"}
{"type":"user","content":"A"}
{"checkpoint":"abc","savedAt":"2026-04-01T10:00:00Z"}
{"type":"gemini","content":"B"}`)
	tr, err := parseTranscript(data)
	if err != nil {
		t.Fatalf("parseTranscript: %v", err)
	}
	if len(tr.Messages) != 2 {
		t.Fatalf("messages count = %d, want 2 (metadata record dropped)", len(tr.Messages))
	}
	if tr.Messages[0].Type != "user" || tr.Messages[1].Type != "gemini" {
		t.Errorf("unexpected message types: %q, %q", tr.Messages[0].Type, tr.Messages[1].Type)
	}
}

func TestParseTranscript_JSONL_SkipsBadLines(t *testing.T) {
	// Best-effort: malformed lines do not abort the stream.
	data := []byte(`{"sessionId":"abc","kind":"main"}
not valid json
{"type":"user","content":"A"}
{"type":"gemini","content":"B"}`)
	tr, err := parseTranscript(data)
	if err != nil {
		t.Fatalf("parseTranscript: %v", err)
	}
	if tr.SessionID != "abc" {
		t.Errorf("SessionID = %q, want abc", tr.SessionID)
	}
	if len(tr.Messages) != 2 {
		t.Fatalf("messages count = %d, want 2 (bad line skipped)", len(tr.Messages))
	}
}

func TestReadSessionIDFromHeader(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "0.40 header",
			in:   `{"sessionId":"abc-123","kind":"main","startTime":"2026-04-01T10:00:00Z"}` + "\n" + `{"type":"user","content":"x"}`,
			want: "abc-123",
		},
		{
			name: "leading blank lines",
			in:   "\n\n" + `{"sessionId":"def-456","kind":"main"}` + "\n",
			want: "def-456",
		},
		{
			name: "legacy JSON",
			in:   `{"messages":[{"type":"user","content":"x"}]}`,
			want: "",
		},
		{
			name: "first line is a message",
			in:   `{"type":"user","content":"x"}` + "\n",
			want: "",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := readSessionIDFromHeader(bytes.NewReader([]byte(tt.in)))
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSessionIDFromTranscript(t *testing.T) {
	// JSONL header value wins.
	tr := &geminiTranscript{SessionID: "uuid-from-header"}
	got := sessionIDFromTranscript(tr, "/path/session-2026-01-01T00-00-deadbeef.json")
	if got != "uuid-from-header" {
		t.Errorf("got %q, want JSONL header value", got)
	}

	// Legacy: fall back to filename.
	legacy := &geminiTranscript{}
	got = sessionIDFromTranscript(legacy, "/path/session-2026-01-01T00-00-deadbeef.json")
	if got != "session-2026-01-01T00-00-deadbeef" {
		t.Errorf("got %q, want filename-derived ID", got)
	}

	// Nil transcript: fall back to filename.
	got = sessionIDFromTranscript(nil, "/path/session-x.json")
	if got != "session-x" {
		t.Errorf("got %q, want filename-derived ID", got)
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
