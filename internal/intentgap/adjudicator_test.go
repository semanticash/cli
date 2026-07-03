package intentgap

import (
	"encoding/json"
	"strings"
	"testing"
)

// adjudicatorBundle returns the shared bundle fixture.
func adjudicatorBundle() Bundle {
	return Bundle{
		Turns: []BundleTurn{{
			TurnID:            "t-1",
			CommitHash:        "c1",
			PromptExcerpt:     "add input validation",
			PromptExcerptHash: "h-1",
		}},
		Diff: []byte("--- a/handler.go\n+++ b/handler.go\n@@ -10,5 +10,11 @@\n line\n+added\n+added2\n line\n line\n"),
	}
}

// adjuIntent matches the shared bundle turn.
func adjuIntent() IntentItem {
	return IntentItem{
		ID:          "i-1",
		Kind:        IntentRequest,
		Summary:     "user asks to add input validation",
		Excerpt:     "add input validation",
		ExcerptHash: "h-1",
		TurnID:      "t-1",
	}
}

// trackBAccept returns a valid Track B acceptance fixture.
func trackBAccept(candID string) (Candidate, VerifierResult) {
	cand := Candidate{
		ID:              candID,
		Kind:            CandUnderImplPartialScope,
		IntentID:        "i-1",
		Score:           0.5,
		Reason:          "intent matched files but no test category file was changed",
		MissingCategory: CatTest,
		DiffPointers:    []HunkRef{{File: "handler.go", StartLine: 12, EndLine: 14}},
	}
	result := VerifierResult{
		CandidateID: candID,
		Verdict:     VerdictAccept,
		Rationale:   "no test file changed alongside the handler edit",
		Acceptance: &AcceptedScope{
			PrimaryFile: "handler.go",
			Regions:     []HunkRef{{File: "handler.go", StartLine: 12, EndLine: 14}},
		},
	}
	return cand, result
}

// One Track B accept produces one stamped under_impl finding.
func TestRunAdjudicator_TrackBAcceptProducesFinding(t *testing.T) {
	cand, result := trackBAccept("c-1")
	in := AdjudicatorInput{
		VerifierResults: []VerifierResult{result},
		CandidatesByID:  map[string]Candidate{cand.ID: cand},
		IntentsByID:     map[string]IntentItem{"i-1": adjuIntent()},
		Bundle:          adjudicatorBundle(),
		RepositoryID:    "repo-abc",
		PRNumber:        42,
	}
	got := RunAdjudicator(in)
	if got.FindingsCount != 1 {
		t.Fatalf("FindingsCount = %d, want 1; Findings=%s", got.FindingsCount, got.Findings)
	}
	var arr []map[string]any
	if err := json.Unmarshal(got.Findings, &arr); err != nil {
		t.Fatalf("findings not parseable: %v", err)
	}
	f := arr[0]
	if f["kind"] != "under_impl" {
		t.Errorf("kind = %v, want under_impl", f["kind"])
	}
	if f["confidence"] != "medium" {
		t.Errorf("confidence = %v, want medium (V1 pins it)", f["confidence"])
	}
	id, _ := f["finding_id"].(string)
	if id == "" || id == "f_0000000000000000" {
		t.Errorf("finding_id not stamped; got %q", id)
	}
}

// expected_intent fields come from the IntentItem.
func TestRunAdjudicator_ExpectedIntentCitesIntentItemNotAcceptance(t *testing.T) {
	cand, result := trackBAccept("c-1")
	in := AdjudicatorInput{
		VerifierResults: []VerifierResult{result},
		CandidatesByID:  map[string]Candidate{cand.ID: cand},
		IntentsByID:     map[string]IntentItem{"i-1": adjuIntent()},
		Bundle:          adjudicatorBundle(),
		RepositoryID:    "repo-abc",
		PRNumber:        42,
	}
	got := RunAdjudicator(in)
	if got.FindingsCount != 1 {
		t.Fatalf("FindingsCount = %d, want 1", got.FindingsCount)
	}
	var arr []map[string]any
	_ = json.Unmarshal(got.Findings, &arr)
	ei, _ := arr[0]["expected_intent"].(map[string]any)
	if ei["turn_id"] != "t-1" {
		t.Errorf("turn_id = %v, want t-1 (bundle)", ei["turn_id"])
	}
	if ei["prompt_excerpt"] != "add input validation" {
		t.Errorf("prompt_excerpt = %v, want bundle excerpt verbatim", ei["prompt_excerpt"])
	}
	if ei["prompt_excerpt_hash"] != "h-1" {
		t.Errorf("prompt_excerpt_hash = %v, want bundle hash verbatim", ei["prompt_excerpt_hash"])
	}
}

