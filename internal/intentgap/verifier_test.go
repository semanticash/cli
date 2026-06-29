package intentgap

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/llm"
)

// trackBVerifierInput builds a minimal Track B verifier input.
func trackBVerifierInput() VerifierInput {
	intent := candIntent("i-1", IntentRequest, 100, "add tests for the new handler")
	change := changeLedgerOf(
		struct {
			path     string
			category FileCategory
		}{"internal/service/handler.go", CatCode},
	)
	change.Files[0].Hunks = []ChangedHunk{{
		StartLine: 12, EndLine: 24,
		Direction: HunkAdded,
		Body:      "+func Handle(req *Request) error { return validate(req) }\n",
	}}
	change.ByPath["internal/service/handler.go"] = &change.Files[0]
	candidate := Candidate{
		ID:              "cand-b",
		Kind:            CandUnderImplPartialScope,
		IntentID:        intent.ID,
		Score:           0.5,
		Reason:          "intent matched files but no test category file was changed",
		DiffPointers:    []HunkRef{{File: "internal/service/handler.go", StartLine: 12, EndLine: 24}},
		MissingCategory: CatTest,
	}
	return VerifierInput{
		Candidate: candidate,
		Intent:    intent,
		Change:    change,
	}
}

// trackAVerifierInput builds a minimal Track A verifier input.
func trackAVerifierInput() VerifierInput {
	intent := candIntent("i-2", IntentRequest, 200, "introduce retrieval scoring helper")
	change := changeLedgerOf(
		struct {
			path     string
			category FileCategory
		}{"internal/unrelated/thing.go", CatCode},
	)
	candidate := Candidate{
		ID:         "cand-a",
		Kind:       CandUnderImplNoRetrievedScope,
		IntentID:   intent.ID,
		Score:      0.2,
		Reason:     "intent has meaningful keywords but no retrieved file crossed the score threshold",
		NearMisses: []string{"internal/unrelated/thing.go"},
	}
	return VerifierInput{
		Candidate: candidate,
		Intent:    intent,
		Change:    change,
		Bundle: Bundle{Commits: []BundleCommit{
			{Hash: "c1", Subject: "wire up the retrieval helper"},
		}},
	}
}

// Track B accept preserves the cited acceptance scope.
func TestRunScopedVerifier_TrackBAcceptHappyPath(t *testing.T) {
	in := trackBVerifierInput()
	canned := `{
		"verdict":"accept",
		"rationale":"the handler change adds validation but no test was added",
		"acceptance":{
			"primary_file":"internal/service/handler.go",
			"regions":[{"file":"internal/service/handler.go","start":12,"end":24}],
			"supporting_action_ids":["a_1111"]
		}
	}`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: canned}}}

	got := RunScopedVerifier(context.Background(), runner, in)
	if got.Verdict != VerdictAccept {
		t.Fatalf("Verdict = %q, want accept; result=%+v", got.Verdict, got)
	}
	if got.CandidateID != "cand-b" {
		t.Errorf("CandidateID = %q, want cand-b", got.CandidateID)
	}
	if got.DropReason != "" {
		t.Errorf("DropReason = %q, want empty on accept", got.DropReason)
	}
	if got.Acceptance == nil {
		t.Fatalf("Acceptance is nil; want populated")
	}
	if got.Acceptance.PrimaryFile != "internal/service/handler.go" {
		t.Errorf("PrimaryFile = %q", got.Acceptance.PrimaryFile)
	}
	if len(got.Acceptance.Regions) != 1 || got.Acceptance.Regions[0].StartLine != 12 {
		t.Errorf("Regions wrong: %+v", got.Acceptance.Regions)
	}
}

// Track B accept requires at least one cited region.
func TestRunScopedVerifier_TrackBAcceptWithoutRegionsDrops(t *testing.T) {
	in := trackBVerifierInput()
	canned := `{
		"verdict":"accept",
		"rationale":"nothing concrete to cite",
		"acceptance":{"primary_file":"internal/service/handler.go","regions":[]}
	}`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: canned}}}

	got := RunScopedVerifier(context.Background(), runner, in)
	if got.Verdict != VerdictDrop {
		t.Errorf("Verdict = %q, want drop", got.Verdict)
	}
	if got.DropReason != DropAcceptedNoRegions {
		t.Errorf("DropReason = %q, want %q", got.DropReason, DropAcceptedNoRegions)
	}
}

