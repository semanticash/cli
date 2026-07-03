package intentgap

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/llm"
)

// expanderInputFor builds an ExpansionInput for test candidates.
func expanderInputFor(t *testing.T, cases []struct {
	id      string
	score   float64
	verdict VerifierVerdict
}) ExpansionInput {
	t.Helper()
	intent := candIntent("i-shared", IntentRequest, 100, "add tests for the new handler")
	change := changeLedgerOf(
		struct {
			path     string
			category FileCategory
		}{"internal/service/handler.go", CatCode},
	)
	change.Files[0].Hunks = []ChangedHunk{{
		StartLine: 100, EndLine: 110,
		Direction: HunkAdded,
		Body:      "+func Handle(req *Request) error { return validate(req) }\n",
	}}
	change.ByPath["internal/service/handler.go"] = &change.Files[0]

	in := ExpansionInput{
		CandidatesByID: map[string]Candidate{},
		IntentsByID:    map[string]IntentItem{intent.ID: intent},
		Change:         change,
	}
	for _, c := range cases {
		cand := Candidate{
			ID:           c.id,
			Kind:         CandUnderImplPartialScope,
			IntentID:     intent.ID,
			Score:        c.score,
			Reason:       "intent matched files but no test category file was changed",
			DiffPointers: []HunkRef{{File: "internal/service/handler.go", StartLine: 100, EndLine: 110}},
		}
		in.CandidatesByID[c.id] = cand
		in.VerifierResults = append(in.VerifierResults, VerifierResult{
			CandidateID: c.id,
			Verdict:     c.verdict,
		})
	}
	return in
}

// scriptedRunner returns responses in call order.
type scriptedRunner struct {
	responses []string
	idx       int
	prompts   []string
}

func (s *scriptedRunner) GenerateText(_ context.Context, prompt string) (*llm.GenerateTextResult, error) {
	s.prompts = append(s.prompts, prompt)
	if s.idx >= len(s.responses) {
		return nil, fmt.Errorf("scriptedRunner: no response for call %d", s.idx)
	}
	r := s.responses[s.idx]
	s.idx++
	return &llm.GenerateTextResult{Text: r}, nil
}

// No needs_more_context result means no expansion.
func TestRunExpander_NoNeedsMoreContextNoOp(t *testing.T) {
	in := expanderInputFor(t, []struct {
		id      string
		score   float64
		verdict VerifierVerdict
	}{
		{"a", 0.5, VerdictAccept},
		{"b", 0.4, VerdictDrop},
	})
	runner := &scriptedRunner{}
	got := RunExpander(context.Background(), runner, in)
	if got.ExpansionAttempts != 0 {
		t.Errorf("ExpansionAttempts = %d, want 0", got.ExpansionAttempts)
	}
	if len(got.UpdatedResults) != len(in.VerifierResults) {
		t.Errorf("UpdatedResults len = %d, want %d", len(got.UpdatedResults), len(in.VerifierResults))
	}
	if runner.idx != 0 {
		t.Errorf("runner should not have been called; got idx=%d", runner.idx)
	}
}

// The expander re-verifies only the top-scoring candidates.
func TestRunExpander_TopNByScore(t *testing.T) {
	in := expanderInputFor(t, []struct {
		id      string
		score   float64
		verdict VerifierVerdict
	}{
		{"a", 0.1, VerdictNeedsMoreContext}, // lowest score
		{"b", 0.9, VerdictNeedsMoreContext}, // highest
		{"c", 0.5, VerdictNeedsMoreContext},
		{"d", 0.7, VerdictNeedsMoreContext},
		{"e", 0.6, VerdictNeedsMoreContext},
	})
	runner := &scriptedRunner{responses: []string{
		`{"verdict":"drop","drop_reason":"intent_too_vague","rationale":"b"}`,
		`{"verdict":"drop","drop_reason":"intent_too_vague","rationale":"d"}`,
		`{"verdict":"drop","drop_reason":"intent_too_vague","rationale":"e"}`,
	}}
	got := RunExpander(context.Background(), runner, in)
	if got.ExpansionAttempts != MaxExpansionsPerRun {
		t.Errorf("ExpansionAttempts = %d, want %d", got.ExpansionAttempts, MaxExpansionsPerRun)
	}

	byID := map[string]VerifierResult{}
	for _, r := range got.UpdatedResults {
		byID[r.CandidateID] = r
	}
	for _, wantExpanded := range []string{"b", "d", "e"} {
		if byID[wantExpanded].Verdict != VerdictDrop {
			t.Errorf("candidate %q should have been expanded → drop; got %+v",
				wantExpanded, byID[wantExpanded])
		}
	}
	for _, wantUnchanged := range []string{"a", "c"} {
		if byID[wantUnchanged].Verdict != VerdictNeedsMoreContext {
			t.Errorf("candidate %q should have passed through unchanged; got %+v",
				wantUnchanged, byID[wantUnchanged])
		}
	}
}

