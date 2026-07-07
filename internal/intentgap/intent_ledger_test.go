package intentgap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
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

// classifierTurnIDRegex extracts the list of turn_ids visible in a
// rendered classifier prompt. Synthetic runners read this to produce
// a matching JSON response without duplicating the prompt renderer.
var classifierTurnIDRegex = regexp.MustCompile(`turn_id=([^\s\n]+)`)

func turnIDsInPrompt(prompt string) []string {
	matches := classifierTurnIDRegex.FindAllStringSubmatch(prompt, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}

// syntheticClassifierRunner produces a valid classifier response for
// whichever turns are present in the prompt. Used by batching tests
// that don't care about the specific classification, just the shape.
type syntheticClassifierRunner struct {
	mu    sync.Mutex
	calls int
}

func (s *syntheticClassifierRunner) GenerateText(_ context.Context, prompt string) (*llm.GenerateTextResult, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	ids := turnIDsInPrompt(prompt)
	type item struct {
		TurnID  string `json:"turn_id"`
		Kind    string `json:"kind"`
		Summary string `json:"summary"`
	}
	items := make([]item, 0, len(ids))
	for _, id := range ids {
		items = append(items, item{TurnID: id, Kind: "request", Summary: "user asks for " + id})
	}
	body, _ := json.Marshal(items)
	return &llm.GenerateTextResult{Text: string(body), Provider: "claude_code", Model: "claude-opus-4-7"}, nil
}

// splitClassifierRunner selectively fails specific batches based on
// call index. failIndex names one batch to fail (single-failure
// tests); failEveryExcept names one batch to succeed (majority-fail
// tests). goodResponders handles the "success" side.
type splitClassifierRunner struct {
	mu              sync.Mutex
	calls           int
	failIndex       int
	failEveryExcept int
	goodResponders  *syntheticClassifierRunner
}

func (s *splitClassifierRunner) GenerateText(ctx context.Context, prompt string) (*llm.GenerateTextResult, error) {
	s.mu.Lock()
	call := s.calls
	s.calls++
	s.mu.Unlock()
	shouldFail := false
	if s.failEveryExcept > 0 {
		shouldFail = call != s.failEveryExcept
	} else if s.failIndex >= 0 {
		shouldFail = call == s.failIndex
	}
	if shouldFail {
		return &llm.GenerateTextResult{Text: "definitely not JSON"}, nil
	}
	return s.goodResponders.GenerateText(ctx, prompt)
}

// constantResponseRunner returns a fixed response for every call.
// Used for "all batches fail" tests.
type constantResponseRunner struct {
	mu    sync.Mutex
	calls int
	text  string
}

func (r *constantResponseRunner) GenerateText(_ context.Context, _ string) (*llm.GenerateTextResult, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return &llm.GenerateTextResult{Text: r.text}, nil
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

// A classifier call with no parseable response returns an error:
// candidate generation has no ledger to work from.
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

// A classifier response with a turn_id that wasn't in the input is
// ignored entirely — the extra row never reaches the ledger
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
			t.Errorf("unexpected turn_id reached the ledger: %+v", it)
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

// The pre-filter drops the exact-match acknowledgement list and keeps
// everything else, including intent-reversing short phrases like
// "no" or "stop wait" that should reach the classifier.
func TestBuildIntentLedger_PrefilterDropsAcknowledgementsOnly(t *testing.T) {
	turns := []BundleTurn{
		{TurnID: "ack-ok", PromptExcerpt: "ok", PromptExcerptHash: "h1", TS: 1},
		{TurnID: "ack-yes", PromptExcerpt: "  Yes  ", PromptExcerptHash: "h2", TS: 2},
		{TurnID: "ack-thankyou", PromptExcerpt: "thank you", PromptExcerptHash: "h3", TS: 3},
		{TurnID: "keep-no", PromptExcerpt: "no", PromptExcerptHash: "h4", TS: 4},
		{TurnID: "keep-actually", PromptExcerpt: "actually revert that change", PromptExcerptHash: "h5", TS: 5},
		{TurnID: "keep-stop", PromptExcerpt: "stop", PromptExcerptHash: "h6", TS: 6},
		{TurnID: "keep-wait", PromptExcerpt: "wait one moment", PromptExcerptHash: "h7", TS: 7},
	}
	// Classifier only sees the 4 kept turns.
	resp := `[
		{"turn_id":"keep-no","kind":"correction","summary":"user rejects the prior direction"},
		{"turn_id":"keep-actually","kind":"correction","summary":"user reverses direction"},
		{"turn_id":"keep-stop","kind":"correction","summary":"user halts current work"},
		{"turn_id":"keep-wait","kind":"context","summary":"user pauses"}
	]`
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: resp}}}

	got, err := BuildIntentLedger(context.Background(), runner, turns)
	if err != nil {
		t.Fatalf("BuildIntentLedger: %v", err)
	}
	if got.PrefilteredCount != 3 {
		t.Errorf("PrefilteredCount = %d, want 3 (ok, yes, thank you)", got.PrefilteredCount)
	}
	if len(got.Items) != 4 {
		t.Fatalf("Items len = %d, want 4 (all non-acked turns should reach the classifier)", len(got.Items))
	}
	// Classifier prompt should not include acknowledgement turn_ids so
	// the LLM budget goes to the substantive prompts only.
	prompt := runner.prompts[0]
	for _, id := range []string{"ack-ok", "ack-yes", "ack-thankyou"} {
		if strings.Contains(prompt, id) {
			t.Errorf("prefiltered turn %q leaked into classifier prompt:\n%s", id, prompt)
		}
	}
	for _, id := range []string{"keep-no", "keep-actually", "keep-stop", "keep-wait"} {
		if !strings.Contains(prompt, id) {
			t.Errorf("substantive turn %q missing from classifier prompt", id)
		}
	}
}

