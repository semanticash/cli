package intentgap

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
)

// PromptTemplateVersion identifies the prompt used to produce findings.
// Bump it when analyzer changes may affect model output. The value is
// part of the AnalysisCache key, so a bump invalidates every cached
// analysis produced by the previous analyzer.
const PromptTemplateVersion = "0.3.0-candidate-first-v1"

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
//
// ProviderFallbackErrors lists per-writer failures the LLM registry
// handled before a downstream writer succeeded. Each entry is
// "writer_name: truncated_error_message" in fallback order.
//
// PromptBadByteCount and PromptBadByteOffsets aggregate UTF-8
// sanitization stats across every GenerateText call this analysis
// made. A non-zero count means upstream rendering produced invalid
// UTF-8 that the registry replaced before sending it to the LLM; the
// offsets point into the per-call prompt and are the fastest way to
// find the offending renderer.
type AnalysisResult struct {
	Findings               json.RawMessage
	CoverageSummary        json.RawMessage
	Provider               string
	Model                  string
	PromptTemplateVersion  string
	SchemaDiagnostics      []string
	ProviderFallbackErrors []string
	PromptBadByteCount     int
	PromptBadByteOffsets   []int
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
	ReasonBundleFailed               ReasonCode = "bundle_failed"
	ReasonLineageUnavailable         ReasonCode = "lineage_unavailable"
	ReasonRedactionFailed            ReasonCode = "redaction_failed"
	ReasonLLMUnavailable             ReasonCode = "llm_unavailable"
	ReasonParseFailed                ReasonCode = "parse_failed"
	ReasonSchemaFailed               ReasonCode = "schema_failed"
	ReasonAnalyzerInternal           ReasonCode = "analyzer_internal"
	ReasonIntentClassificationFailed ReasonCode = "intent_classification_failed"
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
	case errors.Is(err, ErrIntentClassifierFailed):
		return ReasonIntentClassificationFailed
	case errors.Is(err, ErrAnalyzerInternal):
		return ReasonAnalyzerInternal
	default:
		return ReasonAnalyzerInternal
	}
}

// codeFencePattern captures optional Markdown wrappers around JSON output.
// Used by ledger and verifier extractors to strip fenced blocks before
// parsing.
var codeFencePattern = regexp.MustCompile("(?s)```(?:json|JSON)?\\s*\\n?(.*?)\\n?```")
