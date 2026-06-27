package intentgap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/semanticash/cli/internal/llm"
)

// PromptTemplateVersion identifies the prompt used to produce findings.
// Bump it when prompt changes may affect model output.
//
// 0.2.0 introduces the three-anchor evidence model (ask, attempt,
// result), surfaces captured agent tool invocations, and documents the
// optional agent_action_citation and no_action_citation fields.
const PromptTemplateVersion = "0.2.0"

// AlgorithmVersionAnalyzed identifies uploads produced by local LLM analysis.
const AlgorithmVersionAnalyzed = "0.1.0-local-llm"

// AnalysisInput contains the repository, pull request, and local evidence
// used to derive findings. RepositoryID and PRNumber namespace finding IDs.
type AnalysisInput struct {
	Bundle       Bundle
	PRNumber     int32
	RepositoryID string
}

// AnalysisResult contains validated findings, coverage metadata, and the
// provider that produced the response.
//
// SchemaDiagnostics lists structural details for findings rejected by
// schema-or-drop. It is intended for local activity-log output and is
// not uploaded.
type AnalysisResult struct {
	Findings              json.RawMessage
	CoverageSummary       json.RawMessage
	Provider              string
	Model                 string
	PromptTemplateVersion string
	SchemaDiagnostics     []string
}

// IntentGapAnalyzer produces findings for one pull request bundle.
type IntentGapAnalyzer interface {
	Analyze(ctx context.Context, in AnalysisInput) (AnalysisResult, error)
}

// Analyzer errors map to stable upload reason codes.
var (
	// ErrAnalyzerLLMUnavailable wraps the underlying LLM-registry
	// error when no installed writer succeeded.
	ErrAnalyzerLLMUnavailable = errors.New("intentgap: no LLM CLI produced a response")
	// ErrAnalyzerParseFailed signals the LLM responded but neither
	// the original nor the retry response parsed into the expected
	// JSON shape.
	ErrAnalyzerParseFailed = errors.New("intentgap: could not parse findings JSON from LLM output")
	// ErrAnalyzerSchemaFailed signals the LLM output parsed as JSON
	// but failed schema validation.
	ErrAnalyzerSchemaFailed = errors.New("intentgap: LLM findings failed schema validation")
	// ErrAnalyzerInternal wraps unexpected analyzer-side errors
	// (cite-or-drop filter failure, coverage encode failure, etc.)
	// so the reason-code mapping has a stable sentinel to map to.
	ErrAnalyzerInternal = errors.New("intentgap: analyzer internal error")
)

// ReasonCode is a sanitized failure label suitable for upload. Detailed
// errors remain in the local activity log.
type ReasonCode string

const (
	ReasonBundleFailed       ReasonCode = "bundle_failed"
	ReasonLineageUnavailable ReasonCode = "lineage_unavailable"
	ReasonRedactionFailed    ReasonCode = "redaction_failed"
	ReasonLLMUnavailable     ReasonCode = "llm_unavailable"
	ReasonParseFailed        ReasonCode = "parse_failed"
	ReasonSchemaFailed       ReasonCode = "schema_failed"
	ReasonAnalyzerInternal   ReasonCode = "analyzer_internal"
)

// ReasonCodeFor maps an error to a stable upload label.
func ReasonCodeFor(err error) ReasonCode {
	switch {
	case errors.Is(err, ErrLineageUnavailable):
		return ReasonLineageUnavailable
	case errors.Is(err, ErrRedactionFailed):
		return ReasonRedactionFailed
	case errors.Is(err, ErrAnalyzerLLMUnavailable):
		return ReasonLLMUnavailable
	case errors.Is(err, ErrAnalyzerParseFailed):
		return ReasonParseFailed
	case errors.Is(err, ErrAnalyzerSchemaFailed):
		return ReasonSchemaFailed
	case errors.Is(err, ErrAnalyzerInternal):
		return ReasonAnalyzerInternal
	default:
		return ReasonAnalyzerInternal
	}
}

// mergeCoverage adds fields to coverage metadata. It preserves the original
// value if the merge cannot be encoded.
func mergeCoverage(existing json.RawMessage, extras map[string]any) json.RawMessage {
	var into map[string]any
	if err := json.Unmarshal(existing, &into); err != nil {
		return existing
	}
	for k, v := range extras {
		into[k] = v
	}
	out, err := json.Marshal(into)
	if err != nil {
		return existing
	}
	return out
}

