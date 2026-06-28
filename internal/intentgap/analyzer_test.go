package intentgap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/semanticash/cli/internal/llm"
)

// truncatePromptExcerpt must back off to a rune boundary when the
// byte cap falls inside a multi-byte UTF-8 sequence. Cutting mid-rune
// produces invalid bytes that the LLM CLIs misbehave on (codex errors
// explicitly, claude silently stalls until its deadline).
func TestTruncatePromptExcerpt_BacksOffToRuneBoundary(t *testing.T) {
	// Smart-quote runes (three bytes each in UTF-8). Pad with ASCII so
	// the byte cap falls inside one of the multi-byte runes.
	prefix := strings.Repeat("a", 398)
	in := prefix + "“”" // " then "
	got := truncatePromptExcerpt(in)
	if !utf8.ValidString(got) {
		t.Errorf("truncated excerpt is not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "...(truncated)") {
		t.Errorf("missing truncation marker; got %q", got)
	}
}

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
//
// The deferred finding cites action a_1111111111111111, which sampleInput
// pairs with a sibling action a_2222222222222222 so DetectEditRevertTrajectories
// emits a trajectory candidate on handler.go that the cite-or-drop
// trajectory rule can accept.
func validDeferredJSON() string {
	body := deferredFinding("t-1", "add input validation", "h-1", "handler.go", lineRange{12, 14})
	body = strings.Replace(body, `"current_state":`,
		`"agent_action_citation":{"action_id":"a_1111111111111111"},"current_state":`, 1)
	return `[` + body + `]`
}

// sampleInput pairs the canonicalBundle with a PR number; analyzer
// tests that need a richer bundle replace the field. The bundle carries
// two captured actions on handler.go outside the diff range, which
// DetectEditRevertTrajectories reads as an add-then-remove sequence,
// so deferred findings citing a_1111111111111111 survive cite-or-drop.
func sampleInput() AnalysisInput {
	b := canonicalBundle()
	b.BaseRef = "main"
	b.BaseSHA = "base-sha"
	b.HeadSHA = "head-sha"
	b.Commits = []BundleCommit{{Hash: "c1", Subject: "first"}}
	b.AgentActions = []BundleAgentAction{
		{ActionID: "a_1111111111111111", TurnID: "t-1", ToolName: "Edit", FilePath: "handler.go", LineRangeStart: 50, LineRangeEnd: 60, TS: 100},
		{ActionID: "a_2222222222222222", TurnID: "t-1", ToolName: "Edit", FilePath: "handler.go", LineRangeStart: 55, LineRangeEnd: 65, TS: 200},
	}
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
		"agent_action_citation":{"action_id":"a_1111111111111111"},
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

// When every finding in the initial response is dropped for schema
// violations, the analyzer asks for one repair pass. The repair embeds
// minimal valid shapes per kind so the model can copy them directly.
func TestLLMAnalyzer_RetryOnSchemaFailure(t *testing.T) {
	bad := `[{
		"schema_version":"1",
		"finding_id":"f_0000000000000000",
		"kind":"under_impl",
		"title":"t",
		"confidence":"medium",
		"expected_intent":{"summary":"s","turn_id":"t-1","prompt_excerpt":"add input validation","prompt_excerpt_hash":"h-1"},
		"observed_diff_evidence":{"summary":"s","ai_authored_regions_checked":true},
		"missing_or_partial_area":{"note":"n"}
	}]`
	good := `[` + underImplFinding("t-1", "add input validation", "h-1", "handler.go", lineRange{12, 14}) + `]`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{canned(bad), canned(good)}}
	a := NewLLMAnalyzer(runner)

	res, err := a.Analyze(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("Analyze should repair schema-invalid response: %v", err)
	}
	if runner.calls != 2 {
		t.Errorf("calls = %d, want 2 (initial + schema repair)", runner.calls)
	}
	if !strings.Contains(runner.prompts[1], "dropped because it failed the intent-gap finding schema") ||
		!strings.Contains(runner.prompts[1], "ai_authored_regions_checked is an ARRAY") {
		t.Errorf("schema repair prompt missing schema guidance; got: %q", runner.prompts[1])
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(res.Findings, &arr); err != nil {
		t.Fatalf("findings not parseable: %v", err)
	}
	if len(arr) != 1 {
		t.Errorf("findings len = %d, want 1", len(arr))
	}
}

// When every finding fails schema validation across both the initial
// response and the repair retry, the analyzer succeeds with an empty
// findings array plus a diagnostic coverage_summary. A single
// malformed finding does not turn the whole analysis into an errored
// row.
func TestLLMAnalyzer_AllFindingsInvalidYieldsEmptyArrayWithDiagnostics(t *testing.T) {
	bad := `[{"whatever": true}]`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{canned(bad), canned(bad)}}
	a := NewLLMAnalyzer(runner)

	res, err := a.Analyze(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("Analyze should succeed with [] when all findings invalid; got %v", err)
	}
	if string(res.Findings) != "[]" {
		t.Errorf("findings = %s, want []", string(res.Findings))
	}
	var cov map[string]any
	if err := json.Unmarshal(res.CoverageSummary, &cov); err != nil {
		t.Fatalf("coverage not object: %v", err)
	}
	if cov["findings_dropped"] == nil {
		t.Errorf("coverage_summary missing findings_dropped: %v", cov)
	}
	reasons, _ := cov["drop_reasons"].(map[string]any)
	if len(reasons) == 0 {
		t.Errorf("coverage_summary missing drop_reasons: %v", cov)
	}
	if len(res.SchemaDiagnostics) == 0 {
		t.Errorf("SchemaDiagnostics should carry structural details for the activity log")
	}
}

// When the repair retry's LLM call fails, the analyzer still succeeds
// with an empty findings array but records schema_repair_call_failed
// in coverage so an all-invalid run with a failed repair is
// distinguishable from one where the repair simply produced more
// invalid findings.
func TestLLMAnalyzer_SchemaRepairCallFailureRecordedInCoverage(t *testing.T) {
	bad := `[{"whatever": true}]`
	runner := &fakeLLMRunner{
		responses: []*llm.GenerateTextResult{canned(bad), nil},
		errs:      []error{nil, errors.New("provider down")},
	}
	a := NewLLMAnalyzer(runner)

	res, err := a.Analyze(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("Analyze should succeed when repair retry fails; got %v", err)
	}
	if string(res.Findings) != "[]" {
		t.Errorf("findings = %s, want []", string(res.Findings))
	}
	var cov map[string]any
	_ = json.Unmarshal(res.CoverageSummary, &cov)
	failures, _ := cov["schema_repair_failures"].(map[string]any)
	if failures["schema_repair_call_failed"] == nil {
		t.Errorf("coverage_summary.schema_repair_failures missing schema_repair_call_failed: %v", failures)
	}
	reasons, _ := cov["drop_reasons"].(map[string]any)
	if reasons["schema_repair_call_failed"] != nil {
		t.Errorf("coverage_summary.drop_reasons should not include repair failures: %v", reasons)
	}
}

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

// A response with one valid finding and one schema-invalid finding
// keeps the valid one and drops the other. The repair retry is skipped
// because at least one finding survived the initial filter.
func TestLLMAnalyzer_MixedSchemaValidityKeepsValidFindings(t *testing.T) {
	good := underImplFinding("t-1", "add input validation", "h-1", "handler.go", lineRange{12, 14})
	badShape := `{
		"schema_version":"1",
		"finding_id":"f_0000000000000000",
		"kind":"under_impl",
		"title":"t",
		"confidence":"medium",
		"expected_intent":{"summary":"s","turn_id":"t-1","prompt_excerpt":"add input validation","prompt_excerpt_hash":"h-1"},
		"observed_diff_evidence":{"summary":"s","ai_authored_regions_checked":true},
		"missing_or_partial_area":{"note":"n"}
	}`
	body := `[` + good + `,` + badShape + `]`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{canned(body)}}
	a := NewLLMAnalyzer(runner)

	res, err := a.Analyze(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("Analyze should succeed when one finding is valid: %v", err)
	}
	if runner.calls != 1 {
		t.Errorf("calls = %d, want 1 (no repair retry when any finding survives)", runner.calls)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(res.Findings, &arr); err != nil {
		t.Fatalf("findings not parseable: %v", err)
	}
	if len(arr) != 1 {
		t.Errorf("findings len = %d, want 1 (one bad finding dropped)", len(arr))
	}
	var cov map[string]any
	_ = json.Unmarshal(res.CoverageSummary, &cov)
	if cov["findings_dropped"] == nil {
		t.Errorf("coverage_summary missing findings_dropped: %v", cov)
	}
	if len(res.SchemaDiagnostics) != 1 {
		t.Errorf("SchemaDiagnostics len = %d, want 1 (one dropped finding recorded)", len(res.SchemaDiagnostics))
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

// The rendered prompt frames the analyzer with the three-anchor
// evidence model so the LLM can reason about ask/attempt/result
// alignment rather than guessing whether the agent tried something.
func TestRenderAnalyzerPrompt_FramesThreeAnchorEvidenceModel(t *testing.T) {
	in := sampleInput()
	prompt := renderAnalyzerPrompt(in)
	for _, want := range []string{
		"ASK",
		"ATTEMPT",
		"RESULT",
		"three mechanical evidence anchors",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q; got:\n%s", want, prompt)
		}
	}
}

// The prompt documents optional citation fields so the LLM can attach
// findings to action evidence. Without these instructions the
// cite-or-drop layer would not receive those fields.
func TestRenderAnalyzerPrompt_DocumentsOptionalCitationFields(t *testing.T) {
	in := sampleInput()
	prompt := renderAnalyzerPrompt(in)
	for _, want := range []string{
		"agent_action_citation",
		"no_action_citation",
		"ai_authored_regions_checked:[{file,lines:[[start,end]]}, ...]",
		"NOT a yes/no boolean",
		"Confidence guidance",
		"unrequested",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q; got:\n%s", want, prompt)
		}
	}
}

// Actions whose file is in the PR diff are rendered for the LLM with
// their action_id, tool name, and line range. Actions that touched
// unrelated files (or have no resolvable file at all) are filtered out
// of the prompt and kept only in the bundle for validation. The
// canonical bundle's diff covers handler.go, so the handler.go edit is
// shown; an unrelated Edit on extras.go is filtered.
func TestRenderAnalyzerPrompt_RendersOnlyDiffRelevantActions(t *testing.T) {
	in := sampleInput()
	in.Bundle.AgentActions = []BundleAgentAction{
		{
			ActionID: "a_1111111111111111", TurnID: "t-1", ToolName: "Edit",
			FilePath: "handler.go", LineRangeStart: 42, LineRangeEnd: 58,
		},
		{
			ActionID: "a_2222222222222222", TurnID: "t-1", ToolName: "Edit",
			FilePath: "extras.go", LineRangeStart: 10, LineRangeEnd: 20,
		},
	}
	prompt := renderAnalyzerPrompt(in)
	for _, want := range []string{
		"action_id=a_1111111111111111",
		"tool=Edit",
		"file=handler.go",
		"lines=42-58",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q; got:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "a_2222222222222222") {
		t.Errorf("prompt must not list actions on files outside the diff; got extras.go action in:\n%s", prompt)
	}
}

// Hidden actions with an unknown FilePath force a no_action_citation
// guard: validation cannot prove non-overlap when at least one action
// it would check against has no file information, so the LLM is told
// not to emit negative citations at all.
func TestRenderAnalyzerPrompt_WarnsWhenHiddenActionHasUnknownPath(t *testing.T) {
	in := sampleInput()
	in.Bundle.AgentActions = []BundleAgentAction{
		{
			ActionID: "a_1111111111111111", TurnID: "t-1", ToolName: "Edit",
			FilePath: "handler.go", LineRangeStart: 42, LineRangeEnd: 58,
		},
		{
			ActionID: "a_2222222222222222", TurnID: "t-1", ToolName: "Bash",
			// Bash with no derivable path: kept in the bundle for
			// validation, hidden from the prompt by the filter.
			FilePath: "",
		},
	}
	prompt := renderAnalyzerPrompt(in)
	if strings.Contains(prompt, "a_2222222222222222") {
		t.Errorf("hidden action_id leaked into prompt: %s", prompt)
	}
	if !strings.Contains(prompt, "Do NOT emit no_action_citation") {
		t.Errorf("prompt missing no_action_citation guard; got:\n%s", prompt)
	}
}

// When the bundle has actions but none touch a file in the diff and
// none are part of a trajectory, the listing explains the empty
// result so the LLM does not invent action_ids to cite.
func TestRenderAnalyzerPrompt_AllActionsFilteredHasExplicitMessage(t *testing.T) {
	in := sampleInput()
	in.Bundle.AgentActions = []BundleAgentAction{
		{ActionID: "a_1111111111111111", ToolName: "Edit", FilePath: "unrelated.go", LineRangeStart: 5, LineRangeEnd: 10},
	}
	prompt := renderAnalyzerPrompt(in)
	if !strings.Contains(prompt, "No captured agent actions touched a file in the PR diff") {
		t.Errorf("prompt should explain the empty filtered listing; got:\n%s", prompt)
	}
}

// When the bundle has no captured actions, the prompt steers the LLM
// away from citation fields that have no verifiable source. This is
// the dual of the no-prompts message that already exists for turns.
func TestRenderAnalyzerPrompt_NoActionsMessageStopsCitations(t *testing.T) {
	in := sampleInput()
	in.Bundle.AgentActions = nil
	prompt := renderAnalyzerPrompt(in)
	if !strings.Contains(prompt, "No captured agent actions") {
		t.Errorf("prompt should explain absence of action data; got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Avoid agent_action_citation") {
		t.Errorf("prompt should tell the model to avoid action citations; got:\n%s", prompt)
	}
}

// Action truncation is surfaced so the LLM knows the listing is not
// complete. Negative action citations are unsafe when older actions
// were dropped at the cap.
func TestRenderAnalyzerPrompt_ReportsActionTruncation(t *testing.T) {
	in := sampleInput()
	in.Bundle.AgentActions = []BundleAgentAction{
		{ActionID: "a_3333333333333333", TurnID: "t-1", ToolName: "Edit", FilePath: "a.go"},
	}
	in.Bundle.Truncated.AgentActionsDropped = 12
	prompt := renderAnalyzerPrompt(in)
	if !strings.Contains(prompt, "12 older actions omitted") {
		t.Errorf("prompt should report action truncation count; got:\n%s", prompt)
	}
}

// The prompt template version is bumped to 0.2.0 to reflect the new
// evidence model; the payload hash depends on it, so downstream
// systems can distinguish v1 (prompt-only) and v2 (action-aware)
// uploads.
func TestPromptTemplateVersion_v2(t *testing.T) {
	if PromptTemplateVersion != "0.2.0" {
		t.Errorf("PromptTemplateVersion = %q, want 0.2.0", PromptTemplateVersion)
	}
}

// Under truncation the prompt tells the LLM not to emit
// no_action_citation. The cite-or-drop layer also rejects it.
func TestRenderAnalyzerPrompt_TruncationDisablesNegativeCitations(t *testing.T) {
	in := sampleInput()
	in.Bundle.AgentActions = []BundleAgentAction{
		{ActionID: "a_3333333333333333", TurnID: "t-1", ToolName: "Edit", FilePath: "a.go"},
	}
	in.Bundle.Truncated.AgentActionsDropped = 5
	prompt := renderAnalyzerPrompt(in)
	if !strings.Contains(prompt, "Do NOT emit no_action_citation") {
		t.Errorf("prompt should ban no_action_citation under truncation; got:\n%s", prompt)
	}
}

// The under_impl / deferred confidence guidance is kept generic, but
// unrequested gets a kind-specific note because its ASK anchor is the
// absence of a supporting prompt rather than positive alignment.
func TestRenderAnalyzerPrompt_UnrequestedGetsKindSpecificConfidenceNote(t *testing.T) {
	in := sampleInput()
	prompt := renderAnalyzerPrompt(in)
	if !strings.Contains(prompt, "Confidence guidance (unrequested):") {
		t.Errorf("prompt should carry a kind-specific guidance for unrequested; got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "captured-intent search that returned no supporting prompt") {
		t.Errorf("unrequested guidance should frame ASK as a complete search; got:\n%s", prompt)
	}
}

// Mechanically detected trajectories surface in the prompt as hints,
// so the LLM can emit a deferred finding when a captured prompt
// maps onto one of the listed scopes. Without this section, the LLM
// would have to re-derive trajectories from the diff and action list.
func TestRenderAnalyzerPrompt_RendersDetectedTrajectoriesAsHints(t *testing.T) {
	in := sampleInput()
	in.Bundle.AgentActions = []BundleAgentAction{
		{ActionID: "a_1111111111111111", TurnID: "t-1", ToolName: "Edit", FilePath: "added.go", LineRangeStart: 10, LineRangeEnd: 20},
		{ActionID: "a_2222222222222222", TurnID: "t-1", ToolName: "Edit", FilePath: "added.go", LineRangeStart: 15, LineRangeEnd: 25},
	}
	// Diff exists for an unrelated file, so the trajectory detector
	// sees no surviving change for added.go.
	in.Bundle.Diff = []byte(
		"--- a/other.go\n+++ b/other.go\n@@ -1,1 +1,2 @@\n line\n+added\n",
	)
	prompt := renderAnalyzerPrompt(in)
	if !strings.Contains(prompt, "Detected revert trajectories") {
		t.Errorf("prompt should announce trajectory hints; got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "file=added.go") || !strings.Contains(prompt, "lines=10-25") {
		t.Errorf("prompt missing trajectory entry; got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "a_1111111111111111") || !strings.Contains(prompt, "a_2222222222222222") {
		t.Errorf("prompt should list contributing action_ids; got:\n%s", prompt)
	}
}

// When no trajectories are detected, the analyzer prompt omits the
// section entirely so the LLM is not nudged toward emitting deferred
// findings without supporting mechanical evidence.
func TestRenderAnalyzerPrompt_OmitsTrajectorySectionWhenNoneDetected(t *testing.T) {
	in := sampleInput()
	// Clear the captured actions so the detector returns nothing.
	in.Bundle.AgentActions = nil
	prompt := renderAnalyzerPrompt(in)
	if strings.Contains(prompt, "Detected revert trajectories") {
		t.Errorf("prompt should omit trajectory section when none detected; got:\n%s", prompt)
	}
}
