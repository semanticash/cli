package scoring

import (
	"strings"
	"testing"
)

func deltaDiff(t *testing.T, body ...string) DiffResult {
	t.Helper()
	lines := append([]string{
		"--- a/f.go",
		"+++ b/f.go",
		"@@ -1,1 +1," + itoa(len(body)+1) + " @@",
		" ctx",
	}, body...)
	lines = append(lines, "")
	return ParseDiff([]byte(strings.Join(lines, "\n")))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

func group(provider string, ts int64, eventID string, lines ...string) DeltaClaimGroup {
	return DeltaClaimGroup{Provider: provider, Ts: ts, InsertSeq: ts, EventID: eventID, Lines: lines}
}

func groupsFor(gs ...DeltaClaimGroup) map[string][]DeltaClaimGroup {
	return map[string][]DeltaClaimGroup{"f.go": gs}
}

func scoreV2(diff DiffResult, aiLines map[string]map[string]struct{}, stamps map[string]map[string][]LineStamp, groups map[string][]DeltaClaimGroup) ([]FileScore, MatchStats) {
	return ScoreFilesWithDeltas(diff, aiLines, nil, nil, nil, stamps, groups)
}

// Delta matches retain their quality and source counters.
func TestScoreV2_AlignedLines(t *testing.T) {
	diff := deltaDiff(t, "+x := 1", "+y  :=  2")
	scores, stats := scoreV2(diff, nil, nil,
		groupsFor(group("claude_code", 100, "e1", "x := 1", "y := 2")))
	fs := scores[0]
	if fs.ExactLines != 1 || fs.FormattedLines != 1 || fs.HumanLines != 0 {
		t.Fatalf("score = %+v", fs)
	}
	if fs.DeltaExactLines != 1 || fs.DeltaFormattedLines != 1 {
		t.Fatalf("delta counters = %+v", fs)
	}
	if fs.ProviderLines["claude_code"] != 2 {
		t.Fatalf("providers = %+v", fs.ProviderLines)
	}
	if stats.DeltaExactMatches != 1 || stats.DeltaNormalizedMatches != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

// Repeated identical lines are attributed per occurrence.
func TestScoreV2_PerOccurrence(t *testing.T) {
	diff := deltaDiff(t, "+return nil", "+return nil", "+return nil")
	scores, _ := scoreV2(diff, nil, nil,
		groupsFor(group("claude_code", 100, "e1", "return nil")))
	fs := scores[0]
	if fs.ExactLines != 1 || fs.HumanLines != 2 {
		t.Fatalf("score = %+v, want one claimed occurrence and two human", fs)
	}
}

// Delta-only overlap does not extend to adjacent lines.
func TestScoreV2_NoModifiedInheritanceFromDeltaOverlap(t *testing.T) {
	diff := deltaDiff(t, "+tool line one", "+human edit here", "+tool line two")
	scores, stats := scoreV2(diff, nil, nil,
		groupsFor(group("claude_code", 100, "e1", "tool line one", "tool line two")))
	fs := scores[0]
	if fs.ExactLines != 2 || fs.ModifiedLines != 0 || fs.HumanLines != 1 {
		t.Fatalf("score = %+v, want the middle line human, never modified", fs)
	}
	if stats.ModifiedMatches != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

// Direct overlap keeps modified inheritance with hunk-provider credit.
func TestScoreV2_DirectOverlapStillInherits(t *testing.T) {
	diff := deltaDiff(t, "+direct line", "+reworked by human", "+delta line")
	aiLines := map[string]map[string]struct{}{"f.go": {"direct line": {}}}
	lineProviders := map[string]map[string]map[string]struct{}{
		"f.go": {"direct line": {"claude_code": {}}},
	}
	scores, _ := ScoreFilesWithDeltas(diff, aiLines, nil, nil, lineProviders, nil,
		groupsFor(group("codex", 100, "e1", "delta line")))
	fs := scores[0]
	if fs.ExactLines != 2 || fs.ModifiedLines != 1 || fs.HumanLines != 0 {
		t.Fatalf("score = %+v", fs)
	}
	if fs.ProviderLines["claude_code"] != 2 || fs.ProviderLines["codex"] != 1 {
		t.Fatalf("providers = %+v", fs.ProviderLines)
	}
}

// Exact evidence outranks normalized evidence regardless of source.
func TestScoreV2_ExactDeltaBeatsNormalizedDirect(t *testing.T) {
	diff := deltaDiff(t, "+x := 1")
	// The direct evidence differs only in whitespace.
	aiLines := map[string]map[string]struct{}{"f.go": {"x :=  1": {}}}
	stamps := map[string]map[string][]LineStamp{
		"f.go": {"x :=  1": {{Provider: "claude_code", Ts: 999, InsertSeq: 999, EventID: "e-direct"}}},
	}
	scores, _ := scoreV2(diff, aiLines, stamps,
		groupsFor(group("codex", 1, "e-delta", "x := 1")))
	fs := scores[0]
	if fs.ExactLines != 1 || fs.FormattedLines != 0 {
		t.Fatalf("score = %+v, want the exact delta to win", fs)
	}
	if fs.ProviderLines["codex"] != 1 || fs.ProviderLines["claude_code"] != 0 {
		t.Fatalf("providers = %+v, want the winner only", fs.ProviderLines)
	}
	if fs.ContestedLines != 1 {
		t.Fatalf("contested = %d, want the competition recorded", fs.ContestedLines)
	}
}

// Equal-quality evidence resolves by recency.
func TestScoreV2_EqualQualityLatestWins(t *testing.T) {
	diff := deltaDiff(t, "+shared line")
	aiLines := map[string]map[string]struct{}{"f.go": {"shared line": {}}}
	stamps := map[string]map[string][]LineStamp{
		"f.go": {"shared line": {{Provider: "claude_code", Ts: 100, InsertSeq: 100, EventID: "e-direct"}}},
	}

	// Delta later: delta provider wins alone.
	scores, _ := scoreV2(diff, aiLines, stamps,
		groupsFor(group("codex", 200, "e-delta", "shared line")))
	fs := scores[0]
	if fs.ProviderLines["codex"] != 1 || fs.ProviderLines["claude_code"] != 0 {
		t.Fatalf("later delta: providers = %+v", fs.ProviderLines)
	}
	if fs.DeltaExactLines != 1 {
		t.Fatalf("later delta: %+v, want delta-sourced win", fs)
	}

	// Direct later: direct provider wins alone.
	stamps["f.go"]["shared line"] = []LineStamp{{Provider: "claude_code", Ts: 300, InsertSeq: 300, EventID: "e-direct"}}
	scores, _ = scoreV2(diff, aiLines, stamps,
		groupsFor(group("codex", 200, "e-delta", "shared line")))
	fs = scores[0]
	if fs.ProviderLines["claude_code"] != 1 || fs.ProviderLines["codex"] != 0 {
		t.Fatalf("later direct: providers = %+v", fs.ProviderLines)
	}
	if fs.DeltaExactLines != 0 {
		t.Fatalf("later direct: %+v, want direct-sourced win", fs)
	}

	// Full tie on (ts, insert_seq): the event ID decides, stably.
	stamps["f.go"]["shared line"] = []LineStamp{{Provider: "claude_code", Ts: 200, InsertSeq: 200, EventID: "e-a"}}
	scores, _ = scoreV2(diff, aiLines, stamps,
		groupsFor(DeltaClaimGroup{Provider: "codex", Ts: 200, InsertSeq: 200, EventID: "e-b", Lines: []string{"shared line"}}))
	fs = scores[0]
	if fs.ProviderLines["codex"] != 1 || fs.ProviderLines["claude_code"] != 0 {
		t.Fatalf("event-id tiebreak: providers = %+v, want e-b > e-a", fs.ProviderLines)
	}
}

// Exact witnesses do not hide normalized competitors.
func TestScoreV2_NormalizedCompetitorBesideExact(t *testing.T) {
	diff := deltaDiff(t, "+x := 1")
	aiLines := map[string]map[string]struct{}{
		"f.go": {"x := 1": {}, "x :=  1": {}},
	}
	stamps := map[string]map[string][]LineStamp{
		"f.go": {
			"x := 1":  {{Provider: "claude_code", Ts: 100, InsertSeq: 1, EventID: "e-exact"}},
			"x :=  1": {{Provider: "codex", Ts: 999, InsertSeq: 999, EventID: "e-norm"}},
		},
	}
	scores, _ := scoreV2(diff, aiLines, stamps, nil)
	fs := scores[0]
	if fs.ExactLines != 1 || fs.ProviderLines["claude_code"] != 1 || fs.ProviderLines["codex"] != 0 {
		t.Fatalf("score = %+v providers = %+v, want the exact witness despite older recency",
			fs, fs.ProviderLines)
	}
	if fs.ContestedLines != 1 {
		t.Fatalf("contested = %d, want the normalized competitor recorded", fs.ContestedLines)
	}
}

// Direct witnesses compete independently.
func TestScoreV2_DirectWitnessesCompete(t *testing.T) {
	diff := deltaDiff(t, "+shared line")
	aiLines := map[string]map[string]struct{}{"f.go": {"shared line": {}}}
	stamps := map[string]map[string][]LineStamp{
		"f.go": {"shared line": {
			{Provider: "claude_code", Ts: 100, InsertSeq: 1, EventID: "e-early"},
			{Provider: "codex", Ts: 200, InsertSeq: 2, EventID: "e-late"},
		}},
	}
	scores, stats := scoreV2(diff, aiLines, stamps, nil)
	fs := scores[0]
	if fs.ProviderLines["codex"] != 1 || fs.ProviderLines["claude_code"] != 0 {
		t.Fatalf("providers = %+v, want the later witness", fs.ProviderLines)
	}
	if fs.ContestedLines != 1 || stats.ContestedLines != 1 {
		t.Fatalf("contested = %d/%d, want direct-vs-direct competition recorded",
			fs.ContestedLines, stats.ContestedLines)
	}
}

// Group alignment is independent of capture order.
func TestScoreV2_ReverseSpatialOrderBothAttributed(t *testing.T) {
	diff := deltaDiff(t, "+top line by B", "+bottom line by A")
	scores, _ := scoreV2(diff, nil, nil, groupsFor(
		group("claude_code", 100, "e-a", "bottom line by A"),
		group("codex", 200, "e-b", "top line by B"),
	))
	fs := scores[0]
	if fs.ExactLines != 2 || fs.HumanLines != 0 {
		t.Fatalf("score = %+v, want both spatially-reversed edits attributed", fs)
	}
	if fs.ProviderLines["claude_code"] != 1 || fs.ProviderLines["codex"] != 1 {
		t.Fatalf("providers = %+v", fs.ProviderLines)
	}
}

// Direct evidence without line metadata uses the file provider.
func TestScoreV2_DirectFallbackSurvivesDeltaCompetition(t *testing.T) {
	diff := deltaDiff(t, "+shared line")
	aiLines := map[string]map[string]struct{}{"f.go": {"shared line": {}}}
	fileProvider := map[string]string{"f.go": "claude_code"}
	// Unstamped direct evidence has zero recency.
	scores, _ := ScoreFilesWithDeltas(diff, aiLines, nil, fileProvider, nil, nil,
		groupsFor(group("codex", 100, "e-delta", "shared line")))
	fs := scores[0]
	if fs.ProviderLines["codex"] != 1 {
		t.Fatalf("providers = %+v", fs.ProviderLines)
	}

	// Without delta evidence, the direct provider wins.
	scores, _ = ScoreFilesWithDeltas(diff, aiLines, nil, fileProvider, nil, nil, nil)
	fs = scores[0]
	if fs.ProviderLines["claude_code"] != 1 || fs.ExactLines != 1 {
		t.Fatalf("fallback lost: %+v", fs)
	}
}

// An incomplete diff parse refuses delta evidence; direct still scores.
func TestScoreV2_IncompleteDiffRefusesAlignment(t *testing.T) {
	diff := deltaDiff(t, "+tool line", "+direct line")
	diff.Complete = false
	aiLines := map[string]map[string]struct{}{"f.go": {"direct line": {}}}
	scores, stats := ScoreFilesWithDeltas(diff, aiLines, nil,
		map[string]string{"f.go": "claude_code"}, nil, nil,
		groupsFor(group("claude_code", 100, "e1", "tool line")))
	fs := scores[0]
	if !fs.DeltaAlignmentRefused || stats.DeltaAlignmentsRefused != 1 {
		t.Fatalf("refusal not surfaced: %+v %+v", fs, stats)
	}
	if fs.DeltaExactLines != 0 || fs.ExactLines != 1 {
		t.Fatalf("score = %+v, want direct-only evidence", fs)
	}
	// Direct evidence can still anchor modified-line attribution.
	if fs.ModifiedLines != 1 || fs.HumanLines != 0 || fs.ProviderOnlyLines != 0 {
		t.Fatalf("score = %+v, want the tool line modified via the direct anchor", fs)
	}
}

// A refused delta takes sidecar credit over generic file-touch evidence.
func TestScoreV2_RefusalCreditsClaimProviderOverTouch(t *testing.T) {
	diff := deltaDiff(t, "+tool line one", "+tool line two")
	diff.Complete = false
	scores, _ := ScoreFilesWithDeltas(diff, nil,
		map[string]string{"f.go": "cursor"}, nil, nil, nil,
		groupsFor(group("claude_code", 100, "e1", "tool line one", "tool line two")))
	fs := scores[0]
	if !fs.DeltaAlignmentRefused {
		t.Fatalf("refusal not surfaced: %+v", fs)
	}
	if fs.ProviderOnlyLines != 2 || fs.HumanLines != 0 {
		t.Fatalf("score = %+v, want both lines in the sidecar", fs)
	}
	if fs.ProviderOnlyLinesByProvider["claude_code"] != 2 || fs.ProviderOnlyLinesByProvider["cursor"] != 0 {
		t.Fatalf("sidecar providers = %+v, want the refused claim provider credited", fs.ProviderOnlyLinesByProvider)
	}
}

// Linear-scan exhaustion refuses all delta line evidence.
func TestScoreV2_ScanBudgetRefusal(t *testing.T) {
	// The alignment table consumes the full budget; unmatched claims then
	// reach the budgeted line scan.
	nc, na := 1023, 4095
	claims := make([]string, nc)
	for i := range claims {
		claims[i] = "claim " + itoa(i)
	}
	added := make([]string, na)
	for i := range added {
		added[i] = "+line " + itoa(i)
	}
	diff := deltaDiff(t, added...)
	scores, stats := scoreV2(diff, nil, nil,
		groupsFor(group("claude_code", 100, "e1", claims...)))
	fs := scores[0]
	if !fs.DeltaAlignmentRefused || stats.DeltaAlignmentsRefused != 1 {
		t.Fatalf("scan exhaustion not refused: %+v %+v", fs, stats)
	}
	if fs.ExactLines != 0 || fs.HumanLines != 0 || fs.ProviderOnlyLines != fs.TotalLines {
		t.Fatalf("score = %+v, want refused evidence degraded to touch", fs)
	}
}

// The cumulative per-file budget refuses over-large alignments.
func TestScoreV2_BudgetRefusal(t *testing.T) {
	n := 2100
	added := make([]string, n)
	claimed := make([]string, n)
	for i := range added {
		added[i] = "+line"
		claimed[i] = "line"
	}
	diff := deltaDiff(t, added...)
	scores, stats := scoreV2(diff, nil, nil,
		groupsFor(group("claude_code", 100, "e1", claimed...)))
	fs := scores[0]
	if !fs.DeltaAlignmentRefused || stats.DeltaAlignmentsRefused != 1 {
		t.Fatalf("refusal not surfaced: %+v", stats)
	}
	if fs.ExactLines != 0 || fs.ProviderOnlyLines != n || fs.HumanLines != 0 {
		t.Fatalf("score = %+v, want no guessed lines, all in the sidecar", fs)
	}
}

// A provider-touched file with delta claims is scored, not provider-only.
func TestScoreV2_TouchedFileWithClaimsScores(t *testing.T) {
	diff := deltaDiff(t, "+tool line")
	scores, _ := ScoreFilesWithDeltas(diff, nil,
		map[string]string{"f.go": "claude_code"}, nil, nil, nil,
		groupsFor(group("claude_code", 100, "e1", "tool line")))
	fs := scores[0]
	if fs.ProviderOnlyLines != 0 || fs.ExactLines != 1 {
		t.Fatalf("score = %+v, want line evidence, not provider-only", fs)
	}
}

// Tool claims do not absorb interleaved human lines.
func TestScoreV2_InterleavedHumanBetweenTools(t *testing.T) {
	diff := deltaDiff(t,
		"+alpha one", "+human insert", "+alpha two", "+beta one")
	scores, _ := scoreV2(diff, nil, nil, groupsFor(
		group("claude_code", 100, "e-a", "alpha one", "alpha two"),
		group("codex", 200, "e-b", "beta one"),
	))
	fs := scores[0]
	if fs.ExactLines != 3 || fs.HumanLines != 1 || fs.ModifiedLines != 0 {
		t.Fatalf("score = %+v, want 3 exact / 1 human / 0 modified", fs)
	}
	if fs.ProviderLines["claude_code"] != 2 || fs.ProviderLines["codex"] != 1 {
		t.Fatalf("providers = %+v", fs.ProviderLines)
	}
}

// Duplicate claims from different groups consume distinct occurrences.
func TestScoreV2_CrossGroupDuplicatesBothAttributed(t *testing.T) {
	diff := deltaDiff(t, "+dup line", "+dup line")
	scores, _ := scoreV2(diff, nil, nil, groupsFor(
		group("claude_code", 100, "e-a", "dup line"),
		group("codex", 200, "e-b", "dup line"),
	))
	fs := scores[0]
	if fs.ExactLines != 2 || fs.HumanLines != 0 {
		t.Fatalf("score = %+v, want both occurrences attributed", fs)
	}
	if fs.ProviderLines["codex"] != 1 || fs.ProviderLines["claude_code"] != 1 {
		t.Fatalf("providers = %+v, want one occurrence each", fs.ProviderLines)
	}
}

// Recency resolves equal-quality contention without hiding it.
func TestScoreV2_ContendedOccurrenceGoesToLatest(t *testing.T) {
	diff := deltaDiff(t, "+dup line")
	scores, stats := scoreV2(diff, nil, nil, groupsFor(
		group("claude_code", 100, "e-a", "dup line"),
		group("codex", 200, "e-b", "dup line"),
	))
	fs := scores[0]
	if fs.ExactLines != 1 || fs.HumanLines != 0 {
		t.Fatalf("score = %+v", fs)
	}
	if fs.ProviderLines["codex"] != 1 || fs.ProviderLines["claude_code"] != 0 {
		t.Fatalf("providers = %+v, want the latest group", fs.ProviderLines)
	}
	if fs.ContestedLines != 1 || stats.ContestedLines != 1 {
		t.Fatalf("contested = %d/%d, want the losing claim recorded on file and aggregate",
			fs.ContestedLines, stats.ContestedLines)
	}
}

// Exact matches outrank newer normalized matches.
func TestScoreV2_AllocationQualityBeforeRecency(t *testing.T) {
	diff := deltaDiff(t, "+x := 1")
	scores, _ := scoreV2(diff, nil, nil, groupsFor(
		group("claude_code", 100, "e-a", "x := 1"),
		group("codex", 200, "e-b", "x  :=  1"),
	))
	fs := scores[0]
	if fs.ExactLines != 1 || fs.FormattedLines != 0 {
		t.Fatalf("score = %+v, want the older exact claim to win", fs)
	}
	if fs.ProviderLines["claude_code"] != 1 || fs.ProviderLines["codex"] != 0 {
		t.Fatalf("providers = %+v, want the exact claimant", fs.ProviderLines)
	}
	if fs.ContestedLines != 1 {
		t.Fatalf("contested = %d, want the displaced normalized claim recorded", fs.ContestedLines)
	}
}

// Normalized matches cannot cross exact anchors.
func TestScoreV2_SecondRoundRespectsAnchors(t *testing.T) {
	diff := deltaDiff(t, "+b x", "+A")
	scores, _ := scoreV2(diff, nil, nil,
		groupsFor(group("claude_code", 100, "e1", "A", "b  x")))
	fs := scores[0]
	// "b  x" follows the "A" claim and cannot match before its anchor.
	if fs.ExactLines != 1 || fs.FormattedLines != 0 || fs.HumanLines != 1 {
		t.Fatalf("score = %+v, want the crossing normalized match rejected", fs)
	}
}

// Duplicate leftovers mark distinct contested occurrences.
func TestScoreV2_ContentionMarksDistinctOccurrences(t *testing.T) {
	diff := deltaDiff(t, "+dup line", "+dup line")
	scores, stats := scoreV2(diff, nil, nil, groupsFor(
		group("claude_code", 100, "e-a", "dup line", "dup line"),
		group("codex", 200, "e-b", "dup line", "dup line"),
	))
	fs := scores[0]
	if fs.ExactLines != 2 || fs.ProviderLines["codex"] != 2 {
		t.Fatalf("score = %+v providers = %+v, want the latest group's claims allocated",
			fs, fs.ProviderLines)
	}
	if fs.ContestedLines != 2 || stats.ContestedLines != 2 {
		t.Fatalf("contested = %d/%d, want both occurrences marked", fs.ContestedLines, stats.ContestedLines)
	}
}

// A lost exact anchor still bounds later claims from its group.
func TestScoreV2_LostAnchorStillBounds(t *testing.T) {
	diff := deltaDiff(t, "+b x", "+A")
	scores, stats := scoreV2(diff, nil, nil, groupsFor(
		group("claude_code", 100, "e-old", "A", "b  x"),
		group("codex", 200, "e-new", "A"),
	))
	fs := scores[0]
	if fs.ExactLines != 1 || fs.FormattedLines != 0 || fs.HumanLines != 1 {
		t.Fatalf("score = %+v, want A exact and b x human", fs)
	}
	if fs.ProviderLines["codex"] != 1 || fs.ProviderLines["claude_code"] != 0 {
		t.Fatalf("providers = %+v, want the newer group owning A", fs.ProviderLines)
	}
	// The lost anchor still contests A.
	if fs.ContestedLines != 1 || stats.ContestedLines != 1 {
		t.Fatalf("contested = %d/%d, want the lost anchor recorded", fs.ContestedLines, stats.ContestedLines)
	}
}

// Exact and normalized leftovers contest distinct occurrences.
func TestScoreV2_MixedWhitespaceContentionDistinct(t *testing.T) {
	diff := deltaDiff(t, "+dup line", "+dup line")
	scores, stats := scoreV2(diff, nil, nil, groupsFor(
		group("claude_code", 100, "e-a", "dup line", "dup  line"),
		group("codex", 200, "e-b", "dup line", "dup line"),
	))
	fs := scores[0]
	if fs.ExactLines != 2 || fs.ProviderLines["codex"] != 2 {
		t.Fatalf("score = %+v providers = %+v", fs, fs.ProviderLines)
	}
	if fs.ContestedLines != 2 || stats.ContestedLines != 2 {
		t.Fatalf("contested = %d/%d, want both occurrences marked", fs.ContestedLines, stats.ContestedLines)
	}
}

// Out-of-range claims do not create contention.
func TestScoreV2_ContentionRespectsAnchorGeometry(t *testing.T) {
	diff := deltaDiff(t, "+dup line", "+X")
	scores, stats := scoreV2(diff, nil, nil, groupsFor(
		group("claude_code", 100, "e-old", "X", "dup  line"),
		group("codex", 200, "e-new", "dup line"),
	))
	fs := scores[0]
	if fs.ExactLines != 2 || fs.HumanLines != 0 {
		t.Fatalf("score = %+v", fs)
	}
	if fs.ProviderLines["codex"] != 1 || fs.ProviderLines["claude_code"] != 1 {
		t.Fatalf("providers = %+v", fs.ProviderLines)
	}
	if fs.ContestedLines != 0 || stats.ContestedLines != 0 {
		t.Fatalf("contested = %d/%d, want no positionally impossible contention",
			fs.ContestedLines, stats.ContestedLines)
	}
}

// Ownership cannot change a group's previously selected alignment.
func TestScoreV2_OwnershipCannotReshapeGeometry(t *testing.T) {
	diff := deltaDiff(t, "+dup", "+X")
	scores, stats := scoreV2(diff, nil, nil, groupsFor(
		group("claude_code", 100, "e-old", "X", "dup"),
		group("codex", 200, "e-new", "dup"),
	))
	fs := scores[0]
	if fs.ExactLines != 1 || fs.HumanLines != 1 {
		t.Fatalf("score = %+v, want dup owned and X human", fs)
	}
	if fs.ProviderLines["codex"] != 1 || fs.ProviderLines["claude_code"] != 0 {
		t.Fatalf("providers = %+v, want the newer group owning dup", fs.ProviderLines)
	}
	// The lost anchor still records contention.
	if fs.ContestedLines != 1 || stats.ContestedLines != 1 {
		t.Fatalf("contested = %d/%d, want the lost anchor recorded", fs.ContestedLines, stats.ContestedLines)
	}
}

// Exact and normalized claims can use distinct occurrences.
func TestScoreV2_AllocationPlacesBothQualities(t *testing.T) {
	diff := deltaDiff(t, "+x := 1", "+x := 1")
	scores, _ := scoreV2(diff, nil, nil, groupsFor(
		group("claude_code", 100, "e-a", "x := 1"),
		group("codex", 200, "e-b", "x  :=  1"),
	))
	fs := scores[0]
	if fs.ExactLines != 1 || fs.FormattedLines != 1 || fs.HumanLines != 0 {
		t.Fatalf("score = %+v, want one exact + one formatted", fs)
	}
	if fs.ProviderLines["claude_code"] != 1 || fs.ProviderLines["codex"] != 1 {
		t.Fatalf("providers = %+v", fs.ProviderLines)
	}
}

// Unmatched rewritten tool output remains human.
func TestScoreV2_RewrittenToolLineStaysHuman(t *testing.T) {
	diff := deltaDiff(t, "+original line heavily edited by dev")
	scores, _ := scoreV2(diff, nil, nil,
		groupsFor(group("claude_code", 100, "e1", "original line")))
	fs := scores[0]
	if fs.HumanLines != 1 || fs.ExactLines != 0 || fs.ModifiedLines != 0 {
		t.Fatalf("score = %+v, want the rewrite human", fs)
	}
}

// Alignment spans hunk boundaries in order.
func TestScoreV2_AcrossGroups(t *testing.T) {
	diff := ParseDiff([]byte(strings.Join([]string{
		"--- a/f.go",
		"+++ b/f.go",
		"@@ -1,2 +1,3 @@",
		" ctx1",
		"+first tool line",
		" ctx2",
		"@@ -10,2 +11,3 @@",
		" ctx10",
		"+second tool line",
		" ctx11",
		"",
	}, "\n")))
	scores, _ := scoreV2(diff, nil, nil,
		groupsFor(group("claude_code", 100, "e1", "first tool line", "second tool line")))
	fs := scores[0]
	if fs.ExactLines != 2 || fs.HumanLines != 0 {
		t.Fatalf("score = %+v", fs)
	}
}