// LLMRunner is the analyzer's interface to the local provider registry.
type LLMRunner interface {
	GenerateText(ctx context.Context, prompt string) (*llm.GenerateTextResult, error)
}

// LLMAnalyzer runs the local provider fallback chain and validates its output.
type LLMAnalyzer struct {
	runner LLMRunner
}

// NewLLMAnalyzer constructs an analyzer backed by runner.
func NewLLMAnalyzer(runner LLMRunner) *LLMAnalyzer {
	return &LLMAnalyzer{runner: runner}
}

// Analyze produces validated findings. It retries once when the first
// response cannot be parsed as JSON.
func (a *LLMAnalyzer) Analyze(ctx context.Context, in AnalysisInput) (AnalysisResult, error) {
	if a.runner == nil {
		return AnalysisResult{}, fmt.Errorf("LLMAnalyzer: runner not wired")
	}

	// Without captured prompts, intent-based findings would be unsupported.
	if len(in.Bundle.Turns) == 0 {
		coverage := mergeCoverage(buildCoverageSummary(in.Bundle), map[string]any{
			"skipped":     true,
			"skip_reason": "no_captured_prompts",
		})
		return AnalysisResult{
			Findings:              json.RawMessage("[]"),
			CoverageSummary:       coverage,
			Provider:              "",
			Model:                 "",
			PromptTemplateVersion: PromptTemplateVersion,
		}, nil
	}

	prompt := renderAnalyzerPrompt(in)
	res, err := a.runner.GenerateText(ctx, prompt)
	if err != nil {
		return AnalysisResult{}, fmt.Errorf("%w: %v", ErrAnalyzerLLMUnavailable, err)
	}

	findings, parseErr := extractFindingsArray(res.Text)
	if parseErr != nil {
		// Retry once with a stricter output-format instruction.
		retry, retryErr := a.runner.GenerateText(ctx, reformatPrompt(res.Text))
		if retryErr != nil {
			return AnalysisResult{}, fmt.Errorf("%w (initial parse: %v; retry call: %v)",
				ErrAnalyzerParseFailed, parseErr, retryErr)
		}
		findings, parseErr = extractFindingsArray(retry.Text)
		if parseErr != nil {
			return AnalysisResult{}, fmt.Errorf("%w: %v", ErrAnalyzerParseFailed, parseErr)
		}
		// A retry may succeed through a different provider in the fallback chain.
		res = retry
	}

	// The CLI owns finding IDs, so stamp them before schema validation.
	stamped, stampErr := stampFindingIDs(findings, in.RepositoryID, in.PRNumber)
	if stampErr != nil {
		return AnalysisResult{}, fmt.Errorf("%w: stamp finding_id: %v", ErrAnalyzerInternal, stampErr)
	}
	findings = stamped

	schemaFilter := FilterFindingsBySchema(findings)
	if schemaFilter.ArrayErr != nil {
		return AnalysisResult{}, fmt.Errorf("%w: %v", ErrAnalyzerSchemaFailed, schemaFilter.ArrayErr)
	}
	findings = schemaFilter.Kept
	repairFailures := map[string]int{}

	// Repair retry fires only when the initial response had findings but
	// every one was dropped. Mixed responses keep the valid findings and
	// move on; this matches the cite-or-drop pattern. Each repair
	// sub-step failure records a distinct reason code so coverage can
	// distinguish "repair never ran" from "repair produced nothing
	// valid"; without that, both look like an all-invalid outcome.
	if schemaFilter.KeptCount == 0 && schemaFilter.DroppedCount > 0 {
		retry, retryErr := a.runner.GenerateText(ctx, schemaRepairPrompt(res.Text, schemaFilter.DroppedSamples))
		switch {
		case retryErr != nil:
			repairFailures["schema_repair_call_failed"]++
			schemaFilter.DroppedSamples = append(schemaFilter.DroppedSamples,
				fmt.Sprintf("schema-repair: call failed: %v", retryErr))
		default:
			repaired, parseErr := extractFindingsArray(retry.Text)
			switch {
			case parseErr != nil:
				repairFailures["schema_repair_parse_failed"]++
				schemaFilter.DroppedSamples = append(schemaFilter.DroppedSamples,
					fmt.Sprintf("schema-repair: parse failed: %v", parseErr))
			default:
				reStamped, stErr := stampFindingIDs(repaired, in.RepositoryID, in.PRNumber)
				switch {
				case stErr != nil:
					repairFailures["schema_repair_stamp_failed"]++
					schemaFilter.DroppedSamples = append(schemaFilter.DroppedSamples,
						fmt.Sprintf("schema-repair: stamp failed: %v", stErr))
				default:
					repairFilter := FilterFindingsBySchema(reStamped)
					if repairFilter.ArrayErr != nil {
						repairFailures["schema_repair_array_failed"]++
						schemaFilter.DroppedSamples = append(schemaFilter.DroppedSamples,
							fmt.Sprintf("schema-repair: array invalid: %v", repairFilter.ArrayErr))
						break
					}
					// Repair filter ran. Kept findings come from the
					// retry; drop counts are merged so coverage_summary
					// reflects both attempts.
					findings = repairFilter.Kept
					mergeIntoCounter(schemaFilter.DroppedReasons, repairFilter.DroppedReasons)
					schemaFilter.DroppedCount += repairFilter.DroppedCount
					schemaFilter.DroppedSamples = append(schemaFilter.DroppedSamples, repairFilter.DroppedSamples...)
					schemaFilter.KeptCount = repairFilter.KeptCount
					if repairFilter.KeptCount > 0 {
						// A repair retry may succeed through a different provider in the fallback chain.
						res = retry
					}
				}
			}
		}
	}

	// Schema validation checks shape; citation filtering checks evidence.
	filtered, filterErr := FilterFindingsByCitations(findings, in.Bundle)
	if filterErr != nil {
		return AnalysisResult{}, fmt.Errorf("%w: cite-or-drop: %v", ErrAnalyzerInternal, filterErr)
	}
	findings = filtered.Findings

	coverage := buildCoverageSummary(in.Bundle)
	totalDropped := schemaFilter.DroppedCount + filtered.DroppedCount
	dropReasons := mergeReasonCounters(schemaFilter.DroppedReasons, filtered.DroppedReasons)
	coverageExtras := map[string]any{}
	if totalDropped > 0 {
		coverageExtras["findings_dropped"] = totalDropped
		coverageExtras["drop_reasons"] = dropReasons
	}
	if len(repairFailures) > 0 {
		coverageExtras["schema_repair_failures"] = repairFailures
	}
	if len(coverageExtras) > 0 {
		coverage = mergeCoverage(coverage, coverageExtras)
	}
	// Map local provider names to the API's wire values.
	wireProvider := res.Provider
	if mapped, ok := MapWriterNameToWire(res.Provider); ok {
		wireProvider = mapped
	}
	return AnalysisResult{
		Findings:              findings,
		CoverageSummary:       coverage,
		Provider:              wireProvider,
		Model:                 res.Model,
		PromptTemplateVersion: PromptTemplateVersion,
		SchemaDiagnostics:     schemaFilter.DroppedSamples,
	}, nil
}