// UpdatedResults preserves input order.
func TestRunExpander_PreservesInputOrder(t *testing.T) {
	in := expanderInputFor(t, []struct {
		id      string
		score   float64
		verdict VerifierVerdict
	}{
		{"first", 0.2, VerdictNeedsMoreContext},
		{"second", 0.9, VerdictNeedsMoreContext},
		{"third", 0.5, VerdictNeedsMoreContext},
	})
	runner := &scriptedRunner{responses: []string{
		// Responses are consumed in expansion order (score desc): second, third, first.
		`{"verdict":"drop","drop_reason":"intent_too_vague","rationale":"second"}`,
		`{"verdict":"drop","drop_reason":"intent_too_vague","rationale":"third"}`,
		`{"verdict":"drop","drop_reason":"intent_too_vague","rationale":"first"}`,
	}}
	got := RunExpander(context.Background(), runner, in)
	wantIDs := []string{"first", "second", "third"}
	for i, want := range wantIDs {
		if got.UpdatedResults[i].CandidateID != want {
			t.Errorf("UpdatedResults[%d].CandidateID = %q, want %q (input order must be preserved)",
				i, got.UpdatedResults[i].CandidateID, want)
		}
	}
}

// A second needs_more_context becomes expansion_inconclusive.
func TestRunExpander_SecondNeedsMoreContextDropsInconclusive(t *testing.T) {
	in := expanderInputFor(t, []struct {
		id      string
		score   float64
		verdict VerifierVerdict
	}{
		{"a", 0.5, VerdictNeedsMoreContext},
	})
	runner := &scriptedRunner{responses: []string{
		`{"verdict":"needs_more_context","rationale":"still cannot tell"}`,
	}}
	got := RunExpander(context.Background(), runner, in)
	if got.Inconclusive != 1 {
		t.Errorf("Inconclusive = %d, want 1", got.Inconclusive)
	}
	if got.UpdatedResults[0].Verdict != VerdictDrop {
		t.Errorf("Verdict = %q, want drop", got.UpdatedResults[0].Verdict)
	}
	if got.UpdatedResults[0].DropReason != DropExpansionInconclusive {
		t.Errorf("DropReason = %q, want %q",
			got.UpdatedResults[0].DropReason, DropExpansionInconclusive)
	}
}

// expandCandidate adds nearby hunks without duplicating refs.
func TestExpandCandidate_AddsNeighboringHunks(t *testing.T) {
	change := changeLedgerOf(
		struct {
			path     string
			category FileCategory
		}{"f.go", CatCode},
	)
	change.Files[0].Hunks = []ChangedHunk{
		{StartLine: 100, EndLine: 110, Direction: HunkAdded, Body: "+a\n"},
		{StartLine: 130, EndLine: 135, Direction: HunkAdded, Body: "+b\n"}, // within window
		{StartLine: 500, EndLine: 510, Direction: HunkAdded, Body: "+c\n"}, // outside
	}
	change.ByPath["f.go"] = &change.Files[0]

	c := Candidate{
		Kind:         CandUnderImplPartialScope,
		DiffPointers: []HunkRef{{File: "f.go", StartLine: 100, EndLine: 110}},
	}
	got := expandCandidate(c, change)
	if len(got.DiffPointers) != 2 {
		t.Fatalf("DiffPointers len = %d, want 2 (original + the in-window neighbor)", len(got.DiffPointers))
	}
	if got.DiffPointers[1].StartLine != 130 {
		t.Errorf("expected neighbor at line 130; got %+v", got.DiffPointers[1])
	}
}

