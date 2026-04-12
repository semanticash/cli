package eval

import (
	"fmt"
	"math"
	"strings"

	"github.com/semanticash/cli/internal/attribution/events"
	"github.com/semanticash/cli/internal/attribution/reporting"
	"github.com/semanticash/cli/internal/attribution/scoring"
)

// CaseResult is the outcome of running a single evaluation case.
type CaseResult struct {
	Name   string
	Passed bool
	Errors []string

	// Observed values for summary reporting.
	AIPercentage  float64
	Evidence string
	FallbackCount int
	FileResults   []FileResult
}

// FileResult is the per-file observed vs expected comparison.
type FileResult struct {
	Path            string
	AILines         int
	HumanLines      int
	PrimaryEvidence reporting.EvidenceClass
	AllEvidence     []reporting.EvidenceClass
	Provider        string // primary provider for this file (for summary breakdown)
}

// Summary aggregates results across all cases.
type Summary struct {
	Total   int
	Passed  int
	Failed  int
	Results []CaseResult

	// Per-evidence-class usage counts (how often each class was primary).
	EvidenceUsage map[reporting.EvidenceClass]int
	// Files with multiple contributing classes.
	MultiEvidenceFiles int
	// Files where primary evidence is a fallback class.
	FallbackFiles int
	// Files where primary (display) class is stronger than the weakest
	// contributing class. Shows how often the "highest wins" rule hides
	// weaker evidence from the user.
	PrimaryStrongerThanWeakest int
	// Provider breakdown: provider -> primary evidence class -> count.
	ProviderEvidence map[string]map[reporting.EvidenceClass]int
}

const aiPercentTolerance = 0.1 // allow 0.1% floating-point tolerance

var evidenceOrder = []reporting.EvidenceClass{
	reporting.EvidenceExact, reporting.EvidenceNormalized, reporting.EvidenceModified,
	reporting.EvidenceProviderTouch, reporting.EvidenceProviderCoarse,
	reporting.EvidenceCarryForward, reporting.EvidenceDeletion, reporting.EvidenceNone,
}

// RunCorpus executes all evaluation cases and returns a summary.
func RunCorpus(cases []EvalCase) Summary {
	s := Summary{
		Total:            len(cases),
		EvidenceUsage:    make(map[reporting.EvidenceClass]int),
		ProviderEvidence: make(map[string]map[reporting.EvidenceClass]int),
	}

	for _, tc := range cases {
		cr := RunCase(tc)
		s.Results = append(s.Results, cr)
		if cr.Passed {
			s.Passed++
		} else {
			s.Failed++
		}
		for _, fr := range cr.FileResults {
			s.EvidenceUsage[fr.PrimaryEvidence]++
			if len(fr.AllEvidence) > 1 {
				s.MultiEvidenceFiles++
			}
			if isFallback(fr.PrimaryEvidence) {
				s.FallbackFiles++
			}

			// Track primary-stronger-than-weakest.
			if len(fr.AllEvidence) > 1 {
				weakest := fr.AllEvidence[len(fr.AllEvidence)-1]
				if fr.PrimaryEvidence != weakest {
					s.PrimaryStrongerThanWeakest++
				}
			}

			// Provider breakdown.
			prov := fr.Provider
			if prov == "" {
				prov = "(none)"
			}
			if s.ProviderEvidence[prov] == nil {
				s.ProviderEvidence[prov] = make(map[reporting.EvidenceClass]int)
			}
			s.ProviderEvidence[prov][fr.PrimaryEvidence]++
		}
	}

	return s
}

func isFallback(ec reporting.EvidenceClass) bool {
	switch ec {
	case reporting.EvidenceProviderTouch, reporting.EvidenceProviderCoarse,
		reporting.EvidenceCarryForward, reporting.EvidenceDeletion:
		return true
	}
	return false
}

