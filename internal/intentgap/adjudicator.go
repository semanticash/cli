package intentgap

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// TrackADiagnostic is one coverage-only Track A diagnostic.
type TrackADiagnostic struct {
	IntentID string `json:"intent_id"`
	TurnID   string `json:"turn_id"`
	Summary  string `json:"summary"`
}

// AdjudicatorInput contains the data needed to render final findings.
type AdjudicatorInput struct {
	VerifierResults []VerifierResult
	CandidatesByID  map[string]Candidate
	IntentsByID     map[string]IntentItem
	Bundle          Bundle
	RepositoryID    string
	PRNumber        int32
}

// AdjudicatorResult contains rendered findings and coverage counters.
//
// ActionCitationsDiscarded counts supporting_action_ids beyond the
// first per accepted candidate. The count is taken before cite-or-drop
// and schema filtering.
//
// UnanchoredAccepts counts Track B verifier accepts that arrived
// without cited regions.
type AdjudicatorResult struct {
	Findings                 json.RawMessage
	FindingsCount            int
	TrackADiagnostics        []TrackADiagnostic
	TrackACandidatesAccepted int
	DedupDropped             int
	CiteOrDropDropped        int
	ActionCitationsDiscarded int
	UnanchoredAccepts        int
}

// RunAdjudicator routes accepted verifier results, dedupes Track B,
// renders under_impl findings, and applies cite-or-drop plus schema
// filtering.
func RunAdjudicator(in AdjudicatorInput) AdjudicatorResult {
	out := AdjudicatorResult{Findings: json.RawMessage("[]")}

	type acceptedTrackB struct {
		result VerifierResult
		cand   Candidate
		intent IntentItem
	}
	var trackB []acceptedTrackB

	for _, r := range in.VerifierResults {
		if r.Verdict != VerdictAccept {
			continue
		}
		cand, ok := in.CandidatesByID[r.CandidateID]
		if !ok {
			continue
		}
		intent, ok := in.IntentsByID[cand.IntentID]
		if !ok {
			continue
		}
		switch cand.Kind {
		case CandUnderImplNoRetrievedScope:
			out.TrackACandidatesAccepted++
			out.TrackADiagnostics = append(out.TrackADiagnostics, TrackADiagnostic{
				IntentID: intent.ID,
				TurnID:   intent.TurnID,
				Summary:  intent.Summary,
			})
		case CandUnderImplPartialScope:
			if r.Acceptance == nil || len(r.Acceptance.Regions) == 0 {
				// Defensive: verifier validation should have dropped
				// this before adjudication.
				out.UnanchoredAccepts++
				continue
			}
			trackB = append(trackB, acceptedTrackB{result: r, cand: cand, intent: intent})
		}
	}

	if len(trackB) == 0 {
		return out
	}

	// Dedup by normalized tuple; higher candidate Score wins.
	type dedupKey struct {
		intentID      string
		primaryFile   string
		spanStart     int
		spanEnd       int
		candidateKind CandidateKind
	}
	bestByKey := map[dedupKey]int{} // -> index into trackB
	for i, b := range trackB {
		primary := normalizeFilePath(b.result.Acceptance.PrimaryFile)
		span := normalizeLineSpan(b.result.Acceptance.Regions)
		key := dedupKey{
			intentID:      b.intent.ID,
			primaryFile:   primary,
			spanStart:     span[0],
			spanEnd:       span[1],
			candidateKind: b.cand.Kind,
		}
		if existing, ok := bestByKey[key]; ok {
			if b.cand.Score > trackB[existing].cand.Score {
				bestByKey[key] = i
				out.DedupDropped++
			} else {
				out.DedupDropped++
			}
			continue
		}
		bestByKey[key] = i
	}

	// Walk trackB in input order; emit each surviving acceptance.
	kept := make([]acceptedTrackB, 0, len(bestByKey))
	keptIndexes := make(map[int]bool, len(bestByKey))
	for _, idx := range bestByKey {
		keptIndexes[idx] = true
	}
	for i, b := range trackB {
		if keptIndexes[i] {
			kept = append(kept, b)
		}
	}

	// Render accepted candidates. ActionCitationsDiscarded is counted
	// before later cite-or-drop/schema filtering.
	rendered := make([]json.RawMessage, 0, len(kept))
	for _, b := range kept {
		body, discarded := renderUnderImpl(b.result, b.intent)
		out.ActionCitationsDiscarded += discarded
		rendered = append(rendered, body)
	}
	arr, err := assembleFindingsArray(rendered)
	if err != nil {
		return out
	}

	// Reuse the existing final validation gates.
	citeFilter, err := FilterFindingsByCitations(arr, in.Bundle)
	if err != nil {
		return out
	}
	if citeFilter.DroppedCount > 0 {
		out.CiteOrDropDropped += citeFilter.DroppedCount
	}
	schemaFilter := FilterFindingsBySchema(citeFilter.Findings)
	if schemaFilter.ArrayErr != nil {
		return out
	}
	if schemaFilter.DroppedCount > 0 {
		out.CiteOrDropDropped += schemaFilter.DroppedCount
	}

	// Stamp finding_id for each surviving finding.
	stamped, err := stampUnderImplFindingIDs(schemaFilter.Kept, in.RepositoryID, in.PRNumber)
	if err != nil {
		return out
	}
	out.Findings = stamped
	out.FindingsCount = countFindings(stamped)
	return out
}

