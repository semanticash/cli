package intentgap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// CiteOrDropResult reports accepted findings and rejection reasons.
type CiteOrDropResult struct {
	Findings       json.RawMessage
	AcceptedCount  int
	DroppedCount   int
	DroppedReasons map[string]int
}

// FilterFindingsByCitations drops findings whose prompt or diff citations
// cannot be verified against the local bundle.
//
// Prompt citations must match captured turns, and diff citations must match
// changed files and line ranges. Rejection counts are returned as metadata.
func FilterFindingsByCitations(findings json.RawMessage, bundle Bundle) (CiteOrDropResult, error) {
	res := CiteOrDropResult{
		Findings:       json.RawMessage("[]"),
		DroppedReasons: map[string]int{},
	}

	if len(findings) == 0 {
		return res, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(findings, &arr); err != nil {
		return res, fmt.Errorf("filter: findings not a JSON array: %w", err)
	}
	if len(arr) == 0 {
		return res, nil
	}

	// Index turns once for per-finding citation checks.
	turnsByID := make(map[string]BundleTurn, len(bundle.Turns))
	for _, t := range bundle.Turns {
		turnsByID[t.TurnID] = t
	}
	capturedPromptCount := len(bundle.Turns)
	changedRegions := parseChangedRegions(bundle.Diff)

	accepted := make([]json.RawMessage, 0, len(arr))
	for _, raw := range arr {
		dropReason, drop := shouldDropFinding(raw, turnsByID, capturedPromptCount, changedRegions)
		if drop {
			res.DroppedCount++
			res.DroppedReasons[dropReason]++
			continue
		}
		accepted = append(accepted, raw)
	}
	res.AcceptedCount = len(accepted)
	out, err := json.Marshal(accepted)
	if err != nil {
		return res, fmt.Errorf("filter: re-marshal accepted findings: %w", err)
	}
	res.Findings = out
	return res, nil
}

// shouldDropFinding validates the evidence required by one finding kind.
func shouldDropFinding(raw json.RawMessage, turnsByID map[string]BundleTurn, capturedPromptCount int, changedRegions map[string][]lineRange) (string, bool) {
	var probe struct {
		Kind      string `json:"kind"`
		FindingID string `json:"finding_id"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "malformed_json", true
	}

	switch probe.Kind {
	case "under_impl":
		var f struct {
			FindingID      string `json:"finding_id"`
			ExpectedIntent struct {
				TurnID            string `json:"turn_id"`
				PromptExcerpt     string `json:"prompt_excerpt"`
				PromptExcerptHash string `json:"prompt_excerpt_hash"`
			} `json:"expected_intent"`
			ObservedDiffEvidence struct {
				AIAuthoredRegionsChecked []fileRegion `json:"ai_authored_regions_checked"`
			} `json:"observed_diff_evidence"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return "malformed_under_impl", true
		}
		if reason, drop := validateCitedTurn(turnsByID, f.ExpectedIntent.TurnID, f.ExpectedIntent.PromptExcerpt, f.ExpectedIntent.PromptExcerptHash); drop {
			return reason, true
		}
		// Keep the evidence requirement enforced even if the schema changes.
		if len(f.ObservedDiffEvidence.AIAuthoredRegionsChecked) == 0 {
			return "no_observed_regions", true
		}
		for _, fr := range f.ObservedDiffEvidence.AIAuthoredRegionsChecked {
			if reason, drop := validateRegionInDiff(fr, changedRegions); drop {
				return reason, true
			}
		}
		return "", false

	case "deferred":
		var f struct {
			FindingID             string `json:"finding_id"`
			OriginallyRequestedIn struct {
				TurnID            string `json:"turn_id"`
				PromptExcerpt     string `json:"prompt_excerpt"`
				PromptExcerptHash string `json:"prompt_excerpt_hash"`
			} `json:"originally_requested_in"`
			CurrentState struct {
				File      string    `json:"file"`
				LineRange lineRange `json:"line_range"`
			} `json:"current_state"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return "malformed_deferred", true
		}
		if reason, drop := validateCitedTurn(turnsByID, f.OriginallyRequestedIn.TurnID, f.OriginallyRequestedIn.PromptExcerpt, f.OriginallyRequestedIn.PromptExcerptHash); drop {
			return reason, true
		}
		if reason, drop := validateRegionInDiff(fileRegion{File: f.CurrentState.File, Lines: []lineRange{f.CurrentState.LineRange}}, changedRegions); drop {
			return reason, true
		}
		return "", false

	case "unrequested":
		var f struct {
			FindingID string `json:"finding_id"`
			Delivered struct {
				File      string    `json:"file"`
				LineRange lineRange `json:"line_range"`
			} `json:"delivered"`
			CapturedIntentSearch struct {
				PromptsConsidered int `json:"prompts_considered"`
			} `json:"captured_intent_search"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return "malformed_unrequested", true
		}
		// Unrequested findings must account for every visible prompt.
		if f.CapturedIntentSearch.PromptsConsidered != capturedPromptCount {
			return "prompt_count_mismatch", true
		}
		if reason, drop := validateRegionInDiff(fileRegion{File: f.Delivered.File, Lines: []lineRange{f.Delivered.LineRange}}, changedRegions); drop {
			return reason, true
		}
		return "", false
	}

	return "unknown_kind", true
}

