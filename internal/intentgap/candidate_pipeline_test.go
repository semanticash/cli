package intentgap

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/llm"
)

// pipelineRunner returns responses in call order. Each call records
// its prompt so tests can inspect which step ran and what it saw.
type pipelineRunner struct {
	responses []*llm.GenerateTextResult
	prompts   []string
	idx       int
}

func (p *pipelineRunner) GenerateText(_ context.Context, prompt string) (*llm.GenerateTextResult, error) {
	p.prompts = append(p.prompts, prompt)
	if p.idx >= len(p.responses) {
		// A missing scripted response is a test-fixture bug; return a
		// verifier-shaped needs_more_context so the pool does not crash
		// and the fixture failure surfaces as a clear assertion below.
		return &llm.GenerateTextResult{Text: `{"verdict":"needs_more_context","rationale":""}`}, nil
	}
	res := p.responses[p.idx]
	p.idx++
	return res, nil
}

// A bundle with no captured turns short-circuits before the
// classifier runs. The result is a successful empty analysis
// carrying the algorithm marker so downstream consumers know a
// candidate-first pipeline was invoked even on an empty run.
func TestRunCandidateFirstAnalyzer_NoTurnsIsEmpty(t *testing.T) {
	runner := &pipelineRunner{}
	got, err := RunCandidateFirstAnalyzer(context.Background(),
		AnalysisInput{Bundle: Bundle{}, RepositoryID: "repo", PRNumber: 1}, runner)
	if err != nil {
		t.Fatalf("RunCandidateFirstAnalyzer: %v", err)
	}
	if string(got.Findings) != "[]" {
		t.Errorf("Findings = %s, want []", string(got.Findings))
	}
	var cov map[string]any
	_ = json.Unmarshal(got.CoverageSummary, &cov)
	if cov["algorithm"] != AlgorithmCandidateFirst {
		t.Errorf("algorithm = %v, want %q", cov["algorithm"], AlgorithmCandidateFirst)
	}
	if cov["skipped"] != true {
		t.Errorf("skipped = %v, want true", cov["skipped"])
	}
	if len(runner.prompts) != 0 {
		t.Errorf("no LLM calls expected on empty-turns short-circuit; got %d", len(runner.prompts))
	}
}

// A canned classification + a Track B accept flows through the whole
// pipeline: intent ledger populates, retrieval matches the changed
// file, candidate generator fires Track B on missing-test category,
// verifier accepts, adjudicator renders one wire finding. The
// coverage_summary carries every counter the plan documents.
func TestRunCandidateFirstAnalyzer_EndToEndTrackB(t *testing.T) {
	classifierResp := `[
		{"turn_id":"t-1","kind":"request","summary":"user asks to add tests for handler",
		 "hint":{"files":["handler.go"],"keywords":["tests","handler"]}}
	]`
	verifierResp := `{
		"verdict":"accept",
		"rationale":"handler edit landed but no test file changed alongside it",
		"acceptance":{
			"primary_file":"handler.go",
			"regions":[{"file":"handler.go","start":12,"end":14}]
		}
	}`
	runner := &pipelineRunner{responses: []*llm.GenerateTextResult{
		{Text: classifierResp, Provider: "claude_code", Model: "sonnet"},
		{Text: verifierResp, Provider: "claude_code", Model: "sonnet"},
	}}

	in := AnalysisInput{
		Bundle: Bundle{
			BaseSHA: "base", HeadSHA: "head",
			Turns: []BundleTurn{{
				TurnID:            "t-1",
				CommitHash:        "c1",
				PromptExcerpt:     "add tests for handler",
				PromptExcerptHash: "h-1",
			}},
			Diff: []byte("--- a/handler.go\n+++ b/handler.go\n@@ -10,5 +10,11 @@\n line\n+added\n+added2\n line\n line\n"),
		},
		RepositoryID: "repo",
		PRNumber:     42,
	}

	got, err := RunCandidateFirstAnalyzer(context.Background(), in, runner)
	if err != nil {
		t.Fatalf("RunCandidateFirstAnalyzer: %v", err)
	}
	if got.Provider != "claude_code" {
		t.Errorf("Provider = %q, want claude_code", got.Provider)
	}
	if got.Model != "sonnet" {
		t.Errorf("Model = %q, want sonnet", got.Model)
	}
	if got.PromptTemplateVersion != PromptTemplateVersion {
		t.Errorf("PromptTemplateVersion = %q, want %q", got.PromptTemplateVersion, PromptTemplateVersion)
	}

	var arr []map[string]any
	if err := json.Unmarshal(got.Findings, &arr); err != nil {
		t.Fatalf("Findings not parseable: %v", err)
	}
	if len(arr) != 1 {
		t.Fatalf("Findings len = %d, want 1; got: %s", len(arr), got.Findings)
	}
	if arr[0]["kind"] != "under_impl" {
		t.Errorf("kind = %v, want under_impl", arr[0]["kind"])
	}

	var cov map[string]any
	_ = json.Unmarshal(got.CoverageSummary, &cov)
	if cov["algorithm"] != AlgorithmCandidateFirst {
		t.Errorf("algorithm = %v", cov["algorithm"])
	}
	// The verifier accept lands in the verdict histogram.
	verdicts, _ := cov["verifier_verdicts"].(map[string]any)
	if verdicts["accept"] != float64(1) {
		t.Errorf("verifier_verdicts[accept] = %v, want 1", verdicts["accept"])
	}
	// Exactly one intent classified.
	if cov["intent_items_total"] != float64(1) {
		t.Errorf("intent_items_total = %v, want 1", cov["intent_items_total"])
	}
}

