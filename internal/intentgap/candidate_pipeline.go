package intentgap

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/semanticash/cli/internal/llm"
)

// AlgorithmCandidateFirst is the coverage_summary.algorithm value the
// candidate-first pipeline stamps on every result. It lets downstream
// consumers tell candidate-first coverage apart from older
// output that might still be sitting in caches or uploads.
const AlgorithmCandidateFirst = "candidate_first_v1"

// HardAnalyzerDeadline caps the total wall-clock the pipeline may
// consume. Each per-writer timeout in the registry still applies to
// individual calls; this deadline bounds the sum of them.
const HardAnalyzerDeadline = 4 * time.Minute

// CandidateFirstRunner is the LLM slice the pipeline needs. It is a
// superset of the classifier / verifier interfaces so a single writer
// registry can drive every step.
type CandidateFirstRunner interface {
	GenerateText(ctx context.Context, prompt string) (*llm.GenerateTextResult, error)
}

// CandidateFirstAnalyzer adapts RunCandidateFirstAnalyzer to the
// IntentGapAnalyzer interface. The service constructs one when
// deps.Analyzer is nil; tests injecting a custom analyzer through
// the same slot still win because the service consults deps first.
type CandidateFirstAnalyzer struct {
	Runner CandidateFirstRunner
}

// NewCandidateFirstAnalyzer wraps a runner in the analyzer adapter.
func NewCandidateFirstAnalyzer(runner CandidateFirstRunner) *CandidateFirstAnalyzer {
	return &CandidateFirstAnalyzer{Runner: runner}
}

// Analyze is the IntentGapAnalyzer implementation. A nil receiver or
// missing runner surfaces as ErrAnalyzerLLMUnavailable so the service
// can map the failure to its existing reason code.
func (a *CandidateFirstAnalyzer) Analyze(ctx context.Context, in AnalysisInput) (AnalysisResult, error) {
	if a == nil || a.Runner == nil {
		return AnalysisResult{}, fmt.Errorf("%w: candidate-first runner not wired", ErrAnalyzerLLMUnavailable)
	}
	return RunCandidateFirstAnalyzer(ctx, in, a.Runner)
}

