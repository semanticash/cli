package intentgap

import (
	"strings"
	"testing"
)

// retrievalIntent is a small constructor that hides the boilerplate
// of populating an IntentItem just for retrieval tests; only the
// fields BuildRetrieval reads are non-zero.
func retrievalIntent(id, summary, excerpt string, ts int64, hintFiles []string) IntentItem {
	return IntentItem{
		ID:      id,
		Kind:    IntentRequest,
		Summary: summary,
		Excerpt: excerpt,
		TurnTS:  ts,
		Hint:    IntentScopeHint{HintFiles: hintFiles},
	}
}

// changedFileWith returns a ChangeLedger holding one file with one
// hunk whose body is the given text. Used by tests that only care
// about a single file's score behavior.
func changedFileWith(path string, category FileCategory, body string) ChangeLedger {
	cf := ChangedFile{
		Path:     path,
		Category: category,
		Hunks:    []ChangedHunk{{StartLine: 1, EndLine: 5, Direction: HunkAdded, Body: body}},
	}
	ledger := ChangeLedger{
		Files:  []ChangedFile{cf},
		ByPath: map[string]*ChangedFile{},
	}
	ledger.ByPath[path] = &ledger.Files[0]
	return ledger
}

// An empty change ledger means the retrieval has nothing to score,
// so every output slice is empty and the call must not panic.
func TestBuildRetrieval_EmptyLedger(t *testing.T) {
	intent := retrievalIntent("i-1", "do something", "do something", 100, nil)
	got := BuildRetrieval(intent, ChangeLedger{ByPath: map[string]*ChangedFile{}}, ActionLedger{})
	if len(got.Files) != 0 || len(got.NearMisses) != 0 || len(got.HunkRefs) != 0 {
		t.Errorf("empty ledger should produce empty scope; got %+v", got)
	}
	if got.IntentID != "i-1" {
		t.Errorf("IntentID = %q, want i-1", got.IntentID)
	}
}

// Lexical path: an intent token that appears as a substring of the
// path lifts the file above the threshold. handler.go matches the
// "handler" token from the intent summary.
func TestBuildRetrieval_LexicalPathSignal(t *testing.T) {
	intent := retrievalIntent("i-1", "add validation to handler logic", "add validation to handler logic", 100, nil)
	change := changedFileWith("internal/service/handler.go", CatCode, "package service\n")
	got := BuildRetrieval(intent, change, ActionLedger{})
	if len(got.Files) != 1 || got.Files[0] != "internal/service/handler.go" {
		t.Errorf("lexical path match should put handler.go in Files; got %+v", got.Files)
	}
}

// Lexical hunk body: when the path shares no tokens with the
// intent, a hunk body that contains the intent's vocabulary still
// crosses the threshold via the weighted-0.8 signal. The body
// needs to mention enough of the intent's tokens that
// (matched/total)*0.8 lands at or above the 0.3 floor — covering at
// least half of the tokens is comfortably above.
func TestBuildRetrieval_LexicalHunkBodySignal(t *testing.T) {
	intent := retrievalIntent("i-1", "introduce retrieval scoring", "introduce retrieval scoring", 100, nil)
	body := "+func retrieval(scoring float64) { introduce() }\n"
	change := changedFileWith("internal/zzz/foo.go", CatCode, body)
	got := BuildRetrieval(intent, change, ActionLedger{})
	if len(got.Files) != 1 {
		t.Fatalf("Files len = %d, want 1; hunk body should match. Scores=%v", len(got.Files), got.Scores)
	}
	if got.Scores["internal/zzz/foo.go"] < RetrievalScoreThreshold {
		t.Errorf("expected score >= threshold; got %v", got.Scores)
	}
}

// Action adjacency alone is enough to cross the retrieval threshold:
// a 0.6 contribution from a single signal clears the 0.3 floor. Outside
// the time window, no signal fires and the file stays out.
func TestBuildRetrieval_ActionAdjacencySignal(t *testing.T) {
	intent := retrievalIntent("i-1", "do unrelated work", "do unrelated work", 1000, nil)
	change := changedFileWith("zzz.go", CatCode, "")
	action := BuildActionLedger([]BundleAgentAction{
		{ActionID: "a1", FilePath: "zzz.go", TS: 1500}, // within ±30min of intent TS=1000
	})
	got := BuildRetrieval(intent, change, action)
	if len(got.Files) != 1 || got.Files[0] != "zzz.go" {
		t.Errorf("in-window action should pull zzz.go into Files; got %+v", got.Files)
	}
	if len(got.ActionIDs) != 1 || got.ActionIDs[0] != "a1" {
		t.Errorf("ActionIDs should expose the matched action; got %v", got.ActionIDs)
	}

	// An action 4 hours away from the intent must not contribute.
	farAction := BuildActionLedger([]BundleAgentAction{
		{ActionID: "a1", FilePath: "zzz.go", TS: 1000 + 4*60*60},
	})
	gotFar := BuildRetrieval(intent, change, farAction)
	if len(gotFar.Files) != 0 {
		t.Errorf("out-of-window action must not lift the file; got %+v", gotFar.Files)
	}
}

