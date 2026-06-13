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
const PromptTemplateVersion = "0.1.0"

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
type AnalysisResult struct {
	Findings              json.RawMessage
	CoverageSummary       json.RawMessage
	Provider              string
	Model                 string
	PromptTemplateVersion string
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

	if err := ValidateFindings(findings); err != nil {
		return AnalysisResult{}, fmt.Errorf("%w: %v", ErrAnalyzerSchemaFailed, err)
	}

	// Schema validation checks shape; citation filtering checks evidence.
	filtered, filterErr := FilterFindingsByCitations(findings, in.Bundle)
	if filterErr != nil {
		return AnalysisResult{}, fmt.Errorf("%w: cite-or-drop: %v", ErrAnalyzerInternal, filterErr)
	}
	findings = filtered.Findings

	coverage := buildCoverageSummary(in.Bundle)
	if filtered.DroppedCount > 0 {
		coverage = mergeCoverage(coverage, map[string]any{
			"findings_dropped": filtered.DroppedCount,
			"drop_reasons":     filtered.DroppedReasons,
		})
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
	}, nil
}

// renderAnalyzerPrompt builds the structured prompt sent to the provider.
func renderAnalyzerPrompt(in AnalysisInput) string {
	var b strings.Builder

	b.WriteString("You are reviewing a pull request for intent-gap analysis.\n\n")
	b.WriteString("Output a JSON array of intent-gap findings. Each finding object MUST\n")
	b.WriteString("match the following schema:\n\n")
	b.WriteString("  schema_version: \"1\"\n")
	b.WriteString("  finding_id:     placeholder, e.g. \"f_0000000000000000\" - the CLI\n")
	b.WriteString("                  computes the real id deterministically from the\n")
	b.WriteString("                  citation anchors; do not invent hex digits.\n")
	b.WriteString("  kind:           one of [\"under_impl\", \"unrequested\", \"deferred\"]\n")
	b.WriteString("  title:          short summary (1-200 chars)\n")
	b.WriteString("  confidence:     one of [\"low\", \"medium\", \"high\"]\n")
	b.WriteString("\nKind-specific required fields:\n")
	b.WriteString("  under_impl:  expected_intent {summary,turn_id,prompt_excerpt,prompt_excerpt_hash},\n")
	b.WriteString("               observed_diff_evidence {summary,ai_authored_regions_checked},\n")
	b.WriteString("               missing_or_partial_area {note}\n")
	b.WriteString("  unrequested: delivered {file,line_range:[start,end],evidence_class,summary},\n")
	b.WriteString("               captured_intent_search {prompts_considered,result,qualifier}\n")
	b.WriteString("  deferred:    originally_requested_in {turn_id,prompt_excerpt,prompt_excerpt_hash},\n")
	b.WriteString("               trajectory_note,\n")
	b.WriteString("               current_state {file,line_range:[start,end],summary}\n")
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
		Commits          int `json:"commits"`
		CommitsDropped   int `json:"commits_dropped"`
		DiffBytes        int `json:"diff_bytes"`
		DiffBytesDropped int `json:"diff_bytes_dropped"`
		Turns            int `json:"turns"`
		TurnsDropped     int `json:"turns_dropped"`
	}
	c := cov{
		Commits:          len(b.Commits),
		CommitsDropped:   b.Truncated.CommitsDropped,
		DiffBytes:        len(b.Diff),
		DiffBytesDropped: b.Truncated.DiffBytesDropped,
		Turns:            len(b.Turns),
		TurnsDropped:     b.Truncated.TurnsDropped,
	}
	out, _ := json.Marshal(c)
	return out
}
