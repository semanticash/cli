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

func TestDecodeProjectPath_OutsideBaseReturnsEmpty(t *testing.T) {
	// Paths outside Claude's project base must decode to "" on every
	// platform.
	if got := DecodeProjectPath("/workspace/.claude/projects/test/sess.jsonl"); got != "" {
		t.Errorf("DecodeProjectPath(out-of-base) = %q, want \"\"", got)
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

func TestExtractFields_TokenUsagePresence(t *testing.T) {
	tests := []struct {
		name  string
		usage string
		valid bool
	}{
		{
			name:  "complete with measured zero",
			usage: `{"input_tokens":100,"output_tokens":30,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`,
			valid: true,
		},
		{
			name:  "missing cache field",
			usage: `{"input_tokens":100,"output_tokens":30,"cache_read_input_tokens":0}`,
		},
		{
			name:  "negative value",
			usage: `{"input_tokens":100,"output_tokens":30,"cache_read_input_tokens":0,"cache_creation_input_tokens":-1}`,
		},
		{
			name:  "fractional value",
			usage: `{"input_tokens":100.5,"output_tokens":30,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line := `{"type":"assistant","message":{"id":"msg-1","model":"claude-opus-4-6","content":[],"usage":` + tt.usage + `}}`
			fields := ExtractFields(line)
			if fields.TokenUsageValid != tt.valid {
				t.Fatalf("TokenUsageValid = %v, want %v", fields.TokenUsageValid, tt.valid)
			}
			if tt.valid && (fields.TokensIn != 100 || fields.TokensOut != 30 || fields.TokensCacheRead != 0 || fields.TokensCacheCreate != 0) {
				t.Fatalf("token usage = %d/%d/%d/%d, want 100/30/0/0",
					fields.TokensIn, fields.TokensOut, fields.TokensCacheRead, fields.TokensCacheCreate)
			}
			if fields.Model != "claude-opus-4-6" {
				t.Fatalf("model = %q, want preserved", fields.Model)
			}
		})
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

func TestFinalAssistantText(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		want    string
		wantErr bool
	}{
		{
			name: "text blocks concatenated",
			line: `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]}}`,
			want: "first\nsecond",
		},
		{
			name: "thinking and tool_use excluded",
			line: `{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"visible"},{"type":"tool_use","name":"Edit"}]}}`,
			want: "visible",
		},
		{
			name: "no text blocks is a valid empty response",
			line: `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash"}]}}`,
			want: "",
		},
		{
			name:    "invalid top-level json",
			line:    `{"message": not json`,
			wantErr: true,
		},
		{
			name:    "missing message",
			line:    `{"type":"assistant"}`,
			wantErr: true,
		},
		{
			name:    "invalid content block",
			line:    `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"},"not-an-object"]}}`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FinalAssistantText(tt.line)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got text %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("text = %q, want %q", got, tt.want)
			}
		})
	}
}
