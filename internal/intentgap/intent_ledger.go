package intentgap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/semanticash/cli/internal/llm"
)

// IntentKind labels each captured user turn by its role in the
// conversation. Only `request` and `correction` drive under_impl
// candidate generation; the other kinds are kept on the ledger so
// future passes (constraint/preference/defer/context surfaces) can
// read them without a second classification pass.
type IntentKind string

const (
	IntentRequest    IntentKind = "request"
	IntentConstraint IntentKind = "constraint"
	IntentCorrection IntentKind = "correction"
	IntentPreference IntentKind = "preference"
	IntentDefer      IntentKind = "defer"
	IntentContext    IntentKind = "context"
)

// IntentScopeHint carries best-effort retrieval seeds emitted by the
// classifier from the prompt text itself. HintFiles must be paths
// the prompt literally mentions; HintKeywords are code-ish nouns and
// verbs the classifier extracted. Both are biased toward recall —
// retrieval intersects them against the actual diff anyway, so a
// false positive hint costs nothing while a false negative loses a
// candidate.
type IntentScopeHint struct {
	HintFiles    []string `json:"files"`
	HintKeywords []string `json:"keywords"`
}

// IntentItem is one row on the intent ledger.
//
// Summary is the classifier's neutral third-person rendering and is
// the field the verifier reads. Excerpt and ExcerptHash are copied
// verbatim from the source BundleTurn so cite-or-drop can verify a
// finding's expected_intent against the original capture; they are
// never derived from the classifier's output.
type IntentItem struct {
	ID          string
	Kind        IntentKind
	Summary     string
	Excerpt     string
	ExcerptHash string
	TurnID      string
	TurnTS      int64
	Hint        IntentScopeHint
}

// IntentLedger is the structured view of captured turns the candidate
// pipeline consumes. InvalidCount counts turns the classifier could
// not produce a well-formed item for (even after one repair retry);
// Unreliable is set when the valid ratio fell below
// MinValidIntentRatio so downstream coverage can surface the
// degradation.
type IntentLedger struct {
	Items        []IntentItem
	InvalidCount int
	Unreliable   bool
}

// MinValidIntentRatio is the floor for "we got enough good intents
// to trust the rest of the pipeline." Below this, the run still
// proceeds but coverage records intent_classification_unreliable.
const MinValidIntentRatio = 0.6

// ErrIntentClassifierFailed wraps the underlying LLM failure when
// the classifier call (and its repair retry) cannot produce any
// parseable response. The service layer maps this to the
// `intent_classification_failed` reason.
var ErrIntentClassifierFailed = errors.New("intent classifier: no parseable response")

// IntentClassifierRunner is the slice of the LLM registry the intent
// ledger needs. Defined locally so tests can substitute a fake
// without depending on the full registry.
type IntentClassifierRunner interface {
	GenerateText(ctx context.Context, prompt string) (*llm.GenerateTextResult, error)
}

// BuildIntentLedger runs one classifier call over the bundle's turns
// and constructs an IntentLedger from the validated items. Items the
// classifier produced in malformed shape get one repair retry. Items
// still malformed after retry are dropped and counted in
// InvalidCount; if the resulting valid ratio is below
// MinValidIntentRatio, Unreliable is set.
//
// turns whose own TurnID is empty are excluded up front so the
// classifier is not asked to classify a prompt that cannot be cited.
func BuildIntentLedger(ctx context.Context, runner IntentClassifierRunner, turns []BundleTurn) (IntentLedger, error) {
	usable := filterUsableTurns(turns)
	if len(usable) == 0 {
		return IntentLedger{}, nil
	}

	prompt := renderIntentClassifierPrompt(usable)
	res, err := runner.GenerateText(ctx, prompt)
	if err != nil {
		return IntentLedger{}, fmt.Errorf("%w: %v", ErrIntentClassifierFailed, err)
	}

	rawItems, parseErr := extractIntentRawItems(res.Text)
	if parseErr != nil {
		return IntentLedger{}, fmt.Errorf("%w: %v", ErrIntentClassifierFailed, parseErr)
	}

	valid, malformed := classifyAndValidate(rawItems, usable)

	if len(malformed) > 0 {
		repairPrompt := renderIntentRepairPrompt(malformed)
		retry, retryErr := runner.GenerateText(ctx, repairPrompt)
		if retryErr == nil {
			repaired, _ := extractIntentRawItems(retry.Text)
			if len(repaired) > 0 {
				repairedValid, _ := classifyAndValidate(repaired, malformed)
				valid = mergeValidByTurnID(valid, repairedValid)
			}
		}
	}

	// Restore input turn order. mergeValidByTurnID appends repaired
	// items after first-pass items; without this sort, a turn that
	// validated only on repair lands after later turns that
	// validated on the initial pass. The classifier prompt asks for
	// turn order, so the ledger should keep it.
	valid = sortByInputOrder(valid, usable)

	ledger := IntentLedger{Items: valid}
	ledger.InvalidCount = len(usable) - len(valid)
	if len(usable) > 0 {
		validRatio := float64(len(valid)) / float64(len(usable))
		if validRatio < MinValidIntentRatio {
			ledger.Unreliable = true
		}
	}
	return ledger, nil
}

