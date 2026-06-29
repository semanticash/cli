package intentgap

import (
	"strings"
	"testing"
)

// candIntent builds the fields the candidate generator reads.
func candIntent(id string, kind IntentKind, ts int64, summary string, hintKeywords ...string) IntentItem {
	return IntentItem{
		ID:      id,
		Kind:    kind,
		TurnID:  id, // good enough for tests; deriveCandidateID never reads TurnID
		Summary: summary,
		Excerpt: summary,
		TurnTS:  ts,
		Hint:    IntentScopeHint{HintKeywords: hintKeywords},
	}
}

// scopeWithFiles builds a retrieved scope for Track B tests.
func scopeWithFiles(intentID string, files ...string) RetrievedScope {
	scope := RetrievedScope{IntentID: intentID, Files: files, Scores: map[string]float64{}}
	for _, p := range files {
		scope.Scores[p] = 0.5
	}
	return scope
}

// scopeWithNearMisses builds a below-threshold scope for Track A.
func scopeWithNearMisses(intentID string, paths ...string) RetrievedScope {
	scope := RetrievedScope{IntentID: intentID, NearMisses: paths, Scores: map[string]float64{}}
	for _, p := range paths {
		scope.Scores[p] = 0.2
	}
	return scope
}

// changeLedgerOf builds the path/category index used by Track B.
func changeLedgerOf(entries ...struct {
	path     string
	category FileCategory
}) ChangeLedger {
	ledger := ChangeLedger{ByPath: map[string]*ChangedFile{}}
	for _, e := range entries {
		ledger.Files = append(ledger.Files, ChangedFile{Path: e.path, Category: e.category})
	}
	for i := range ledger.Files {
		ledger.ByPath[ledger.Files[i].Path] = &ledger.Files[i]
	}
	return ledger
}

// Track A fires when no file crosses the retrieval threshold.
func TestGenerateUnderImpl_TrackAFiresOnNoRetrievedScope(t *testing.T) {
	intent := candIntent("i-1", IntentRequest, 100, "introduce retrieval scoring helper")
	scope := scopeWithNearMisses(intent.ID, "internal/zzz/foo.go")
	in := UnderImplGenInput{
		Intents: []IntentItem{intent},
		Scopes:  map[string]RetrievedScope{intent.ID: scope},
	}
	got := GenerateUnderImplCandidates(in)
	if len(got.Candidates) != 1 {
		t.Fatalf("Candidates len = %d, want 1", len(got.Candidates))
	}
	c := got.Candidates[0]
	if c.Kind != CandUnderImplNoRetrievedScope {
		t.Errorf("Kind = %q, want %q", c.Kind, CandUnderImplNoRetrievedScope)
	}
	if c.IntentID != intent.ID {
		t.Errorf("IntentID = %q, want %q", c.IntentID, intent.ID)
	}
	if len(c.NearMisses) != 1 || c.NearMisses[0] != "internal/zzz/foo.go" {
		t.Errorf("NearMisses = %v, want the near-miss path", c.NearMisses)
	}
	if len(c.DiffPointers) != 0 {
		t.Errorf("Track A should not carry DiffPointers; got %v", c.DiffPointers)
	}
}

// Stoplisted wording alone does not trigger Track A.
func TestGenerateUnderImpl_TrackAStoplistedIntentDoesNotFire(t *testing.T) {
	intent := candIntent("i-1", IntentRequest, 100, "please make this work")
	in := UnderImplGenInput{
		Intents: []IntentItem{intent},
		Scopes:  map[string]RetrievedScope{intent.ID: {IntentID: intent.ID}},
	}
	got := GenerateUnderImplCandidates(in)
	if len(got.Candidates) != 0 {
		t.Errorf("stoplisted intent should not produce Track A candidates; got %+v", got.Candidates)
	}
}

// HintKeywords can supply the meaningful token for Track A.
func TestGenerateUnderImpl_TrackAHintKeywordRescuesVagueSummary(t *testing.T) {
	intent := candIntent("i-1", IntentRequest, 100, "please make this work", "retrieval")
	in := UnderImplGenInput{
		Intents: []IntentItem{intent},
		Scopes:  map[string]RetrievedScope{intent.ID: {IntentID: intent.ID}},
	}
	got := GenerateUnderImplCandidates(in)
	if len(got.Candidates) != 1 {
		t.Errorf("hint keyword should make Track A fire; got %+v", got.Candidates)
	}
}