// lineRange is the [start,end] integer pair the schema uses. JSON
// decodes a two-element array directly into this type.
type lineRange [2]int

// fileRegion mirrors the schema's fileRegion shape.
type fileRegion struct {
	File  string      `json:"file"`
	Lines []lineRange `json:"lines"`
}

// validateCitedTurn verifies a prompt citation against captured data.
func validateCitedTurn(turnsByID map[string]BundleTurn, turnID, excerpt, excerptHash string) (string, bool) {
	turn, ok := turnsByID[turnID]
	if !ok {
		return "unknown_turn_id", true
	}
	if excerpt != turn.PromptExcerpt {
		return "prompt_excerpt_mismatch", true
	}
	if excerptHash != turn.PromptExcerptHash {
		return "prompt_hash_mismatch", true
	}
	return "", false
}

// validateRegionInDiff verifies that each cited range intersects the PR diff.
func validateRegionInDiff(fr fileRegion, changedRegions map[string][]lineRange) (string, bool) {
	if fr.File == "" {
		return "missing_file_citation", true
	}
	regions, ok := changedRegions[fr.File]
	if !ok {
		return "file_not_in_diff", true
	}
	// Keep the range requirement enforced even if the schema changes.
	if len(fr.Lines) == 0 {
		return "line_range_missing", true
	}
	for _, lr := range fr.Lines {
		if lr[0] == 0 && lr[1] == 0 {
			// A zero range cannot identify source evidence.
			return "line_range_missing", true
		}
		if lr[0] < 1 || lr[1] < lr[0] {
			return "line_range_invalid", true
		}
		if !lineRangeIntersects(lr, regions) {
			return "line_range_outside_diff", true
		}
	}
	return "", false
}

// lineRangeIntersects reports whether [lr.Start, lr.End] overlaps any
// region in regions. Used by the diff-intersection check.
func lineRangeIntersects(lr lineRange, regions []lineRange) bool {
	for _, r := range regions {
		if lr[0] <= r[1] && lr[1] >= r[0] {
			return true
		}
	}
	return false
}

// deriveFindingIDFromAnchors returns the schema-formatted hash prefix for
// repository, pull request, kind, and kind-specific anchors.
func deriveFindingIDFromAnchors(parts ...string) string {
	joined := strings.Join(parts, "|")
	sum := sha256.Sum256([]byte(joined))
	return "f_" + hex.EncodeToString(sum[:])[:16]
}

