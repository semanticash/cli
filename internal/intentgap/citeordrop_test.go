package intentgap

import (
	"encoding/json"
	"fmt"
	"strings"
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
// is also dropped: a partial intent search cannot support an
// unrequested-code finding.
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
func TestFilterFindingsByCitations_UnrequestedValidKept(t *testing.T) {
	bundle := canonicalBundle()
	findings := json.RawMessage("[" + unrequestedFinding(1, "handler.go", lineRange{12, 14}) + "]")

	res, _ := FilterFindingsByCitations(findings, bundle)
	if res.AcceptedCount != 1 {
		t.Errorf("valid unrequested finding dropped: %+v", res)
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

// A positive action citation must point at a real captured action.
// An empty or unknown ActionID drops the finding.
func TestValidateAgentActionCitation_RequiresKnownActionID(t *testing.T) {
	idx := indexActionsByID([]BundleAgentAction{
		{ActionID: "a1", ToolName: "Edit", FilePath: "a.go"},
	})

	if reason, drop := validateAgentActionCitation(agentActionCitation{ActionID: ""}, idx); !drop || reason != "missing_action_id" {
		t.Errorf("empty action_id should drop with missing_action_id; got reason=%q drop=%v", reason, drop)
	}
	if reason, drop := validateAgentActionCitation(agentActionCitation{ActionID: "ghost"}, idx); !drop || reason != "unknown_action_id" {
		t.Errorf("unknown action_id should drop; got reason=%q drop=%v", reason, drop)
	}
}

// A citation that names a real action and no scope is accepted: the
// ActionID alone is enough to anchor the finding.
func TestValidateAgentActionCitation_ScopelessCitationAccepted(t *testing.T) {
	idx := indexActionsByID([]BundleAgentAction{{ActionID: "a1", ToolName: "Edit", FilePath: "a.go"}})
	if reason, drop := validateAgentActionCitation(agentActionCitation{ActionID: "a1"}, idx); drop {
		t.Errorf("scopeless citation should be accepted; got reason=%q", reason)
	}
}

// A scope file that disagrees with the action's recorded file drops
// the citation.
func TestValidateAgentActionCitation_ScopeFileMustMatchActionFile(t *testing.T) {
	idx := indexActionsByID([]BundleAgentAction{{ActionID: "a1", ToolName: "Edit", FilePath: "a.go"}})
	cit := agentActionCitation{
		ActionID: "a1",
		Scope:    &citationScope{File: "b.go"},
	}
	if reason, drop := validateAgentActionCitation(cit, idx); !drop || reason != "action_file_mismatch" {
		t.Errorf("file mismatch should drop; got reason=%q drop=%v", reason, drop)
	}
}

// When an action's FilePath is empty (e.g. an unknown-path Bash
// fallback), the validator does not reject a scoped citation - the
// action recorded no path to compare against.
func TestValidateAgentActionCitation_EmptyActionFilePathPassesFileScope(t *testing.T) {
	idx := indexActionsByID([]BundleAgentAction{{ActionID: "a1", ToolName: "Bash", FilePath: ""}})
	cit := agentActionCitation{
		ActionID: "a1",
		Scope:    &citationScope{File: "a.go"},
	}
	if reason, drop := validateAgentActionCitation(cit, idx); drop {
		t.Errorf("unknown action FilePath should not reject file scope; got reason=%q", reason)
	}
}

// When both the action and the citation carry line ranges, they must
// overlap. Non-overlapping ranges drop the citation.
func TestValidateAgentActionCitation_LineRangeMustOverlap(t *testing.T) {
	idx := indexActionsByID([]BundleAgentAction{
		{ActionID: "a1", ToolName: "Edit", FilePath: "a.go", LineRangeStart: 10, LineRangeEnd: 20},
	})
	cit := agentActionCitation{
		ActionID: "a1",
		Scope:    &citationScope{File: "a.go", LineRange: lineRange{30, 40}},
	}
	if reason, drop := validateAgentActionCitation(cit, idx); !drop || reason != "action_line_range_mismatch" {
		t.Errorf("non-overlapping line range should drop; got reason=%q drop=%v", reason, drop)
	}
	overlap := agentActionCitation{
		ActionID: "a1",
		Scope:    &citationScope{File: "a.go", LineRange: lineRange{15, 25}},
	}
	if reason, drop := validateAgentActionCitation(overlap, idx); drop {
		t.Errorf("overlapping line range should be accepted; got reason=%q", reason)
	}
}

// When either side's line range is zero, the line-range check is not
// asserted. Line ranges are best-effort, so unknown ranges are not
// treated as mismatches.
func TestValidateAgentActionCitation_UnknownLineRangesSkipRangeCheck(t *testing.T) {
	idx := indexActionsByID([]BundleAgentAction{
		{ActionID: "a1", ToolName: "Edit", FilePath: "a.go"},
	})
	citWithRange := agentActionCitation{
		ActionID: "a1",
		Scope:    &citationScope{File: "a.go", LineRange: lineRange{30, 40}},
	}
	if reason, drop := validateAgentActionCitation(citWithRange, idx); drop {
		t.Errorf("action with no range should not reject citation's range; got reason=%q", reason)
	}
}

// Negative citations without a resolved scope are not provable. They
// must be dropped rather than accepted as universal negatives.
func TestValidateNoActionCitation_RequiresScope(t *testing.T) {
	actions := []BundleAgentAction{{ActionID: "a1", FilePath: "a.go"}}
	if reason, drop := validateNoActionCitation(noActionCitation{Scope: nil}, actions); !drop || reason != "negative_citation_requires_scope" {
		t.Errorf("nil scope should drop; got reason=%q drop=%v", reason, drop)
	}
	if reason, drop := validateNoActionCitation(noActionCitation{Scope: &citationScope{File: ""}}, actions); !drop || reason != "negative_citation_requires_scope" {
		t.Errorf("empty scope file should drop; got reason=%q drop=%v", reason, drop)
	}
}

// A file-level negative citation ("no action touched a.go") is
// dropped whenever any action on that file exists. The file-only
// scope is the strictest case.
func TestValidateNoActionCitation_FileLevelDroppedWhenAnyActionTouchedFile(t *testing.T) {
	actions := []BundleAgentAction{
		{ActionID: "a1", FilePath: "a.go", LineRangeStart: 5, LineRangeEnd: 10},
	}
	cit := noActionCitation{Scope: &citationScope{File: "a.go"}}
	if reason, drop := validateNoActionCitation(cit, actions); !drop || reason != "action_touched_negative_scope" {
		t.Errorf("file-level negative should drop when any action touched the file; got reason=%q drop=%v", reason, drop)
	}
}

// A line-narrowed negative citation is accepted only when no
// action overlaps the cited lines.
func TestValidateNoActionCitation_LineNarrowed(t *testing.T) {
	actions := []BundleAgentAction{
		{ActionID: "a1", FilePath: "a.go", LineRangeStart: 5, LineRangeEnd: 10},
	}
	overlapping := noActionCitation{
		Scope: &citationScope{File: "a.go", LineRange: lineRange{8, 12}},
	}
	if reason, drop := validateNoActionCitation(overlapping, actions); !drop {
		t.Errorf("overlapping line negative should drop; got reason=%q", reason)
	}
	disjoint := noActionCitation{
		Scope: &citationScope{File: "a.go", LineRange: lineRange{50, 60}},
	}
	if reason, drop := validateNoActionCitation(disjoint, actions); drop {
		t.Errorf("disjoint line negative should be accepted; got reason=%q", reason)
	}
}

// An action whose line range is unknown is treated conservatively
// for negative citations: the validator cannot prove non-overlap, so
// the negative is dropped.
func TestValidateNoActionCitation_UnknownActionRangeDropsNegative(t *testing.T) {
	actions := []BundleAgentAction{
		{ActionID: "a1", FilePath: "a.go"}, // no line range
	}
	cit := noActionCitation{
		Scope: &citationScope{File: "a.go", LineRange: lineRange{50, 60}},
	}
	if reason, drop := validateNoActionCitation(cit, actions); !drop {
		t.Errorf("unknown action range should drop the negative citation; got reason=%q", reason)
	}
}

// A negative citation on a file with no actions is accepted.
func TestValidateNoActionCitation_NoActionsOnFileAccepted(t *testing.T) {
	actions := []BundleAgentAction{
		{ActionID: "a1", FilePath: "other.go", LineRangeStart: 5, LineRangeEnd: 10},
	}
	cit := noActionCitation{Scope: &citationScope{File: "a.go"}}
	if reason, drop := validateNoActionCitation(cit, actions); drop {
		t.Errorf("no actions on cited file should accept negative; got reason=%q", reason)
	}
}

// A captured action whose FilePath is unknown (typically a Bash
// invocation whose command did not parse into a concrete path) blocks
// any file-scoped negative citation. The validator cannot prove the
// agent did not touch the cited file, so the conservative result is
// to drop the negative rather than accept it on insufficient evidence.
func TestValidateNoActionCitation_UnknownActionFilePathDropsNegative(t *testing.T) {
	actions := []BundleAgentAction{
		{ActionID: "a1", ToolName: "Bash", FilePath: ""}, // unknown-path Bash
	}
	cit := noActionCitation{Scope: &citationScope{File: "a.go"}}
	if reason, drop := validateNoActionCitation(cit, actions); !drop || reason != "action_touched_negative_scope" {
		t.Errorf("unknown action FilePath should drop the negative; got reason=%q drop=%v", reason, drop)
	}
}

// A line-narrowed negative on a specific file is also blocked by an
// unknown-path action - that activity may have touched any line of
// any file. The validator stays conservative across both scope
// shapes.
func TestValidateNoActionCitation_UnknownActionFilePathBlocksLineNarrowedNegative(t *testing.T) {
	actions := []BundleAgentAction{
		{ActionID: "a1", ToolName: "Bash", FilePath: ""},
	}
	cit := noActionCitation{
		Scope: &citationScope{File: "a.go", LineRange: lineRange{50, 60}},
	}
	if reason, drop := validateNoActionCitation(cit, actions); !drop {
		t.Errorf("unknown action FilePath should block line-narrowed negative; got reason=%q drop=%v", reason, drop)
	}
}

// A finding that carries a valid positive action citation passes the
// pipeline end-to-end and lands in the accepted set, with no drop
// reasons recorded. Uses under_impl because the deferred-kind rule
// additionally requires the cited action to be part of a trajectory;
// that case is covered by the dedicated trajectory tests below.
func TestFilterFindingsByCitations_ValidPositiveActionCitationKept(t *testing.T) {
	bundle := canonicalBundle()
	bundle.AgentActions = []BundleAgentAction{
		{ActionID: "a_known", ToolName: "Edit", FilePath: "handler.go"},
	}
	body := underImplFinding("t-1", "add input validation", "h-1", "handler.go", lineRange{12, 14})
	body = strings.Replace(body, `"observed_diff_evidence":`,
		`"agent_action_citation":{"action_id":"a_known","scope":{"file":"handler.go"}},"observed_diff_evidence":`, 1)

	res, err := FilterFindingsByCitations(json.RawMessage("["+body+"]"), bundle)
	if err != nil {
		t.Fatalf("FilterFindingsByCitations: %v", err)
	}
	if res.AcceptedCount != 1 {
		t.Errorf("expected 1 accepted finding; got %d, drops=%v", res.AcceptedCount, res.DroppedReasons)
	}
}

// A finding whose positive action citation names an ActionID that is
// not in the bundle is rejected end-to-end. This is the analogue of
// the unknown_turn_id drop for prompt citations.
func TestFilterFindingsByCitations_UnknownActionCitationDropped(t *testing.T) {
	bundle := canonicalBundle()
	bundle.AgentActions = []BundleAgentAction{
		{ActionID: "a_known", ToolName: "Edit", FilePath: "handler.go"},
	}
	body := deferredFinding("t-1", "add input validation", "h-1", "handler.go", lineRange{12, 14})
	body = strings.Replace(body, `"current_state":`,
		`"agent_action_citation":{"action_id":"a_ghost"},"current_state":`, 1)

	res, err := FilterFindingsByCitations(json.RawMessage("["+body+"]"), bundle)
	if err != nil {
		t.Fatalf("FilterFindingsByCitations: %v", err)
	}
	if res.AcceptedCount != 0 {
		t.Errorf("finding with unknown action citation should have been dropped; got accepted=%d", res.AcceptedCount)
	}
	if res.DroppedReasons["unknown_action_id"] != 1 {
		t.Errorf("expected unknown_action_id drop; got %v", res.DroppedReasons)
	}
}

// A finding with a negative no-action citation is rejected when an
// action in the bundle does touch the cited scope. The negative
// citation is unverifiable, so the finding must not survive the filter.
func TestFilterFindingsByCitations_NoActionCitationDroppedWhenTouched(t *testing.T) {
	bundle := canonicalBundle()
	bundle.AgentActions = []BundleAgentAction{
		{ActionID: "a_known", ToolName: "Edit", FilePath: "handler.go"},
	}
	body := deferredFinding("t-1", "add input validation", "h-1", "handler.go", lineRange{12, 14})
	body = strings.Replace(body, `"current_state":`,
		`"no_action_citation":{"scope":{"file":"handler.go"}},"current_state":`, 1)

	res, err := FilterFindingsByCitations(json.RawMessage("["+body+"]"), bundle)
	if err != nil {
		t.Fatalf("FilterFindingsByCitations: %v", err)
	}
	if res.AcceptedCount != 0 {
		t.Errorf("negative on a touched scope should drop; got accepted=%d", res.AcceptedCount)
	}
	if res.DroppedReasons["action_touched_negative_scope"] != 1 {
		t.Errorf("expected action_touched_negative_scope drop; got %v", res.DroppedReasons)
	}
}

// Findings that do not carry the new citation fields behave exactly
// as they did before action evidence was added. The wiring must be a
// no-op for legacy producers.
func TestFilterFindingsByCitations_NoActionCitationFieldsPreservesLegacyBehavior(t *testing.T) {
	bundle := canonicalBundle()
	// Two findings: the first is valid by the existing rules, the
	// second has a citation flaw the legacy pipeline already catches.
	good := deferredFinding("t-1", "add input validation", "h-1", "handler.go", lineRange{12, 14})
	bad := deferredFinding("t-1", "WRONG excerpt", "h-1", "handler.go", lineRange{12, 14})

	res, err := FilterFindingsByCitations(json.RawMessage("["+good+","+bad+"]"), bundle)
	if err != nil {
		t.Fatalf("FilterFindingsByCitations: %v", err)
	}
	if res.AcceptedCount != 1 {
		t.Errorf("expected 1 accepted; got %d", res.AcceptedCount)
	}
	if res.DroppedReasons["prompt_excerpt_mismatch"] != 1 {
		t.Errorf("legacy excerpt-mismatch drop should still fire; got %v", res.DroppedReasons)
	}
}

// A finding that contains agent_action_citation with the wrong JSON
// type (e.g. a string instead of an object) is rejected. Treating a
// malformed field as no-op would violate the invariant: if action
// citation fields appear in a finding, they are verified or dropped.
func TestFilterFindingsByCitations_MalformedActionCitationDropped(t *testing.T) {
	bundle := canonicalBundle()
	bundle.AgentActions = []BundleAgentAction{
		{ActionID: "a_known", ToolName: "Edit", FilePath: "handler.go"},
	}
	body := deferredFinding("t-1", "add input validation", "h-1", "handler.go", lineRange{12, 14})
	body = strings.Replace(body, `"current_state":`,
		`"agent_action_citation":"not-an-object","current_state":`, 1)

	res, err := FilterFindingsByCitations(json.RawMessage("["+body+"]"), bundle)
	if err != nil {
		t.Fatalf("FilterFindingsByCitations: %v", err)
	}
	if res.AcceptedCount != 0 {
		t.Errorf("malformed citation should drop; got accepted=%d", res.AcceptedCount)
	}
	if res.DroppedReasons["malformed_action_citation"] != 1 {
		t.Errorf("expected malformed_action_citation drop; got %v", res.DroppedReasons)
	}
}

// The same rule applies to the negative citation field: a malformed
// no_action_citation is rejected rather than ignored.
func TestFilterFindingsByCitations_MalformedNoActionCitationDropped(t *testing.T) {
	bundle := canonicalBundle()
	body := deferredFinding("t-1", "add input validation", "h-1", "handler.go", lineRange{12, 14})
	body = strings.Replace(body, `"current_state":`,
		`"no_action_citation":[1,2,3],"current_state":`, 1)

	res, err := FilterFindingsByCitations(json.RawMessage("["+body+"]"), bundle)
	if err != nil {
		t.Fatalf("FilterFindingsByCitations: %v", err)
	}
	if res.AcceptedCount != 0 {
		t.Errorf("malformed negative citation should drop; got accepted=%d", res.AcceptedCount)
	}
	if res.DroppedReasons["malformed_no_action_citation"] != 1 {
		t.Errorf("expected malformed_no_action_citation drop; got %v", res.DroppedReasons)
	}
}

// An explicit JSON null is treated as "field omitted." Producers can
// clear a citation by emitting null without paying a drop penalty.
func TestFilterFindingsByCitations_NullActionCitationFieldsAreOmitted(t *testing.T) {
	bundle := canonicalBundle()
	body := deferredFinding("t-1", "add input validation", "h-1", "handler.go", lineRange{12, 14})
	body = strings.Replace(body, `"current_state":`,
		`"agent_action_citation":null,"no_action_citation":null,"current_state":`, 1)

	res, err := FilterFindingsByCitations(json.RawMessage("["+body+"]"), bundle)
	if err != nil {
		t.Fatalf("FilterFindingsByCitations: %v", err)
	}
	if res.AcceptedCount != 1 {
		t.Errorf("null citation fields should be ignored; got accepted=%d, drops=%v", res.AcceptedCount, res.DroppedReasons)
	}
}

// A no_action_citation must be rejected whenever the captured-actions
// list was truncated by the bundle's size cap. An older action that
// was dropped at the cap could have touched the cited scope, so the
// negative citation is unverifiable from the retained action data.
func TestFilterFindingsByCitations_NoActionCitationDroppedUnderTruncation(t *testing.T) {
	bundle := canonicalBundle()
	bundle.AgentActions = []BundleAgentAction{
		{ActionID: "a_known", ToolName: "Edit", FilePath: "other.go"},
	}
	bundle.Truncated.AgentActionsDropped = 3

	body := deferredFinding("t-1", "add input validation", "h-1", "handler.go", lineRange{12, 14})
	// File-only negative on handler.go: no visible action touches it,
	// but truncation means the validator cannot prove that.
	body = strings.Replace(body, `"current_state":`,
		`"no_action_citation":{"scope":{"file":"handler.go"}},"current_state":`, 1)

	res, err := FilterFindingsByCitations(json.RawMessage("["+body+"]"), bundle)
	if err != nil {
		t.Fatalf("FilterFindingsByCitations: %v", err)
	}
	if res.AcceptedCount != 0 {
		t.Errorf("negative under truncation should drop; got accepted=%d", res.AcceptedCount)
	}
	if res.DroppedReasons["actions_truncated_negative_unverifiable"] != 1 {
		t.Errorf("expected actions_truncated_negative_unverifiable drop; got %v", res.DroppedReasons)
	}
}

// Positive citations remain valid under truncation. The cited action
// must exist in the retained action list. Uses under_impl because
// the deferred-kind trajectory rule is exercised by dedicated tests.
func TestFilterFindingsByCitations_PositiveCitationUnchangedUnderTruncation(t *testing.T) {
	bundle := canonicalBundle()
	bundle.AgentActions = []BundleAgentAction{
		{ActionID: "a_known", ToolName: "Edit", FilePath: "handler.go"},
	}
	bundle.Truncated.AgentActionsDropped = 3
	body := underImplFinding("t-1", "add input validation", "h-1", "handler.go", lineRange{12, 14})
	body = strings.Replace(body, `"observed_diff_evidence":`,
		`"agent_action_citation":{"action_id":"a_known","scope":{"file":"handler.go"}},"observed_diff_evidence":`, 1)

	res, err := FilterFindingsByCitations(json.RawMessage("["+body+"]"), bundle)
	if err != nil {
		t.Fatalf("FilterFindingsByCitations: %v", err)
	}
	if res.AcceptedCount != 1 {
		t.Errorf("positive citation under truncation should still pass; got accepted=%d drops=%v", res.AcceptedCount, res.DroppedReasons)
	}
}

// A deferred finding that cites an action belonging to a detected
// revert trajectory on the SAME file as current_state passes cite-
// or-drop end-to-end. The bundle here records changes on
// handler.go:10-20 (in the diff) while the trajectory cluster sits
// at handler.go:50-65 (no diff overlap), so both file alignment and
// the trajectory detection hold.
func TestFilterFindingsByCitations_DeferredCitingTrajectoryActionKept(t *testing.T) {
	bundle := canonicalBundle()
	bundle.AgentActions = []BundleAgentAction{
		{ActionID: "a_traj_one", TurnID: "t-1", ToolName: "Edit", FilePath: "handler.go", LineRangeStart: 50, LineRangeEnd: 60},
		{ActionID: "a_traj_two", TurnID: "t-1", ToolName: "Edit", FilePath: "handler.go", LineRangeStart: 55, LineRangeEnd: 65},
	}
	body := deferredFinding("t-1", "add input validation", "h-1", "handler.go", lineRange{12, 14})
	body = strings.Replace(body, `"current_state":`,
		`"agent_action_citation":{"action_id":"a_traj_one"},"current_state":`, 1)

	res, err := FilterFindingsByCitations(json.RawMessage("["+body+"]"), bundle)
	if err != nil {
		t.Fatalf("FilterFindingsByCitations: %v", err)
	}
	if res.AcceptedCount != 1 {
		t.Errorf("deferred citing a same-file trajectory action should pass; got accepted=%d drops=%v", res.AcceptedCount, res.DroppedReasons)
	}
}

// A deferred finding whose cited action belongs to a trajectory on
// a different file than current_state is rejected. Without this
// scope bound, a reverted edit elsewhere in the PR could anchor a
// deferred finding regardless of where the finding points.
func TestFilterFindingsByCitations_DeferredCitingTrajectoryOnUnrelatedFileDropped(t *testing.T) {
	bundle := canonicalBundle()
	// Trajectory on extras.go (no diff for that file), but the
	// finding's current_state points at handler.go.
	bundle.AgentActions = []BundleAgentAction{
		{ActionID: "a_traj_one", TurnID: "t-1", ToolName: "Edit", FilePath: "extras.go", LineRangeStart: 1, LineRangeEnd: 5},
		{ActionID: "a_traj_two", TurnID: "t-1", ToolName: "Edit", FilePath: "extras.go", LineRangeStart: 3, LineRangeEnd: 7},
	}
	body := deferredFinding("t-1", "add input validation", "h-1", "handler.go", lineRange{12, 14})
	body = strings.Replace(body, `"current_state":`,
		`"agent_action_citation":{"action_id":"a_traj_one"},"current_state":`, 1)

	res, err := FilterFindingsByCitations(json.RawMessage("["+body+"]"), bundle)
	if err != nil {
		t.Fatalf("FilterFindingsByCitations: %v", err)
	}
	if res.AcceptedCount != 0 {
		t.Errorf("trajectory on different file should drop deferred; got accepted=%d", res.AcceptedCount)
	}
	if res.DroppedReasons["deferred_trajectory_scope_mismatch"] != 1 {
		t.Errorf("expected deferred_trajectory_scope_mismatch; got %v", res.DroppedReasons)
	}
}

// A deferred finding whose cited action is NOT part of any detected
// revert trajectory is rejected. A deferred finding references a
// mechanical add-then-remove sequence, so the cited action must
// belong to a detected trajectory; a single clean Edit that landed
// in the diff does not match that shape.
func TestFilterFindingsByCitations_DeferredCitingNonTrajectoryActionDropped(t *testing.T) {
	bundle := canonicalBundle()
	// One Edit on handler.go (which the diff covers) is not a
	// trajectory: a single action cannot be add-then-remove.
	bundle.AgentActions = []BundleAgentAction{
		{ActionID: "a_landed", TurnID: "t-1", ToolName: "Edit", FilePath: "handler.go"},
	}
	body := deferredFinding("t-1", "add input validation", "h-1", "handler.go", lineRange{12, 14})
	body = strings.Replace(body, `"current_state":`,
		`"agent_action_citation":{"action_id":"a_landed"},"current_state":`, 1)

	res, err := FilterFindingsByCitations(json.RawMessage("["+body+"]"), bundle)
	if err != nil {
		t.Fatalf("FilterFindingsByCitations: %v", err)
	}
	if res.AcceptedCount != 0 {
		t.Errorf("deferred citing non-trajectory action should drop; got accepted=%d", res.AcceptedCount)
	}
	if res.DroppedReasons["deferred_action_not_in_trajectory"] != 1 {
		t.Errorf("expected deferred_action_not_in_trajectory; got %v", res.DroppedReasons)
	}
}

// The trajectory rule is scoped to deferred findings. An under_impl
// finding may cite any captured action whose file and line range
// match the finding; trajectory membership is irrelevant there.
func TestFilterFindingsByCitations_UnderImplCitingNonTrajectoryActionNotSubjectToRule(t *testing.T) {
	bundle := canonicalBundle()
	bundle.AgentActions = []BundleAgentAction{
		{ActionID: "a_landed", TurnID: "t-1", ToolName: "Edit", FilePath: "handler.go"},
	}
	body := underImplFinding("t-1", "add input validation", "h-1", "handler.go", lineRange{12, 14})
	body = strings.Replace(body, `"observed_diff_evidence":`,
		`"agent_action_citation":{"action_id":"a_landed"},"observed_diff_evidence":`, 1)

	res, err := FilterFindingsByCitations(json.RawMessage("["+body+"]"), bundle)
	if err != nil {
		t.Fatalf("FilterFindingsByCitations: %v", err)
	}
	if res.AcceptedCount != 1 {
		t.Errorf("under_impl with non-trajectory action should still pass; got accepted=%d drops=%v", res.AcceptedCount, res.DroppedReasons)
	}
}

// A deferred finding that omits agent_action_citation entirely
// continues to pass under the existing prompt + diff checks. The
// trajectory rule only fires when the citation is present.
func TestFilterFindingsByCitations_DeferredWithoutAgentActionCitationStillKept(t *testing.T) {
	bundle := canonicalBundle()
	body := deferredFinding("t-1", "add input validation", "h-1", "handler.go", lineRange{12, 14})
	// No agent_action_citation field added.

	res, err := FilterFindingsByCitations(json.RawMessage("["+body+"]"), bundle)
	if err != nil {
		t.Fatalf("FilterFindingsByCitations: %v", err)
	}
	if res.AcceptedCount != 1 {
		t.Errorf("deferred without citation should still pass; got accepted=%d drops=%v", res.AcceptedCount, res.DroppedReasons)
	}
}