// Track B fires when an implied category is missing from scope.
func TestGenerateUnderImpl_TrackBFiresOnMissingTestCategory(t *testing.T) {
	intent := candIntent("i-1", IntentRequest, 100, "add tests for the new handler")
	change := changeLedgerOf(
		struct {
			path     string
			category FileCategory
		}{"internal/service/handler.go", CatCode},
	)
	scope := scopeWithFiles(intent.ID, "internal/service/handler.go")
	in := UnderImplGenInput{
		Intents: []IntentItem{intent},
		Scopes:  map[string]RetrievedScope{intent.ID: scope},
		Change:  change,
	}
	got := GenerateUnderImplCandidates(in)
	if len(got.Candidates) != 1 {
		t.Fatalf("expected 1 Track B candidate; got %+v", got.Candidates)
	}
	c := got.Candidates[0]
	if c.Kind != CandUnderImplPartialScope {
		t.Errorf("Kind = %q, want %q", c.Kind, CandUnderImplPartialScope)
	}
	if !strings.Contains(c.Reason, "test") {
		t.Errorf("Reason should name the missing category; got %q", c.Reason)
	}
}

// Track B does not fire when the implied category is present.
func TestGenerateUnderImpl_TrackBSilentWhenCategoryPresent(t *testing.T) {
	intent := candIntent("i-1", IntentRequest, 100, "add tests for the new handler")
	change := changeLedgerOf(
		struct {
			path     string
			category FileCategory
		}{"internal/service/handler.go", CatCode},
		struct {
			path     string
			category FileCategory
		}{"internal/service/handler_test.go", CatTest},
	)
	scope := scopeWithFiles(intent.ID, "internal/service/handler.go", "internal/service/handler_test.go")
	in := UnderImplGenInput{
		Intents: []IntentItem{intent},
		Scopes:  map[string]RetrievedScope{intent.ID: scope},
		Change:  change,
	}
	got := GenerateUnderImplCandidates(in)
	if len(got.Candidates) != 0 {
		t.Errorf("no candidate expected when test category is covered; got %+v", got.Candidates)
	}
}

// Track B emits one candidate per missing category.
func TestGenerateUnderImpl_TrackBOneCandidatePerMissingCategory(t *testing.T) {
	intent := candIntent("i-1", IntentRequest, 100, "update the docs and the schema for the new endpoint")
	change := changeLedgerOf(
		struct {
			path     string
			category FileCategory
		}{"internal/api/endpoint.go", CatCode},
	)
	scope := scopeWithFiles(intent.ID, "internal/api/endpoint.go")
	in := UnderImplGenInput{
		Intents: []IntentItem{intent},
		Scopes:  map[string]RetrievedScope{intent.ID: scope},
		Change:  change,
	}
	got := GenerateUnderImplCandidates(in)
	if len(got.Candidates) != 2 {
		t.Fatalf("expected 2 Track B candidates; got %+v", got.Candidates)
	}
	gotKinds := map[string]bool{}
	for _, c := range got.Candidates {
		// Each Reason names the missing category.
		for _, want := range []string{"doc", "schema"} {
			if strings.Contains(c.Reason, want) {
				gotKinds[want] = true
			}
		}
	}
	if !gotKinds["doc"] || !gotKinds["schema"] {
		t.Errorf("expected one candidate per missing category; got %+v", got.Candidates)
	}
}

// Only request and correction intents produce V1 candidates.
func TestGenerateUnderImpl_NonRequestKindsSkipped(t *testing.T) {
	for _, kind := range []IntentKind{IntentConstraint, IntentPreference, IntentDefer, IntentContext} {
		intent := candIntent("i-1", kind, 100, "add tests for the new handler")
		scope := scopeWithFiles(intent.ID, "internal/service/handler.go")
		change := changeLedgerOf(
			struct {
				path     string
				category FileCategory
			}{"internal/service/handler.go", CatCode},
		)
		in := UnderImplGenInput{
			Intents: []IntentItem{intent},
			Scopes:  map[string]RetrievedScope{intent.ID: scope},
			Change:  change,
		}
		got := GenerateUnderImplCandidates(in)
		if len(got.Candidates) != 0 {
			t.Errorf("kind=%q should not produce candidates; got %+v", kind, got.Candidates)
		}
	}
}

