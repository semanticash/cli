package intentgap

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/semanticash/cli/internal/llm"
)

// VerifierVerdict is the model's decision for one candidate.
type VerifierVerdict string

const (
	VerdictAccept           VerifierVerdict = "accept"
	VerdictDrop             VerifierVerdict = "drop"
	VerdictNeedsMoreContext VerifierVerdict = "needs_more_context"
)

// Verifier drop reason codes. Model-provided and verifier-derived
// drops share this namespace for coverage reporting.
const (
	DropIntentAlreadyDelivered   = "intent_already_delivered"
	DropIntentNotARequest        = "intent_not_actually_a_request"
	DropDiffEvidenceUnrelated    = "diff_evidence_unrelated"
	DropCannotAnchorWithEvidence = "cannot_anchor_with_evidence"
	DropIntentTooVague           = "intent_too_vague"
	DropVerifierInvalidShape     = "verifier_invalid_shape"
	DropVerifierCallFailed       = "verifier_call_failed"
	DropAcceptedNoRegions        = "accepted_no_regions" // Track B accept with empty regions
	DropAcceptedNoPrimaryFile    = "accepted_no_primary_file"
	DropPrimaryFileNotInRegions  = "accepted_primary_file_not_in_regions"
)

// modelDropReasons is the subset the prompt allows the model to emit.
var modelDropReasons = map[string]bool{
	DropIntentAlreadyDelivered:   true,
	DropIntentNotARequest:        true,
	DropDiffEvidenceUnrelated:    true,
	DropCannotAnchorWithEvidence: true,
	DropIntentTooVague:           true,
}

// AcceptedScope is the evidence packet the adjudicator turns into an
// under_impl finding or Track A diagnostic.
type AcceptedScope struct {
	PrimaryFile         string
	Regions             []HunkRef
	SupportingActionIDs []string
}

// VerifierResult is the structured outcome of one verifier call.
type VerifierResult struct {
	CandidateID string
	Verdict     VerifierVerdict
	Rationale   string
	DropReason  string
	Acceptance  *AcceptedScope
}

// maxVerifierRationaleRunes bounds rationale text on a rune boundary.
const maxVerifierRationaleRunes = 400

// ScopedVerifierRunner is the LLM interface used by the verifier.
type ScopedVerifierRunner interface {
	GenerateText(ctx context.Context, prompt string) (*llm.GenerateTextResult, error)
}

// VerifierInput contains the data needed for one verifier call.
type VerifierInput struct {
	Candidate Candidate
	Intent    IntentItem
	Change    ChangeLedger
	Action    ActionLedger
	Bundle    Bundle
}

// RunScopedVerifier performs one verifier LLM call. Failures return a
// typed drop result so coverage still accounts for the candidate.
func RunScopedVerifier(ctx context.Context, runner ScopedVerifierRunner, in VerifierInput) VerifierResult {
	prompt := renderVerifierPrompt(in)
	res, err := runner.GenerateText(ctx, prompt)
	if err != nil || res == nil {
		return VerifierResult{
			CandidateID: in.Candidate.ID,
			Verdict:     VerdictDrop,
			DropReason:  DropVerifierCallFailed,
			Rationale:   verifierFailureRationale(err),
		}
	}
	return parseVerifierResponse(in.Candidate, res.Text)
}

