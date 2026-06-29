package intentgap

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/llm"
)

// turnIDsOf returns just the TurnID slice for a set of ledger items
// so test failures can print a compact view of the actual ordering.
func turnIDsOf(items []IntentItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.TurnID)
	}
	return out
}

// turnsForLedger produces a small canonical set of BundleTurns used by
// the intent-ledger tests. Each turn has a distinct TurnID + Excerpt
// + ExcerptHash so the verbatim-copy invariant is observable.
func turnsForLedger() []BundleTurn {
	return []BundleTurn{
		{TurnID: "t-1", PromptExcerpt: "add input validation to handler.go", PromptExcerptHash: "h-1", TS: 100, CommitHash: "c1"},
		{TurnID: "t-2", PromptExcerpt: "actually no, prefer struct tags", PromptExcerptHash: "h-2", TS: 200, CommitHash: "c1"},
		{TurnID: "t-3", PromptExcerpt: "what does this function return?", PromptExcerptHash: "h-3", TS: 300, CommitHash: "c2"},
	}
}

// Happy path: classifier returns one valid item per turn, all are
// kept, no repair retry fires, the ledger is fully reliable.
func TestBuildIntentLedger_HappyPath(t *testing.T) {
	resp := `[
		{"turn_id":"t-1","kind":"request","summary":"user asks to add input validation","hint":{"files":["handler.go"],"keywords":["validation"]}},
		{"turn_id":"t-2","kind":"correction","summary":"user prefers struct tags","hint":{"files":[],"keywords":["struct","tags"]}},
		{"turn_id":"t-3","kind":"context","summary":"user asks what this function returns","hint":{"files":[],"keywords":[]}}
	]`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: resp}}}

	got, err := BuildIntentLedger(context.Background(), runner, turnsForLedger())
	if err != nil {
		t.Fatalf("BuildIntentLedger: %v", err)
	}
	if len(got.Items) != 3 {
		t.Errorf("Items len = %d, want 3", len(got.Items))
	}
	if got.InvalidCount != 0 {
		t.Errorf("InvalidCount = %d, want 0", got.InvalidCount)
	}
	if got.Unreliable {
		t.Errorf("Unreliable = true; classifier returned a valid item per turn")
	}
	if runner.calls != 1 {
		t.Errorf("calls = %d, want 1 (no repair retry should fire when everything validates)", runner.calls)
	}
}

// Excerpt and ExcerptHash are copied verbatim from the BundleTurn,
// not from classifier output. Cite-or-drop relies on this invariant:
// an under_impl finding's prompt_excerpt_hash must match the bundle.
func TestBuildIntentLedger_ExcerptAndHashComeFromBundleNotClassifier(t *testing.T) {
	// The classifier emits a different summary, but no excerpt/hash
	// in its output. The ledger must still carry the bundle's
	// verbatim excerpt and hash.
	resp := `[{"turn_id":"t-1","kind":"request","summary":"classifier-paraphrased summary"}]`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: resp}}}

	got, err := BuildIntentLedger(context.Background(), runner, turnsForLedger()[:1])
	if err != nil || len(got.Items) != 1 {
		t.Fatalf("BuildIntentLedger: %v / %+v", err, got)
	}
	it := got.Items[0]
	if it.Excerpt != "add input validation to handler.go" {
		t.Errorf("Excerpt = %q, want bundle excerpt verbatim", it.Excerpt)
	}
	if it.ExcerptHash != "h-1" {
		t.Errorf("ExcerptHash = %q, want bundle hash verbatim", it.ExcerptHash)
	}
	if it.Summary != "classifier-paraphrased summary" {
		t.Errorf("Summary = %q, want classifier output verbatim", it.Summary)
	}
}

// A malformed item (unknown kind) in the first response triggers one
// repair retry. If the retry returns a well-formed item for the
// same turn, the ledger ends up with the full set.
func TestBuildIntentLedger_MalformedItemSucceedsOnRepair(t *testing.T) {
	bad := `[
		{"turn_id":"t-1","kind":"NOT_A_KIND","summary":"x"},
		{"turn_id":"t-2","kind":"correction","summary":"keep this one"}
	]`
	repair := `[{"turn_id":"t-1","kind":"request","summary":"user asks for validation"}]`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: bad}, {Text: repair}}}

	got, err := BuildIntentLedger(context.Background(), runner, turnsForLedger()[:2])
	if err != nil {
		t.Fatalf("BuildIntentLedger: %v", err)
	}
	if len(got.Items) != 2 {
		t.Errorf("Items len = %d, want 2 (repaired item should merge with the originally-good one)", len(got.Items))
	}
	if got.InvalidCount != 0 {
		t.Errorf("InvalidCount = %d, want 0 (repair recovered the bad item)", got.InvalidCount)
	}
	if runner.calls != 2 {
		t.Errorf("calls = %d, want 2 (initial + one repair retry)", runner.calls)
	}
}

