package intentgap

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// CandidateKind identifies the predicate that produced a candidate.
type CandidateKind string

const (
	CandUnderImplNoRetrievedScope CandidateKind = "under_impl_no_retrieved_scope"
	CandUnderImplPartialScope     CandidateKind = "under_impl_partial_scope"
)

// Candidate is one generated under_impl candidate for verifier input.
//
// Track A uses NearMisses and no DiffPointers. Track B uses
// DiffPointers and MissingCategory, and leaves NearMisses empty.
type Candidate struct {
	ID              string
	Kind            CandidateKind
	IntentID        string
	Score           float64
	Reason          string
	DiffPointers    []HunkRef
	ActionIDs       []string
	NearMisses      []string
	MissingCategory FileCategory
}

// Candidate generation limits. Overflow is reported through
// candidates_truncated_at_cap.
const (
	MaxCandidatesPerRun         = 12
	MaxDiffPointersPerCandidate = 3
	MaxActionsPerCandidate      = 5
	MaxNearMissesInCandidate    = 5
)

// intentStoplist contains common filler and classifier-boilerplate
// words that should not trigger Track A on their own.
var intentStoplist = map[string]bool{
	// Common English filler.
	"this": true, "that": true, "these": true, "those": true,
	"with": true, "from": true, "into": true, "onto": true,
	"what": true, "when": true, "where": true, "which": true,
	"while": true, "since": true,
	"please": true, "would": true, "could": true, "should": true,
	"want": true, "need": true, "going": true, "ahead": true,
	"very": true, "much": true, "just": true, "only": true,
	"also": true, "like": true, "make": true, "ensure": true,
	"work": true, "thing": true, "stuff": true,

	// Classifier-boilerplate emitted by the neutral third-person
	// summary instruction in renderIntentClassifierPrompt.
	"user": true, "asks": true, "asked": true, "asking": true,
	"states": true, "stated": true, "stating": true,
	"explicitly": true,
}

// categoryKeywords maps intent words to categories Track B expects
// to see in the retrieved scope.
var categoryKeywords = []struct {
	keyword  string
	category FileCategory
}{
	{"test", CatTest}, {"spec", CatTest}, {"fixture", CatTest},
	{"doc", CatDoc}, {"docs", CatDoc}, {"readme", CatDoc}, {"help", CatDoc},
	{"config", CatConfig}, {"setting", CatConfig}, {"flag", CatConfig},
	{"schema", CatSchema}, {"migration", CatSchema},
}

// UnderImplGenInput contains the ledgers used by under_impl candidate
// generation.
type UnderImplGenInput struct {
	Intents []IntentItem
	Scopes  map[string]RetrievedScope // keyed by IntentID
	Change  ChangeLedger
	Action  ActionLedger
}

// UnderImplGenResult contains kept candidates and cap diagnostics.
type UnderImplGenResult struct {
	Candidates     []Candidate
	TruncatedAtCap int
}

// GenerateUnderImplCandidates produces Track A and Track B candidates
// deterministically, then applies the per-run cap.
func GenerateUnderImplCandidates(in UnderImplGenInput) UnderImplGenResult {
	var all []Candidate
	for _, intent := range in.Intents {
		if intent.Kind != IntentRequest && intent.Kind != IntentCorrection {
			continue
		}
		scope := in.Scopes[intent.ID]

		if len(scope.Files) == 0 {
			if c, ok := buildTrackACandidate(intent, scope, in.Change, in.Action); ok {
				all = append(all, c)
			}
			continue
		}

		all = append(all, buildTrackBCandidates(intent, scope, in.Change, in.Action)...)
	}

	// Sort: highest score first; intent kind (request beats
	// correction) breaks ties; oldest turn breaks remaining ties.
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Score != all[j].Score {
			return all[i].Score > all[j].Score
		}
		ki := candidateKindRank(in.Intents, all[i].IntentID)
		kj := candidateKindRank(in.Intents, all[j].IntentID)
		if ki != kj {
			return ki > kj
		}
		return candidateTurnTS(in.Intents, all[i].IntentID) < candidateTurnTS(in.Intents, all[j].IntentID)
	})

	result := UnderImplGenResult{}
	if len(all) > MaxCandidatesPerRun {
		result.TruncatedAtCap = len(all) - MaxCandidatesPerRun
		all = all[:MaxCandidatesPerRun]
	}
	result.Candidates = all
	return result
}

// buildTrackACandidate emits Track A when no file crossed the
// retrieval threshold and the intent has a non-stoplisted token.
func buildTrackACandidate(intent IntentItem, scope RetrievedScope, change ChangeLedger, action ActionLedger) (Candidate, bool) {
	if !hasMeaningfulKeyword(intent) {
		return Candidate{}, false
	}
	near := scope.NearMisses
	if len(near) > MaxNearMissesInCandidate {
		near = near[:MaxNearMissesInCandidate]
	}
	score := highestNearMissScore(scope)
	actions := collectTimeAdjacentActions(intent, action, change, MaxActionsPerCandidate)
	c := Candidate{
		Kind:       CandUnderImplNoRetrievedScope,
		IntentID:   intent.ID,
		Score:      score,
		Reason:     "intent has meaningful keywords but no retrieved file crossed the score threshold; nothing in the diff plausibly addresses this ask",
		ActionIDs:  actions,
		NearMisses: append([]string(nil), near...),
	}
	c.ID = deriveCandidateID(c.Kind, intent.ID, "", near)
	return c, true
}