// Candidate IDs do not depend on file slice order.
func TestGenerateUnderImpl_CandidateIDIsDeterministic(t *testing.T) {
	id1 := deriveCandidateID(CandUnderImplPartialScope, "i-1", CatTest, []string{"b.go", "a.go"})
	id2 := deriveCandidateID(CandUnderImplPartialScope, "i-1", CatTest, []string{"a.go", "b.go"})
	if id1 != id2 {
		t.Errorf("candidate ID should not depend on file slice order; got %q vs %q", id1, id2)
	}
	id3 := deriveCandidateID(CandUnderImplNoRetrievedScope, "i-1", "", []string{"a.go", "b.go"})
	if id3 == id1 {
		t.Errorf("candidate ID must differ when kind differs")
	}
}

// MissingCategory is part of the Track B candidate identity.
func TestGenerateUnderImpl_TrackBCandidateIDsDifferByMissingCategory(t *testing.T) {
	intent := candIntent("i-1", IntentRequest, 100, "update the docs and the schema for the new endpoint")
	change := changeLedgerOf(
		struct {
			path     string
			category FileCategory
		}{"internal/api/endpoint.go", CatCode},
	)
	scope := scopeWithFiles(intent.ID, "internal/api/endpoint.go")
	in := UnderImplGenInput{
		Intents: []IntentItem{intent},
		Scopes:  map[string]RetrievedScope{intent.ID: scope},
		Change:  change,
	}
	got := GenerateUnderImplCandidates(in)
	if len(got.Candidates) != 2 {
		t.Fatalf("expected 2 Track B candidates; got %+v", got.Candidates)
	}
	if got.Candidates[0].ID == got.Candidates[1].ID {
		t.Errorf("Track B candidates with different missing categories must have distinct IDs; got %q == %q",
			got.Candidates[0].ID, got.Candidates[1].ID)
	}
	// MissingCategory is available without parsing Reason.
	cats := map[FileCategory]bool{
		got.Candidates[0].MissingCategory: true,
		got.Candidates[1].MissingCategory: true,
	}
	if !cats[CatDoc] || !cats[CatSchema] {
		t.Errorf("expected one candidate per missing category on the struct; got %v", cats)
	}
}

// Classifier boilerplate does not trigger Track A.
func TestGenerateUnderImpl_TrackAClassifierBoilerplateDoesNotFire(t *testing.T) {
	for _, summary := range []string{
		"user asks to make this work",
		"user asked to make this work",
	} {
		intent := candIntent("i-1", IntentRequest, 100, summary)
		in := UnderImplGenInput{
			Intents: []IntentItem{intent},
			Scopes:  map[string]RetrievedScope{intent.ID: {IntentID: intent.ID}},
		}
		got := GenerateUnderImplCandidates(in)
		if len(got.Candidates) != 0 {
			t.Errorf("classifier-style boilerplate summary should not trigger Track A; got %+v for summary=%q",
				got.Candidates, summary)
		}
	}
}

// MaxCandidatesPerRun keeps the highest-scoring candidates.
func TestGenerateUnderImpl_CapWithTruncatedCount(t *testing.T) {
	const N = MaxCandidatesPerRun + 4
	var intents []IntentItem
	scopes := map[string]RetrievedScope{}
	for i := 0; i < N; i++ {
		id := "i-" + itoa(i)
		intent := candIntent(id, IntentRequest, int64(100+i), "add tests for module "+itoa(i))
		intents = append(intents, intent)
		scope := scopeWithFiles(id, "module"+itoa(i)+".go")
		scope.Scores["module"+itoa(i)+".go"] = float64(N-i) / float64(N) // higher for earlier intents
		scopes[id] = scope
	}
	change := ChangeLedger{ByPath: map[string]*ChangedFile{}}
	for i := 0; i < N; i++ {
		change.Files = append(change.Files, ChangedFile{Path: "module" + itoa(i) + ".go", Category: CatCode})
	}
	for i := range change.Files {
		change.ByPath[change.Files[i].Path] = &change.Files[i]
	}

	in := UnderImplGenInput{Intents: intents, Scopes: scopes, Change: change}
	got := GenerateUnderImplCandidates(in)
	if len(got.Candidates) != MaxCandidatesPerRun {
		t.Errorf("Candidates len = %d, want %d", len(got.Candidates), MaxCandidatesPerRun)
	}
	if got.TruncatedAtCap != N-MaxCandidatesPerRun {
		t.Errorf("TruncatedAtCap = %d, want %d", got.TruncatedAtCap, N-MaxCandidatesPerRun)
	}
}