// renderAnalyzerPrompt builds the structured prompt sent to the provider.
func renderAnalyzerPrompt(in AnalysisInput) string {
	var b strings.Builder

	b.WriteString("You are reviewing a pull request for intent-gap analysis.\n\n")
	b.WriteString("You have three mechanical evidence anchors:\n")
	b.WriteString("  1. ASK     - captured user prompts (turns below)\n")
	b.WriteString("  2. ATTEMPT - captured agent tool invocations (actions below)\n")
	b.WriteString("  3. RESULT  - the cumulative pull request diff\n\n")
	b.WriteString("Each anchor is mechanical. A turn proves the user typed text; an\n")
	b.WriteString("action proves the agent invoked a tool on a file; a diff proves\n")
	b.WriteString("text landed on disk. None alone proves the agent attempted any\n")
	b.WriteString("particular semantic goal. Semantic claims require multiple anchors\n")
	b.WriteString("aligning on the same resolved scope.\n\n")
	b.WriteString("Output a JSON array of intent-gap findings. Each finding object MUST\n")
	b.WriteString("match the following schema:\n\n")
	b.WriteString("  schema_version: \"1\"\n")
	b.WriteString("  finding_id:     placeholder, e.g. \"f_0000000000000000\" - Semantica\n")
	b.WriteString("                  computes the real id deterministically from the\n")
	b.WriteString("                  citation anchors; do not invent hex digits.\n")
	b.WriteString("  kind:           one of [\"under_impl\", \"unrequested\", \"deferred\"]\n")
	b.WriteString("  title:          short summary (1-200 chars)\n")
	b.WriteString("  confidence:     one of [\"low\", \"medium\", \"high\"]\n")
	b.WriteString("\nKind-specific required fields:\n")
	b.WriteString("  under_impl:  expected_intent {summary,turn_id,prompt_excerpt,prompt_excerpt_hash},\n")
	b.WriteString("               observed_diff_evidence {summary,\n")
	b.WriteString("                 ai_authored_regions_checked:[{file,lines:[[start,end]]}, ...]}\n")
	b.WriteString("                 (ai_authored_regions_checked is an ARRAY of regions you\n")
	b.WriteString("                  inspected inside the PR diff - at least one required;\n")
	b.WriteString("                  it is NOT a yes/no boolean),\n")
	b.WriteString("               missing_or_partial_area {note,\n")
	b.WriteString("                 closest_match? {file,lines:[start,end]}}\n")
	b.WriteString("  unrequested: delivered {file,line_range:[start,end],evidence_class,summary},\n")
	b.WriteString("               captured_intent_search {prompts_considered,result,qualifier}\n")
	b.WriteString("  deferred:    originally_requested_in {turn_id,prompt_excerpt,prompt_excerpt_hash},\n")
	b.WriteString("               trajectory_note,\n")
	b.WriteString("               current_state {file,line_range:[start,end],summary},\n")
	b.WriteString("               agent_action_citation {action_id} - REQUIRED. The cited\n")
	b.WriteString("                 action must belong to a detected revert trajectory\n")
	b.WriteString("                 below, and the trajectory's file must match\n")
	b.WriteString("                 current_state.file. Deferred findings without\n")
	b.WriteString("                 a trajectory citation are dropped.\n")
	b.WriteString("\nOptional citation fields (apply to any kind):\n")
	b.WriteString("  agent_action_citation: {action_id, scope?}\n")
	b.WriteString("      Anchors a finding to a captured action whose mechanical\n")
	b.WriteString("      activity supports the finding. Use an action_id from the\n")
	b.WriteString("      captured actions list below; scope is optional and may\n")
	b.WriteString("      narrow to {file, line_range?}. Semantica verifies the\n")
	b.WriteString("      cited action exists and that any cited scope matches the\n")
	b.WriteString("      action's recorded file and line range. Required for\n")
	b.WriteString("      deferred findings (see kind-specific section above).\n")
	b.WriteString("  no_action_citation: {scope}\n")
	b.WriteString("      For findings that state no captured action touched a\n")
	b.WriteString("      concrete scope. scope must include file; line_range\n")
	b.WriteString("      narrows it. Semantica verifies that no captured action\n")
	b.WriteString("      overlaps the scope. Actions whose own file or line range\n")
	b.WriteString("      is unknown cannot prove non-overlap, so negative findings\n")
	b.WriteString("      against ambiguous activity are dropped.\n")
	b.WriteString("\nConfidence guidance (under_impl and deferred):\n")
	b.WriteString("  high   - All three anchors align on the resolved scope.\n")
	b.WriteString("  medium - Two anchors align; one is partial or absent.\n")
	b.WriteString("  low    - Only one anchor is present, or evidence is thin.\n")
	b.WriteString("           Prefer dropping a low-confidence finding to emitting it.\n")
	b.WriteString("\nConfidence guidance (unrequested):\n")
	b.WriteString("  The ASK anchor for unrequested findings is a complete\n")
	b.WriteString("  captured-intent search that returned no supporting prompt,\n")
	b.WriteString("  not positive alignment with a prompt. Reserve high for\n")
	b.WriteString("  cases where the diff is clear and the search is complete\n")
	b.WriteString("  (captured_intent_search.prompts_considered equals the\n")
	b.WriteString("  number of visible turns). Use lower confidence when the\n")
	b.WriteString("  search is partial or capture coverage is in doubt.\n")
	b.WriteString("\nReturn an empty array [] if you find no gaps. Do not invent findings.\n")
	b.WriteString("Respond with ONLY the JSON array. No prose, no markdown code fences.\n\n")

	fmt.Fprintf(&b, "PR #%d\n", in.PRNumber)
	fmt.Fprintf(&b, "Base SHA: %s\n", in.Bundle.BaseSHA)
	fmt.Fprintf(&b, "Head SHA: %s\n", in.Bundle.HeadSHA)
	fmt.Fprintf(&b, "Base ref: %s\n", in.Bundle.BaseRef)

	if len(in.Bundle.Commits) > 0 {
		b.WriteString("\nCommits in this PR (oldest first):\n")
		for _, c := range in.Bundle.Commits {
			fmt.Fprintf(&b, "- %s %s\n", c.Hash, c.Subject)
		}
	}
	if in.Bundle.Truncated.CommitsDropped > 0 {
		fmt.Fprintf(&b, "(...%d additional commits omitted due to size cap)\n",
			in.Bundle.Truncated.CommitsDropped)
	}

	if len(in.Bundle.Turns) > 0 {
		b.WriteString("\nCaptured user prompts (oldest first). These are the\n")
		b.WriteString("ONLY valid citation sources for under_impl and deferred\n")
		b.WriteString("findings; do NOT cite turn IDs or excerpts not listed here.\n")
		for _, t := range in.Bundle.Turns {
			fmt.Fprintf(&b, "- turn_id=%s prompt_excerpt_hash=%s commit=%s\n  excerpt: %s\n",
				t.TurnID, t.PromptExcerptHash, t.CommitHash, truncatePromptExcerpt(t.PromptExcerpt))
		}
		if in.Bundle.Truncated.TurnsDropped > 0 {
			fmt.Fprintf(&b, "(...%d older turns omitted due to size cap)\n",
				in.Bundle.Truncated.TurnsDropped)
		}
	} else {
		b.WriteString("\nNo captured user prompts are available for this PR's\n")
		b.WriteString("commits. Do NOT emit under_impl or deferred findings\n")
		b.WriteString("(both require turn_id citations). For unrequested\n")
		b.WriteString("findings, set captured_intent_search.prompts_considered=0\n")
		b.WriteString("and a qualifier acknowledging the absence of captured intent.\n")
	}

	renderAgentActions(&b, in.Bundle)
	renderRevertTrajectories(&b, DetectEditRevertTrajectories(in.Bundle))

	b.WriteString("\nCumulative diff base..head:\n")
	b.WriteString("```diff\n")
	b.Write(in.Bundle.Diff)
	if !strings.HasSuffix(string(in.Bundle.Diff), "\n") {
		b.WriteString("\n")
	}
	b.WriteString("```\n")
	if in.Bundle.Truncated.DiffBytesDropped > 0 {
		fmt.Fprintf(&b, "(diff truncated; %d bytes dropped at the tail due to size cap)\n",
			in.Bundle.Truncated.DiffBytesDropped)
	}

	return b.String()
}

