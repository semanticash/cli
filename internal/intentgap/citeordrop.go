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

	// Index turns and actions once for per-finding citation checks.
	turnsByID := make(map[string]BundleTurn, len(bundle.Turns))
	for _, t := range bundle.Turns {
		turnsByID[t.TurnID] = t
	}
	capturedPromptCount := len(bundle.Turns)
	changedRegions := parseChangedRegions(bundle.Diff)
	actionsByID := indexActionsByID(bundle.AgentActions)

	actionsTruncated := bundle.Truncated.AgentActionsDropped > 0
	trajectoriesByActionID := indexTrajectoriesByActionID(DetectEditRevertTrajectories(bundle))

	accepted := make([]json.RawMessage, 0, len(arr))
	for _, raw := range arr {
		dropReason, drop := shouldDropFinding(raw, turnsByID, capturedPromptCount, changedRegions, actionsByID, bundle.AgentActions, actionsTruncated, trajectoriesByActionID)
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
func shouldDropFinding(raw json.RawMessage, turnsByID map[string]BundleTurn, capturedPromptCount int, changedRegions map[string][]lineRange, actionsByID map[string]BundleAgentAction, actions []BundleAgentAction, actionsTruncated bool, trajectoriesByActionID map[string]*TrajectoryCandidate) (string, bool) {
	var probe struct {
		Kind      string `json:"kind"`
		FindingID string `json:"finding_id"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "malformed_json", true
	}

	// Optional action-evidence citations. The validator runs for every
	// kind and verifies the cited action exists, the scope matches the
	// action's recorded file and line range, and negative citations are
	// not present under truncation. Kind-specific rules (e.g. trajectory
	// alignment for deferred) live in the kind branches below.
	if reason, drop := validateOptionalActionCitations(raw, actionsByID, actions, actionsTruncated); drop {
		return reason, true
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
		if reason, drop := validateDeferredTrajectoryCitation(raw, f.CurrentState.File, trajectoriesByActionID); drop {
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

// agentActionCitation is the optional citation an analyzer attaches
// to a finding to anchor it to a captured tool invocation. Cited by
// ActionID; Scope is the file or line range the finding references.
// Validated against the bundle's AgentActions list.
type agentActionCitation struct {
	ActionID string         `json:"action_id"`
	Scope    *citationScope `json:"scope,omitempty"`
}

// noActionCitation is the optional citation a negative finding
// attaches to anchor "no captured action touched this scope" to a
// concrete, bounded region. Negative citations without a scope cannot
// be verified deterministically.
type noActionCitation struct {
	Scope *citationScope `json:"scope"`
}

// citationScope is a file or file+line target. A zero LineRange
// indicates a file-level scope; a non-zero LineRange narrows the
// scope to those lines.
type citationScope struct {
	File      string    `json:"file"`
	LineRange lineRange `json:"line_range"`
}

// validateAgentActionCitation verifies that a positive action
// citation refers to a real captured action whose file path (and
// line range, when known) overlap the cited scope. The action's
// own fields may carry partial data: a missing FilePath or zero
// LineRange means the bundle doesn't have that precision, and the
// validator falls back to less strict matching.
func validateAgentActionCitation(cit agentActionCitation, actionsByID map[string]BundleAgentAction) (string, bool) {
	if cit.ActionID == "" {
		return "missing_action_id", true
	}
	a, ok := actionsByID[cit.ActionID]
	if !ok {
		return "unknown_action_id", true
	}
	if cit.Scope == nil {
		return "", false
	}
	if cit.Scope.File == "" {
		return "scope_missing_file", true
	}
	// FilePath is best-effort. If the action recorded a path,
	// require an exact match; if not, the scope file is uncheckable
	// here and the validator falls back to the other finding anchors.
	if a.FilePath != "" && a.FilePath != cit.Scope.File {
		return "action_file_mismatch", true
	}
	// Line-range check is gated on both the action and the citation
	// carrying ranges. Zero on either side means "not asserted at this
	// granularity," which is a documented fallback.
	if a.LineRangeStart > 0 && a.LineRangeEnd > 0 &&
		cit.Scope.LineRange[0] > 0 && cit.Scope.LineRange[1] > 0 {
		if !lineRangeIntersects(cit.Scope.LineRange, []lineRange{{a.LineRangeStart, a.LineRangeEnd}}) {
			return "action_line_range_mismatch", true
		}
	}
	return "", false
}

// validateNoActionCitation verifies that a negative citation's
// resolved scope has empty intersection with the bundle's actions.
// The scope must be concrete (file required; line range optional).
//
// When the citation's scope is file-level, any action recorded on
// that file invalidates the negative citation. When the citation narrows
// to a line range, the action must overlap on lines too.
//
// Two best-effort fallbacks are handled conservatively, because the
// validator must be able to *prove* non-overlap before accepting:
//
//   - Actions with an unknown FilePath (e.g. a Bash invocation whose
//     command did not parse into a concrete path) might have touched
//     the cited file. They block any file-scoped negative.
//   - Actions whose own line range is unknown might overlap the cited
//     lines. They block line-narrowed negatives on the same file.
//
// Both rules reject negative citations when available action data is too
// coarse to prove non-overlap.
func validateNoActionCitation(cit noActionCitation, actions []BundleAgentAction) (string, bool) {
	if cit.Scope == nil || cit.Scope.File == "" {
		return "negative_citation_requires_scope", true
	}
	fileOnly := cit.Scope.LineRange[0] == 0 && cit.Scope.LineRange[1] == 0
	for _, a := range actions {
		// Unknown FilePath: cannot prove the action did not touch the
		// cited file, so the negative cannot be verified.
		if a.FilePath == "" {
			return "action_touched_negative_scope", true
		}
		if a.FilePath != cit.Scope.File {
			continue
		}
		if fileOnly {
			return "action_touched_negative_scope", true
		}
		if a.LineRangeStart == 0 || a.LineRangeEnd == 0 {
			// Unknown line range: cannot prove non-overlap with the
			// cited lines.
			return "action_touched_negative_scope", true
		}
		if lineRangeIntersects(cit.Scope.LineRange, []lineRange{{a.LineRangeStart, a.LineRangeEnd}}) {
			return "action_touched_negative_scope", true
		}
	}
	return "", false
}

// indexActionsByID returns the bundle actions keyed by ActionID for
// O(1) positive-citation lookups.
func indexActionsByID(actions []BundleAgentAction) map[string]BundleAgentAction {
	out := make(map[string]BundleAgentAction, len(actions))
	for _, a := range actions {
		out[a.ActionID] = a
	}
	return out
}

// indexTrajectoriesByActionID maps every ActionID participating in a
// detected revert trajectory back to the candidate it belongs to. The
// pointer is shared across IDs from the same candidate so consumers
// can read the trajectory's file and line range alongside the
// membership check.
func indexTrajectoriesByActionID(trajectories []TrajectoryCandidate) map[string]*TrajectoryCandidate {
	out := map[string]*TrajectoryCandidate{}
	for i := range trajectories {
		c := &trajectories[i]
		for _, id := range c.ActionIDs {
			out[id] = c
		}
	}
	return out
}

// validateDeferredTrajectoryCitation runs after the deferred finding's
// turn and diff citations have validated. It is a no-op when the
// finding omits agent_action_citation. When the citation is present:
//
//   - The cited action must belong to a detected revert trajectory.
//   - The trajectory's file must match the finding's current_state
//     file. Without this bound, a reverted edit elsewhere in the PR
//     could validate any deferred finding regardless of where the
//     finding says the deferred work would live.
//
// Line-range alignment is intentionally not enforced. A deferred
// finding's current_state typically references surviving code in the
// diff (which is where validateRegionInDiff confirms it landed), while
// the trajectory range covers reverted lines that are not in the diff
// by construction. These ranges naturally differ; requiring overlap
// would make the rule unsatisfiable in the realistic case.
func validateDeferredTrajectoryCitation(raw json.RawMessage, currentFile string, trajectoriesByActionID map[string]*TrajectoryCandidate) (string, bool) {
	var probe struct {
		Cit *agentActionCitation `json:"agent_action_citation"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "", false
	}
	if probe.Cit == nil || probe.Cit.ActionID == "" {
		return "", false
	}
	cand, ok := trajectoriesByActionID[probe.Cit.ActionID]
	if !ok {
		return "deferred_action_not_in_trajectory", true
	}
	if cand.File != currentFile {
		return "deferred_trajectory_scope_mismatch", true
	}
	return "", false
}

// validateOptionalActionCitations inspects a finding for the optional
// agent_action_citation and no_action_citation fields and runs the
// corresponding validators when present. Findings that omit both
// fields pass through unchanged, preserving the existing behavior
// for producers that have not yet adopted the new citation shape.
//
// The lookup is two-phase. First the raw JSON is probed for field
// presence; only then is each present field decoded into its typed
// shape. This separation lets the validator distinguish "field
// absent" from "field present but malformed," so a producer that
// emits agent_action_citation as the wrong JSON type (string, array,
// number, etc.) is rejected explicitly instead of passing
// through as if the field were omitted. That preserves the
// invariant: if action citation fields appear, they are verified or
// dropped.
func validateOptionalActionCitations(raw json.RawMessage, actionsByID map[string]BundleAgentAction, actions []BundleAgentAction, actionsTruncated bool) (string, bool) {
	var presence struct {
		AgentActionCitation json.RawMessage `json:"agent_action_citation"`
		NoActionCitation    json.RawMessage `json:"no_action_citation"`
	}
	if err := json.Unmarshal(raw, &presence); err != nil {
		// The kind-specific parsers below produce a precise reason
		// for malformed findings; treat this layer as no-op on parse
		// failure so the error surfaces from the right location.
		return "", false
	}

	if isJSONFieldPresent(presence.AgentActionCitation) {
		var cit agentActionCitation
		if err := json.Unmarshal(presence.AgentActionCitation, &cit); err != nil {
			return "malformed_action_citation", true
		}
		if reason, drop := validateAgentActionCitation(cit, actionsByID); drop {
			return reason, true
		}
	}
	if isJSONFieldPresent(presence.NoActionCitation) {
		// A truncated action list cannot prove non-overlap. An older
		// action that was dropped at the size cap might have touched
		// the cited scope, so the negative citation is unverifiable from
		// the data the validator can see.
		if actionsTruncated {
			return "actions_truncated_negative_unverifiable", true
		}
		var cit noActionCitation
		if err := json.Unmarshal(presence.NoActionCitation, &cit); err != nil {
			return "malformed_no_action_citation", true
		}
		if reason, drop := validateNoActionCitation(cit, actions); drop {
			return reason, true
		}
	}
	return "", false
}

// isJSONFieldPresent reports whether a raw-message field is set to a
// non-null value. Absent fields decode to a nil slice; explicit JSON
// nulls decode to the four-byte literal `null`. Both forms are
// treated as "omitted" so a producer can clear a citation by sending
// null without paying a drop penalty.
func isJSONFieldPresent(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	return string(raw) != "null"
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