// A bundle whose turns are all acknowledgements skips the LLM
// entirely. The ledger returns cleanly (no error) with an empty item
// slice and the pre-filter count carried into coverage.
func TestBuildIntentLedger_AllAcknowledgementsSkipsClassifier(t *testing.T) {
	turns := []BundleTurn{
		{TurnID: "a", PromptExcerpt: "ok", PromptExcerptHash: "h1", TS: 1},
		{TurnID: "b", PromptExcerpt: "continue", PromptExcerptHash: "h2", TS: 2},
	}
	runner := &fakeLLMRunner{responses: []*llm.GenerateTextResult{{Text: "must not be called"}}}

	got, err := BuildIntentLedger(context.Background(), runner, turns)
	if err != nil {
		t.Fatalf("BuildIntentLedger: %v", err)
	}
	if len(got.Items) != 0 {
		t.Errorf("Items len = %d, want 0", len(got.Items))
	}
	if got.PrefilteredCount != 2 {
		t.Errorf("PrefilteredCount = %d, want 2", got.PrefilteredCount)
	}
	if runner.calls != 0 {
		t.Errorf("calls = %d, want 0 (classifier must not run when nothing survives the pre-filter)", runner.calls)
	}
}

// A long bundle chunks into multiple batches. All batches produce
// items and the merged ledger stays in original input order.
func TestBuildIntentLedger_BatchesLargeBundleInInputOrder(t *testing.T) {
	// Two full batches plus a tail so we exercise both boundary paths.
	total := IntentClassifierBatchSize*2 + 5
	turns := make([]BundleTurn, total)
	for i := range turns {
		turns[i] = BundleTurn{
			TurnID:            fmt.Sprintf("t-%03d", i),
			PromptExcerpt:     fmt.Sprintf("substantive prompt number %d that is long enough to skip prefilter", i),
			PromptExcerptHash: fmt.Sprintf("h-%03d", i),
			TS:                int64(1000 + i),
		}
	}
	// Each batch echoes a valid classification for every turn it was
	// given. Because runOneClassifierBatch reads the prompt to know
	// which turns to answer, use a canned-response synthesizer.
	runner := &syntheticClassifierRunner{}

	got, err := BuildIntentLedger(context.Background(), runner, turns)
	if err != nil {
		t.Fatalf("BuildIntentLedger: %v", err)
	}
	if got.BatchesTotal != 3 {
		t.Errorf("BatchesTotal = %d, want 3", got.BatchesTotal)
	}
	if got.BatchesFailed != 0 {
		t.Errorf("BatchesFailed = %d, want 0", got.BatchesFailed)
	}
	if len(got.Items) != total {
		t.Fatalf("Items len = %d, want %d", len(got.Items), total)
	}
	for i, it := range got.Items {
		want := fmt.Sprintf("t-%03d", i)
		if it.TurnID != want {
			t.Errorf("Items[%d].TurnID = %q, want %q (batches must merge in input order)", i, it.TurnID, want)
			break
		}
	}
}