// sortByInputOrder reorders items so they match the position of their
// TurnID in the original turns slice. Items whose TurnID is somehow
// not in the input (defensive guard; classifyAndValidate already
// drops hallucinations) end up at the tail in their existing order.
func sortByInputOrder(items []IntentItem, turns []BundleTurn) []IntentItem {
	if len(items) <= 1 {
		return items
	}
	pos := make(map[string]int, len(turns))
	for i, t := range turns {
		pos[t.TurnID] = i
	}
	const sentinel = -1
	sort.SliceStable(items, func(i, j int) bool {
		pi, oki := pos[items[i].TurnID]
		pj, okj := pos[items[j].TurnID]
		if !oki {
			pi = sentinel
		}
		if !okj {
			pj = sentinel
		}
		// Items not in the input map sort to the tail, behind any
		// item the input map did know about.
		if pi == sentinel && pj != sentinel {
			return false
		}
		if pi != sentinel && pj == sentinel {
			return true
		}
		return pi < pj
	})
	return items
}

// filterUsableTurns drops turns the classifier cannot anchor an
// intent against: a turn with no TurnID has nothing for cite-or-drop
// to verify against later, so feeding it to the classifier would
// only increase prompt size.
func filterUsableTurns(turns []BundleTurn) []BundleTurn {
	out := make([]BundleTurn, 0, len(turns))
	for _, t := range turns {
		if t.TurnID == "" {
			continue
		}
		out = append(out, t)
	}
	return out
}

// rawIntentItem is the unvalidated shape we expect the classifier to
// produce. Decoded into typed IntentItem only after kind/summary/etc
// have been checked. json.RawMessage on Hint lets us tolerate a
// missing or null hint object without breaking the whole item.
type rawIntentItem struct {
	TurnID  string          `json:"turn_id"`
	Kind    string          `json:"kind"`
	Summary string          `json:"summary"`
	Hint    json.RawMessage `json:"hint"`
}

// extractIntentRawItems parses the classifier response into a slice
// of rawIntentItem. It accepts a top-level JSON array (preferred) or
// the first JSON array embedded inside prose / a code fence; both
// shapes appear in the wild from different writer CLIs.
func extractIntentRawItems(text string) ([]rawIntentItem, error) {
	trim := strings.TrimSpace(text)
	if trim == "" {
		return nil, fmt.Errorf("empty classifier response")
	}
	candidates := []string{trim}
	for _, m := range codeFencePattern.FindAllStringSubmatch(trim, -1) {
		if len(m) >= 2 {
			candidates = append(candidates, strings.TrimSpace(m[1]))
		}
	}
	if start := strings.IndexByte(trim, '['); start >= 0 {
		if end := strings.LastIndexByte(trim, ']'); end > start {
			candidates = append(candidates, trim[start:end+1])
		}
	}
	for _, c := range candidates {
		var items []rawIntentItem
		if err := json.Unmarshal([]byte(c), &items); err == nil {
			return items, nil
		}
	}
	return nil, fmt.Errorf("classifier response is not a JSON array of intent items")
}