// Track A accept may be diagnostic-only with no regions.
func TestRunScopedVerifier_TrackAAcceptWithEmptyRegionsKept(t *testing.T) {
	in := trackAVerifierInput()
	canned := `{
		"verdict":"accept",
		"rationale":"intent plausibly references the unrelated thing.go but nothing addresses it",
		"acceptance":{"primary_file":"internal/unrelated/thing.go","regions":[]}
	}`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: canned}}}

	got := RunScopedVerifier(context.Background(), runner, in)
	if got.Verdict != VerdictAccept {
		t.Fatalf("Verdict = %q, want accept; result=%+v", got.Verdict, got)
	}
	if got.Acceptance == nil || got.Acceptance.PrimaryFile != "internal/unrelated/thing.go" {
		t.Errorf("PrimaryFile = %+v, want internal/unrelated/thing.go", got.Acceptance)
	}
	if len(got.Acceptance.Regions) != 0 {
		t.Errorf("Track A accept must allow empty Regions; got %v", got.Acceptance.Regions)
	}
}

// Known drop reasons pass through.
func TestRunScopedVerifier_DropWithEnumReason(t *testing.T) {
	in := trackBVerifierInput()
	canned := `{"verdict":"drop","drop_reason":"intent_already_delivered","rationale":"the diff already adds the validation the intent asked for"}`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: canned}}}

	got := RunScopedVerifier(context.Background(), runner, in)
	if got.Verdict != VerdictDrop {
		t.Fatalf("Verdict = %q, want drop", got.Verdict)
	}
	if got.DropReason != DropIntentAlreadyDelivered {
		t.Errorf("DropReason = %q, want %q", got.DropReason, DropIntentAlreadyDelivered)
	}
}

// Unknown drop reasons are treated as invalid shape.
func TestRunScopedVerifier_DropWithUnknownReasonIsInvalid(t *testing.T) {
	in := trackBVerifierInput()
	canned := `{"verdict":"drop","drop_reason":"banana","rationale":"x"}`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: canned}}}

	got := RunScopedVerifier(context.Background(), runner, in)
	if got.DropReason != DropVerifierInvalidShape {
		t.Errorf("DropReason = %q, want %q", got.DropReason, DropVerifierInvalidShape)
	}
}

// needs_more_context carries neither drop reason nor acceptance.
func TestRunScopedVerifier_NeedsMoreContext(t *testing.T) {
	in := trackBVerifierInput()
	canned := `{"verdict":"needs_more_context","rationale":"I would like to see surrounding code"}`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: canned}}}

	got := RunScopedVerifier(context.Background(), runner, in)
	if got.Verdict != VerdictNeedsMoreContext {
		t.Errorf("Verdict = %q, want needs_more_context", got.Verdict)
	}
	if got.DropReason != "" || got.Acceptance != nil {
		t.Errorf("needs_more_context must not carry drop or acceptance; got %+v", got)
	}
}

// Non-JSON responses become invalid-shape drops.
func TestRunScopedVerifier_NonJSONResponseDrops(t *testing.T) {
	in := trackBVerifierInput()
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: "Sure, here you go!"}}}

	got := RunScopedVerifier(context.Background(), runner, in)
	if got.DropReason != DropVerifierInvalidShape {
		t.Errorf("DropReason = %q, want %q", got.DropReason, DropVerifierInvalidShape)
	}
}

// Markdown-fenced JSON is accepted.
func TestRunScopedVerifier_CodeFenceWrappedResponseAccepted(t *testing.T) {
	in := trackBVerifierInput()
	canned := "Here's the verdict:\n\n```json\n" + `{
		"verdict":"drop",
		"drop_reason":"intent_too_vague",
		"rationale":"cannot anchor this against the diff"
	}` + "\n```\n"
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: canned}}}

	got := RunScopedVerifier(context.Background(), runner, in)
	if got.Verdict != VerdictDrop || got.DropReason != DropIntentTooVague {
		t.Errorf("fence-wrapped response not parsed; got %+v", got)
	}
}

// LLM call failures become typed drops.
func TestRunScopedVerifier_CallFailureDrops(t *testing.T) {
	in := trackBVerifierInput()
	runner := &fakeLLMRunner{
		responses: []*llm.GenerateTextResult{nil},
		errs:      []error{errors.New("network unreachable")},
	}

	got := RunScopedVerifier(context.Background(), runner, in)
	if got.Verdict != VerdictDrop {
		t.Errorf("Verdict = %q, want drop", got.Verdict)
	}
	if got.DropReason != DropVerifierCallFailed {
		t.Errorf("DropReason = %q, want %q", got.DropReason, DropVerifierCallFailed)
	}
	if !strings.Contains(got.Rationale, "network unreachable") {
		t.Errorf("Rationale should mention the underlying error; got %q", got.Rationale)
	}
}