// Track A accepts do not become wire findings but do surface in
// coverage_summary.track_a_intents so the developer sees which asks
// the analyzer believes went unaddressed.
func TestRunCandidateFirstAnalyzer_TrackAAcceptSurfacesInCoverageOnly(t *testing.T) {
	classifierResp := `[
		{"turn_id":"t-1","kind":"request","summary":"user asks about migration strategy",
		 "hint":{"files":[],"keywords":["migration","strategy"]}}
	]`
	verifierResp := `{
		"verdict":"accept",
		"rationale":"intent asks about migration but nothing in the diff addresses it",
		"acceptance":{"primary_file":"missing.go"}
	}`
	runner := &pipelineRunner{responses: []*llm.GenerateTextResult{
		{Text: classifierResp},
		{Text: verifierResp},
	}}

	in := AnalysisInput{
		Bundle: Bundle{
			Turns: []BundleTurn{{
				TurnID:            "t-1",
				CommitHash:        "c1",
				PromptExcerpt:     "how should we handle migration",
				PromptExcerptHash: "h-1",
			}},
			// Empty diff: retrieval returns nothing, Track A fires.
			Diff: nil,
		},
		RepositoryID: "repo",
		PRNumber:     42,
	}

	got, err := RunCandidateFirstAnalyzer(context.Background(), in, runner)
	if err != nil {
		t.Fatalf("RunCandidateFirstAnalyzer: %v", err)
	}
	if string(got.Findings) != "[]" {
		t.Errorf("Track A must not produce wire findings; got %s", got.Findings)
	}
	var cov map[string]any
	_ = json.Unmarshal(got.CoverageSummary, &cov)
	if cov["track_a_candidates_accepted"] != float64(1) {
		t.Errorf("track_a_candidates_accepted = %v, want 1", cov["track_a_candidates_accepted"])
	}
	diags, ok := cov["track_a_intents"].([]any)
	if !ok || len(diags) != 1 {
		t.Errorf("track_a_intents missing or wrong shape; got %v", cov["track_a_intents"])
	}
}

// A run where the pre-filter drops some turns and one batch fails
// surfaces every batching counter in coverage_summary. The pipeline
// otherwise completes so the wire body still uploads.
func TestRunCandidateFirstAnalyzer_BatchingCoverageSurfaced(t *testing.T) {
	// Build a bundle with 5 acknowledgement turns (all prefiltered)
	// plus enough substantive turns to force two classifier batches.
	total := IntentClassifierBatchSize + 2
	turns := []BundleTurn{
		{TurnID: "ack-1", PromptExcerpt: "ok", PromptExcerptHash: "ha1"},
		{TurnID: "ack-2", PromptExcerpt: "yes", PromptExcerptHash: "ha2"},
	}
	for i := 0; i < total; i++ {
		turns = append(turns, BundleTurn{
			TurnID:            "sub-" + strings.Repeat("x", 1) + "-" + itoaBatched(i),
			PromptExcerpt:     "a substantive prompt that is long enough to bypass prefilter " + itoaBatched(i),
			PromptExcerptHash: "hh-" + itoaBatched(i),
		})
	}
	// One batch of the two returns non-JSON text so BatchesFailed=1 lands.
	runner := &splitClassifierRunner{
		failIndex:      1,
		goodResponders: &syntheticClassifierRunner{},
	}
	got, err := RunCandidateFirstAnalyzer(context.Background(),
		AnalysisInput{Bundle: Bundle{Turns: turns}, RepositoryID: "repo", PRNumber: 42}, runner)
	if err != nil {
		t.Fatalf("RunCandidateFirstAnalyzer: %v", err)
	}
	var cov map[string]any
	if err := json.Unmarshal(got.CoverageSummary, &cov); err != nil {
		t.Fatalf("coverage_summary not JSON: %v", err)
	}
	if cov["intent_turns_prefiltered"] != float64(2) {
		t.Errorf("intent_turns_prefiltered = %v, want 2", cov["intent_turns_prefiltered"])
	}
	if cov["intent_batches_total"] != float64(2) {
		t.Errorf("intent_batches_total = %v, want 2", cov["intent_batches_total"])
	}
	if cov["intent_batches_failed"] != float64(1) {
		t.Errorf("intent_batches_failed = %v, want 1", cov["intent_batches_failed"])
	}
}