// Track A accepts produce diagnostics, not wire findings.
func TestRunAdjudicator_TrackAAcceptIsDiagnosticOnly(t *testing.T) {
	intent := adjuIntent()
	cand := Candidate{
		ID:       "c-a",
		Kind:     CandUnderImplNoRetrievedScope,
		IntentID: intent.ID,
		Score:    0.2,
	}
	result := VerifierResult{
		CandidateID: cand.ID,
		Verdict:     VerdictAccept,
		Rationale:   "intent plausibly references the missing area",
		Acceptance: &AcceptedScope{
			PrimaryFile: "missing.go",
		},
	}
	in := AdjudicatorInput{
		VerifierResults: []VerifierResult{result},
		CandidatesByID:  map[string]Candidate{cand.ID: cand},
		IntentsByID:     map[string]IntentItem{intent.ID: intent},
		Bundle:          adjudicatorBundle(),
		RepositoryID:    "repo-abc",
		PRNumber:        42,
	}
	got := RunAdjudicator(in)
	if got.FindingsCount != 0 {
		t.Errorf("Track A produced wire findings; got %d", got.FindingsCount)
	}
	if got.TrackACandidatesAccepted != 1 || len(got.TrackADiagnostics) != 1 {
		t.Errorf("Track A acceptance should surface as diagnostic; got %+v", got)
	}
	if got.TrackADiagnostics[0].IntentID != intent.ID {
		t.Errorf("TrackADiagnostics[0].IntentID = %q, want %q",
			got.TrackADiagnostics[0].IntentID, intent.ID)
	}
}

// Dedup keeps the higher-score candidate on key collision.
func TestRunAdjudicator_DedupKeepsHigherScore(t *testing.T) {
	cand1, r1 := trackBAccept("c-low")
	cand1.Score = 0.3
	r1.Rationale = "low-score rationale"
	cand2, r2 := trackBAccept("c-high")
	cand2.Score = 0.9
	r2.Rationale = "high-score rationale"
	in := AdjudicatorInput{
		VerifierResults: []VerifierResult{r1, r2},
		CandidatesByID:  map[string]Candidate{cand1.ID: cand1, cand2.ID: cand2},
		IntentsByID:     map[string]IntentItem{"i-1": adjuIntent()},
		Bundle:          adjudicatorBundle(),
		RepositoryID:    "repo-abc",
		PRNumber:        42,
	}
	got := RunAdjudicator(in)
	if got.FindingsCount != 1 {
		t.Fatalf("FindingsCount = %d, want 1", got.FindingsCount)
	}
	if got.DedupDropped != 1 {
		t.Errorf("DedupDropped = %d, want 1", got.DedupDropped)
	}
	if !strings.Contains(string(got.Findings), "high-score rationale") {
		t.Errorf("high-Score candidate should win dedup; got: %s", got.Findings)
	}
}

// Extra supporting action IDs are counted and not rendered.
func TestRunAdjudicator_ExtraActionCitationsCounted(t *testing.T) {
	const aFirst = "a_1111111111111111"
	const aSecond = "a_2222222222222222"
	const aThird = "a_3333333333333333"
	cand, result := trackBAccept("c-1")
	result.Acceptance.SupportingActionIDs = []string{aFirst, aSecond, aThird}
	bundle := adjudicatorBundle()
	bundle.AgentActions = []BundleAgentAction{
		{ActionID: aFirst, ToolName: "Edit", FilePath: "handler.go", LineRangeStart: 12, LineRangeEnd: 14},
		{ActionID: aSecond, ToolName: "Edit", FilePath: "handler.go", LineRangeStart: 12, LineRangeEnd: 14},
		{ActionID: aThird, ToolName: "Edit", FilePath: "handler.go", LineRangeStart: 12, LineRangeEnd: 14},
	}
	in := AdjudicatorInput{
		VerifierResults: []VerifierResult{result},
		CandidatesByID:  map[string]Candidate{cand.ID: cand},
		IntentsByID:     map[string]IntentItem{"i-1": adjuIntent()},
		Bundle:          bundle,
		RepositoryID:    "repo-abc",
		PRNumber:        42,
	}
	got := RunAdjudicator(in)
	if got.FindingsCount != 1 {
		t.Fatalf("FindingsCount = %d, want 1; Findings=%s", got.FindingsCount, got.Findings)
	}
	if got.ActionCitationsDiscarded != 2 {
		t.Errorf("ActionCitationsDiscarded = %d, want 2", got.ActionCitationsDiscarded)
	}
	if !strings.Contains(string(got.Findings), aFirst) {
		t.Errorf("finding did not include the first action_id; got: %s", got.Findings)
	}
	if strings.Contains(string(got.Findings), aSecond) || strings.Contains(string(got.Findings), aThird) {
		t.Errorf("extras were rendered; got: %s", got.Findings)
	}
}