// renderVerifierPrompt builds the per-candidate prompt.
func renderVerifierPrompt(in VerifierInput) string {
	c := in.Candidate
	intent := in.Intent

	var b strings.Builder
	b.WriteString("You are verifying ONE intent-gap candidate.\n\n")
	fmt.Fprintf(&b, "Candidate kind: %s\n", c.Kind)
	fmt.Fprintf(&b, "Mechanical reason: %s\n\n", c.Reason)

	b.WriteString("Captured intent:\n")
	fmt.Fprintf(&b, "  turn_id: %s\n", intent.TurnID)
	fmt.Fprintf(&b, "  kind: %s\n", intent.Kind)
	fmt.Fprintf(&b, "  summary: %s\n", intent.Summary)
	fmt.Fprintf(&b, "  excerpt: %s\n", intent.Excerpt)

	switch c.Kind {
	case CandUnderImplPartialScope:
		b.WriteString("\nDiff hunks (cite their file + line range when you accept):\n")
		if len(c.DiffPointers) == 0 {
			b.WriteString("  (none retrieved)\n")
		}
		for _, p := range c.DiffPointers {
			fmt.Fprintf(&b, "- %s:%d-%d\n", p.File, p.StartLine, p.EndLine)
			body := lookupHunkBody(in.Change, p)
			if body != "" {
				b.WriteString(indent(body, "    "))
				if !strings.HasSuffix(body, "\n") {
					b.WriteByte('\n')
				}
			}
		}
		if c.MissingCategory != "" {
			fmt.Fprintf(&b, "\nMissing category in scope: %s\n", c.MissingCategory)
		}

	case CandUnderImplNoRetrievedScope:
		b.WriteString("\nNo diff hunks aligned with this intent. Near-miss files\n")
		b.WriteString("(below the retrieval score threshold):\n")
		if len(c.NearMisses) == 0 {
			b.WriteString("  (none)\n")
		}
		for _, p := range c.NearMisses {
			cat := categoryOf(in.Change, p)
			hunks := len(hunksOf(in.Change, p))
			fmt.Fprintf(&b, "- path=%s category=%s changed_hunks=%d\n", p, cat, hunks)
		}
		b.WriteString("\nCommit subjects in this PR (text only; no per-commit file lists\n")
		b.WriteString("available in this bundle):\n")
		written := 0
		const maxSubjects = 5
		for _, commit := range in.Bundle.Commits {
			if written >= maxSubjects {
				break
			}
			subj := strings.TrimSpace(commit.Subject)
			if subj == "" {
				continue
			}
			fmt.Fprintf(&b, "- %s\n", subj)
			written++
		}
		if written == 0 {
			b.WriteString("  (none captured)\n")
		}
	}

	b.WriteString("\nCaptured agent actions (cite their action_ids):\n")
	if len(c.ActionIDs) == 0 {
		b.WriteString("  (none in scope)\n")
	}
	for _, id := range c.ActionIDs {
		a, ok := in.Action.ByID[id]
		if !ok {
			fmt.Fprintf(&b, "- action_id=%s (details unavailable)\n", id)
			continue
		}
		fmt.Fprintf(&b, "- action_id=%s tool=%s file=%s", id, a.ToolName, displayPath(a.FilePath))
		if a.LineRangeStart > 0 && a.LineRangeEnd > 0 {
			fmt.Fprintf(&b, " lines=%d-%d", a.LineRangeStart, a.LineRangeEnd)
		}
		b.WriteByte('\n')
	}

	b.WriteString("\nDecide ONE of:\n")
	b.WriteString("- accept: this candidate is a real under_impl gap.\n")
	switch c.Kind {
	case CandUnderImplPartialScope:
		b.WriteString("  - Populate acceptance.regions with the diff regions you\n")
		b.WriteString("    actually relied on (file + start + end line).\n")
		b.WriteString("  - acceptance.primary_file must be one of the cited regions'\n")
		b.WriteString("    files.\n")
	case CandUnderImplNoRetrievedScope:
		b.WriteString("  - acceptance.regions is expected to be empty for this kind.\n")
		b.WriteString("    Populate acceptance.primary_file with the file (or\n")
		b.WriteString("    near-miss path) the intent most plausibly referenced.\n")
		b.WriteString("    Do not invent regions to satisfy a schema with citations\n")
		b.WriteString("    you cannot anchor; the acceptance is used for diagnostics\n")
		b.WriteString("    in V1, not a final finding.\n")
	}
	b.WriteString("- drop: this candidate is not a real gap. drop_reason must be\n")
	b.WriteString("  one of [intent_already_delivered, intent_not_actually_a_request,\n")
	b.WriteString("  diff_evidence_unrelated, cannot_anchor_with_evidence, intent_too_vague].\n")
	b.WriteString("- needs_more_context: you cannot tell from the evidence above.\n\n")

	b.WriteString("Reply with ONLY a JSON object matching this shape:\n")
	b.WriteString("{\n")
	b.WriteString("  \"verdict\": \"accept\" | \"drop\" | \"needs_more_context\",\n")
	b.WriteString("  \"rationale\": \"<=400 chars\",\n")
	b.WriteString("  \"drop_reason\": \"<enum, only when verdict=drop>\",\n")
	b.WriteString("  \"acceptance\": {                                  // only when verdict=accept\n")
	b.WriteString("    \"primary_file\": \"path\",\n")
	b.WriteString("    \"regions\": [{\"file\":\"path\",\"start\":N,\"end\":N}, ...],\n")
	b.WriteString("    \"supporting_action_ids\": [\"a_...\"]\n")
	b.WriteString("  }\n")
	b.WriteString("}\n")
	b.WriteString("No markdown code fences, no commentary outside the JSON.\n")

	return b.String()
}