func itoaBatched(i int) string {
	return string(rune('a'+i%26)) + string(rune('0'+(i/10)%10)) + string(rune('0'+i%10))
}

// A classifier failure (LLM unavailable) surfaces as an error so the
// service records an errored row upstream.
func TestRunCandidateFirstAnalyzer_ClassifierFailureIsHardError(t *testing.T) {
	runner := &pipelineRunner{responses: []*llm.GenerateTextResult{
		nil, // classifier returns nil
	}}
	// Wrap in a runner that also returns an error for the classifier call.
	failingRunner := &failingClassifierRunner{}

	in := AnalysisInput{
		Bundle: Bundle{
			Turns: []BundleTurn{{
				TurnID:            "t-1",
				PromptExcerpt:     "add x",
				PromptExcerptHash: "h-1",
			}},
		},
	}
	_, err := RunCandidateFirstAnalyzer(context.Background(), in, failingRunner)
	if err == nil {
		t.Fatalf("classifier failure must surface as an error; got nil")
	}
	_ = runner // only wrapper used
}

// failingClassifierRunner always returns an error so the classifier
// path surfaces the failure.
type failingClassifierRunner struct{}

func (failingClassifierRunner) GenerateText(context.Context, string) (*llm.GenerateTextResult, error) {
	return nil, errClassifierBoom
}

var errClassifierBoom = &verifierBoomErr{"provider unavailable"}

type verifierBoomErr struct{ msg string }

func (e *verifierBoomErr) Error() string { return e.msg }

// captureRunner records the first non-empty Provider/Model across
// every call, aggregates the registry's fallback errors, and folds
// UTF-8 sanitization stats into the AnalysisResult so the service-
// side logging surface stays the same across analyzer versions.
func TestRunCandidateFirstAnalyzer_CaptureRunnerAggregatesAttribution(t *testing.T) {
	classifier := &llm.GenerateTextResult{
		Text:                 `[{"turn_id":"t-1","kind":"request","summary":"add tests"}]`,
		Provider:             "claude_code",
		Model:                "sonnet",
		FallbackErrors:       []string{"prev_writer: timeout"},
		PromptBadByteCount:   2,
		PromptBadByteOffsets: []int{15486, 15487},
	}
	verifier := &llm.GenerateTextResult{
		Text: `{"verdict":"drop","drop_reason":"intent_too_vague","rationale":"x"}`,
	}
	runner := &pipelineRunner{responses: []*llm.GenerateTextResult{classifier, verifier}}

	in := AnalysisInput{
		Bundle: Bundle{
			Turns: []BundleTurn{{
				TurnID:            "t-1",
				PromptExcerpt:     "add tests for the handler",
				PromptExcerptHash: "h-1",
			}},
			Diff: []byte("--- a/handler.go\n+++ b/handler.go\n@@ -10,5 +10,11 @@\n line\n+added\n+added2\n line\n line\n"),
		},
		RepositoryID: "repo",
		PRNumber:     42,
	}

	got, err := RunCandidateFirstAnalyzer(context.Background(), in, runner)
	if err != nil {
		t.Fatalf("RunCandidateFirstAnalyzer: %v", err)
	}
	if got.Provider == "" {
		t.Errorf("Provider not captured")
	}
	if got.PromptBadByteCount != 2 {
		t.Errorf("PromptBadByteCount = %d, want 2", got.PromptBadByteCount)
	}
	if len(got.PromptBadByteOffsets) != 2 {
		t.Errorf("PromptBadByteOffsets len = %d, want 2", len(got.PromptBadByteOffsets))
	}
	if len(got.ProviderFallbackErrors) == 0 ||
		!strings.Contains(got.ProviderFallbackErrors[0], "prev_writer") {
		t.Errorf("fallback errors not surfaced; got %v", got.ProviderFallbackErrors)
	}
}
