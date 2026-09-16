package commands

import (
	"bytes"
	"encoding/json"
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

func TestCapturePresentationReflectsUnattributedLines(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		status                  string
		total, ai, unattributed int
		want, absent            string
	}{
		{"fully_matched", "incomplete", 244, 244, 0, "All 244 changed lines matched AI evidence; no lines were left unattributed.", "Capture:"},
		{"partial", "incomplete", 100, 20, 80, "Capture:      incomplete (80 lines have unknown authorship)", "All 100"},
		{"rounded_percentage", "incomplete", 100000, 99999, 1, "Capture:      incomplete (1 line has unknown authorship)", "All 100000"},
		{"pending", "pending", 244, 244, 0, "Some command capture evidence is still pending.", "is unavailable"},
		{"no_lines", "incomplete", 0, 0, 0, "Some command capture evidence is unavailable.", "All 0"},
		{"complete", "complete", 100, 100, 0, "Human:        0 lines", "command capture evidence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := &service.AttributionResult{
				Capture:    &service.CaptureReadiness{Status: tc.status, Gaps: []service.CaptureGap{{Reason: "completion_missing"}}},
				TotalLines: tc.total, AILines: tc.ai, UnattributedLines: tc.unattributed, AIPercentage: 100,
				Diagnostics: service.AttributionDiagnostics{Notes: []string{"Existing evidence note."}},
			}
			if tc.status != "complete" {
				res.Diagnostics.Notes = append(res.Diagnostics.Notes, "Capture is incomplete. Unmatched lines are unattributed, not confirmed human changes.")
			}
			before, err := json.Marshal(res)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			writeAttributionCounts(&out, res)
			writeAttributionNotes(&out, res)
			text := out.String()
			if !strings.Contains(text, tc.want) || strings.Contains(text, tc.absent) || strings.Contains(text, "Capture is incomplete.") || !strings.Contains(text, "Existing evidence note.") {
				t.Fatalf("unexpected presentation:\n%s", text)
			}
			after, err := json.Marshal(res)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("text rendering changed JSON diagnostics")
			}
		})
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
