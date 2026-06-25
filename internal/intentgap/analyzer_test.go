package intentgap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/llm"
)

// fakeLLMRunner returns a canned sequence of responses, one per call.
// Each call advances the cursor; tests pin which response shape the
// analyzer is exercising (first attempt only vs first attempt + retry).
type fakeLLMRunner struct {
	responses []*llm.GenerateTextResult
	errs      []error
	calls     int
	prompts   []string
}

func (f *fakeLLMRunner) GenerateText(_ context.Context, prompt string) (*llm.GenerateTextResult, error) {
	idx := f.calls
	f.calls++
	f.prompts = append(f.prompts, prompt)
	if idx >= len(f.responses) {
		return nil, errors.New("fakeLLMRunner: ran out of canned responses")
	}
	if idx < len(f.errs) && f.errs[idx] != nil {
		return nil, f.errs[idx]
	}
	return f.responses[idx], nil
}

func canned(text string) *llm.GenerateTextResult {
	return &llm.GenerateTextResult{Text: text, Provider: "claude_code", Model: "claude-opus-4-7"}
}

// validDeferredJSON returns a single-finding array whose citations
// match the canonicalBundle helper defined in citeordrop_test.go.
// Used by analyzer happy-path tests that need the pipeline to survive
// schema validation AND cite-or-drop.
func validDeferredJSON() string {
	return `[` + deferredFinding("t-1", "add input validation", "h-1", "handler.go", lineRange{12, 14}) + `]`
}

// sampleInput pairs the canonicalBundle with a PR number; analyzer
// tests that need a richer bundle replace the field.
func sampleInput() AnalysisInput {
	b := canonicalBundle()
	b.BaseRef = "main"
	b.BaseSHA = "base-sha"
	b.HeadSHA = "head-sha"
	b.Commits = []BundleCommit{{Hash: "c1", Subject: "first"}}
	return AnalysisInput{PRNumber: 42, Bundle: b}
}

// Finding IDs are stamped before schema validation because the CLI owns them.
func TestLLMAnalyzer_StampsBeforeSchemaValidation(t *testing.T) {
	// Omit finding_id to verify that stamping supplies it before validation.
	withoutID := `[{
		"schema_version":"1",
		"kind":"deferred",
		"title":"t",
		"confidence":"medium",
		"originally_requested_in":{"turn_id":"t-1","prompt_excerpt":"add input validation","prompt_excerpt_hash":"h-1"},
		"trajectory_note":"n",
		"current_state":{"file":"handler.go","line_range":[12,14],"summary":"s"}
	}]`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{canned(withoutID)}}
	a := NewLLMAnalyzer(runner)
	in := sampleInput()
	in.RepositoryID = "repo-abc"

	res, err := a.Analyze(context.Background(), in)
	if err != nil {
		t.Fatalf("Analyze should succeed when model omits finding_id (stamp runs before schema validation): %v", err)
	}
	var arr []map[string]any
	if err := json.Unmarshal(res.Findings, &arr); err != nil {
		t.Fatalf("findings not parseable: %v", err)
	}
	gotID, _ := arr[0]["finding_id"].(string)
	wantID := canonicalFindingIDForDeferred("repo-abc", in.PRNumber, "t-1", "h-1", "handler.go", 12, 14)
	if gotID != wantID {
		t.Errorf("finding_id = %q, want %q", gotID, wantID)
	}
}

// The analyzer replaces model-provided IDs with the canonical derivation.
func TestLLMAnalyzer_OverwritesFindingIDFromCanonicalAnchors(t *testing.T) {
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{canned(validDeferredJSON())}}
	a := NewLLMAnalyzer(runner)

	in := sampleInput()
	in.RepositoryID = "repo-abc"
	res, err := a.Analyze(context.Background(), in)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	var arr []map[string]any
	if err := json.Unmarshal(res.Findings, &arr); err != nil {
		t.Fatalf("findings not parseable: %v", err)
	}
	gotID, _ := arr[0]["finding_id"].(string)
	wantID := canonicalFindingIDForDeferred("repo-abc", in.PRNumber, "t-1", "h-1", "handler.go", 12, 14)
	if gotID != wantID {
		t.Errorf("finding_id = %q, want %q (placeholder must be rewritten)", gotID, wantID)
	}
	if gotID == placeholderFindingID {
		t.Errorf("placeholder leaked through analyzer: %q", gotID)
	}
}