// classifyAndValidate keeps the items whose shape and citations
// match the inputs; malformed items come back as the BundleTurn
// slice the repair retry feeds back to the classifier.
//
// The prompt asks the classifier for exactly one item per turn. A
// duplicate response for the same TurnID violates that contract and
// invalidates that turn: both duplicates are dropped and the input
// turn falls through to repair. "First wins" would hide an
// inconsistent classifier response that the repair path can handle.
func classifyAndValidate(rawItems []rawIntentItem, inputs []BundleTurn) ([]IntentItem, []BundleTurn) {
	byTurnID := make(map[string]BundleTurn, len(inputs))
	for _, t := range inputs {
		byTurnID[t.TurnID] = t
	}
	// Count how many times each TurnID appears across the response.
	// Duplicates are repaired rather than resolved by position.
	occurrences := map[string]int{}
	for _, r := range rawItems {
		if r.TurnID == "" {
			continue
		}
		occurrences[r.TurnID]++
	}
	seen := map[string]bool{}
	var valid []IntentItem
	for _, r := range rawItems {
		if r.TurnID == "" {
			continue
		}
		turn, ok := byTurnID[r.TurnID]
		if !ok {
			// Classifier hallucinated a turn_id; ignore it.
			continue
		}
		if occurrences[r.TurnID] > 1 {
			// Duplicate output for this TurnID: refuse to pick one.
			continue
		}
		if seen[r.TurnID] {
			// Defensive (occurrences guard already excludes duplicates).
			continue
		}
		item, ok := buildIntentItem(r, turn)
		if !ok {
			continue
		}
		valid = append(valid, item)
		seen[r.TurnID] = true
	}
	// Any input turn without a valid item, including a turn with
	// duplicate output, goes back as malformed and is repaired.
	var malformed []BundleTurn
	for _, t := range inputs {
		if !seen[t.TurnID] {
			malformed = append(malformed, t)
		}
	}
	return valid, malformed
}

// buildIntentItem applies the per-item validation rules and copies
// the immutable citation fields (Excerpt / ExcerptHash) verbatim
// from the source BundleTurn. The classifier's Summary is trimmed
// and validated: an empty summary is rejected, while an oversize
// summary is truncated to the documented rune cap (not rejected).
// Truncation is rune-safe so a non-ASCII tail cannot be cut
// mid-character.
func buildIntentItem(raw rawIntentItem, turn BundleTurn) (IntentItem, bool) {
	kind, ok := parseIntentKind(raw.Kind)
	if !ok {
		return IntentItem{}, false
	}
	summary := strings.TrimSpace(raw.Summary)
	if summary == "" {
		return IntentItem{}, false
	}
	if utf8.RuneCountInString(summary) > maxIntentSummaryRunes {
		summary = truncateRunes(summary, maxIntentSummaryRunes)
	}
	hint := parseIntentScopeHint(raw.Hint)
	return IntentItem{
		ID:          deriveIntentID(turn.TurnID, summary),
		Kind:        kind,
		Summary:     summary,
		Excerpt:     turn.PromptExcerpt,
		ExcerptHash: turn.PromptExcerptHash,
		TurnID:      turn.TurnID,
		TurnTS:      turn.TS,
		Hint:        hint,
	}, true
}

// maxIntentSummaryRunes caps the classifier's summary at the
// documented 200-rune limit. Counting runes (not bytes) so a
// summary full of non-ASCII characters does not silently truncate
// halfway through a multi-byte sequence.
const maxIntentSummaryRunes = 200

func parseIntentKind(s string) (IntentKind, bool) {
	switch IntentKind(strings.TrimSpace(strings.ToLower(s))) {
	case IntentRequest:
		return IntentRequest, true
	case IntentConstraint:
		return IntentConstraint, true
	case IntentCorrection:
		return IntentCorrection, true
	case IntentPreference:
		return IntentPreference, true
	case IntentDefer:
		return IntentDefer, true
	case IntentContext:
		return IntentContext, true
	}
	return "", false
}

// parseIntentScopeHint decodes the classifier's hint payload, never
// failing. A missing, null, or malformed hint returns an empty
// IntentScopeHint. The verifier and retrieval treat missing hints as
// valid because the classifier may have nothing concrete to extract.
// A malformed hint should not reject an otherwise valid classification.
func parseIntentScopeHint(raw json.RawMessage) IntentScopeHint {
	if len(raw) == 0 {
		return IntentScopeHint{}
	}
	var h IntentScopeHint
	if err := json.Unmarshal(raw, &h); err != nil {
		return IntentScopeHint{}
	}
	// Drop any blanks the classifier may have emitted.
	h.HintFiles = trimAndDeduplicate(h.HintFiles)
	h.HintKeywords = trimAndDeduplicate(h.HintKeywords)
	return h
}