// renderAgentActions writes the captured tool-invocation listing. The
// listing is the ground truth the LLM cites with agent_action_citation;
// without it, the LLM has no verifiable source for ATTEMPT evidence and
// must stay at low confidence on activity that requires action evidence.
func renderAgentActions(b *strings.Builder, bundle Bundle) {
	if len(bundle.AgentActions) == 0 {
		b.WriteString("\nNo captured agent actions are available for this PR's\n")
		b.WriteString("commits. Mechanical evidence about what the agent attempted\n")
		b.WriteString("cannot be verified from this bundle. Avoid agent_action_citation\n")
		b.WriteString("entirely; avoid no_action_citation because absence of capture\n")
		b.WriteString("is not proof the agent did not act.\n")
		return
	}
	b.WriteString("\nCaptured agent actions (oldest first). These are the tool\n")
	b.WriteString("invocations recorded inside the analyzed commit window. Cite\n")
	b.WriteString("an action_id with agent_action_citation when a finding references\n")
	b.WriteString("mechanical activity on a file. Do NOT cite action_ids not\n")
	b.WriteString("listed here.\n")
	for _, a := range bundle.AgentActions {
		b.WriteString("- ")
		fmt.Fprintf(b, "action_id=%s turn_id=%s tool=%s", a.ActionID, a.TurnID, a.ToolName)
		if a.FilePath != "" {
			fmt.Fprintf(b, " file=%s", a.FilePath)
		} else {
			b.WriteString(" file=(unknown)")
		}
		if a.LineRangeStart > 0 && a.LineRangeEnd > 0 {
			fmt.Fprintf(b, " lines=%d-%d", a.LineRangeStart, a.LineRangeEnd)
		}
		b.WriteString("\n")
	}
	if bundle.Truncated.AgentActionsDropped > 0 {
		fmt.Fprintf(b, "(...%d older actions omitted due to size cap)\n",
			bundle.Truncated.AgentActionsDropped)
		b.WriteString("Do NOT emit no_action_citation while older actions are omitted.\n")
		b.WriteString("The listing above is incomplete and cannot prove non-overlap.\n")
	}
}