// A valid first response preserves findings and analyzer attribution.
func TestLLMAnalyzer_HappyPath(t *testing.T) {
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{canned(validDeferredJSON())}}
	a := NewLLMAnalyzer(runner)

	res, err := a.Analyze(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if res.Provider != "claude_code" || res.Model != "claude-opus-4-7" {
		t.Errorf("provider/model not surfaced: %+v", res)
	}
	if res.PromptTemplateVersion != PromptTemplateVersion {
		t.Errorf("PromptTemplateVersion = %q, want %q", res.PromptTemplateVersion, PromptTemplateVersion)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(res.Findings, &arr); err != nil {
		t.Fatalf("findings not valid JSON array: %v", err)
	}
	if len(arr) != 1 {
		t.Errorf("findings len = %d, want 1", len(arr))
	}
}

// An empty finding set is a successful analysis result.
func TestLLMAnalyzer_EmptyFindings(t *testing.T) {
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{canned("[]")}}
	a := NewLLMAnalyzer(runner)

	res, err := a.Analyze(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if string(res.Findings) != "[]" {
		t.Errorf("findings = %q, want []", string(res.Findings))
	}
}

// Accept JSON wrapped in Markdown code fences.
func TestLLMAnalyzer_StripsCodeFences(t *testing.T) {
	wrapped := "Here are the findings:\n\n```json\n" + validDeferredJSON() + "\n```\n"
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{canned(wrapped)}}
	a := NewLLMAnalyzer(runner)

	res, err := a.Analyze(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(res.Findings, &arr); err != nil {
		t.Fatalf("findings not parseable: %v", err)
	}
	if len(arr) != 1 {
		t.Errorf("findings len = %d, want 1", len(arr))
	}
}

// Leading prose then a JSON array. The extractor's first-array fallback
// pulls it out.
func TestLLMAnalyzer_ExtractsFromInlineProse(t *testing.T) {
	wrapped := "Here is the analysis:\n" + validDeferredJSON() + "\nThanks!\n"
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{canned(wrapped)}}
	a := NewLLMAnalyzer(runner)

	if _, err := a.Analyze(context.Background(), sampleInput()); err != nil {
		t.Fatalf("Analyze: %v", err)
	}
}

// An invalid response receives one strict-format retry.
func TestLLMAnalyzer_RetryOnParseFailure(t *testing.T) {
	runner := &fakeLLMRunner{
		responses: []*llm.GenerateTextResult{
			canned("Sure, here you go. But I forgot what JSON is."),
			canned(validDeferredJSON()),
		},
	}
	a := NewLLMAnalyzer(runner)

	if _, err := a.Analyze(context.Background(), sampleInput()); err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if runner.calls != 2 {
		t.Errorf("calls = %d, want 2 (initial + retry)", runner.calls)
	}
	if !strings.Contains(runner.prompts[1], "Reply with ONLY the JSON array") {
		t.Errorf("retry prompt should instruct strict JSON; got: %q", runner.prompts[1])
	}
}

