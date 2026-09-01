package service

import "testing"

func TestDistinctSessionProviders(t *testing.T) {
	sessions := []SessionSummary{
		{Provider: "cursor", Model: "grok-4.6"},
		{Provider: "cursor", Model: "grok-4.6"},
		{Provider: "codex", Model: "gpt-5.6-sol"},
		{},
	}

	assertStringsEqual(t, DistinctSessionProviders(sessions), []string{
		"codex (gpt-5.6-sol)",
		"cursor (grok-4.6)",
	})

	sessions[1].Model = "claude-4.6"
	assertStringsEqual(t, DistinctSessionProviders(sessions), []string{
		"codex (gpt-5.6-sol)",
		"cursor",
	})

	if got := DistinctSessionProviders(nil); got != nil {
		t.Fatalf("nil sessions = %v, want nil", got)
	}
}

func assertStringsEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("labels = %v, want %v", got, want)
		}
	}
}