// A batch that fails LLM parsing does not fail the whole run. The
// surviving batches contribute their items; BatchesFailed records the
// failed batch.
func TestBuildIntentLedger_PartialBatchFailureKeepsSurvivors(t *testing.T) {
	total := IntentClassifierBatchSize + 5 // 2 batches
	turns := make([]BundleTurn, total)
	for i := range turns {
		turns[i] = BundleTurn{
			TurnID:            fmt.Sprintf("t-%03d", i),
			PromptExcerpt:     fmt.Sprintf("substantive prompt %d that survives prefilter", i),
			PromptExcerptHash: fmt.Sprintf("h-%03d", i),
			TS:                int64(1000 + i),
		}
	}
	// One batch parses cleanly; the other returns non-JSON text.
	runner := &splitClassifierRunner{
		failIndex:      1, // second batch (5-turn tail) returns non-JSON text
		goodResponders: &syntheticClassifierRunner{},
	}

	got, err := BuildIntentLedger(context.Background(), runner, turns)
	if err != nil {
		t.Fatalf("BuildIntentLedger: %v (partial failure must not abort the run)", err)
	}
	if got.BatchesTotal != 2 {
		t.Errorf("BatchesTotal = %d, want 2", got.BatchesTotal)
	}
	if got.BatchesFailed != 1 {
		t.Errorf("BatchesFailed = %d, want 1", got.BatchesFailed)
	}
	if len(got.Items) == 0 {
		t.Errorf("Items empty; the good batch should still have produced items")
	}
	// The 5-turn tail is where all invalid items concentrate.
	if got.InvalidCount < 5 {
		t.Errorf("InvalidCount = %d, want >= 5 (all turns in the failed batch are invalid)", got.InvalidCount)
	}
}

// Every batch failing surfaces as ErrIntentClassifierFailed only when
// no items survived. The error wraps the sentinel so ReasonCodeFor can
// map it to intent_classification_failed.
func TestBuildIntentLedger_AllBatchesFailReturnsClassifierErr(t *testing.T) {
	total := IntentClassifierBatchSize + 1 // 2 batches
	turns := make([]BundleTurn, total)
	for i := range turns {
		turns[i] = BundleTurn{
			TurnID:            fmt.Sprintf("t-%03d", i),
			PromptExcerpt:     fmt.Sprintf("substantive prompt %d that survives prefilter", i),
			PromptExcerptHash: fmt.Sprintf("h-%03d", i),
			TS:                int64(1000 + i),
		}
	}
	// Every batch returns non-JSON text so no valid items land.
	runner := &constantResponseRunner{text: "definitely not JSON"}
	_, err := BuildIntentLedger(context.Background(), runner, turns)
	if !errors.Is(err, ErrIntentClassifierFailed) {
		t.Errorf("err = %v, want wrapping ErrIntentClassifierFailed", err)
	}
}

// When over half the batches fail, Unreliable is set even if items
// survived. Coverage consumers use this bit to mark a run as
// degraded regardless of the per-item ratio.
func TestBuildIntentLedger_UnreliableWhenMajorityOfBatchesFail(t *testing.T) {
	total := IntentClassifierBatchSize*3 + 1 // 4 batches
	turns := make([]BundleTurn, total)
	for i := range turns {
		turns[i] = BundleTurn{
			TurnID:            fmt.Sprintf("t-%03d", i),
			PromptExcerpt:     fmt.Sprintf("substantive prompt %d that survives prefilter", i),
			PromptExcerptHash: fmt.Sprintf("h-%03d", i),
			TS:                int64(1000 + i),
		}
	}
	// Three of four batches fail; the last one produces items.
	runner := &splitClassifierRunner{
		failIndex:       -1, // sentinel unused
		failEveryExcept: 3,  // batch index 3 (the tail) is the only success
		goodResponders:  &syntheticClassifierRunner{},
	}
	got, err := BuildIntentLedger(context.Background(), runner, turns)
	if err != nil {
		t.Fatalf("BuildIntentLedger: %v", err)
	}
	if !got.Unreliable {
		t.Errorf("Unreliable should be true when %d/%d batches failed", got.BatchesFailed, got.BatchesTotal)
	}
	if len(got.Items) == 0 {
		t.Errorf("Items should still carry the one successful batch's classifications")
	}
}

