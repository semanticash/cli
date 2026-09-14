package commands

import (
	"bytes"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/service"
)

func TestAttributionCountsDoNotCallCaptureGapsHuman(t *testing.T) {
	for _, status := range []string{"pending", "incomplete"} {
		var out bytes.Buffer
		writeAttributionCounts(&out, &service.AttributionResult{
			Capture: &service.CaptureReadiness{Status: status}, UnattributedLines: 756,
			AIExactLines: 240, AILines: 240, TotalLines: 996, AIPercentage: 24.1,
		})
		text := out.String()
		if strings.Contains(text, "Human:") || strings.Contains(text, "AI %:") || !strings.Contains(text, "Unattributed: 756") || !strings.Contains(text, "AI matched:   24.1%") {
			t.Fatalf("misleading incomplete capture output:\n%s", text)
		}
	}
}

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
