package intentgap

import (
	"encoding/json"
	"fmt"
	"testing"
)

// --- Test helpers -------------------------------------------------

// canonicalBundle returns a bundle with one captured turn and a diff
// touching handler.go lines 10..20, sufficient for most cite-or-drop
// happy-path cases.
func canonicalBundle() Bundle {
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

// placeholderFindingID is what the helpers use because cite-or-drop
// no longer inspects finding_id - the analyzer overwrites it from the
// canonical anchors after the filter accepts the finding. Any
// schema-shaped string works here.
const placeholderFindingID = "f_0000000000000000"

// underImplFinding returns a structurally valid finding whose
// citations match the supplied values.
func underImplFinding(turnID, excerpt, hash, file string, lr lineRange) string {
	return fmt.Sprintf(`{
		"schema_version":"1",
		"finding_id":"%s",
		"kind":"under_impl",
		"title":"t",
		"confidence":"medium",
		"expected_intent":{"summary":"s","turn_id":"%s","prompt_excerpt":"%s","prompt_excerpt_hash":"%s"},
		"observed_diff_evidence":{"summary":"s","ai_authored_regions_checked":[{"file":"%s","lines":[[%d,%d]]}]},
		"missing_or_partial_area":{"note":"n"}
	}`, placeholderFindingID, turnID, excerpt, hash, file, lr[0], lr[1])
}

func deferredFinding(turnID, excerpt, hash, file string, lr lineRange) string {
	return fmt.Sprintf(`{
		"schema_version":"1",
		"finding_id":"%s",
		"kind":"deferred",
		"title":"t",
		"confidence":"medium",
		"originally_requested_in":{"turn_id":"%s","prompt_excerpt":"%s","prompt_excerpt_hash":"%s"},
		"trajectory_note":"n",
		"current_state":{"file":"%s","line_range":[%d,%d],"summary":"s"}
	}`, placeholderFindingID, turnID, excerpt, hash, file, lr[0], lr[1])
}

func unrequestedFinding(promptsConsidered int, file string, lr lineRange) string {
	return fmt.Sprintf(`{
		"schema_version":"1",
		"finding_id":"%s",
		"kind":"unrequested",
		"title":"t",
		"confidence":"medium",
		"delivered":{"file":"%s","line_range":[%d,%d],"evidence_class":"ai_exact","summary":"s"},
		"captured_intent_search":{"prompts_considered":%d,"result":"none","qualifier":"q"}
	}`, placeholderFindingID, file, lr[0], lr[1], promptsConsidered)
}

// --- Tests --------------------------------------------------------

// Empty findings list returns an empty accepted list.
func TestFilterFindingsByCitations_EmptyIsNoOp(t *testing.T) {
	res, err := FilterFindingsByCitations(json.RawMessage(`[]`), Bundle{})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if res.AcceptedCount != 0 || res.DroppedCount != 0 {
		t.Errorf("counts = %+v, want all zero", res)
	}
}

// Happy path: under_impl with matching turn, excerpt, hash, diff
// region, and derived finding_id is kept verbatim.
func TestFilterFindingsByCitations_UnderImplFullValidKept(t *testing.T) {
	bundle := canonicalBundle()
	findings := json.RawMessage("[" + underImplFinding("t-1", "add input validation", "h-1", "handler.go", lineRange{12, 14}) + "]")

	res, err := FilterFindingsByCitations(findings, bundle)
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if res.AcceptedCount != 1 {
		t.Errorf("valid finding dropped: %+v", res)
	}
}

// Unknown turn_id: drop with unknown_turn_id reason.
func TestFilterFindingsByCitations_UnknownTurnIDDropped(t *testing.T) {
	bundle := canonicalBundle()
	findings := json.RawMessage("[" + underImplFinding("t-FAKE", "add input validation", "h-1", "handler.go", lineRange{12, 14}) + "]")

	res, _ := FilterFindingsByCitations(findings, bundle)
	if res.DroppedReasons["unknown_turn_id"] != 1 {
		t.Errorf("DroppedReasons = %v, want unknown_turn_id=1", res.DroppedReasons)
	}
}

// Real turn but invented excerpt: drop with prompt_excerpt_mismatch.
func TestFilterFindingsByCitations_ExcerptMismatchDropped(t *testing.T) {
	bundle := canonicalBundle()
	findings := json.RawMessage("[" + underImplFinding("t-1", "WRONG excerpt", "h-1", "handler.go", lineRange{12, 14}) + "]")

	res, _ := FilterFindingsByCitations(findings, bundle)
	if res.DroppedReasons["prompt_excerpt_mismatch"] != 1 {
		t.Errorf("DroppedReasons = %v, want prompt_excerpt_mismatch=1", res.DroppedReasons)
	}
}

// Real turn + real excerpt but wrong hash: drop with prompt_hash_mismatch.
func TestFilterFindingsByCitations_HashMismatchDropped(t *testing.T) {
	bundle := canonicalBundle()
	findings := json.RawMessage("[" + underImplFinding("t-1", "add input validation", "h-DIFFERENT", "handler.go", lineRange{12, 14}) + "]")

	res, _ := FilterFindingsByCitations(findings, bundle)
	if res.DroppedReasons["prompt_hash_mismatch"] != 1 {
		t.Errorf("DroppedReasons = %v, want prompt_hash_mismatch=1", res.DroppedReasons)
	}
}

// File not in diff: drop with file_not_in_diff.
func TestFilterFindingsByCitations_FileNotInDiffDropped(t *testing.T) {
	bundle := canonicalBundle()
	findings := json.RawMessage("[" + underImplFinding("t-1", "add input validation", "h-1", "other.go", lineRange{12, 14}) + "]")

	res, _ := FilterFindingsByCitations(findings, bundle)
	if res.DroppedReasons["file_not_in_diff"] != 1 {
		t.Errorf("DroppedReasons = %v, want file_not_in_diff=1", res.DroppedReasons)
	}
}

// Line range outside changed regions: drop with line_range_outside_diff.
func TestFilterFindingsByCitations_LineRangeOutsideDiffDropped(t *testing.T) {
	bundle := canonicalBundle()
	findings := json.RawMessage("[" + underImplFinding("t-1", "add input validation", "h-1", "handler.go", lineRange{100, 110}) + "]")

	res, _ := FilterFindingsByCitations(findings, bundle)
	if res.DroppedReasons["line_range_outside_diff"] != 1 {
		t.Errorf("DroppedReasons = %v, want line_range_outside_diff=1", res.DroppedReasons)
	}
}

func TestFilterFindingsByCitations_ReversedLineRangeDropped(t *testing.T) {
	bundle := canonicalBundle()
	findings := json.RawMessage("[" + underImplFinding("t-1", "add input validation", "h-1", "handler.go", lineRange{14, 12}) + "]")

	res, _ := FilterFindingsByCitations(findings, bundle)
	if res.DroppedReasons["line_range_invalid"] != 1 {
		t.Errorf("DroppedReasons = %v, want line_range_invalid=1", res.DroppedReasons)
	}
}

// finding_id is no longer checked by cite-or-drop; the analyzer
// rewrites it from canonical anchors after this filter accepts the
// finding. Verify that a placeholder id passes the filter and that
// stampFindingIDs replaces it with the canonical derivation.
func TestStampFindingIDs_RewritesPlaceholderToCanonicalDerivation(t *testing.T) {
	bundle := canonicalBundle()
	good := underImplFinding("t-1", "add input validation", "h-1", "handler.go", lineRange{12, 14})
	res, _ := FilterFindingsByCitations(json.RawMessage("["+good+"]"), bundle)
	if res.AcceptedCount != 1 {
		t.Fatalf("filter dropped valid finding: %+v", res)
	}

	stamped, err := stampFindingIDs(res.Findings, "repo-abc", 7)
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	var arr []map[string]any
	if err := json.Unmarshal(stamped, &arr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	gotID, _ := arr[0]["finding_id"].(string)
	wantID := canonicalFindingIDForUnderImpl("repo-abc", 7, "t-1", "h-1", regionsOf("handler.go", lineRange{12, 14}))
	if gotID != wantID {
		t.Errorf("finding_id = %q, want %q", gotID, wantID)
	}
	if gotID == placeholderFindingID {
		t.Errorf("placeholder not overwritten: %q", gotID)
	}
}

// regionsOf builds a single file-region citation.
func regionsOf(file string, lines ...lineRange) []fileRegion {
	return []fileRegion{{File: file, Lines: lines}}
}

// Different observed regions produce different finding IDs.
func TestStampFindingIDs_UnderImplDifferentRegionsDoNotCollide(t *testing.T) {
	a := canonicalFindingIDForUnderImpl("r", 1, "t-1", "h-1", regionsOf("handler.go", lineRange{10, 12}))
	b := canonicalFindingIDForUnderImpl("r", 1, "t-1", "h-1", regionsOf("handler.go", lineRange{50, 52}))
	if a == b {
		t.Errorf("same prompt, different regions collided: %q", a)
	}
	c := canonicalFindingIDForUnderImpl("r", 1, "t-1", "h-1", regionsOf("other.go", lineRange{10, 12}))
	if a == c {
		t.Errorf("same prompt, different files collided: %q", a)
	}
}

// Every observed region contributes to the finding ID.
func TestStampFindingIDs_UnderImplAllRegionsContributeToID(t *testing.T) {
	common := lineRange{10, 12}
	a := canonicalFindingIDForUnderImpl("r", 1, "t-1", "h-1",
		[]fileRegion{{File: "a.go", Lines: []lineRange{common}}, {File: "b.go", Lines: []lineRange{{20, 22}}}})
	b := canonicalFindingIDForUnderImpl("r", 1, "t-1", "h-1",
		[]fileRegion{{File: "a.go", Lines: []lineRange{common}}, {File: "b.go", Lines: []lineRange{{50, 52}}}})
	if a == b {
		t.Errorf("findings sharing only the first region collided: %q", a)
	}
}

// Equivalent same-file region shapes produce the same finding ID.
func TestStampFindingIDs_UnderImplSameFileMultipleObjectsCollapse(t *testing.T) {
	split := []fileRegion{
		{File: "a.go", Lines: []lineRange{{10, 12}}},
		{File: "a.go", Lines: []lineRange{{30, 32}}},
	}
	splitReversed := []fileRegion{
		{File: "a.go", Lines: []lineRange{{30, 32}}},
		{File: "a.go", Lines: []lineRange{{10, 12}}},
	}
	merged := []fileRegion{
		{File: "a.go", Lines: []lineRange{{10, 12}, {30, 32}}},
	}
	a := canonicalFindingIDForUnderImpl("r", 1, "t-1", "h-1", split)
	b := canonicalFindingIDForUnderImpl("r", 1, "t-1", "h-1", splitReversed)
	c := canonicalFindingIDForUnderImpl("r", 1, "t-1", "h-1", merged)
	if a != b || a != c {
		t.Errorf("same set, different shapes produced different ids: %q / %q / %q", a, b, c)
	}
}

// Duplicate line ranges do not affect the finding ID.
func TestStampFindingIDs_UnderImplDuplicateRangesDedupe(t *testing.T) {
	withDup := []fileRegion{{File: "a.go", Lines: []lineRange{{10, 12}, {10, 12}, {30, 32}}}}
	clean := []fileRegion{{File: "a.go", Lines: []lineRange{{10, 12}, {30, 32}}}}
	if canonicalFindingIDForUnderImpl("r", 1, "t-1", "h-1", withDup) !=
		canonicalFindingIDForUnderImpl("r", 1, "t-1", "h-1", clean) {
		t.Errorf("duplicate ranges produced different ids")
	}
}

// Canonical encoding distinguishes filenames containing delimiter characters.
func TestStampFindingIDs_UnderImplFilenamesWithDelimitersDoNotCollide(t *testing.T) {
	// Compare a delimiter-bearing filename with two separate files.
	one := []fileRegion{{File: "a;b.go", Lines: []lineRange{{10, 12}}}}
	two := []fileRegion{
		{File: "a", Lines: []lineRange{{10, 12}}},
		{File: "b.go", Lines: []lineRange{{10, 12}}},
	}
	if canonicalFindingIDForUnderImpl("r", 1, "t-1", "h-1", one) ==
		canonicalFindingIDForUnderImpl("r", 1, "t-1", "h-1", two) {
		t.Errorf("delimiter-collision: 'a;b.go' encoded identically to 'a'+'b.go'")
	}
	// Other common delimiter characters must also remain distinct.
	colon := []fileRegion{{File: "path:to.go", Lines: []lineRange{{10, 12}}}}
	pipe := []fileRegion{{File: "path|to.go", Lines: []lineRange{{10, 12}}}}
	if canonicalFindingIDForUnderImpl("r", 1, "t-1", "h-1", colon) ==
		canonicalFindingIDForUnderImpl("r", 1, "t-1", "h-1", pipe) {
		t.Errorf("colon and pipe filenames must produce different ids")
	}
}

// Regions without line ranges are not actionable citations.
func TestFilterFindingsByCitations_UnderImplEmptyLinesRejected(t *testing.T) {
	bundle := canonicalBundle()
	// Build a finding with a region that has an empty Lines array.
	raw := `[{
		"schema_version":"1",
		"finding_id":"` + placeholderFindingID + `",
		"kind":"under_impl",
		"title":"t",
		"confidence":"medium",
		"expected_intent":{"summary":"s","turn_id":"t-1","prompt_excerpt":"add input validation","prompt_excerpt_hash":"h-1"},
		"observed_diff_evidence":{"summary":"s","ai_authored_regions_checked":[{"file":"handler.go","lines":[]}]},
		"missing_or_partial_area":{"note":"n"}
	}]`
	res, _ := FilterFindingsByCitations(json.RawMessage(raw), bundle)
	if res.AcceptedCount != 0 {
		t.Errorf("finding with empty lines should be dropped; res=%+v", res)
	}
	if res.DroppedReasons["line_range_missing"] != 1 {
		t.Errorf("expected line_range_missing drop; got %v", res.DroppedReasons)
	}
}

// Region and line-range ordering does not affect the finding ID.
func TestStampFindingIDs_UnderImplRegionOrderingIsCanonical(t *testing.T) {
	regions1 := []fileRegion{
		{File: "b.go", Lines: []lineRange{{20, 22}}},
		{File: "a.go", Lines: []lineRange{{30, 32}, {10, 12}}},
	}
	regions2 := []fileRegion{
		{File: "a.go", Lines: []lineRange{{10, 12}, {30, 32}}},
		{File: "b.go", Lines: []lineRange{{20, 22}}},
	}
	a := canonicalFindingIDForUnderImpl("r", 1, "t-1", "h-1", regions1)
	b := canonicalFindingIDForUnderImpl("r", 1, "t-1", "h-1", regions2)
	if a != b {
		t.Errorf("reordered regions produced different ids: %q vs %q", a, b)
	}
}

// Two deferred findings citing the same prompt but leaving deferred
// code in different files must produce different ids.
func TestStampFindingIDs_DeferredDifferentLocationsDoNotCollide(t *testing.T) {
	a := canonicalFindingIDForDeferred("r", 1, "t-1", "h-1", "a.go", 10, 12)
	b := canonicalFindingIDForDeferred("r", 1, "t-1", "h-1", "b.go", 10, 12)
	if a == b {
		t.Errorf("same prompt, different deferred files collided: %q", a)
	}
}

// unrequested has no turn citation; its canonical id derives from
// repository_id, pr_number, kind, file, and line_range.
func TestStampFindingIDs_UnrequestedDerivesFromFileAndLineRange(t *testing.T) {
	bundle := canonicalBundle()
	good := unrequestedFinding(1, "handler.go", lineRange{12, 14})
	res, _ := FilterFindingsByCitations(json.RawMessage("["+good+"]"), bundle)
	if res.AcceptedCount != 1 {
		t.Fatalf("filter dropped valid unrequested: %+v", res)
	}
	stamped, err := stampFindingIDs(res.Findings, "repo-abc", 7)
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	var arr []map[string]any
	_ = json.Unmarshal(stamped, &arr)
	gotID, _ := arr[0]["finding_id"].(string)
	wantID := canonicalFindingIDForUnrequested("repo-abc", 7, "handler.go", 12, 14)
	if gotID != wantID {
		t.Errorf("finding_id = %q, want %q", gotID, wantID)
	}
}

// Repeated runs over identical anchors must produce identical ids so
// feedback rows can join on finding_id across re-uploads. Different
// (repo_id, pr_number) must produce different ids to prevent cross-PR
// collisions.
func TestStampFindingIDs_DeterministicAcrossRuns(t *testing.T) {
	a := canonicalFindingIDForUnderImpl("repo-A", 1, "t-1", "h-1", regionsOf("handler.go", lineRange{12, 14}))
	b := canonicalFindingIDForUnderImpl("repo-A", 1, "t-1", "h-1", regionsOf("handler.go", lineRange{12, 14}))
	if a != b {
		t.Errorf("id not deterministic: %q vs %q", a, b)
	}
	c := canonicalFindingIDForUnderImpl("repo-A", 2, "t-1", "h-1", regionsOf("handler.go", lineRange{12, 14}))
	if a == c {
		t.Errorf("id should change with pr_number: %q == %q", a, c)
	}
	d := canonicalFindingIDForUnderImpl("repo-B", 1, "t-1", "h-1", regionsOf("handler.go", lineRange{12, 14}))
	if a == d {
		t.Errorf("id should change with repository_id: %q == %q", a, d)
	}
}

// deferred follows the same citation rules.
func TestFilterFindingsByCitations_DeferredValidCitationKept(t *testing.T) {
	bundle := canonicalBundle()
	findings := json.RawMessage("[" + deferredFinding("t-1", "add input validation", "h-1", "handler.go", lineRange{12, 14}) + "]")

	res, _ := FilterFindingsByCitations(findings, bundle)
	if res.AcceptedCount != 1 {
		t.Errorf("valid deferred dropped: %+v", res)
	}
}

// unrequested with mismatched prompt count (higher than visible)
// drops with prompt_count_mismatch.
func TestFilterFindingsByCitations_UnrequestedOverstatedPromptCountDropped(t *testing.T) {
	bundle := canonicalBundle()
	findings := json.RawMessage("[" + unrequestedFinding(99, "handler.go", lineRange{12, 14}) + "]")

	res, _ := FilterFindingsByCitations(findings, bundle)
	if res.DroppedReasons["prompt_count_mismatch"] != 1 {
		t.Errorf("DroppedReasons = %v, want prompt_count_mismatch=1", res.DroppedReasons)
	}
}

// unrequested with prompt_count BELOW visible count (partial search)
// is also dropped: claiming "code is unrequested" while only having
// looked at some of the intent is not actionable.
func TestFilterFindingsByCitations_UnrequestedPartialSearchDropped(t *testing.T) {
	bundle := canonicalBundle() // 1 turn visible
	findings := json.RawMessage("[" + unrequestedFinding(0, "handler.go", lineRange{12, 14}) + "]")

	res, _ := FilterFindingsByCitations(findings, bundle)
	if res.DroppedReasons["prompt_count_mismatch"] != 1 {
		t.Errorf("DroppedReasons = %v, want prompt_count_mismatch=1; got %v", res.DroppedReasons, res)
	}
}

// unrequested with prompts_considered == visible count + valid file
// is kept.
func TestFilterFindingsByCitations_UnrequestedHonestKept(t *testing.T) {
	bundle := canonicalBundle()
	findings := json.RawMessage("[" + unrequestedFinding(1, "handler.go", lineRange{12, 14}) + "]")

	res, _ := FilterFindingsByCitations(findings, bundle)
	if res.AcceptedCount != 1 {
		t.Errorf("honest unrequested dropped: %+v", res)
	}
}

// Empty bundle.Turns drops cited kinds. unrequested(0) with a valid
// in-diff file survives.
func TestFilterFindingsByCitations_EmptyTurnsDropsCitedKinds(t *testing.T) {
	bundle := Bundle{
		Turns: nil,
		Diff:  []byte("--- a/handler.go\n+++ b/handler.go\n@@ -10,5 +10,11 @@\n+added\n"),
	}
	findings := json.RawMessage(`[` +
		underImplFinding("t-1", "x", "h", "handler.go", lineRange{12, 14}) + `,` +
		deferredFinding("t-1", "x", "h", "handler.go", lineRange{12, 14}) + `,` +
		unrequestedFinding(0, "handler.go", lineRange{12, 14}) +
		`]`)
	res, _ := FilterFindingsByCitations(findings, bundle)
	if res.AcceptedCount != 1 {
		t.Errorf("only unrequested(0) with valid diff cite should survive; got %+v", res)
	}
	if res.DroppedReasons["unknown_turn_id"] != 2 {
		t.Errorf("expected 2 unknown_turn_id drops; got %v", res.DroppedReasons)
	}
}

// --- Parse-changed-regions covers --------------------------------

func TestParseChangedRegions_SimpleSingleFile(t *testing.T) {
	diff := []byte("--- a/foo.go\n+++ b/foo.go\n@@ -10,3 +10,5 @@\n line\n+added\n+added\n line\n")
	out := parseChangedRegions(diff)
	if len(out["foo.go"]) != 1 || out["foo.go"][0] != (lineRange{10, 14}) {
		t.Errorf("regions = %v, want {foo.go:[[10,14]]}", out)
	}
}

func TestParseChangedRegions_MultipleHunksMultipleFiles(t *testing.T) {
	diff := []byte(
		"--- a/foo.go\n+++ b/foo.go\n@@ -1,2 +1,3 @@\n+x\n@@ -10,2 +10,3 @@\n+y\n" +
			"--- a/bar.go\n+++ b/bar.go\n@@ -5,1 +5,2 @@\n+z\n",
	)
	out := parseChangedRegions(diff)
	if len(out["foo.go"]) != 2 || len(out["bar.go"]) != 1 {
		t.Errorf("regions = %v, want foo.go:2 bar.go:1", out)
	}
}