// Track B accepts without regions are counted and skipped.
func TestRunAdjudicator_TrackBEmptyRegionsCountedAndSkipped(t *testing.T) {
	cand, result := trackBAccept("c-1")
	result.Acceptance.Regions = nil
	in := AdjudicatorInput{
		VerifierResults: []VerifierResult{result},
		CandidatesByID:  map[string]Candidate{cand.ID: cand},
		IntentsByID:     map[string]IntentItem{"i-1": adjuIntent()},
		Bundle:          adjudicatorBundle(),
		RepositoryID:    "repo-abc",
		PRNumber:        42,
	}
	got := RunAdjudicator(in)
	if got.FindingsCount != 0 {
		t.Errorf("FindingsCount = %d, want 0",
			got.FindingsCount)
	}
	if got.UnanchoredAccepts != 1 {
		t.Errorf("UnanchoredAccepts = %d, want 1",
			got.UnanchoredAccepts)
	}
}

// Out-of-diff cited regions drop at cite-or-drop.
func TestRunAdjudicator_CiteOrDropDropsOutOfDiffRegions(t *testing.T) {
	cand, result := trackBAccept("c-1")
	cand.DiffPointers = []HunkRef{{File: "handler.go", StartLine: 200, EndLine: 210}}
	result.Acceptance.Regions = []HunkRef{{File: "handler.go", StartLine: 200, EndLine: 210}}
	in := AdjudicatorInput{
		VerifierResults: []VerifierResult{result},
		CandidatesByID:  map[string]Candidate{cand.ID: cand},
		IntentsByID:     map[string]IntentItem{"i-1": adjuIntent()},
		Bundle:          adjudicatorBundle(),
		RepositoryID:    "repo-abc",
		PRNumber:        42,
	}
	got := RunAdjudicator(in)
	if got.FindingsCount != 0 {
		t.Errorf("FindingsCount = %d, want 0", got.FindingsCount)
	}
	if got.CiteOrDropDropped == 0 {
		t.Errorf("CiteOrDropDropped should be > 0; got %+v", got)
	}
}

// Findings are byte-stable across repeated runs.
func TestRunAdjudicator_FindingsAreByteStableAcrossRuns(t *testing.T) {
	cand, result := trackBAccept("c-1")
	in := AdjudicatorInput{
		VerifierResults: []VerifierResult{result},
		CandidatesByID:  map[string]Candidate{cand.ID: cand},
		IntentsByID:     map[string]IntentItem{"i-1": adjuIntent()},
		Bundle:          adjudicatorBundle(),
		RepositoryID:    "repo-abc",
		PRNumber:        42,
	}
	first := RunAdjudicator(in)
	second := RunAdjudicator(in)
	if string(first.Findings) != string(second.Findings) {
		t.Errorf("findings differ across runs:\n  first:  %s\n  second: %s",
			first.Findings, second.Findings)
	}
}

// normalizeLineSpan rounds outward to nearest multiple of 5.
func TestNormalizeLineSpan_RoundsOutward(t *testing.T) {
	cases := []struct {
		in   []HunkRef
		want [2]int
	}{
		{nil, [2]int{0, 0}},
		{[]HunkRef{{StartLine: 13, EndLine: 22}}, [2]int{10, 25}},
		{[]HunkRef{{StartLine: 15, EndLine: 20}}, [2]int{15, 20}},
		{[]HunkRef{{StartLine: 11, EndLine: 14}, {StartLine: 100, EndLine: 102}}, [2]int{10, 105}},
	}
	for _, tc := range cases {
		got := normalizeLineSpan(tc.in)
		if got != tc.want {
			t.Errorf("normalizeLineSpan(%+v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// normalizeFilePath collapses case and path separators so two
// candidates that differ only there dedup together.
func TestNormalizeFilePath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Internal/Service/Handler.go", "internal/service/handler.go"},
		{`internal\service\handler.go`, "internal/service/handler.go"},
		{"handler.go", "handler.go"},
	}
	for _, tc := range cases {
		if got := normalizeFilePath(tc.in); got != tc.want {
			t.Errorf("normalizeFilePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