// RunCandidateFirstAnalyzer wires every candidate-first slice into a
// single AnalysisResult the service can plug into BuildAnalyzedBody.
//
// The pipeline is deadline-aware: it wraps ctx in HardAnalyzerDeadline
// and passes the derived context to every LLM step. Each step reports
// its own coverage counters, and the orchestrator folds them into
// one coverage_summary blob so the wire payload stays the same shape
// the service expects today.
//
// Provider / Model come from the runner's first successful response;
// the pipeline wraps the incoming runner with a small captureRunner
// that records the first non-empty attribution across the whole run.
func RunCandidateFirstAnalyzer(ctx context.Context, in AnalysisInput, runner CandidateFirstRunner) (AnalysisResult, error) {
	ctx, cancel := context.WithTimeout(ctx, HardAnalyzerDeadline)
	defer cancel()

	// Wrap the caller's runner so we can attribute the run without
	// changing every downstream signature.
	capture := &captureRunner{inner: runner}

	// A bundle with no captured turns cannot support intent-based
	// findings — no classifier call runs, and no candidates are
	// generated. The service treats this as a legitimate "no gaps
	// found" result and uploads an empty findings array.
	if len(in.Bundle.Turns) == 0 {
		coverage := buildEmptyPipelineCoverage(in.Bundle, "no_captured_prompts")
		return AnalysisResult{
			Findings:              json.RawMessage("[]"),
			CoverageSummary:       coverage,
			PromptTemplateVersion: PromptTemplateVersion,
		}, nil
	}

	// Step 1: intent ledger (1 LLM call + optional repair).
	intentLedger, err := BuildIntentLedger(ctx, capture, in.Bundle.Turns)
	if err != nil {
		return AnalysisResult{}, err
	}

	// Step 2: deterministic ledgers over the bundle. No LLM calls
	// happen here; each function is cheap and idempotent.
	change := BuildChangeLedger(in.Bundle.Diff)
	action := BuildActionLedger(in.Bundle.AgentActions)

	// Step 3: retrieval per request/correction intent. Non-actionable
	// kinds (constraint, preference, defer, context) still live on
	// the ledger for follow-up passes but do not consume retrieval.
	scopes := make(map[string]RetrievedScope, len(intentLedger.Items))
	for _, it := range intentLedger.Items {
		if it.Kind != IntentRequest && it.Kind != IntentCorrection {
			continue
		}
		scopes[it.ID] = BuildRetrieval(it, change, action)
	}

	// Step 4: candidate generation (Track A + Track B).
	genResult := GenerateUnderImplCandidates(UnderImplGenInput{
		Intents: intentLedger.Items,
		Scopes:  scopes,
		Change:  change,
		Action:  action,
	})

	intentsByID := indexIntentsByID(intentLedger.Items)
	candidatesByID := indexCandidatesByID(genResult.Candidates)

	// Step 5: verifier pool (parallel LLM calls, hard-deadline aware).
	poolResult := RunVerifierPool(ctx, capture, VerifierPoolInput{
		Candidates:  genResult.Candidates,
		IntentsByID: intentsByID,
		Change:      change,
		Action:      action,
		Bundle:      in.Bundle,
	})

	// Step 6: expander (top-3 needs_more_context; single re-verify).
	expansion := RunExpander(ctx, capture, ExpansionInput{
		VerifierResults: poolResult.Results,
		CandidatesByID:  candidatesByID,
		IntentsByID:     intentsByID,
		Change:          change,
		Action:          action,
		Bundle:          in.Bundle,
	})

	// Step 7: adjudication (dedup + render + cite-or-drop + stamp).
	adj := RunAdjudicator(AdjudicatorInput{
		VerifierResults: expansion.UpdatedResults,
		CandidatesByID:  candidatesByID,
		IntentsByID:     intentsByID,
		Bundle:          in.Bundle,
		RepositoryID:    in.RepositoryID,
		PRNumber:        in.PRNumber,
	})

	coverage := buildPipelineCoverage(in.Bundle, intentLedger, genResult, poolResult, expansion, adj)

	provider, model := capture.attribution()
	wireProvider := provider
	if mapped, ok := MapWriterNameToWire(provider); ok {
		wireProvider = mapped
	}

	return AnalysisResult{
		Findings:               adj.Findings,
		CoverageSummary:        coverage,
		Provider:               wireProvider,
		Model:                  model,
		PromptTemplateVersion:  PromptTemplateVersion,
		ProviderFallbackErrors: capture.fallbackErrors(),
		PromptBadByteCount:     capture.badByteCount(),
		PromptBadByteOffsets:   capture.badByteOffsets(),
	}, nil
}

// buildPipelineCoverage assembles the coverage_summary JSON for a
// successful pipeline run. Every counter the plan documents lands
// here so downstream consumers see a single shape regardless of
// whether they read the wire or the local cache.
func buildPipelineCoverage(
	bundle Bundle,
	intentLedger IntentLedger,
	gen UnderImplGenResult,
	pool VerifierPoolResult,
	exp ExpansionResult,
	adj AdjudicatorResult,
) json.RawMessage {
	verdictCounts := map[string]int{}
	verifierDropReasons := map[string]int{}
	for _, r := range exp.UpdatedResults {
		verdictCounts[string(r.Verdict)]++
		if r.Verdict == VerdictDrop && r.DropReason != "" {
			verifierDropReasons[r.DropReason]++
		}
	}

	summary := map[string]any{
		"algorithm":                               AlgorithmCandidateFirst,
		"commits":                                 len(bundle.Commits),
		"turns":                                   len(bundle.Turns),
		"diff_bytes":                              len(bundle.Diff),
		"agent_actions_count":                     len(bundle.AgentActions),
		"intent_items_total":                      len(intentLedger.Items),
		"intent_items_by_kind":                    intentKindHistogram(intentLedger.Items),
		"intent_invalid_count":                    intentLedger.InvalidCount,
		"candidates_generated":                    len(gen.Candidates) + gen.TruncatedAtCap,
		"candidates_truncated_at_cap":             gen.TruncatedAtCap,
		"verifier_verdicts":                       verdictCounts,
		"verifier_drop_reasons":                   verifierDropReasons,
		"expansion_attempts":                      exp.ExpansionAttempts,
		"expansion_inconclusive":                  exp.Inconclusive,
		"adjudicator_dedup_dropped":               adj.DedupDropped,
		"adjudicator_cite_or_drop_dropped":        adj.CiteOrDropDropped,
		"action_citations_discarded":              adj.ActionCitationsDiscarded,
		"unanchored_accepts":                      adj.UnanchoredAccepts,
		"track_a_candidates_accepted":             adj.TrackACandidatesAccepted,
		"analyzer_skipped_candidates_on_deadline": pool.SkippedOnDeadline,
	}
	if intentLedger.Unreliable {
		summary["intent_classification_unreliable"] = true
	}
	if pool.DeadlineExceeded {
		summary["analyzer_deadline_exceeded"] = true
	}
	if len(adj.TrackADiagnostics) > 0 {
		summary["track_a_intents"] = adj.TrackADiagnostics
	}
	out, _ := json.Marshal(summary)
	return out
}