// RunCase runs a single evaluation case through the domain pipeline.
func RunCase(tc EvalCase) CaseResult {
	cr := CaseResult{Name: tc.Name}

	// Step 1: Parse diff.
	diff := scoring.ParseDiff([]byte(tc.Diff))

	// Step 2: Build candidates from events.
	cands, _ := events.BuildCandidatesFromRows(tc.Events, tc.RepoRoot, nil)

	// Step 3: Score files.
	scores, _ := scoring.ScoreFiles(diff, cands.AILines, cands.ProviderTouchedFiles, cands.FileProvider)

	// Step 4: Build touch origins (mirrors deriveFileTouchOrigins in the service).
	touchOrigins := make(map[string]reporting.TouchOrigin)
	for fp, prov := range cands.ProviderTouchedFiles {
		switch {
		case len(cands.AILines[fp]) > 0 || cands.FileProvider[fp] != "":
			touchOrigins[fp] = reporting.TouchOriginLineLevel
		case prov == "claude_code":
			// Claude touch without line-level content = deletion inference.
			touchOrigins[fp] = reporting.TouchOriginDeletion
		default:
			touchOrigins[fp] = reporting.TouchOriginProviderEdit
		}
	}
	// Apply explicit overrides from the case (for testing specific origins).
	for fp, origin := range tc.TouchOriginOverrides {
		touchOrigins[fp] = origin
	}

	// Step 5: Build reporting input.
	fsInputs := make([]reporting.FileScoreInput, len(scores))
	for i, s := range scores {
		fsInputs[i] = reporting.FileScoreInput{
			Path:           s.Path,
			TotalLines:     s.TotalLines,
			ExactLines:     s.ExactLines,
			FormattedLines: s.FormattedLines,
			ModifiedLines:  s.ModifiedLines,
			HumanLines:     s.HumanLines,
			ProviderLines:  s.ProviderLines,
		}
	}

	touched := make(map[string]bool, len(cands.ProviderTouchedFiles))
	for fp := range cands.ProviderTouchedFiles {
		touched[fp] = true
	}

	result := reporting.BuildCommitResult(reporting.CommitResultInput{
		FileScores:        fsInputs,
		FilesCreated:      diff.FilesCreated,
		FilesDeleted:      diff.FilesDeleted,
		TouchedFiles:      touched,
		ProviderModels:    cands.ProviderModel,
		FileTouchOrigins:  touchOrigins,
		CarryForwardFiles: tc.CarryForwardFiles,
	})

	// Record observed values.
	// Build a path->provider lookup from the scored inputs.
	fileProviderLookup := make(map[string]string)
	for _, fsi := range fsInputs {
		var bestProv string
		var bestN int
		for p, n := range fsi.ProviderLines {
			if n > bestN {
				bestN = n
				bestProv = p
			}
		}
		if bestProv != "" {
			fileProviderLookup[fsi.Path] = bestProv
		}
	}

	cr.AIPercentage = result.AIPercentage
	cr.Evidence = result.Evidence
	cr.FallbackCount = result.FallbackCount
	for _, f := range result.Files {
		cr.FileResults = append(cr.FileResults, FileResult{
			Path:            f.Path,
			AILines:         f.AIExactLines + f.AIFormattedLines + f.AIModifiedLines,
			HumanLines:      f.HumanLines,
			PrimaryEvidence: f.PrimaryEvidence,
			AllEvidence:     f.AllEvidence,
			Provider:        fileProviderLookup[f.Path],
		})
	}

	// Validate against expected.
	cr.Passed = true

	// AI percentage.
	if math.Abs(result.AIPercentage-tc.Expected.AIPercentage) > aiPercentTolerance {
		cr.fail("AIPercentage = %.1f, want %.1f", result.AIPercentage, tc.Expected.AIPercentage)
	}

	// Evidence label.
	if tc.Expected.Evidence != "" && result.Evidence != tc.Expected.Evidence {
		cr.fail("Evidence = %q, want %q", result.Evidence, tc.Expected.Evidence)
	}

	// Fallback count.
	if result.FallbackCount != tc.Expected.FallbackCount {
		cr.fail("FallbackCount = %d, want %d", result.FallbackCount, tc.Expected.FallbackCount)
	}

	// Per-file validation.
	if len(result.Files) != len(tc.Expected.Files) {
		cr.fail("file count = %d, want %d", len(result.Files), len(tc.Expected.Files))
		return cr
	}

	resultByPath := make(map[string]reporting.FileAttributionOutput, len(result.Files))
	for _, f := range result.Files {
		resultByPath[f.Path] = f
	}

	for _, ef := range tc.Expected.Files {
		got, ok := resultByPath[ef.Path]
		if !ok {
			cr.fail("missing file %q in result", ef.Path)
			continue
		}

		gotAI := got.AIExactLines + got.AIFormattedLines + got.AIModifiedLines
		if gotAI != ef.AILines {
			cr.fail("%s: AILines = %d, want %d", ef.Path, gotAI, ef.AILines)
		}
		if got.HumanLines != ef.HumanLines {
			cr.fail("%s: HumanLines = %d, want %d", ef.Path, got.HumanLines, ef.HumanLines)
		}
		if got.PrimaryEvidence != ef.PrimaryEvidence {
			cr.fail("%s: PrimaryEvidence = %q, want %q", ef.Path, got.PrimaryEvidence, ef.PrimaryEvidence)
		}

		// Check contributing evidence classes.
		if len(ef.ContributingEvidence) > 0 {
			gotSet := make(map[reporting.EvidenceClass]bool)
			for _, c := range got.AllEvidence {
				gotSet[c] = true
			}
			for _, want := range ef.ContributingEvidence {
				if !gotSet[want] {
					cr.fail("%s: missing contributing evidence %q (got %v)", ef.Path, want, got.AllEvidence)
				}
			}
		}
	}

	return cr
}

func (cr *CaseResult) fail(format string, args ...any) {
	cr.Passed = false
	cr.Errors = append(cr.Errors, fmt.Sprintf(format, args...))
}

// FormatSummary returns a human-readable summary string.
func FormatSummary(s Summary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Evaluation: %d/%d passed\n", s.Passed, s.Total)

	if s.Failed > 0 {
		b.WriteString("\nFailed cases:\n")
		for _, r := range s.Results {
			if r.Passed {
				continue
			}
			fmt.Fprintf(&b, "  %s:\n", r.Name)
			for _, e := range r.Errors {
				fmt.Fprintf(&b, "    - %s\n", e)
			}
		}
	}

	b.WriteString("\nEvidence usage (primary class):\n")
	for _, ec := range evidenceOrder {
		if count, ok := s.EvidenceUsage[ec]; ok && count > 0 {
			fmt.Fprintf(&b, "  %-20s %d files\n", ec, count)
		}
	}

	fmt.Fprintf(&b, "\nMulti-evidence files:           %d\n", s.MultiEvidenceFiles)
	fmt.Fprintf(&b, "Fallback files:                 %d\n", s.FallbackFiles)
	fmt.Fprintf(&b, "Primary stronger than weakest:  %d\n", s.PrimaryStrongerThanWeakest)

	if len(s.ProviderEvidence) > 0 {
		b.WriteString("\nProvider breakdown:\n")
		for prov, classes := range s.ProviderEvidence {
			for _, ec := range evidenceOrder {
				if count, ok := classes[ec]; ok && count > 0 {
					fmt.Fprintf(&b, "  %-15s %-20s %d files\n", prov, ec, count)
				}
			}
		}
	}

	return b.String()
}