// buildTrackBCandidates emits one Track B candidate per missing
// category implied by the intent.
func buildTrackBCandidates(intent IntentItem, scope RetrievedScope, change ChangeLedger, action ActionLedger) []Candidate {
	intentLower := strings.ToLower(intent.Summary + " " + intent.Excerpt + " " + strings.Join(intent.Hint.HintKeywords, " "))
	presentCategories := categoriesInScope(scope, change)

	var out []Candidate
	emittedCat := map[FileCategory]bool{}
	for _, ck := range categoryKeywords {
		if emittedCat[ck.category] {
			continue
		}
		if !containsWord(intentLower, ck.keyword) {
			continue
		}
		if presentCategories[ck.category] {
			continue
		}
		emittedCat[ck.category] = true

		score := highestRetrievedScore(scope)
		pointers := selectDiffPointers(scope, MaxDiffPointersPerCandidate)
		actions := collectActionsOnScopeFiles(scope, action, MaxActionsPerCandidate)
		c := Candidate{
			Kind:            CandUnderImplPartialScope,
			IntentID:        intent.ID,
			Score:           score,
			Reason:          "intent matched files in the diff but no " + string(ck.category) + " category file was changed alongside them",
			DiffPointers:    pointers,
			ActionIDs:       actions,
			MissingCategory: ck.category,
		}
		c.ID = deriveCandidateID(c.Kind, intent.ID, ck.category, scope.Files)
		out = append(out, c)
	}
	return out
}

// hasMeaningfulKeyword checks for at least one non-stoplisted token.
// HintKeywords are included because they often carry the code noun.
func hasMeaningfulKeyword(intent IntentItem) bool {
	tokens := tokenizeIntent(intent.Summary + " " + intent.Excerpt + " " + strings.Join(intent.Hint.HintKeywords, " "))
	for _, t := range tokens {
		if !intentStoplist[t] {
			return true
		}
	}
	return false
}

// containsWord checks for a left-boundary match. The right side is
// open so "test" matches "tests", "testing", and "tested".
func containsWord(lowerText, word string) bool {
	for {
		i := strings.Index(lowerText, word)
		if i < 0 {
			return false
		}
		left := i == 0 || !isWordChar(lowerText[i-1])
		if left {
			return true
		}
		lowerText = lowerText[i+1:]
	}
}

func isWordChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_'
}

// categoriesInScope returns categories present in the retrieved scope.
func categoriesInScope(scope RetrievedScope, change ChangeLedger) map[FileCategory]bool {
	out := map[FileCategory]bool{}
	for _, p := range scope.Files {
		if f := change.ByPath[p]; f != nil {
			out[f.Category] = true
		}
	}
	return out
}

// selectDiffPointers copies the top hunk refs from the retrieved scope.
func selectDiffPointers(scope RetrievedScope, max int) []HunkRef {
	if len(scope.HunkRefs) == 0 {
		return nil
	}
	n := max
	if len(scope.HunkRefs) < n {
		n = len(scope.HunkRefs)
	}
	out := make([]HunkRef, n)
	copy(out, scope.HunkRefs[:n])
	return out
}

// collectActionsOnScopeFiles copies retrieved action IDs up to max.
func collectActionsOnScopeFiles(scope RetrievedScope, _ ActionLedger, max int) []string {
	if len(scope.ActionIDs) == 0 {
		return nil
	}
	n := max
	if len(scope.ActionIDs) < n {
		n = len(scope.ActionIDs)
	}
	out := make([]string, n)
	copy(out, scope.ActionIDs[:n])
	return out
}

// collectTimeAdjacentActions returns actions within the intent's time
// window, regardless of file. Track A has no file scope.
func collectTimeAdjacentActions(intent IntentItem, action ActionLedger, _ ChangeLedger, max int) []string {
	windowStart := intent.TurnTS - int64(ActionAdjacencyWindowSec)
	windowEnd := intent.TurnTS + int64(ActionAdjacencyWindowSec)
	var out []string
	for _, a := range action.All {
		if a.TS < windowStart || a.TS > windowEnd {
			continue
		}
		out = append(out, a.ActionID)
		if len(out) >= max {
			break
		}
	}
	return out
}

// highestRetrievedScore returns the top score in the retrieved scope.
func highestRetrievedScore(scope RetrievedScope) float64 {
	var max float64
	for _, p := range scope.Files {
		if s, ok := scope.Scores[p]; ok && s > max {
			max = s
		}
	}
	return max
}

// highestNearMissScore returns Track A's best below-threshold score.
func highestNearMissScore(scope RetrievedScope) float64 {
	var max float64
	for _, p := range scope.NearMisses {
		if s, ok := scope.Scores[p]; ok && s > max {
			max = s
		}
	}
	return max
}

// deriveCandidateID hashes the stable candidate identity fields.
// Files are sorted so map iteration order does not affect the ID.
func deriveCandidateID(kind CandidateKind, intentID string, missingCat FileCategory, files []string) string {
	sorted := append([]string(nil), files...)
	sort.Strings(sorted)
	payload := string(kind) + "\x00" + intentID + "\x00" + string(missingCat) + "\x00" + strings.Join(sorted, ",")
	h := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(h[:8])
}

// candidateKindRank ranks requests above corrections for sort ties.
func candidateKindRank(intents []IntentItem, intentID string) int {
	for _, it := range intents {
		if it.ID == intentID {
			if it.Kind == IntentRequest {
				return 2
			}
			return 1
		}
	}
	return 0
}

// candidateTurnTS returns the intent timestamp, or max int64 when
// the intent is missing.
func candidateTurnTS(intents []IntentItem, intentID string) int64 {
	for _, it := range intents {
		if it.ID == intentID {
			return it.TurnTS
		}
	}
	return 1<<63 - 1
}