// A malformed item that the repair retry ALSO fails to fix is
// counted in InvalidCount, the ratio threshold determines whether
// Unreliable fires.
func TestBuildIntentLedger_RepairFailsItemDroppedAndCounted(t *testing.T) {
	bad := `[
		{"turn_id":"t-1","kind":"NOT_A_KIND","summary":"x"},
		{"turn_id":"t-2","kind":"correction","summary":"keep this one"},
		{"turn_id":"t-3","kind":"context","summary":"keep this one too"}
	]`
	// Repair also fails for t-1; the rest stay valid from the first pass.
	repair := `[{"turn_id":"t-1","kind":"STILL_BAD","summary":"x"}]`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: bad}, {Text: repair}}}

	got, err := BuildIntentLedger(context.Background(), runner, turnsForLedger())
	if err != nil {
		t.Fatalf("BuildIntentLedger: %v", err)
	}
	if len(got.Items) != 2 {
		t.Errorf("Items len = %d, want 2 (one item should drop after repair fails)", len(got.Items))
	}
	if got.InvalidCount != 1 {
		t.Errorf("InvalidCount = %d, want 1", got.InvalidCount)
	}
	// 2 / 3 = 0.66, which is above the 0.6 reliability floor.
	if got.Unreliable {
		t.Errorf("Unreliable = true; valid ratio (2/3) should be above floor")
	}
}

// When the valid ratio falls below the floor, Unreliable is set so
// coverage can surface the degradation downstream.
func TestBuildIntentLedger_UnreliableBelowRatioThreshold(t *testing.T) {
	// 2 of 3 fail validation, 1 succeeds → ratio 1/3 ≈ 0.33 < 0.6.
	bad := `[
		{"turn_id":"t-1","kind":"NOT_A_KIND","summary":"x"},
		{"turn_id":"t-2","kind":"NOT_A_KIND","summary":"y"},
		{"turn_id":"t-3","kind":"context","summary":"keep this one"}
	]`
	// Repair fails on both.
	repair := `[
		{"turn_id":"t-1","kind":"STILL_BAD","summary":"x"},
		{"turn_id":"t-2","kind":"STILL_BAD","summary":"y"}
	]`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: bad}, {Text: repair}}}

	got, err := BuildIntentLedger(context.Background(), runner, turnsForLedger())
	if err != nil {
		t.Fatalf("BuildIntentLedger: %v", err)
	}
	if !got.Unreliable {
		t.Errorf("Unreliable should be true when valid ratio < %.2f", MinValidIntentRatio)
	}
	if got.InvalidCount != 2 {
		t.Errorf("InvalidCount = %d, want 2", got.InvalidCount)
	}
}

// The classifier's call failing entirely (no parseable response) is
// a hard error: candidate generation has no ledger to work from.
func TestBuildIntentLedger_ClassifierFailureIsHardError(t *testing.T) {
	runner := &fakeLLMRunner{
		responses: []*llm.GenerateTextResult{nil},
		errs:      []error{errors.New("provider unavailable")},
	}
	_, err := BuildIntentLedger(context.Background(), runner, turnsForLedger())
	if !errors.Is(err, ErrIntentClassifierFailed) {
		t.Errorf("err = %v, want wrapping ErrIntentClassifierFailed", err)
	}
}

// A turn whose own TurnID is empty cannot be cited later, so the
// classifier should never see it. The rendered prompt must omit
// such turns entirely.
func TestBuildIntentLedger_DropsTurnsWithoutTurnID(t *testing.T) {
	turns := []BundleTurn{
		{TurnID: "", PromptExcerpt: "a", PromptExcerptHash: "h", TS: 1},
		{TurnID: "t-real", PromptExcerpt: "b", PromptExcerptHash: "h2", TS: 2},
	}
	resp := `[{"turn_id":"t-real","kind":"request","summary":"x"}]`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: resp}}}

	got, err := BuildIntentLedger(context.Background(), runner, turns)
	if err != nil {
		t.Fatalf("BuildIntentLedger: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].TurnID != "t-real" {
		t.Errorf("expected only the t-real intent; got %+v", got.Items)
	}
	if !strings.Contains(runner.prompts[0], "t-real") || strings.Contains(runner.prompts[0], "turn_id=\n") {
		t.Errorf("classifier prompt should not include the empty-turn-id row; got:\n%s", runner.prompts[0])
	}
}