// A successful retry is attributed to the writer that produced it.
func TestLLMAnalyzer_RetryProviderWins(t *testing.T) {
	runner := &fakeLLMRunner{
		responses: []*llm.GenerateTextResult{
			{Text: "not json", Provider: "claude_code", Model: "claude-opus-4-7"},
			{Text: validDeferredJSON(), Provider: "codex", Model: "gpt-5"},
		},
	}
	a := NewLLMAnalyzer(runner)

	res, err := a.Analyze(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if res.Provider != "codex" {
		t.Errorf("Provider = %q, want codex (the writer that actually parsed)", res.Provider)
	}
	if res.Model != "gpt-5" {
		t.Errorf("Model = %q, want gpt-5", res.Model)
	}
}

// Two invalid responses produce a parse failure.
func TestLLMAnalyzer_RetryAlsoFailsSurfacesAsParseError(t *testing.T) {
	runner := &fakeLLMRunner{
		responses: []*llm.GenerateTextResult{
			canned("not json"),
			canned("still not json"),
		},
	}
	a := NewLLMAnalyzer(runner)

	_, err := a.Analyze(context.Background(), sampleInput())
	if !errors.Is(err, ErrAnalyzerParseFailed) {
		t.Fatalf("err should wrap ErrAnalyzerParseFailed; got %v", err)
	}
}

// Writer failures produce an unavailable error.
func TestLLMAnalyzer_LLMCallFailsSurfacesAsUnavailable(t *testing.T) {
	runner := &fakeLLMRunner{errs: []error{errors.New("no providers")}, responses: []*llm.GenerateTextResult{nil}}
	a := NewLLMAnalyzer(runner)

	_, err := a.Analyze(context.Background(), sampleInput())
	if !errors.Is(err, ErrAnalyzerLLMUnavailable) {
		t.Fatalf("err should wrap ErrAnalyzerLLMUnavailable; got %v", err)
	}
}

// Schema-invalid findings produce a schema failure.
func TestLLMAnalyzer_BadSchemaSurfacesAsSchemaError(t *testing.T) {
	bad := `[{"whatever": true}]`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{canned(bad)}}
	a := NewLLMAnalyzer(runner)

	_, err := a.Analyze(context.Background(), sampleInput())
	if !errors.Is(err, ErrAnalyzerSchemaFailed) {
		t.Fatalf("err should wrap ErrAnalyzerSchemaFailed; got %v", err)
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
		{errors.New("some other failure"), ReasonAnalyzerInternal},
	}
	for _, tc := range cases {
		got := ReasonCodeFor(tc.err)
		if got != tc.want {
			t.Errorf("ReasonCodeFor(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// Local writer names are mapped to API provider values.
func TestLLMAnalyzer_MapsProviderToWireEnum(t *testing.T) {
	cases := []struct {
		writerName string
		wantWire   string
	}{
		{"cursor", "cursor_cli"},
		{"copilot", "copilot_cli"},
		{"claude_code", "claude_code"}, // already wire-compatible
	}
	for _, tc := range cases {
		t.Run(tc.writerName, func(t *testing.T) {
			runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{
				{Text: "[]", Provider: tc.writerName, Model: "m"},
			}}
			a := NewLLMAnalyzer(runner)
			res, err := a.Analyze(context.Background(), sampleInput())
			if err != nil {
				t.Fatalf("Analyze: %v", err)
			}
			if res.Provider != tc.wantWire {
				t.Errorf("Provider = %q, want %q", res.Provider, tc.wantWire)
			}
		})
	}
}

// Coverage metadata includes bundle size and truncation counts.
func TestLLMAnalyzer_CoverageSummaryReflectsBundle(t *testing.T) {
	in := sampleInput()
	in.Bundle.Truncated = BundleTruncation{DiffBytesDropped: 4096, CommitsDropped: 3}
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{canned("[]")}}
	a := NewLLMAnalyzer(runner)

	res, err := a.Analyze(context.Background(), in)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	var cov map[string]int
	if err := json.Unmarshal(res.CoverageSummary, &cov); err != nil {
		t.Fatalf("CoverageSummary not JSON object: %v", err)
	}
	if cov["commits_dropped"] != 3 || cov["diff_bytes_dropped"] != 4096 {
		t.Errorf("truncation counts not surfaced: %+v", cov)
	}
}

// Agent action counts are reported in coverage_summary without
// including the actions themselves in the upload.
func TestLLMAnalyzer_CoverageSummaryReportsAgentActionCounts(t *testing.T) {
	in := sampleInput()
	in.Bundle.AgentActions = []BundleAgentAction{
		{ActionID: "a1", TurnID: "t1", ToolName: "Edit", FilePath: "a.go"},
		{ActionID: "a2", TurnID: "t1", ToolName: "Edit", FilePath: "b.go"},
	}
	in.Bundle.Truncated.AgentActionsDropped = 7
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{canned("[]")}}
	a := NewLLMAnalyzer(runner)

	res, err := a.Analyze(context.Background(), in)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	var cov map[string]int
	if err := json.Unmarshal(res.CoverageSummary, &cov); err != nil {
		t.Fatalf("CoverageSummary not JSON object: %v", err)
	}
	if cov["agent_actions_count"] != 2 {
		t.Errorf("agent_actions_count = %d, want 2", cov["agent_actions_count"])
	}
	if cov["agent_actions_dropped"] != 7 {
		t.Errorf("agent_actions_dropped = %d, want 7", cov["agent_actions_dropped"])
	}
}

// The retry prompt includes the previous response for correction.
func TestReformatPrompt_CitesPrevious(t *testing.T) {
	got := reformatPrompt("the bad response")
	if !strings.Contains(got, "the bad response") {
		t.Errorf("reformat prompt should cite previous response; got: %q", got)
	}
}

// firstJSONArray must respect string literals so brackets nested
// inside strings do not throw off the bracket depth counter.
func TestFirstJSONArray_RespectsStringLiterals(t *testing.T) {
	in := `prefix [{"key":"]"}, {"key2":"["}] suffix`
	got, ok := firstJSONArray(in)
	if !ok {
		t.Fatalf("expected to find an array")
	}
	if got != `[{"key":"]"}, {"key2":"["}]` {
		t.Errorf("got %q", got)
	}
}

// firstJSONArray must respect escapes - a backslash-quote inside a
// string should not toggle string state and confuse bracket counting.
func TestFirstJSONArray_RespectsEscapes(t *testing.T) {
	in := fmt.Sprintf(`[%q]`, `she said "hi"`)
	got, ok := firstJSONArray(in)
	if !ok {
		t.Fatalf("expected to find an array")
	}
	if got != in {
		t.Errorf("got %q, want %q", got, in)
	}
}

// End-to-end check that cite-or-drop runs inside the analyzer
// pipeline and surfaces the drop count in coverage_summary.
func TestLLMAnalyzer_CiteOrDropDropsHallucinatedFindings(t *testing.T) {
	// The response cites a turn_id that is not present in the bundle.
	in := AnalysisInput{
		PRNumber: 42,
		Bundle: Bundle{
			BaseRef: "main",
			BaseSHA: "base-sha",
			HeadSHA: "head-sha",
			Diff:    []byte("--- a\n+++ b\n"),
			Turns: []BundleTurn{{
				TurnID:            "t-REAL",
				CommitHash:        "c1",
				PromptExcerpt:     "real prompt",
				PromptExcerptHash: "h-real",
			}},
		},
	}
	bad := `[
		{
			"schema_version":"1",
			"finding_id":"f_0000000000000001",
			"kind":"deferred",
			"title":"hallucinated",
			"confidence":"medium",
			"originally_requested_in":{"turn_id":"t-FAKE","prompt_excerpt":"fake","prompt_excerpt_hash":"h-fake"},
			"trajectory_note":"x",
			"current_state":{"file":"x","line_range":[1,2],"summary":"s"}
		}
	]`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{canned(bad)}}
	a := NewLLMAnalyzer(runner)

	res, err := a.Analyze(context.Background(), in)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(res.Findings, &arr); err != nil {
		t.Fatalf("findings not array: %v", err)
	}
	if len(arr) != 0 {
		t.Errorf("expected hallucinated finding to be dropped; got %d findings", len(arr))
	}

	var cov map[string]any
	if err := json.Unmarshal(res.CoverageSummary, &cov); err != nil {
		t.Fatalf("coverage not object: %v", err)
	}
	if cov["findings_dropped"] != float64(1) {
		t.Errorf("coverage findings_dropped = %v, want 1", cov["findings_dropped"])
	}
}

