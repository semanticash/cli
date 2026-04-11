package reporting

import "testing"

func TestRenderDiagnosticNote_NoEvents(t *testing.T) {
	got := RenderDiagnosticNote(DiagnosticsInput{
		EventStats: EventStatsInput{EventsConsidered: 0},
	})
	want := "No agent events found in the delta window between checkpoints."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderDiagnosticNote_NoToolEvents(t *testing.T) {
	got := RenderDiagnosticNote(DiagnosticsInput{
		EventStats: EventStatsInput{
			EventsConsidered: 5,
			EventsAssistant:  3,
			AIToolEvents:     0,
		},
	})
	want := "Agent events found but none contained file-modifying tool calls (Edit/Write)."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderDiagnosticNote_NoPayloads(t *testing.T) {
	got := RenderDiagnosticNote(DiagnosticsInput{
		EventStats: EventStatsInput{
			EventsConsidered: 5,
			AIToolEvents:     2,
			PayloadsLoaded:   0,
		},
	})
	want := "Agent tool calls found but payloads could not be loaded from blob store."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderDiagnosticNote_NoMatchingLines(t *testing.T) {
	got := RenderDiagnosticNote(DiagnosticsInput{
		EventStats: EventStatsInput{
			EventsConsidered: 5,
			AIToolEvents:     2,
			PayloadsLoaded:   2,
		},
	})
	want := "Agent tool calls found but no added lines in the commit matched AI-produced output."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderDiagnosticNote_MixedMatches(t *testing.T) {
	got := RenderDiagnosticNote(DiagnosticsInput{
		AIPercent: 73.3,
		MatchStats: MatchStatsInput{
			ExactMatches:      10,
			NormalizedMatches: 3,
			ModifiedMatches:   2,
		},
	})
	want := "AI matches: 10 exact, 3 normalized, 2 modified."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderDiagnosticNote_ExactOnly(t *testing.T) {
	got := RenderDiagnosticNote(DiagnosticsInput{
		AIPercent: 100,
		MatchStats: MatchStatsInput{
			ExactMatches: 15,
		},
	})
	// No normalized or modified -> empty note (all exact is the happy path).
	if got != "" {
		t.Errorf("got %q, want empty (all exact matches)", got)
	}
}

func TestRenderDiagnosticNote_NormalizedOnly(t *testing.T) {
	got := RenderDiagnosticNote(DiagnosticsInput{
		AIPercent: 50,
		MatchStats: MatchStatsInput{
			ExactMatches:      5,
			NormalizedMatches: 3,
		},
	})
	want := "AI matches: 5 exact, 3 normalized."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