// renderUnderImpl renders one Track B acceptance as under_impl JSON.
func renderUnderImpl(r VerifierResult, intent IntentItem) (json.RawMessage, int) {
	regions := regionsToFileRegions(r.Acceptance.Regions)
	body := map[string]any{
		"schema_version": "1",
		// Required by schema before canonical stamping.
		"finding_id": "f_0000000000000000",
		"kind":       "under_impl",
		"title":      truncateRunes(intent.Summary, maxUnderImplTitleRunes),
		"confidence": "medium",
		"expected_intent": map[string]any{
			"summary":             intent.Summary,
			"turn_id":             intent.TurnID,
			"prompt_excerpt":      intent.Excerpt,
			"prompt_excerpt_hash": intent.ExcerptHash,
		},
		"observed_diff_evidence": map[string]any{
			"summary":                     renderObservedSummary(regions),
			"ai_authored_regions_checked": regions,
		},
		"missing_or_partial_area": map[string]any{
			"note": r.Rationale,
		},
	}

	discarded := 0
	if r.Acceptance != nil && len(r.Acceptance.SupportingActionIDs) > 0 {
		body["agent_action_citation"] = map[string]any{
			"action_id": r.Acceptance.SupportingActionIDs[0],
		}
		if len(r.Acceptance.SupportingActionIDs) > 1 {
			discarded = len(r.Acceptance.SupportingActionIDs) - 1
		}
	}
	encoded, _ := json.Marshal(body)
	return encoded, discarded
}

// maxUnderImplTitleRunes caps title length for V1 output.
const maxUnderImplTitleRunes = 120

// regionsToFileRegions groups HunkRefs by file.
func regionsToFileRegions(refs []HunkRef) []fileRegion {
	byFile := map[string][]lineRange{}
	var order []string
	for _, r := range refs {
		if _, ok := byFile[r.File]; !ok {
			order = append(order, r.File)
		}
		byFile[r.File] = append(byFile[r.File], lineRange{r.StartLine, r.EndLine})
	}
	out := make([]fileRegion, 0, len(order))
	for _, f := range order {
		out = append(out, fileRegion{File: f, Lines: byFile[f]})
	}
	return out
}

