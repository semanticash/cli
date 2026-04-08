package claude

import (
	"strings"
	"testing"
)

func TestExtractSessionIDFromPath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/foo/2bf0ed4d-9912-41c0-af0c-e18267a124eb.jsonl", "2bf0ed4d-9912-41c0-af0c-e18267a124eb"},
		{"/foo/subagents/agent-acompact-123.jsonl", ""},
		{"/foo/not-a-uuid.jsonl", ""},
	}
	for _, tt := range tests {
		got := extractSessionIDFromPath(tt.path)
		if got != tt.want {
			t.Errorf("extractSessionIDFromPath(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestExtractParentSessionID(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/base/2bf0ed4d-9912-41c0-af0c-e18267a124eb/subagents/agent-x.jsonl", "2bf0ed4d-9912-41c0-af0c-e18267a124eb"},
		{"/base/some-session/agent.jsonl", ""},
		{"/base/not-uuid/subagents/agent.jsonl", ""},
	}
	for _, tt := range tests {
		got := extractParentSessionID(tt.path)
		if got != tt.want {
			t.Errorf("extractParentSessionID(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestExtractFields_ReadToolResultUsesMetadataSummary(t *testing.T) {
	line := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_read","type":"tool_result","content":"     1->alpha\n     2->beta"}]},"uuid":"user-1","timestamp":"2026-03-23T14:12:33.339Z","toolUseResult":{"type":"text","file":{"filePath":"/tmp/step1.txt","content":"alpha\nbeta\n","numLines":2,"startLine":1,"totalLines":2}},"sessionId":"sess-1"}`

	fields := ExtractFields(line)
	if !fields.IsToolResult {
		t.Fatal("expected tool_result")
	}
	if !fields.IsFileReadResult {
		t.Fatal("expected file read result")
	}
	want := "Read(/tmp/step1.txt) -> 2 lines"
	if fields.Summary != want {
		t.Fatalf("summary = %q, want %q", fields.Summary, want)
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