// canonicalFindingIDForUnderImpl includes the cited prompt and the complete,
// order-independent set of observed regions.
func canonicalFindingIDForUnderImpl(repoID string, prNumber int32, turnID, excerptHash string, regions []fileRegion) string {
	return deriveFindingIDFromAnchors(
		repoID,
		fmt.Sprintf("%d", prNumber),
		"under_impl",
		turnID,
		excerptHash,
		canonicalRegionsString(regions),
	)
}

// canonicalRegionsString merges duplicate files and ranges, sorts the
// normalized values, and encodes them as deterministic JSON.
func canonicalRegionsString(regions []fileRegion) string {
	if len(regions) == 0 {
		return ""
	}
	byFile := map[string]map[[2]int]struct{}{}
	for _, r := range regions {
		if r.File == "" {
			continue
		}
		set, ok := byFile[r.File]
		if !ok {
			set = map[[2]int]struct{}{}
			byFile[r.File] = set
		}
		for _, lr := range r.Lines {
			set[[2]int{lr[0], lr[1]}] = struct{}{}
		}
	}
	if len(byFile) == 0 {
		return ""
	}

	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)

	type canonicalEntry struct {
		File  string  `json:"file"`
		Lines [][]int `json:"lines"`
	}
	out := make([]canonicalEntry, 0, len(files))
	for _, f := range files {
		set := byFile[f]
		ranges := make([][]int, 0, len(set))
		for lr := range set {
			ranges = append(ranges, []int{lr[0], lr[1]})
		}
		sort.Slice(ranges, func(a, b int) bool {
			if ranges[a][0] != ranges[b][0] {
				return ranges[a][0] < ranges[b][0]
			}
			return ranges[a][1] < ranges[b][1]
		})
		out = append(out, canonicalEntry{File: f, Lines: ranges})
	}
	// A sorted slice of fixed-shape structs has deterministic JSON output.
	buf, err := json.Marshal(out)
	if err != nil {
		// Preserve a non-empty anchor if encoding fails unexpectedly.
		return fmt.Sprintf("err:%d", len(regions))
	}
	return string(buf)
}

// canonicalFindingIDForDeferred includes the cited prompt and current location.
func canonicalFindingIDForDeferred(repoID string, prNumber int32, turnID, excerptHash, currentFile string, currentLineStart, currentLineEnd int) string {
	return deriveFindingIDFromAnchors(
		repoID,
		fmt.Sprintf("%d", prNumber),
		"deferred",
		turnID,
		excerptHash,
		currentFile,
		fmt.Sprintf("%d-%d", currentLineStart, currentLineEnd),
	)
}

// canonicalFindingIDForUnrequested uses the delivered diff location.
func canonicalFindingIDForUnrequested(repoID string, prNumber int32, file string, lineStart, lineEnd int) string {
	return deriveFindingIDFromAnchors(repoID, fmt.Sprintf("%d", prNumber), "unrequested", file, fmt.Sprintf("%d-%d", lineStart, lineEnd))
}