// buildEmptyPipelineCoverage produces the coverage_summary for the
// early-return "no turns to classify" case. Downstream consumers see
// the algorithm marker even on an empty run so they know a
// candidate-first pipeline was invoked.
func buildEmptyPipelineCoverage(bundle Bundle, skipReason string) json.RawMessage {
	summary := map[string]any{
		"algorithm":           AlgorithmCandidateFirst,
		"commits":             len(bundle.Commits),
		"turns":               len(bundle.Turns),
		"diff_bytes":          len(bundle.Diff),
		"agent_actions_count": len(bundle.AgentActions),
		"skipped":             true,
		"skip_reason":         skipReason,
	}
	out, _ := json.Marshal(summary)
	return out
}

// intentKindHistogram returns a fresh map counting how many intent
// items landed under each IntentKind. Included in coverage so
// downstream consumers see the classifier's shape at a glance.
func intentKindHistogram(items []IntentItem) map[string]int {
	out := map[string]int{}
	for _, it := range items {
		out[string(it.Kind)]++
	}
	return out
}

// indexIntentsByID builds the lookup map the verifier pool and
// expander both need. Duplicate IDs (should never happen given the
// classifier's uniqueness invariants) collapse to the last write.
func indexIntentsByID(items []IntentItem) map[string]IntentItem {
	out := make(map[string]IntentItem, len(items))
	for _, it := range items {
		out[it.ID] = it
	}
	return out
}

// indexCandidatesByID mirrors indexIntentsByID for the candidate
// slice. The pool's ID-driven lookup requires this shape.
func indexCandidatesByID(cands []Candidate) map[string]Candidate {
	out := make(map[string]Candidate, len(cands))
	for _, c := range cands {
		out[c.ID] = c
	}
	return out
}

// captureRunner wraps the caller's runner so the pipeline can record
// per-run attribution (Provider / Model) and aggregate the registry-
// side diagnostics (fallback errors, UTF-8 sanitization stats)
// without threading them through every step's signature.
//
// The wrapper is goroutine-safe: the verifier pool runs multiple
// GenerateText calls in parallel, and all counters update under the
// mutex.
type captureRunner struct {
	inner CandidateFirstRunner

	mu            sync.Mutex
	firstProvider string
	firstModel    string
	fallbacks     []string
	badBytes      int
	badByteSample []int
}

func (c *captureRunner) GenerateText(ctx context.Context, prompt string) (*llm.GenerateTextResult, error) {
	res, err := c.inner.GenerateText(ctx, prompt)
	if res == nil {
		return res, err
	}
	c.mu.Lock()
	if c.firstProvider == "" && res.Provider != "" {
		c.firstProvider = res.Provider
	}
	if c.firstModel == "" && res.Model != "" {
		c.firstModel = res.Model
	}
	if len(res.FallbackErrors) > 0 {
		c.fallbacks = append(c.fallbacks, res.FallbackErrors...)
	}
	c.badBytes += res.PromptBadByteCount
	if len(res.PromptBadByteOffsets) > 0 && len(c.badByteSample) == 0 {
		// Keep the first non-empty offset set; the rest would point
		// into other prompts and would be confusing without per-
		// prompt attribution.
		c.badByteSample = append([]int(nil), res.PromptBadByteOffsets...)
	}
	c.mu.Unlock()
	return res, err
}

func (c *captureRunner) attribution() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.firstProvider, c.firstModel
}

func (c *captureRunner) fallbackErrors() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.fallbacks) == 0 {
		return nil
	}
	out := make([]string, len(c.fallbacks))
	copy(out, c.fallbacks)
	return out
}

func (c *captureRunner) badByteCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.badBytes
}

func (c *captureRunner) badByteOffsets() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.badByteSample) == 0 {
		return nil
	}
	out := make([]int, len(c.badByteSample))
	copy(out, c.badByteSample)
	return out
}