// renderRevertTrajectories writes any add-then-remove sequences the
// detector found. These are hints for the LLM: scopes where the
// agent touched a file repeatedly but the cumulative diff records no
// surviving change. A captured prompt that maps onto one of these
// scopes is the deferred case (#42); without a matching prompt the
// trajectory is just mechanical activity and should not become a
// finding by itself.
func renderRevertTrajectories(b *strings.Builder, candidates []TrajectoryCandidate) {
	if len(candidates) == 0 {
		return
	}
	b.WriteString("\nDetected revert trajectories. The agent touched these scopes\n")
	b.WriteString("repeatedly but the diff records no surviving change. When a\n")
	b.WriteString("captured prompt requests work in one of these scopes, you may\n")
	b.WriteString("emit a deferred finding that cites one listed action with\n")
	b.WriteString("agent_action_citation and describes the sequence in\n")
	b.WriteString("trajectory_note. A deferred finding that cites any action_id\n")
	b.WriteString("not listed here is rejected.\n")
	for _, c := range candidates {
		b.WriteString("- file=" + c.File)
		if c.LineStart > 0 && c.LineEnd > 0 {
			fmt.Fprintf(b, " lines=%d-%d", c.LineStart, c.LineEnd)
		} else {
			b.WriteString(" lines=(file-level)")
		}
		b.WriteString(" actions=[")
		for i, id := range c.ActionIDs {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(id)
		}
		b.WriteString("]\n")
	}
}