// When the bundle has no captured prompts the analyzer must refuse to
// call the LLM. Running anyway would produce findings the cite-or-drop
// filter would discard AND would guess at intent without evidence.
func TestLLMAnalyzer_NoCapturedPromptsSkipsLLM(t *testing.T) {
	in := AnalysisInput{
		PRNumber: 42,
		Bundle: Bundle{
			BaseRef: "main",
			BaseSHA: "base",
			HeadSHA: "head",
			Diff:    []byte("diff"),
			Turns:   nil,
		},
	}
	runner := panicLLMRunner{}
	a := NewLLMAnalyzer(runner)

	res, err := a.Analyze(context.Background(), in)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if string(res.Findings) != "[]" {
		t.Errorf("Findings = %q, want []", string(res.Findings))
	}
	var cov map[string]any
	if err := json.Unmarshal(res.CoverageSummary, &cov); err != nil {
		t.Fatalf("coverage: %v", err)
	}
	if cov["skipped"] != true {
		t.Errorf("expected coverage.skipped = true; got %v", cov)
	}
	if cov["skip_reason"] != "no_captured_prompts" {
		t.Errorf("expected skip_reason no_captured_prompts; got %v", cov["skip_reason"])
	}
}

// panicLLMRunner asserts the analyzer does not invoke the LLM at all.
type panicLLMRunner struct{}

func (panicLLMRunner) GenerateText(context.Context, string) (*llm.GenerateTextResult, error) {
	panic("LLM must not be called when bundle has no captured prompts")
}