// stampFindingIDs replaces model-supplied IDs with canonical IDs. Unknown
// kinds pass through for the subsequent schema validation step.
func stampFindingIDs(findings json.RawMessage, repoID string, prNumber int32) (json.RawMessage, error) {
	if len(findings) == 0 {
		return findings, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(findings, &arr); err != nil {
		return nil, fmt.Errorf("stamp: findings not a JSON array: %w", err)
	}
	out := make([]json.RawMessage, 0, len(arr))
	for _, raw := range arr {
		stamped, err := stampOneFindingID(raw, repoID, prNumber)
		if err != nil {
			return nil, err
		}
		out = append(out, stamped)
	}
	return json.Marshal(out)
}

// stampOneFindingID writes a canonical ID from the anchors available before
// schema validation. Missing anchors are rejected by later validation.
func stampOneFindingID(raw json.RawMessage, repoID string, prNumber int32) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("stamp: unmarshal object: %w", err)
	}

	var kindProbe struct {
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(raw, &kindProbe)

	var canonicalID string
	switch kindProbe.Kind {
	case "under_impl":
		var f struct {
			ExpectedIntent struct {
				TurnID            string `json:"turn_id"`
				PromptExcerptHash string `json:"prompt_excerpt_hash"`
			} `json:"expected_intent"`
			ObservedDiffEvidence struct {
				AIAuthoredRegionsChecked []fileRegion `json:"ai_authored_regions_checked"`
			} `json:"observed_diff_evidence"`
		}
		_ = json.Unmarshal(raw, &f)
		canonicalID = canonicalFindingIDForUnderImpl(
			repoID, prNumber,
			f.ExpectedIntent.TurnID, f.ExpectedIntent.PromptExcerptHash,
			f.ObservedDiffEvidence.AIAuthoredRegionsChecked,
		)
	case "deferred":
		var f struct {
			OriginallyRequestedIn struct {
				TurnID            string `json:"turn_id"`
				PromptExcerptHash string `json:"prompt_excerpt_hash"`
			} `json:"originally_requested_in"`
			CurrentState struct {
				File      string    `json:"file"`
				LineRange lineRange `json:"line_range"`
			} `json:"current_state"`
		}
		_ = json.Unmarshal(raw, &f)
		canonicalID = canonicalFindingIDForDeferred(
			repoID, prNumber,
			f.OriginallyRequestedIn.TurnID, f.OriginallyRequestedIn.PromptExcerptHash,
			f.CurrentState.File, f.CurrentState.LineRange[0], f.CurrentState.LineRange[1],
		)
	case "unrequested":
		var f struct {
			Delivered struct {
				File      string    `json:"file"`
				LineRange lineRange `json:"line_range"`
			} `json:"delivered"`
		}
		_ = json.Unmarshal(raw, &f)
		canonicalID = canonicalFindingIDForUnrequested(repoID, prNumber, f.Delivered.File, f.Delivered.LineRange[0], f.Delivered.LineRange[1])
	default:
		return raw, nil
	}

	idBytes, _ := json.Marshal(canonicalID)
	obj["finding_id"] = idBytes
	return json.Marshal(obj)
}

// hunkHeader matches the @@ -a,b +c,d @@ line that introduces each
// diff hunk. Group 1 is the new-file start line, group 2 (optional)
// is the count; with no count, the count is 1 per git's convention.
var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// diffFileHeader matches the per-file +++ b/<path> line so we know
// which file the following hunks describe. "--- a/<path>" headers
// are ignored because additions and modifications are surfaced via
// the +++ line's path.
var diffFileHeader = regexp.MustCompile(`^\+\+\+ (?:b/)?(.+)$`)

// parseChangedRegions extracts the new-side line ranges per file from a
// unified diff. The cite-or-drop filter uses this to reject findings
// that cite files or line ranges outside the PR's changes.
func parseChangedRegions(diff []byte) map[string][]lineRange {
	out := map[string][]lineRange{}
	if len(diff) == 0 {
		return out
	}
	var currentFile string
	for _, line := range strings.Split(string(diff), "\n") {
		if m := diffFileHeader.FindStringSubmatch(line); m != nil {
			currentFile = strings.TrimSpace(m[1])
			if currentFile == "/dev/null" {
				currentFile = ""
			}
			continue
		}
		if currentFile == "" {
			continue
		}
		if m := hunkHeader.FindStringSubmatch(line); m != nil {
			start := atoi(m[1])
			count := 1
			if m[2] != "" {
				count = atoi(m[2])
			}
			if start == 0 || count == 0 {
				continue
			}
			out[currentFile] = append(out[currentFile], lineRange{start, start + count - 1})
		}
	}
	return out
}

// atoi is a stdlib-free integer parser scoped to non-negative
// well-formed inputs from the hunk regex.
func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