// Test/doc sibling: a test file in the same directory as a
// positively-scored code file picks up the 0.3 bonus. Without that
// bonus the test file would have no score at all (its own path /
// body share no intent vocabulary).
func TestBuildRetrieval_TestDocSiblingSignal(t *testing.T) {
	intent := retrievalIntent("i-1", "rework intentgap analyzer", "rework intentgap analyzer", 100, nil)
	codeFile := ChangedFile{
		Path:     "internal/intentgap/analyzer.go",
		Category: CatCode,
		Hunks:    []ChangedHunk{{Body: "// analyzer"}},
	}
	testFile := ChangedFile{
		Path:     "internal/intentgap/analyzer_test.go",
		Category: CatTest,
		Hunks:    []ChangedHunk{{Body: "// some completely unrelated content"}},
	}
	ledger := ChangeLedger{
		Files:  []ChangedFile{codeFile, testFile},
		ByPath: map[string]*ChangedFile{},
	}
	ledger.ByPath[codeFile.Path] = &ledger.Files[0]
	ledger.ByPath[testFile.Path] = &ledger.Files[1]

	got := BuildRetrieval(intent, ledger, ActionLedger{})
	if len(got.TestPaths) != 1 || got.TestPaths[0] != "internal/intentgap/analyzer_test.go" {
		t.Errorf("sibling test should land in TestPaths; got TestPaths=%v Files=%v Scores=%v",
			got.TestPaths, got.Files, got.Scores)
	}
}

// Threshold boundary: a file whose composite score is below the
// threshold but still positive must end up in NearMisses, not in
// Files. A file whose score is exactly at the threshold goes into
// Files (the gate is >= threshold).
func TestBuildRetrieval_ThresholdBoundary(t *testing.T) {
	intent := retrievalIntent("i-1", "alpha beta gamma delta", "alpha beta gamma delta", 100, nil)
	// "alpha" appears in the path → 1/4 path score. Weighted = 0.25, below threshold.
	belowFile := ChangedFile{Path: "alpha.go", Category: CatCode, Hunks: []ChangedHunk{{Body: "// no body match"}}}
	// "beta" appears in the path AND the body → 1/4 path + 1/4 body
	// = 0.25 + 0.8*0.25 = 0.45, above threshold.
	aboveFile := ChangedFile{Path: "beta.go", Category: CatCode, Hunks: []ChangedHunk{{Body: "// beta in body"}}}
	ledger := ChangeLedger{Files: []ChangedFile{belowFile, aboveFile}, ByPath: map[string]*ChangedFile{}}
	ledger.ByPath[belowFile.Path] = &ledger.Files[0]
	ledger.ByPath[aboveFile.Path] = &ledger.Files[1]

	got := BuildRetrieval(intent, ledger, ActionLedger{})
	if len(got.Files) != 1 || got.Files[0] != "beta.go" {
		t.Errorf("above-threshold file expected; Files=%v Scores=%v", got.Files, got.Scores)
	}
	if len(got.NearMisses) != 1 || got.NearMisses[0] != "alpha.go" {
		t.Errorf("below-threshold file expected in NearMisses; NearMisses=%v Scores=%v",
			got.NearMisses, got.Scores)
	}
}

// HintFiles boost: an exact path match in HintFiles saturates the
// lexical-path signal regardless of the summary's word overlap.
func TestBuildRetrieval_HintFilesBoostsExactPathMatch(t *testing.T) {
	intent := retrievalIntent("i-1", "make it work", "make it work", 100, []string{"internal/llm/registry.go"})
	change := changedFileWith("internal/llm/registry.go", CatCode, "")
	got := BuildRetrieval(intent, change, ActionLedger{})
	if len(got.Files) != 1 {
		t.Fatalf("hint-file exact match should put the file in Files; got %+v", got.Files)
	}
	if got.Scores["internal/llm/registry.go"] < 1.0 {
		t.Errorf("hint-file exact match should saturate lexical path; got %v", got.Scores)
	}
}

// Files cap: when more files cross the threshold than
// MaxRetrievedFilesPerIntent, only the highest-scoring N survive.
func TestBuildRetrieval_FilesCappedAtMax(t *testing.T) {
	intent := retrievalIntent("i-1", "intentgap intentgap intentgap", "intentgap intentgap", 100, nil)
	ledger := ChangeLedger{ByPath: map[string]*ChangedFile{}}
	const N = MaxRetrievedFilesPerIntent + 4
	for i := 0; i < N; i++ {
		path := "internal/intentgap/file" + itoa(i) + ".go"
		ledger.Files = append(ledger.Files, ChangedFile{
			Path:     path,
			Category: CatCode,
			Hunks:    []ChangedHunk{{Body: "// intentgap content"}},
		})
	}
	for i := range ledger.Files {
		ledger.ByPath[ledger.Files[i].Path] = &ledger.Files[i]
	}

	got := BuildRetrieval(intent, ledger, ActionLedger{})
	if len(got.Files) != MaxRetrievedFilesPerIntent {
		t.Errorf("Files len = %d, want %d (cap must hold)", len(got.Files), MaxRetrievedFilesPerIntent)
	}
}

