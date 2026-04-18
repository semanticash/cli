package reporting

import (
	"reflect"
	"testing"
)

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

// --- AssembleCommitNotes ---

// Nothing to say: no pipeline note, no fallback count, no evidence
// classes that trigger factual notes. Returns nil so the response-level
// omitempty drops the field.
func TestAssembleCommitNotes_NothingToSay(t *testing.T) {
	got := AssembleCommitNotes("", CommitResult{})
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

// Pipeline-state note only: when the attribution pipeline produces a
// diagnostic message but no factual notes apply, Notes carries exactly
// one entry (the pipeline message).
func TestAssembleCommitNotes_PipelineNoteOnly(t *testing.T) {
	got := AssembleCommitNotes("No agent events found.", CommitResult{})
	want := []string{"No agent events found."}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// FallbackCount produces a factual note pinned to the actual count.
// No pipeline note: the factual note is the only entry.
func TestAssembleCommitNotes_FallbackCountOnly(t *testing.T) {
	got := AssembleCommitNotes("", CommitResult{FallbackCount: 3})
	want := []string{"3 file(s) attributed using weaker fallback signals."}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Carry-forward and deletion each trigger at most once regardless of
// how many files exhibit the class. Order is: fallback, carry-forward,
// deletion - stable so downstream formatting does not see ordering
// churn between runs on the same data.
func TestAssembleCommitNotes_MultipleEvidenceClasses(t *testing.T) {
	cr := CommitResult{
		FallbackCount: 2,
		Files: []FileAttributionOutput{
			{Path: "a.go", PrimaryEvidence: EvidenceCarryForward},
			{Path: "b.go", PrimaryEvidence: EvidenceCarryForward}, // second instance: still just one note
			{Path: "c.go", PrimaryEvidence: EvidenceDeletion},
		},
	}
	got := AssembleCommitNotes("pipeline message", cr)
	want := []string{
		"pipeline message",
		"2 file(s) attributed using weaker fallback signals.",
		"Attribution includes historical carry-forward.",
		"Some file attribution is inferred from deletion events.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// --- AssembleCheckpointNotes ---

func TestAssembleCheckpointNotes_Empty(t *testing.T) {
	if got := AssembleCheckpointNotes(""); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestAssembleCheckpointNotes_NonEmpty(t *testing.T) {
	got := AssembleCheckpointNotes("No agent events found.")
	want := []string{"No agent events found."}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