// renderObservedSummary summarizes cited regions deterministically.
func renderObservedSummary(regions []fileRegion) string {
	if len(regions) == 0 {
		return "no diff regions cited"
	}
	var b strings.Builder
	b.WriteString("cited regions:")
	for _, fr := range regions {
		for _, lr := range fr.Lines {
			fmt.Fprintf(&b, " %s:%d-%d", fr.File, lr[0], lr[1])
		}
	}
	return b.String()
}

// normalizeFilePath normalizes paths for dedup keys.
func normalizeFilePath(p string) string {
	return strings.ToLower(strings.ReplaceAll(p, "\\", "/"))
}

// normalizeLineSpan rounds the covered span outward to a 5-line grid.
func normalizeLineSpan(regions []HunkRef) [2]int {
	if len(regions) == 0 {
		return [2]int{0, 0}
	}
	minStart, maxEnd := regions[0].StartLine, regions[0].EndLine
	for _, r := range regions[1:] {
		if r.StartLine < minStart {
			minStart = r.StartLine
		}
		if r.EndLine > maxEnd {
			maxEnd = r.EndLine
		}
	}
	roundedStart := (minStart / 5) * 5
	roundedEnd := ((maxEnd + 4) / 5) * 5
	return [2]int{roundedStart, roundedEnd}
}

// assembleFindingsArray joins finding objects into one JSON array.
func assembleFindingsArray(bodies []json.RawMessage) (json.RawMessage, error) {
	if len(bodies) == 0 {
		return json.RawMessage("[]"), nil
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, body := range bodies {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(body)
	}
	b.WriteByte(']')
	out := []byte(b.String())
	if !json.Valid(out) {
		return nil, fmt.Errorf("assembled findings array is not valid JSON")
	}
	return out, nil
}

// stampUnderImplFindingIDs rewrites finding_id with the canonical
// under_impl derivation. Inputs have already passed cite-or-drop and
// schema validation.
func stampUnderImplFindingIDs(arr json.RawMessage, repoID string, prNumber int32) (json.RawMessage, error) {
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(arr, &items); err != nil {
		return nil, fmt.Errorf("stamp: parse array: %w", err)
	}
	for i := range items {
		var turn struct {
			TurnID      string `json:"turn_id"`
			ExcerptHash string `json:"prompt_excerpt_hash"`
		}
		if raw, ok := items[i]["expected_intent"]; ok {
			_ = json.Unmarshal(raw, &turn)
		}
		var observed struct {
			Regions []fileRegion `json:"ai_authored_regions_checked"`
		}
		if raw, ok := items[i]["observed_diff_evidence"]; ok {
			_ = json.Unmarshal(raw, &observed)
		}
		id := canonicalFindingIDForUnderImpl(repoID, prNumber, turn.TurnID, turn.ExcerptHash, observed.Regions)
		idBytes, _ := json.Marshal(id)
		items[i]["finding_id"] = idBytes
	}
	// Stable key order keeps output byte-identical across runs.
	out, err := encodeFindingsArrayStable(items)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// encodeFindingsArrayStable serializes objects with sorted keys.
func encodeFindingsArrayStable(items []map[string]json.RawMessage) (json.RawMessage, error) {
	var b strings.Builder
	b.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		keys := make([]string, 0, len(item))
		for k := range item {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for j, k := range keys {
			if j > 0 {
				b.WriteByte(',')
			}
			keyBytes, _ := json.Marshal(k)
			b.Write(keyBytes)
			b.WriteByte(':')
			b.Write(item[k])
		}
		b.WriteByte('}')
	}
	b.WriteByte(']')
	out := []byte(b.String())
	if !json.Valid(out) {
		return nil, fmt.Errorf("stamped findings array is not valid JSON")
	}
	return out, nil
}

// countFindings reports how many objects the stamped findings array
// contains. Used by the adjudicator's result counter so the
// orchestrator does not have to re-parse the JSON.
func countFindings(arr json.RawMessage) int {
	var items []json.RawMessage
	if err := json.Unmarshal(arr, &items); err != nil {
		return 0
	}
	return len(items)
}
