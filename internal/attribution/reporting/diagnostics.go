package reporting

import (
	"fmt"
	"strings"
)

// RenderDiagnosticNote produces a human-readable diagnostic note explaining
// why a particular AI percentage was computed. When AI% is 0 it identifies
// which pipeline stage had no data. When AI% > 0 and non-exact matches
// contributed, it breaks down the match tiers.
func RenderDiagnosticNote(in DiagnosticsInput) string {
	if in.AIPercent == 0 {
		switch {
		case in.EventStats.EventsConsidered == 0:
			return "No agent events found in the delta window between checkpoints."
		case in.EventStats.AIToolEvents == 0:
			return "Agent events found but none contained file-modifying tool calls (Edit/Write)."
		case in.EventStats.PayloadsLoaded == 0:
			return "Agent tool calls found but payloads could not be loaded from blob store."
		default:
			return "Agent tool calls found but no added lines in the commit matched AI-produced output."
		}
	}

	if in.MatchStats.NormalizedMatches > 0 || in.MatchStats.ModifiedMatches > 0 {
		parts := []string{fmt.Sprintf("%d exact", in.MatchStats.ExactMatches)}
		if in.MatchStats.NormalizedMatches > 0 {
			parts = append(parts, fmt.Sprintf("%d normalized", in.MatchStats.NormalizedMatches))
		}
		if in.MatchStats.ModifiedMatches > 0 {
			parts = append(parts, fmt.Sprintf("%d modified", in.MatchStats.ModifiedMatches))
		}
		return fmt.Sprintf("AI matches: %s.", strings.Join(parts, ", "))
	}

	return ""
}

// AssembleCommitNotes builds the combined notes bundle for a commit
// attribution. The pipeline-state note (if non-empty) leads; factual
// notes derived from the commit result (weaker fallback signals,
// carry-forward, deletion inference) follow.
//
// Used by both the CLI's attribution-display command and the push
// payload builder so both surfaces emit the same bundle. The wire
// shape is `notes []string`; the CLI display iterates the same slice
// and formats it as a bulleted list.
func AssembleCommitNotes(pipelineNote string, cr CommitResult) []string {
	var notes []string
	if pipelineNote != "" {
		notes = append(notes, pipelineNote)
	}
	if cr.FallbackCount > 0 {
		notes = append(notes, fmt.Sprintf("%d file(s) attributed using weaker fallback signals.", cr.FallbackCount))
	}
	hasCarryForward := false
	hasDeletion := false
	for _, f := range cr.Files {
		if f.PrimaryEvidence == EvidenceCarryForward {
			hasCarryForward = true
		}
		if f.PrimaryEvidence == EvidenceDeletion {
			hasDeletion = true
		}
		if hasCarryForward && hasDeletion {
			break
		}
	}
	if hasCarryForward {
		notes = append(notes, "Attribution includes historical carry-forward.")
	}
	if hasDeletion {
		notes = append(notes, "Some file attribution is inferred from deletion events.")
	}
	return notes
}

// AssembleCheckpointNotes is the checkpoint-only equivalent of
// AssembleCommitNotes. Checkpoint attribution has no line-level
// scoring, no fallback count, and no per-file evidence classes, so the
// result is just the pipeline-state note wrapped in a slice (or nil
// when empty). Kept as a named helper so the CLI's call sites do not
// sprout ad-hoc "wrap a string in a slice" code.
func AssembleCheckpointNotes(pipelineNote string) []string {
	if pipelineNote == "" {
		return nil
	}
	return []string{pipelineNote}
}