// A classifier hallucinating a turn_id that wasn't in the input is
// ignored entirely — the hallucinated row never reaches the ledger
// and the corresponding input turn shows up as malformed for the
// repair pass.
func TestBuildIntentLedger_IgnoresHallucinatedTurnIDs(t *testing.T) {
	bad := `[{"turn_id":"t-IMAGINED","kind":"request","summary":"y"}]`
	repair := `[
		{"turn_id":"t-1","kind":"request","summary":"recovered"},
		{"turn_id":"t-2","kind":"request","summary":"also recovered"},
		{"turn_id":"t-3","kind":"context","summary":"and this"}
	]`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: bad}, {Text: repair}}}

	got, err := BuildIntentLedger(context.Background(), runner, turnsForLedger())
	if err != nil {
		t.Fatalf("BuildIntentLedger: %v", err)
	}
	if len(got.Items) != 3 {
		t.Errorf("Items len = %d, want 3 after repair recovers them", len(got.Items))
	}
	for _, it := range got.Items {
		if it.TurnID == "t-IMAGINED" {
			t.Errorf("hallucinated turn_id leaked into the ledger: %+v", it)
		}
	}
}

// Repaired items must reappear in the original input turn order, not
// at the tail behind first-pass items. The classifier prompt asks for
// turn order; the ledger should preserve it so diagnostics and
// downstream readers stay readable.
func TestBuildIntentLedger_PreservesInputTurnOrderAcrossRepair(t *testing.T) {
	// First pass fails t-1, passes t-2 and t-3.
	bad := `[
		{"turn_id":"t-1","kind":"NOT_A_KIND","summary":"x"},
		{"turn_id":"t-2","kind":"correction","summary":"keep two"},
		{"turn_id":"t-3","kind":"context","summary":"keep three"}
	]`
	// Repair recovers t-1.
	repair := `[{"turn_id":"t-1","kind":"request","summary":"recovered one"}]`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: bad}, {Text: repair}}}

	got, err := BuildIntentLedger(context.Background(), runner, turnsForLedger())
	if err != nil {
		t.Fatalf("BuildIntentLedger: %v", err)
	}
	if len(got.Items) != 3 {
		t.Fatalf("Items len = %d, want 3", len(got.Items))
	}
	wantOrder := []string{"t-1", "t-2", "t-3"}
	for i, want := range wantOrder {
		if got.Items[i].TurnID != want {
			t.Errorf("Items[%d].TurnID = %q, want %q; full order=%v",
				i, got.Items[i].TurnID, want, turnIDsOf(got.Items))
		}
	}
}

// A duplicate turn_id in the classifier response invalidates that
// turn rather than letting the first occurrence win. Both copies are
// dropped from the first pass; the turn falls through to repair, and
// only the repaired item survives.
func TestBuildIntentLedger_DuplicateTurnIDInvalidatesAndRepairs(t *testing.T) {
	// First pass emits t-1 twice with two different kinds; both must drop.
	bad := `[
		{"turn_id":"t-1","kind":"request","summary":"alpha"},
		{"turn_id":"t-1","kind":"correction","summary":"beta"},
		{"turn_id":"t-2","kind":"context","summary":"keep two"}
	]`
	repair := `[{"turn_id":"t-1","kind":"request","summary":"clean classification"}]`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: bad}, {Text: repair}}}

	got, err := BuildIntentLedger(context.Background(), runner, turnsForLedger()[:2])
	if err != nil {
		t.Fatalf("BuildIntentLedger: %v", err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("Items len = %d, want 2", len(got.Items))
	}
	var t1 *IntentItem
	for i := range got.Items {
		if got.Items[i].TurnID == "t-1" {
			t1 = &got.Items[i]
		}
	}
	if t1 == nil {
		t.Fatalf("t-1 missing from ledger: %+v", got.Items)
	}
	if t1.Summary != "clean classification" {
		t.Errorf("t-1 summary = %q, want the repaired value (\"clean classification\"); first-wins would have kept %q",
			t1.Summary, "alpha")
	}
	if runner.calls != 2 {
		t.Errorf("calls = %d, want 2 (repair retry must fire for the duplicated turn)", runner.calls)
	}
}

// An over-long summary is truncated to the documented rune cap, not
// the byte cap. This keeps a non-ASCII summary from being cut mid-rune.
func TestBuildIntentLedger_SummaryTruncatedAtRuneBoundary(t *testing.T) {
	// 250 copies of a single 3-byte UTF-8 rune, well past the 200-rune cap.
	big := strings.Repeat("文", 250)
	resp := `[{"turn_id":"t-1","kind":"request","summary":"` + big + `"}]`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: resp}}}

	got, err := BuildIntentLedger(context.Background(), runner, turnsForLedger()[:1])
	if err != nil || len(got.Items) != 1 {
		t.Fatalf("BuildIntentLedger: %v / %+v", err, got)
	}
	runes := []rune(got.Items[0].Summary)
	if len(runes) != maxIntentSummaryRunes {
		t.Errorf("Summary rune count = %d, want %d", len(runes), maxIntentSummaryRunes)
	}
}