// HunkRefs cap: even when many hunks contain intent tokens, the
// scope only carries the top MaxHunkRefsPerScope by per-hunk match
// count.
func TestBuildRetrieval_HunkRefsCappedAtMax(t *testing.T) {
	intent := retrievalIntent("i-1", "intentgap classifier", "intentgap classifier", 100, nil)
	hunks := make([]ChangedHunk, 0, MaxHunkRefsPerScope+3)
	for i := 0; i < MaxHunkRefsPerScope+3; i++ {
		hunks = append(hunks, ChangedHunk{
			StartLine: i*10 + 1,
			EndLine:   i*10 + 5,
			Direction: HunkAdded,
			Body:      "+intentgap classifier extra body line\n",
		})
	}
	cf := ChangedFile{Path: "x.go", Category: CatCode, Hunks: hunks}
	ledger := ChangeLedger{Files: []ChangedFile{cf}, ByPath: map[string]*ChangedFile{}}
	ledger.ByPath[cf.Path] = &ledger.Files[0]

	got := BuildRetrieval(intent, ledger, ActionLedger{})
	if len(got.HunkRefs) != MaxHunkRefsPerScope {
		t.Errorf("HunkRefs len = %d, want %d", len(got.HunkRefs), MaxHunkRefsPerScope)
	}
}

// Short tokens (length < minIntentTokenLen) are filtered out. A
// stop-list-style intent like "do it" produces no tokens and
// therefore no lexical-side score, so the file falls into
// NearMisses or is excluded entirely depending on other signals.
func TestBuildRetrieval_ShortTokensFiltered(t *testing.T) {
	intent := retrievalIntent("i-1", "do it", "do it", 100, nil)
	change := changedFileWith("internal/intentgap/analyzer.go", CatCode, "// some intentgap logic")
	got := BuildRetrieval(intent, change, ActionLedger{})
	if len(got.Files) != 0 {
		t.Errorf("short tokens should not trigger lexical signals; Files=%v", got.Files)
	}
}

// Deleted files (Deleted=true) still participate in retrieval so
// retrieval doesn't miss "delete X" intents. The category-driven
// score signals run on Path the same way, and HintFiles still match.
func TestBuildRetrieval_DeletedFileMatchesHintFile(t *testing.T) {
	intent := retrievalIntent("i-1", "delete the old pre-push file", "delete the old pre-push file",
		100, []string{"internal/service/pre-push.go"})
	cf := ChangedFile{
		Path:     "internal/service/pre-push.go",
		Category: CatCode,
		Deleted:  true,
		Hunks:    []ChangedHunk{{StartLine: 1, EndLine: 50, Direction: HunkRemoved, Body: "-package service\n"}},
	}
	ledger := ChangeLedger{Files: []ChangedFile{cf}, ByPath: map[string]*ChangedFile{}}
	ledger.ByPath[cf.Path] = &ledger.Files[0]

	got := BuildRetrieval(intent, ledger, ActionLedger{})
	if len(got.Files) != 1 || got.Files[0] != "internal/service/pre-push.go" {
		t.Errorf("deleted file matching HintFile must enter Files; got %+v", got)
	}
}

// parentDir returns "" at the root and the literal parent for nested
// paths. The directory grouping for the sibling signal relies on
// this, so a regression test pins the expectations.
func TestParentDir(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"a.go", ""},
		{"foo/a.go", "foo"},
		{"foo/bar/a.go", "foo/bar"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := parentDir(tc.path); got != tc.want {
			t.Errorf("parentDir(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// tokenizeIntent drops duplicates so a repeated word does not inflate
// downstream scoring; output is deterministic in insertion order.
func TestTokenizeIntent_DropsDuplicatesAndShortTokens(t *testing.T) {
	got := tokenizeIntent("validate validation handler do it is for")
	want := []string{"validate", "validation", "handler"}
	if !equalStringSlices(got, want) {
		t.Errorf("got = %v, want %v", got, want)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Ensure intent text and excerpts that include code-ish punctuation
// still extract the right tokens. A path like "handler.go" should
// produce the "handler" token after non-alnum splitting.
func TestTokenizeIntent_HandlesPunctuation(t *testing.T) {
	got := tokenizeIntent("touch handler.go and main.go please")
	joined := strings.Join(got, " ")
	for _, want := range []string{"touch", "handler", "main", "please"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected token %q in %v", want, got)
		}
	}
}
