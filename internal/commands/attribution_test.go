package commands

import (
	"testing"

	"github.com/semanticash/cli/internal/service"
)

func TestAttributionAgentLabels(t *testing.T) {
	details := []service.ProviderAttribution{
		{Provider: "codex", Model: "gpt-5.6-sol"},
		{Provider: "claude_code", Model: "Opus 4.6"},
		{Provider: "cursor"},
		{Provider: ""},
		{Provider: "cursor"},
	}

	got := attributionAgentLabels(details)
	want := []string{
		"Codex (gpt-5.6-sol)",
		"Claude Code (Opus 4.6)",
		"Cursor",
	}
	if len(got) != len(want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("labels = %v, want %v", got, want)
		}
	}
}
