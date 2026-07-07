package intentgap

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The dropped-finding diagnostic carries kind, schema error, and the
// top-level key list - never the raw finding bytes. This guards
// against prompt excerpts, file paths, or code snippets leaking
// through the local activity log.
func TestFilterFindingsBySchema_DiagnosticIsStructuralOnly(t *testing.T) {
	// Invalid because ai_authored_regions_checked must be an array.
	bad := `[{
		"schema_version":"1",
		"finding_id":"f_0000000000000000",
		"kind":"under_impl",
		"title":"t",
		"confidence":"medium",
		"expected_intent":{"summary":"SECRET SUMMARY","turn_id":"t-1","prompt_excerpt":"PRIVATE PROMPT","prompt_excerpt_hash":"h-1"},
		"observed_diff_evidence":{"summary":"another secret","ai_authored_regions_checked":true},
		"missing_or_partial_area":{"note":"do not leak"}
	}]`
	result := FilterFindingsBySchema(json.RawMessage(bad))
	if len(result.DroppedSamples) != 1 {
		t.Fatalf("DroppedSamples len = %d, want 1", len(result.DroppedSamples))
	}
	sample := result.DroppedSamples[0]
	for _, leak := range []string{"SECRET SUMMARY", "PRIVATE PROMPT", "another secret", "do not leak"} {
		if strings.Contains(sample, leak) {
			t.Errorf("diagnostic leaked %q: %s", leak, sample)
		}
	}
	if !strings.Contains(sample, "kind=under_impl") {
		t.Errorf("diagnostic missing kind: %s", sample)
	}
	if !strings.Contains(sample, "keys=[") {
		t.Errorf("diagnostic missing top-level keys list: %s", sample)
	}
}

// ReasonCodeFor returns stable labels without exposing local error details.
func TestReasonCodeFor_StableLabels(t *testing.T) {
	cases := []struct {
		err  error
		want ReasonCode
	}{
		{fmt.Errorf("wrap: %w", ErrAnalyzerLLMUnavailable), ReasonLLMUnavailable},
		{fmt.Errorf("wrap: %w", ErrAnalyzerParseFailed), ReasonParseFailed},
		{fmt.Errorf("wrap: %w", ErrAnalyzerSchemaFailed), ReasonSchemaFailed},
		{fmt.Errorf("wrap: %w", ErrIntentClassifierFailed), ReasonIntentClassificationFailed},
		{errors.New("some other failure"), ReasonAnalyzerInternal},
	}
	for _, tc := range cases {
		got := ReasonCodeFor(tc.err)
		if got != tc.want {
			t.Errorf("ReasonCodeFor(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// The prompt template version is bumped to reflect the candidate-first
// pipeline. The value is part of the AnalysisCache key, so bumping here
// invalidates cached analyses from previous analyzer versions.
func TestPromptTemplateVersion_CandidateFirstV1(t *testing.T) {
	if PromptTemplateVersion != "0.3.0-candidate-first-v1" {
		t.Errorf("PromptTemplateVersion = %q, want 0.3.0-candidate-first-v1", PromptTemplateVersion)
	}
}
