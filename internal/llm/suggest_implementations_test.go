package llm

import (
	"testing"
)

func TestParseSuggestImplementationOutput(t *testing.T) {
	raw := `{
		"title": "Migrate auth to OAuth2",
		"summary": "Replaced session-based auth with OAuth2 middleware in the API server and updated the Python SDK to use the new token refresh flow.",
		"review_priority": [
			{"priority": "high", "repo": "sdk", "file": "client/auth.py", "reason": "New auth flow"},
			{"priority": "medium", "repo": "api", "file": "auth/middleware.go", "reason": "Core change"}
		]
	}`

	out, err := ParseSuggestImplementationOutput(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if out.Title != "Migrate auth to OAuth2" {
		t.Errorf("title: got %q", out.Title)
	}
	if len(out.ReviewPriority) != 2 {
		t.Errorf("review_priority: got %d items", len(out.ReviewPriority))
	}
	if out.ReviewPriority[0].Priority != "high" {
		t.Errorf("first priority: got %q", out.ReviewPriority[0].Priority)
	}
}

func TestParseSuggestImplementationOutput_WrappedInMarkdown(t *testing.T) {
	raw := "```json\n{\"title\": \"Fix timezone handling\", \"summary\": \"Fixed it.\", \"review_priority\": []}\n```"

	out, err := ParseSuggestImplementationOutput(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if out.Title != "Fix timezone handling" {
		t.Errorf("title: got %q", out.Title)
	}
}

func TestParseSuggestImplementationOutput_EmptyTitle(t *testing.T) {
	raw := `{"title": "", "summary": "stuff", "review_priority": []}`

	_, err := ParseSuggestImplementationOutput(raw)
	if err == nil {
		t.Error("expected error for empty title")
	}
}

func TestParseSuggestMergeCandidatesOutput(t *testing.T) {
	raw := `{
		"titles": [
			{"implementation_id": "abc-123", "title": "Add rate limiting"}
		],
		"merges": [
			{"implementation_a": "abc-123", "implementation_b": "def-456", "reason": "Same auth migration effort"}
		]
	}`

	out, err := ParseSuggestMergeCandidatesOutput(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out.Titles) != 1 {
		t.Errorf("titles: got %d", len(out.Titles))
	}
	if out.Titles[0].Title != "Add rate limiting" {
		t.Errorf("title: got %q", out.Titles[0].Title)
	}
	if len(out.Merges) != 1 {
		t.Errorf("merges: got %d", len(out.Merges))
	}
	if out.Merges[0].Reason != "Same auth migration effort" {
		t.Errorf("reason: got %q", out.Merges[0].Reason)
	}
}

func TestBuildSuggestImplementationPrompt_ContainsContext(t *testing.T) {
	prompt := BuildSuggestImplementationPrompt(
		"active",
		"api (origin), sdk (downstream)",
		3,
		"45.2k", "3.8k",
		"api abc123\nsdk def456",
		"api edit auth/middleware.go\n→ sdk edit client/auth.py",
	)

	for _, want := range []string{
		"active",
		"api (origin), sdk (downstream)",
		"45.2k",
		"abc123",
		"auth/middleware.go",
	} {
		if !contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && searchString(s, sub)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