// rawVerifierResponse is the unvalidated JSON response shape.
// Acceptance is a pointer so an omitted accept payload is detectable.
// Extra top-level fields are tolerated.
type rawVerifierResponse struct {
	Verdict    string         `json:"verdict"`
	Rationale  string         `json:"rationale"`
	DropReason string         `json:"drop_reason"`
	Acceptance *rawAcceptance `json:"acceptance"`
}

type rawAcceptance struct {
	PrimaryFile         string      `json:"primary_file"`
	Regions             []rawRegion `json:"regions"`
	SupportingActionIDs []string    `json:"supporting_action_ids"`
}

type rawRegion struct {
	File  string `json:"file"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

// parseVerifierResponse decodes the model reply and applies the
// verifier's shape rules:
//
//   - verdict must be one of {accept, drop, needs_more_context}.
//   - drop must include a drop_reason from the documented enum.
//   - accept must include an acceptance object with a non-empty
//     primary_file; Track B accepts additionally require non-empty
//     regions and primary_file to match one of those region files.
//
// Extra top-level fields are tolerated. Malformed responses return a
// typed drop result so coverage can report them.
func parseVerifierResponse(candidate Candidate, text string) VerifierResult {
	fail := func(reason, rationale string) VerifierResult {
		return VerifierResult{
			CandidateID: candidate.ID,
			Verdict:     VerdictDrop,
			DropReason:  reason,
			Rationale:   truncateRationale(rationale),
		}
	}

	trim := strings.TrimSpace(text)
	if trim == "" {
		return fail(DropVerifierInvalidShape, "verifier returned empty body")
	}
	// Accept a top-level object, a fenced object, or an object embedded
	// in prose.
	candidates := []string{trim}
	for _, m := range codeFencePattern.FindAllStringSubmatch(trim, -1) {
		if len(m) >= 2 {
			candidates = append(candidates, strings.TrimSpace(m[1]))
		}
	}
	if start := strings.IndexByte(trim, '{'); start >= 0 {
		if end := strings.LastIndexByte(trim, '}'); end > start {
			candidates = append(candidates, trim[start:end+1])
		}
	}

	var raw rawVerifierResponse
	parsed := false
	for _, c := range candidates {
		if err := json.Unmarshal([]byte(c), &raw); err == nil {
			parsed = true
			break
		}
	}
	if !parsed {
		return fail(DropVerifierInvalidShape, "verifier response is not a JSON object")
	}

	verdict, ok := parseVerifierVerdict(raw.Verdict)
	if !ok {
		return fail(DropVerifierInvalidShape, fmt.Sprintf("unknown verdict %q", raw.Verdict))
	}

	rationale := truncateRationale(raw.Rationale)

	switch verdict {
	case VerdictDrop:
		// Store the normalized reason so coverage uses one bucket.
		dropReason := strings.TrimSpace(raw.DropReason)
		if !modelDropReasons[dropReason] {
			return fail(DropVerifierInvalidShape, fmt.Sprintf("unknown drop_reason %q", raw.DropReason))
		}
		return VerifierResult{
			CandidateID: candidate.ID,
			Verdict:     VerdictDrop,
			DropReason:  dropReason,
			Rationale:   rationale,
		}

	case VerdictNeedsMoreContext:
		return VerifierResult{
			CandidateID: candidate.ID,
			Verdict:     VerdictNeedsMoreContext,
			Rationale:   rationale,
		}

	case VerdictAccept:
		if raw.Acceptance == nil {
			return fail(DropVerifierInvalidShape, "accept missing acceptance object")
		}
		acc := convertAcceptance(raw.Acceptance)
		if acc.PrimaryFile == "" {
			return fail(DropAcceptedNoPrimaryFile, "accept missing primary_file")
		}
		if candidate.Kind == CandUnderImplPartialScope {
			if len(acc.Regions) == 0 {
				return fail(DropAcceptedNoRegions, "Track B accept without cited regions")
			}
			if !primaryFileInRegions(acc.PrimaryFile, acc.Regions) {
				return fail(DropPrimaryFileNotInRegions,
					"primary_file not among cited region files")
			}
		}
		return VerifierResult{
			CandidateID: candidate.ID,
			Verdict:     VerdictAccept,
			Rationale:   rationale,
			Acceptance:  &acc,
		}
	}

	return fail(DropVerifierInvalidShape, "unreachable verdict branch")
}

func parseVerifierVerdict(s string) (VerifierVerdict, bool) {
	switch VerifierVerdict(strings.TrimSpace(strings.ToLower(s))) {
	case VerdictAccept:
		return VerdictAccept, true
	case VerdictDrop:
		return VerdictDrop, true
	case VerdictNeedsMoreContext:
		return VerdictNeedsMoreContext, true
	}
	return "", false
}

func convertAcceptance(raw *rawAcceptance) AcceptedScope {
	out := AcceptedScope{
		PrimaryFile:         strings.TrimSpace(raw.PrimaryFile),
		SupportingActionIDs: append([]string(nil), raw.SupportingActionIDs...),
	}
	for _, r := range raw.Regions {
		file := strings.TrimSpace(r.File)
		if file == "" || r.Start <= 0 || r.End < r.Start {
			// Drop malformed regions; Track B validates that at least
			// one valid region remains.
			continue
		}
		out.Regions = append(out.Regions, HunkRef{File: file, StartLine: r.Start, EndLine: r.End})
	}
	return out
}

// truncateRationale enforces the prompt's rationale cap.
func truncateRationale(s string) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= maxVerifierRationaleRunes {
		return s
	}
	return truncateRunes(s, maxVerifierRationaleRunes)
}

// verifierFailureRationale formats a failed verifier call.
func verifierFailureRationale(err error) string {
	if err == nil {
		return "verifier runner returned nil result with no error"
	}
	return truncateRationale("verifier call failed: " + err.Error())
}

// lookupHunkBody returns the body for an exact hunk reference.
func lookupHunkBody(change ChangeLedger, ref HunkRef) string {
	f := change.ByPath[ref.File]
	if f == nil {
		return ""
	}
	for _, h := range f.Hunks {
		if h.StartLine == ref.StartLine && h.EndLine == ref.EndLine {
			return h.Body
		}
	}
	return ""
}

// indent prefixes each non-empty line with prefix.
func indent(body, prefix string) string {
	if body == "" {
		return ""
	}
	lines := strings.Split(body, "\n")
	var b strings.Builder
	for i, line := range lines {
		if i == len(lines)-1 && line == "" {
			break
		}
		b.WriteString(prefix)
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func categoryOf(change ChangeLedger, path string) FileCategory {
	if f, ok := change.ByPath[path]; ok && f != nil {
		return f.Category
	}
	return ""
}

func hunksOf(change ChangeLedger, path string) []ChangedHunk {
	if f, ok := change.ByPath[path]; ok && f != nil {
		return f.Hunks
	}
	return nil
}

func displayPath(p string) string {
	if p == "" {
		return "(unknown)"
	}
	return p
}

// primaryFileInRegions checks Track B's primary-file invariant.
func primaryFileInRegions(primary string, regions []HunkRef) bool {
	for _, r := range regions {
		if r.File == primary {
			return true
		}
	}
	return false
}