// truncatePromptExcerpt bounds one prompt's contribution to model context.
func truncatePromptExcerpt(s string) string {
	const maxExcerpt = 400
	if len(s) <= maxExcerpt {
		return s
	}
	return s[:maxExcerpt] + "...(truncated)"
}

// reformatPrompt requests a JSON-only retry after a parse failure.
func reformatPrompt(previous string) string {
	var b strings.Builder
	b.WriteString("Your previous response could not be parsed as a JSON array of intent-gap findings.\n")
	b.WriteString("Reply with ONLY the JSON array - no markdown code fences, no prose, no comments.\n")
	b.WriteString("If you had no findings, reply with the literal text: []\n\n")
	b.WriteString("Previous response:\n")
	b.WriteString(previous)
	return b.String()
}

// schemaRepairPrompt requests one JSON-only correction after every
// finding in the prior response was dropped for schema violations. The
// prompt embeds a minimal valid example per kind so the model can copy
// the exact shape rather than guess from field names.
func schemaRepairPrompt(previous string, droppedSamples []string) string {
	var b strings.Builder
	b.WriteString("Every finding in your previous response was dropped because it failed the intent-gap finding schema.\n")
	b.WriteString("Reply with ONLY a corrected JSON array - no markdown code fences, no prose, no comments.\n")
	b.WriteString("If there are no valid findings, reply with the literal text: []\n\n")

	b.WriteString("Minimal valid shapes (copy these exactly and substitute real values):\n\n")
	b.WriteString("under_impl:\n")
	b.WriteString(`{
  "schema_version":"1",
  "finding_id":"f_0000000000000000",
  "kind":"under_impl",
  "title":"short summary",
  "confidence":"medium",
  "expected_intent":{"summary":"...","turn_id":"t-1","prompt_excerpt":"...","prompt_excerpt_hash":"h-..."},
  "observed_diff_evidence":{"summary":"...","ai_authored_regions_checked":[{"file":"path/to/file.go","lines":[[12,14]]}]},
  "missing_or_partial_area":{"note":"..."}
}` + "\n")
	b.WriteString("Note: ai_authored_regions_checked is an ARRAY of region objects, not a boolean.\n\n")

	b.WriteString("deferred (requires agent_action_citation pointing at a detected trajectory action):\n")
	b.WriteString(`{
  "schema_version":"1",
  "finding_id":"f_0000000000000000",
  "kind":"deferred",
  "title":"short summary",
  "confidence":"medium",
  "originally_requested_in":{"turn_id":"t-1","prompt_excerpt":"...","prompt_excerpt_hash":"h-..."},
  "trajectory_note":"...",
  "agent_action_citation":{"action_id":"a_0123456789abcdef"},
  "current_state":{"file":"path/to/file.go","line_range":[12,14],"summary":"..."}
}` + "\n\n")

	b.WriteString("unrequested:\n")
	b.WriteString(`{
  "schema_version":"1",
  "finding_id":"f_0000000000000000",
  "kind":"unrequested",
  "title":"short summary",
  "confidence":"medium",
  "delivered":{"file":"path/to/file.go","line_range":[12,14],"evidence_class":"ai_exact","summary":"..."},
  "captured_intent_search":{"prompts_considered":N,"result":"none","qualifier":"..."}
}` + "\n\n")

	if len(droppedSamples) > 0 {
		b.WriteString("Why your previous findings were dropped:\n")
		for _, s := range droppedSamples {
			b.WriteString("- ")
			b.WriteString(s)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("Previous response:\n")
	b.WriteString(previous)
	return b.String()
}

// mergeIntoCounter adds the counts from src into dst in place. Used to
// fold a follow-up filter's drop totals into the running tally.
func mergeIntoCounter(dst, src map[string]int) {
	for k, v := range src {
		dst[k] += v
	}
}

// mergeReasonCounters returns a fresh map combining two reason->count
// maps. Used to build the coverage_summary.drop_reasons value without
// mutating either input.
func mergeReasonCounters(a, b map[string]int) map[string]int {
	out := make(map[string]int, len(a)+len(b))
	for k, v := range a {
		out[k] += v
	}
	for k, v := range b {
		out[k] += v
	}
	return out
}

// codeFencePattern captures optional Markdown wrappers around JSON output.
var codeFencePattern = regexp.MustCompile("(?s)```(?:json|JSON)?\\s*\\n?(.*?)\\n?```")

// extractFindingsArray accepts raw JSON, fenced JSON, or an embedded array.
func extractFindingsArray(text string) (json.RawMessage, error) {
	trim := strings.TrimSpace(text)
	if trim == "" {
		return nil, fmt.Errorf("empty LLM response")
	}

	candidates := []string{trim}
	for _, m := range codeFencePattern.FindAllStringSubmatch(trim, -1) {
		if len(m) >= 2 {
			candidates = append(candidates, strings.TrimSpace(m[1]))
		}
	}
	if extracted, ok := firstJSONArray(trim); ok {
		candidates = append(candidates, extracted)
	}

	var lastErr error
	for _, cand := range candidates {
		if !strings.HasPrefix(cand, "[") {
			continue
		}
		var arr []json.RawMessage
		if err := json.Unmarshal([]byte(cand), &arr); err == nil {
			return json.RawMessage(cand), nil
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no JSON array found in response")
	}
	return nil, lastErr
}

// firstJSONArray returns the first balanced JSON array in s.
func firstJSONArray(s string) (string, bool) {
	start := strings.IndexByte(s, '[')
	if start < 0 {
		return "", false
	}
	depth := 0
	inStr := false
	escape := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if escape {
			escape = false
			continue
		}
		if inStr {
			switch c {
			case '\\':
				escape = true
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

// buildCoverageSummary records analyzed and truncated input counts.
func buildCoverageSummary(b Bundle) json.RawMessage {
	type cov struct {
		Commits             int `json:"commits"`
		CommitsDropped      int `json:"commits_dropped"`
		DiffBytes           int `json:"diff_bytes"`
		DiffBytesDropped    int `json:"diff_bytes_dropped"`
		Turns               int `json:"turns"`
		TurnsDropped        int `json:"turns_dropped"`
		AgentActions        int `json:"agent_actions_count"`
		AgentActionsDropped int `json:"agent_actions_dropped"`
	}
	c := cov{
		Commits:             len(b.Commits),
		CommitsDropped:      b.Truncated.CommitsDropped,
		DiffBytes:           len(b.Diff),
		DiffBytesDropped:    b.Truncated.DiffBytesDropped,
		Turns:               len(b.Turns),
		TurnsDropped:        b.Truncated.TurnsDropped,
		AgentActions:        len(b.AgentActions),
		AgentActionsDropped: b.Truncated.AgentActionsDropped,
	}
	out, _ := json.Marshal(c)
	return out
}