// expandCandidate adds capped same-directory sibling files.
func TestExpandCandidate_AddsSiblingFiles(t *testing.T) {
	change := changeLedgerOf(
		struct {
			path     string
			category FileCategory
		}{"pkg/a.go", CatCode},
		struct {
			path     string
			category FileCategory
		}{"pkg/b.go", CatCode},
		struct {
			path     string
			category FileCategory
		}{"pkg/c.go", CatCode},
		struct {
			path     string
			category FileCategory
		}{"pkg/d.go", CatCode}, // fourth sibling; capped out
		struct {
			path     string
			category FileCategory
		}{"other/e.go", CatCode},
	)
	for i := range change.Files {
		change.Files[i].Hunks = []ChangedHunk{{StartLine: 1, EndLine: 5, Direction: HunkAdded, Body: "+x\n"}}
		change.ByPath[change.Files[i].Path] = &change.Files[i]
	}

	c := Candidate{
		Kind:         CandUnderImplPartialScope,
		DiffPointers: []HunkRef{{File: "pkg/a.go", StartLine: 1, EndLine: 5}},
	}
	got := expandCandidate(c, change)
	if len(got.DiffPointers) != 1+MaxSiblingFilesPerExpansion {
		t.Fatalf("DiffPointers len = %d, want %d (original + cap siblings)",
			len(got.DiffPointers), 1+MaxSiblingFilesPerExpansion)
	}
	for _, ref := range got.DiffPointers {
		if !strings.HasPrefix(ref.File, "pkg/") {
			t.Errorf("non-sibling file pulled in: %q", ref.File)
		}
	}
}

// neighboringTurns handles head, middle, tail, and missing anchors.
func TestNeighboringTurns_BoundaryCases(t *testing.T) {
	turns := []BundleTurn{
		{TurnID: "t-0", PromptExcerpt: "zero"},
		{TurnID: "t-1", PromptExcerpt: "one"},
		{TurnID: "t-2", PromptExcerpt: "two"},
	}
	cases := []struct {
		name   string
		anchor string
		want   []string
	}{
		{"head", "t-0", []string{"one"}},
		{"middle", "t-1", []string{"zero", "two"}},
		{"tail", "t-2", []string{"one"}},
		{"missing", "t-MISSING", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := neighboringTurns(tc.anchor, turns)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tc.want))
			}
			for i, want := range tc.want {
				if got[i].PromptExcerpt != want {
					t.Errorf("got[%d] = %q, want %q", i, got[i].PromptExcerpt, want)
				}
			}
		})
	}
}

// Duplicate CandidateIDs still update by result position.
func TestRunExpander_DuplicateCandidateIDsDoNotCollapse(t *testing.T) {
	intent := candIntent("i-shared", IntentRequest, 100, "add tests for the new handler")
	cand := Candidate{
		ID:           "dup",
		Kind:         CandUnderImplPartialScope,
		IntentID:     intent.ID,
		Score:        0.5,
		DiffPointers: []HunkRef{{File: "f.go", StartLine: 1, EndLine: 5}},
	}
	change := changeLedgerOf(
		struct {
			path     string
			category FileCategory
		}{"f.go", CatCode},
	)
	change.Files[0].Hunks = []ChangedHunk{{StartLine: 1, EndLine: 5, Body: "+x\n"}}
	change.ByPath["f.go"] = &change.Files[0]

	in := ExpansionInput{
		VerifierResults: []VerifierResult{
			{CandidateID: "dup", Verdict: VerdictNeedsMoreContext},
			{CandidateID: "dup", Verdict: VerdictNeedsMoreContext},
		},
		CandidatesByID: map[string]Candidate{"dup": cand},
		IntentsByID:    map[string]IntentItem{intent.ID: intent},
		Change:         change,
	}
	runner := &scriptedRunner{responses: []string{
		`{"verdict":"drop","drop_reason":"intent_too_vague","rationale":"first"}`,
		`{"verdict":"drop","drop_reason":"intent_too_vague","rationale":"second"}`,
	}}
	got := RunExpander(context.Background(), runner, in)
	if got.ExpansionAttempts != 2 {
		t.Fatalf("ExpansionAttempts = %d, want 2 (each slot gets an expansion call)",
			got.ExpansionAttempts)
	}
	if got.UpdatedResults[0].Rationale != "first" || got.UpdatedResults[1].Rationale != "second" {
		t.Errorf("result slots carried wrong rationales; got [%q, %q]",
			got.UpdatedResults[0].Rationale, got.UpdatedResults[1].Rationale)
	}
}