// Sort: highest score first; ties broken by IntentKind (request
// over correction); remaining ties broken by TurnTS ascending. Each
// rule gets its own assertion so a regression flags exactly which
// rule broke.
func TestGenerateUnderImpl_SortRules(t *testing.T) {
	// Two intents on the same retrieved file. iReq is a request,
	// iCor is a correction. Both have the same score; request must
	// sort first.
	change := changeLedgerOf(
		struct {
			path     string
			category FileCategory
		}{"app.go", CatCode},
	)
	scope := scopeWithFiles("ignored", "app.go")
	scope.Scores["app.go"] = 0.5

	scopeReq := scope
	scopeReq.IntentID = "i-req"
	scopeCor := scope
	scopeCor.IntentID = "i-cor"

	intents := []IntentItem{
		// Older request, newer correction; same score.
		candIntent("i-cor", IntentCorrection, 200, "add tests for app"),
		candIntent("i-req", IntentRequest, 100, "add tests for app"),
	}
	in := UnderImplGenInput{
		Intents: intents,
		Scopes:  map[string]RetrievedScope{"i-req": scopeReq, "i-cor": scopeCor},
		Change:  change,
	}
	got := GenerateUnderImplCandidates(in)
	if len(got.Candidates) != 2 {
		t.Fatalf("Candidates len = %d, want 2 (one per intent)", len(got.Candidates))
	}
	if got.Candidates[0].IntentID != "i-req" {
		t.Errorf("request candidate should sort first when scores tie; got order %s, %s",
			got.Candidates[0].IntentID, got.Candidates[1].IntentID)
	}

	// Now same Kind, same score, different TurnTS. Oldest first.
	intentsAge := []IntentItem{
		candIntent("i-new", IntentRequest, 500, "add docs for module x"),
		candIntent("i-old", IntentRequest, 100, "add docs for module x"),
	}
	scopeNew := scopeWithFiles("i-new", "module_x.go")
	scopeOld := scopeWithFiles("i-old", "module_x.go")
	change2 := changeLedgerOf(
		struct {
			path     string
			category FileCategory
		}{"module_x.go", CatCode},
	)
	inAge := UnderImplGenInput{
		Intents: intentsAge,
		Scopes:  map[string]RetrievedScope{"i-new": scopeNew, "i-old": scopeOld},
		Change:  change2,
	}
	gotAge := GenerateUnderImplCandidates(inAge)
	if len(gotAge.Candidates) != 2 {
		t.Fatalf("Candidates len = %d, want 2", len(gotAge.Candidates))
	}
	if gotAge.Candidates[0].IntentID != "i-old" {
		t.Errorf("older intent should sort first when scores and kinds tie; got order %s, %s",
			gotAge.Candidates[0].IntentID, gotAge.Candidates[1].IntentID)
	}
}

// Track B sorts before Track A when both are eligible.
func TestGenerateUnderImpl_TrackBSortsAheadOfTrackA(t *testing.T) {
	intentA := candIntent("i-a", IntentRequest, 100, "introduce retrieval scoring helper")
	intentB := candIntent("i-b", IntentRequest, 200, "add tests for app")

	scopeA := scopeWithNearMisses(intentA.ID, "near.go") // Track A
	scopeB := scopeWithFiles(intentB.ID, "app.go")       // Track B
	change := changeLedgerOf(
		struct {
			path     string
			category FileCategory
		}{"app.go", CatCode},
	)
	in := UnderImplGenInput{
		Intents: []IntentItem{intentA, intentB},
		Scopes:  map[string]RetrievedScope{intentA.ID: scopeA, intentB.ID: scopeB},
		Change:  change,
	}
	got := GenerateUnderImplCandidates(in)
	if len(got.Candidates) != 2 {
		t.Fatalf("Candidates len = %d, want 2 (Track A + Track B)", len(got.Candidates))
	}
	if got.Candidates[0].Kind != CandUnderImplPartialScope {
		t.Errorf("Track B candidate should sort first; got order: %+v", got.Candidates)
	}
}