// Every batch returns parseable JSON, but every item inside is
// invalid (unknown kind), and repair returns more of the same. The
// ledger reports the run as unreliable so coverage consumers can
// distinguish this from a successful empty result.
func TestBuildIntentLedger_ZeroValidItemsIsUnreliable(t *testing.T) {
	total := IntentClassifierBatchSize + 1 // 2 batches, both parse fine
	turns := make([]BundleTurn, total)
	for i := range turns {
		turns[i] = BundleTurn{
			TurnID:            fmt.Sprintf("t-%03d", i),
			PromptExcerpt:     fmt.Sprintf("substantive prompt %d that survives prefilter", i),
			PromptExcerptHash: fmt.Sprintf("h-%03d", i),
			TS:                int64(1000 + i),
		}
	}
	// Response has one entry per turn but every kind is invalid so
	// classifyAndValidate drops all of them; repair returns the same
	// shape and also drops. Batches themselves are batchOK because
	// their LLM calls succeeded and their JSON parsed.
	runner := &invalidKindClassifierRunner{}
	got, err := BuildIntentLedger(context.Background(), runner, turns)
	if err != nil {
		t.Fatalf("BuildIntentLedger: %v (batches parsed, so this should return a degraded ledger)", err)
	}
	if len(got.Items) != 0 {
		t.Fatalf("Items len = %d, want 0 (every item was invalid)", len(got.Items))
	}
	if got.BatchesFailed != 0 {
		t.Errorf("BatchesFailed = %d, want 0 (batches parsed even though items were rejected)", got.BatchesFailed)
	}
	if got.InvalidCount != len(turns) {
		t.Errorf("InvalidCount = %d, want %d", got.InvalidCount, len(turns))
	}
	if !got.Unreliable {
		t.Errorf("Unreliable should be true when 0 valid items survive; got %+v", got)
	}
}

// invalidKindClassifierRunner returns one item per prompted turn but
// with a kind classifyAndValidate rejects on both the initial call
// and the repair retry. This simulates a classifier that produces
// well-formed JSON with unusable items.
type invalidKindClassifierRunner struct {
	mu    sync.Mutex
	calls int
}

func (r *invalidKindClassifierRunner) GenerateText(_ context.Context, prompt string) (*llm.GenerateTextResult, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	ids := turnIDsInPrompt(prompt)
	type item struct {
		TurnID  string `json:"turn_id"`
		Kind    string `json:"kind"`
		Summary string `json:"summary"`
	}
	items := make([]item, 0, len(ids))
	for _, id := range ids {
		items = append(items, item{TurnID: id, Kind: "NOT_A_KIND", Summary: "bad"})
	}
	body, _ := json.Marshal(items)
	return &llm.GenerateTextResult{Text: string(body), Provider: "claude_code", Model: "claude-opus-4-7"}, nil
}

// A ctx that expires before batches launch marks every batch as
// deadline-missed. No items survive, so the error surfaces; the
// counter records the reason so coverage distinguishes it from a
// provider failure.
func TestBuildIntentLedger_DeadlineExceededBeforeStart(t *testing.T) {
	total := IntentClassifierBatchSize + 1
	turns := make([]BundleTurn, total)
	for i := range turns {
		turns[i] = BundleTurn{
			TurnID:            fmt.Sprintf("t-%03d", i),
			PromptExcerpt:     fmt.Sprintf("substantive prompt %d that survives prefilter", i),
			PromptExcerptHash: fmt.Sprintf("h-%03d", i),
			TS:                int64(1000 + i),
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // deadline exceeded before any batch runs
	runner := &constantResponseRunner{text: "must not be called"}
	_, err := BuildIntentLedger(ctx, runner, turns)
	if !errors.Is(err, ErrIntentClassifierFailed) {
		t.Errorf("err = %v, want ErrIntentClassifierFailed when the deadline expired before start", err)
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