// Orphaned results are excluded from expansion ranking.
func TestRunExpander_MissingCandidateMetadataIsExcludedFromRanking(t *testing.T) {
	intent := candIntent("i-shared", IntentRequest, 100, "add tests for the new handler")
	change := changeLedgerOf(
		struct {
			path     string
			category FileCategory
		}{"f.go", CatCode},
	)
	change.Files[0].Hunks = []ChangedHunk{{StartLine: 1, EndLine: 5, Body: "+x\n"}}
	change.ByPath["f.go"] = &change.Files[0]

	validCand := Candidate{
		ID:           "valid",
		Kind:         CandUnderImplPartialScope,
		IntentID:     intent.ID,
		Score:        0.5,
		DiffPointers: []HunkRef{{File: "f.go", StartLine: 1, EndLine: 5}},
	}

	in := ExpansionInput{
		VerifierResults: []VerifierResult{
			{CandidateID: "ghost-1", Verdict: VerdictNeedsMoreContext},
			{CandidateID: "ghost-2", Verdict: VerdictNeedsMoreContext},
			{CandidateID: "ghost-3", Verdict: VerdictNeedsMoreContext},
			{CandidateID: "valid", Verdict: VerdictNeedsMoreContext},
		},
		CandidatesByID: map[string]Candidate{"valid": validCand},
		IntentsByID:    map[string]IntentItem{intent.ID: intent},
		Change:         change,
	}
	runner := &scriptedRunner{responses: []string{
		`{"verdict":"drop","drop_reason":"intent_too_vague","rationale":""}`,
	}}
	got := RunExpander(context.Background(), runner, in)
	if got.ExpansionAttempts != 1 {
		t.Errorf("ExpansionAttempts = %d, want 1 (only the valid candidate should expand)",
			got.ExpansionAttempts)
	}
	for i, want := range []string{"ghost-1", "ghost-2", "ghost-3"} {
		if got.UpdatedResults[i].CandidateID != want {
			t.Errorf("UpdatedResults[%d].CandidateID = %q, want %q",
				i, got.UpdatedResults[i].CandidateID, want)
		}
		if got.UpdatedResults[i].Verdict != VerdictNeedsMoreContext {
			t.Errorf("UpdatedResults[%d] orphan verdict = %q, want needs_more_context",
				i, got.UpdatedResults[i].Verdict)
		}
	}
	if got.UpdatedResults[3].Verdict != VerdictDrop {
		t.Errorf("valid candidate should have been expanded to drop; got %+v", got.UpdatedResults[3])
	}
}

// Expanded verifier calls include neighboring turn context.
func TestRunExpander_SecondCallIncludesNeighboringTurns(t *testing.T) {
	in := expanderInputFor(t, []struct {
		id      string
		score   float64
		verdict VerifierVerdict
	}{
		{"a", 0.5, VerdictNeedsMoreContext},
	})
	in.Bundle.Turns = []BundleTurn{
		{TurnID: "t-prev", PromptExcerpt: "earlier context"},
		{TurnID: in.IntentsByID["i-shared"].TurnID, PromptExcerpt: "the cited turn"},
		{TurnID: "t-next", PromptExcerpt: "later context"},
	}
	runner := &scriptedRunner{responses: []string{
		`{"verdict":"drop","drop_reason":"intent_too_vague","rationale":""}`,
	}}
	_ = RunExpander(context.Background(), runner, in)
	if len(runner.prompts) != 1 {
		t.Fatalf("expected 1 expansion call; got %d", len(runner.prompts))
	}
	prompt := runner.prompts[0]
	for _, want := range []string{"earlier context", "later context", "Neighboring captured prompts"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("expansion prompt missing %q; got:\n%s", want, prompt)
		}
	}
}