// Rationale truncation is rune-safe.
func TestRunScopedVerifier_RationaleTruncatedAtRuneBoundary(t *testing.T) {
	in := trackBVerifierInput()
	big := strings.Repeat("文", maxVerifierRationaleRunes+50)
	canned := `{"verdict":"drop","drop_reason":"intent_too_vague","rationale":"` + big + `"}`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: canned}}}

	got := RunScopedVerifier(context.Background(), runner, in)
	runes := []rune(got.Rationale)
	if len(runes) != maxVerifierRationaleRunes {
		t.Errorf("Rationale rune count = %d, want %d", len(runes), maxVerifierRationaleRunes)
	}
}

// Track B primary_file must match one cited region file.
func TestRunScopedVerifier_TrackBPrimaryFileMustBeInRegions(t *testing.T) {
	in := trackBVerifierInput()
	canned := `{
		"verdict":"accept",
		"rationale":"primary_file doesn't match any region",
		"acceptance":{
			"primary_file":"unrelated.go",
			"regions":[{"file":"internal/service/handler.go","start":12,"end":24}]
		}
	}`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: canned}}}

	got := RunScopedVerifier(context.Background(), runner, in)
	if got.Verdict != VerdictDrop {
		t.Errorf("Verdict = %q, want drop", got.Verdict)
	}
	if got.DropReason != DropPrimaryFileNotInRegions {
		t.Errorf("DropReason = %q, want %q", got.DropReason, DropPrimaryFileNotInRegions)
	}
}

// drop_reason is stored trimmed after validation.
func TestRunScopedVerifier_DropReasonWhitespaceIsStripped(t *testing.T) {
	in := trackBVerifierInput()
	canned := `{"verdict":"drop","drop_reason":"  intent_too_vague  ","rationale":"x"}`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: canned}}}

	got := RunScopedVerifier(context.Background(), runner, in)
	if got.Verdict != VerdictDrop {
		t.Fatalf("Verdict = %q, want drop", got.Verdict)
	}
	if got.DropReason != DropIntentTooVague {
		t.Errorf("DropReason = %q (raw: %q), want %q (trimmed)",
			got.DropReason, "  intent_too_vague  ", DropIntentTooVague)
	}
}

// Prose-wrapped JSON is accepted.
func TestRunScopedVerifier_JSONEmbeddedInProseAccepted(t *testing.T) {
	in := trackBVerifierInput()
	canned := "Sure, here is the verdict for that candidate: " +
		`{"verdict":"drop","drop_reason":"intent_already_delivered","rationale":"already there"}` +
		" Hope that helps!"
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: canned}}}

	got := RunScopedVerifier(context.Background(), runner, in)
	if got.Verdict != VerdictDrop {
		t.Fatalf("Verdict = %q, want drop; result=%+v", got.Verdict, got)
	}
	if got.DropReason != DropIntentAlreadyDelivered {
		t.Errorf("DropReason = %q, want %q", got.DropReason, DropIntentAlreadyDelivered)
	}
}

// Prompt evidence sections depend on the candidate track.
func TestRunScopedVerifier_PromptSectionsByTrack(t *testing.T) {
	runnerB := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: `{"verdict":"needs_more_context","rationale":""}`}}}
	_ = RunScopedVerifier(context.Background(), runnerB, trackBVerifierInput())
	promptB := runnerB.prompts[0]
	if !strings.Contains(promptB, "internal/service/handler.go:12-24") {
		t.Errorf("Track B prompt missing hunk file:lines header; got:\n%s", promptB)
	}
	if !strings.Contains(promptB, "+func Handle") {
		t.Errorf("Track B prompt missing hunk body; got:\n%s", promptB)
	}
	if !strings.Contains(promptB, "Missing category in scope: test") {
		t.Errorf("Track B prompt should name the missing category; got:\n%s", promptB)
	}
	if strings.Contains(promptB, "Near-miss files") {
		t.Errorf("Track B prompt should not include Near-miss files section; got:\n%s", promptB)
	}

	runnerA := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: `{"verdict":"needs_more_context","rationale":""}`}}}
	_ = RunScopedVerifier(context.Background(), runnerA, trackAVerifierInput())
	promptA := runnerA.prompts[0]
	if !strings.Contains(promptA, "Near-miss files") {
		t.Errorf("Track A prompt missing Near-miss files section; got:\n%s", promptA)
	}
	if !strings.Contains(promptA, "Commit subjects") {
		t.Errorf("Track A prompt missing Commit subjects section; got:\n%s", promptA)
	}
	if !strings.Contains(promptA, "wire up the retrieval helper") {
		t.Errorf("Track A prompt missing commit subject text; got:\n%s", promptA)
	}
	if strings.Contains(promptA, "Diff hunks (cite") {
		t.Errorf("Track A prompt should not include diff hunks section; got:\n%s", promptA)
	}
}
