package llm

import (
	"strings"
	"testing"
)

func TestBuildSuggestPRPrompt_WithTemplate(t *testing.T) {
	prompt := BuildSuggestPRPrompt("diff content", "subject 1\nsubject 2", "## Summary\n## Test plan")

	if !strings.Contains(prompt, "## Summary") {
		t.Error("prompt should contain the PR template content")
	}
	if strings.Contains(prompt, "no template provided") {
		t.Error("prompt should not contain fallback text when template is present")
	}
}

func TestBuildSuggestPRPrompt_WithoutTemplate(t *testing.T) {
	prompt := BuildSuggestPRPrompt("diff content", "subject 1", "")

	if !strings.Contains(prompt, "no template provided") {
		t.Error("prompt should contain fallback text when no template")
	}
}

func TestBuildSuggestPRPrompt_LargeDiff(t *testing.T) {
	largeDiff := strings.Repeat("+some added line\n", 100_000)
	prompt := BuildSuggestPRPrompt(largeDiff, "subject", "template")

	if len(prompt) > 100_000 {
		t.Errorf("prompt too large: %d chars; should be bounded by summary+excerpt", len(prompt))
	}
}

func TestParseSuggestPROutput_Valid(t *testing.T) {
	raw := `{"title": "Add user auth", "body": "## Summary\n- Added auth flow"}`
	out, err := ParseSuggestPROutput(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Title != "Add user auth" {
		t.Errorf("title = %q, want %q", out.Title, "Add user auth")
	}
	if !strings.Contains(out.Body, "auth flow") {
		t.Errorf("body missing expected content: %q", out.Body)
	}
}

func TestParseSuggestPROutput_MarkdownWrapped(t *testing.T) {
	raw := "```json\n{\"title\": \"Fix bug\", \"body\": \"Fixed it\"}\n```"
	out, err := ParseSuggestPROutput(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Title != "Fix bug" {
		t.Errorf("title = %q", out.Title)
	}
}

func TestParseSuggestPROutput_EmptyTitle(t *testing.T) {
	raw := `{"title": "", "body": "some body"}`
	_, err := ParseSuggestPROutput(raw)
	if err == nil {
		t.Error("expected error for empty title")
	}
}

func TestParseSuggestPROutput_WhitespaceOnlyTitle(t *testing.T) {
	raw := `{"title": "   \t  ", "body": "some body"}`
	_, err := ParseSuggestPROutput(raw)
	if err == nil {
		t.Error("expected error for whitespace-only title")
	}
}

func TestParseSuggestPROutput_NewlineInTitle(t *testing.T) {
	raw := `{"title": "Fix X\n\nMore details here", "body": "body"}`
	out, err := ParseSuggestPROutput(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out.Title, "\n") {
		t.Errorf("title should not contain newlines, got %q", out.Title)
	}
	if out.Title != "Fix X" {
		t.Errorf("title = %q, want %q", out.Title, "Fix X")
	}
}

func TestStripHTMLComments(t *testing.T) {
	input := "## Summary\n\n<!-- What does this PR do? -->\n\n- [ ] Tests pass"
	got := StripHTMLComments(input)
	if strings.Contains(got, "<!--") {
		t.Errorf("HTML comments not stripped: %q", got)
	}
	if !strings.Contains(got, "## Summary") {
		t.Error("heading was stripped")
	}
	if !strings.Contains(got, "- [ ] Tests pass") {
		t.Error("checklist was stripped")
	}
}