func trimAndDeduplicate(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		t := strings.TrimSpace(s)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// deriveIntentID is sha256(turn_id + canonical_summary) truncated to
// 16 hex chars, mirroring the canonical id style used elsewhere in
// the package. Two intents with the same TurnID but different
// summaries produce different IDs. Duplicate classifier entries for a
// turn are rejected before IDs affect ledger output, but the
// derivation remains stable for tests and diagnostics.
func deriveIntentID(turnID, summary string) string {
	h := sha256.Sum256([]byte(turnID + "\x00" + summary))
	return hex.EncodeToString(h[:8])
}

// truncateRunes returns a prefix of s containing at most n runes.
// The cut always falls on a rune boundary because we iterate one
// rune at a time.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// mergeValidByTurnID appends repaired items whose TurnIDs are not
// already present in the original valid slice. Ordering follows
// "original valid first, repaired second" so call-site assertions
// about stability hold.
func mergeValidByTurnID(original, repaired []IntentItem) []IntentItem {
	if len(repaired) == 0 {
		return original
	}
	present := map[string]bool{}
	for _, it := range original {
		present[it.TurnID] = true
	}
	out := append([]IntentItem(nil), original...)
	for _, it := range repaired {
		if present[it.TurnID] {
			continue
		}
		out = append(out, it)
		present[it.TurnID] = true
	}
	return out
}

// renderIntentClassifierPrompt builds the single-call classifier
// prompt. The format is intentionally tight: instructions first,
// then the turns themselves, then a strict reminder about output
// shape. The classifier reads turns in the same order BuildIntentLedger
// passes them, so the model never has to re-order anything.
func renderIntentClassifierPrompt(turns []BundleTurn) string {
	var b strings.Builder
	b.WriteString("You are extracting structured intent items from a sequence of user\n")
	b.WriteString("prompts in a coding session. For each prompt emit one IntentItem:\n\n")
	b.WriteString("  kind: one of [request, constraint, correction, preference, defer, context]\n")
	b.WriteString("    - request: user asks for new work (implementation, fix, design)\n")
	b.WriteString("    - constraint: user states a rule the work must follow\n")
	b.WriteString("    - correction: user reverses or amends a prior decision\n")
	b.WriteString("    - preference: user states a stylistic / framing preference\n")
	b.WriteString("    - defer: user explicitly defers work to later\n")
	b.WriteString("    - context: question, discussion, status check (NOT a work item)\n\n")
	b.WriteString("  summary: <=200 chars, neutral third-person (\"user asks ...\",\n")
	b.WriteString("           not \"I want ...\")\n\n")
	b.WriteString("  hint:\n")
	b.WriteString("    files: file paths LITERALLY mentioned in the prompt; do NOT\n")
	b.WriteString("           invent paths that are not in the prompt text.\n")
	b.WriteString("    keywords: code-ish nouns/verbs visible in the prompt (function\n")
	b.WriteString("              names, user-visible features, file extensions).\n")
	b.WriteString("              Prefer recall over precision.\n\n")
	b.WriteString("Reply with ONLY a JSON array, one object per turn, in turn order.\n")
	b.WriteString("Each object MUST include turn_id verbatim from the input. No\n")
	b.WriteString("markdown code fences, no commentary outside the JSON.\n\n")
	b.WriteString("TURNS:\n")
	for _, t := range turns {
		fmt.Fprintf(&b, "\n- turn_id=%s\n  excerpt: %s\n", t.TurnID, t.PromptExcerpt)
	}
	return b.String()
}

// renderIntentRepairPrompt re-issues the classification request for
// the malformed subset. The repair prompt keeps the schema reminder
// short — the classifier already saw the full rules on the first
// call — and lists only the turns that need re-classification.
func renderIntentRepairPrompt(malformed []BundleTurn) string {
	var b strings.Builder
	b.WriteString("Your previous response did not produce a valid IntentItem for\n")
	b.WriteString("each of these turns. Re-emit ONLY these items as a JSON array\n")
	b.WriteString("matching the same schema as before:\n\n")
	b.WriteString("  {\"turn_id\":\"...\",\"kind\":\"<one of: request, constraint,\n")
	b.WriteString("   correction, preference, defer, context>\",\n")
	b.WriteString("   \"summary\":\"<=200 chars\", \"hint\":{\"files\":[],\"keywords\":[]}}\n\n")
	b.WriteString("Reply with ONLY the JSON array; no markdown fences, no prose.\n\n")
	b.WriteString("TURNS:\n")
	for _, t := range malformed {
		fmt.Fprintf(&b, "\n- turn_id=%s\n  excerpt: %s\n", t.TurnID, t.PromptExcerpt)
	}
	return b.String()
}
