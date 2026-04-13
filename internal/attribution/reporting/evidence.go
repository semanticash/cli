package reporting

import "fmt"

// ResolveFileEvidence determines the primary evidence class for a file
// based on its scored lines, touch origin, and carry-forward status.
// Returns the highest-quality evidence class that applies.
func ResolveFileEvidence(fs FileScoreInput, touch TouchOrigin, isCarryForward bool) EvidenceClass {
	aiLines := fs.ExactLines + fs.FormattedLines + fs.ModifiedLines

	// Line-level evidence takes priority (highest to lowest quality).
	if fs.ExactLines > 0 {
		return EvidenceExact
	}
	if fs.FormattedLines > 0 {
		return EvidenceNormalized
	}
	if fs.ModifiedLines > 0 {
		return EvidenceModified
	}

	// No scored lines -- check touch-based evidence.
	if aiLines == 0 && isCarryForward {
		return EvidenceCarryForward
	}
	if aiLines == 0 && touch == TouchOriginDeletion {
		return EvidenceDeletion
	}
	if aiLines == 0 && touch == TouchOriginProviderEdit {
		return EvidenceProviderTouch
	}
	if aiLines == 0 && touch == TouchOriginCoarse {
		return EvidenceProviderCoarse
	}
	// TouchOriginLineLevel with zero scored lines shouldn't normally happen
	// (line-level extraction should produce scored lines), but treat it as
	// provider-touch if it does.
	if aiLines == 0 && touch == TouchOriginLineLevel {
		return EvidenceProviderTouch
	}

	return EvidenceNone
}

// CollectFileEvidence returns all evidence classes that contributed to a
// file's attribution. Used by the evaluation harness to track which
// evidence paths were active, not just which one won.
func CollectFileEvidence(fs FileScoreInput, touch TouchOrigin, isCarryForward bool) []EvidenceClass {
	var classes []EvidenceClass

	if fs.ExactLines > 0 {
		classes = append(classes, EvidenceExact)
	}
	if fs.FormattedLines > 0 {
		classes = append(classes, EvidenceNormalized)
	}
	if fs.ModifiedLines > 0 {
		classes = append(classes, EvidenceModified)
	}
	if touch == TouchOriginProviderEdit {
		classes = append(classes, EvidenceProviderTouch)
	}
	if touch == TouchOriginCoarse {
		classes = append(classes, EvidenceProviderCoarse)
	}
	if touch == TouchOriginLineLevel && fs.ExactLines == 0 && fs.FormattedLines == 0 && fs.ModifiedLines == 0 {
		classes = append(classes, EvidenceProviderTouch)
	}
	if isCarryForward {
		classes = append(classes, EvidenceCarryForward)
	}
	if touch == TouchOriginDeletion {
		classes = append(classes, EvidenceDeletion)
	}

	if len(classes) == 0 {
		classes = append(classes, EvidenceNone)
	}
	return classes
}

// CommitEvidence computes the evidence level, score, and fallback count
// from per-file evidence using a weighted formula.
//
// Line-evidence score (0-1): LineScore = (1.00*Exact + 0.85*Normalized + 0.55*Modified) / max(1, AILines)
// File-evidence penalty (0-1): FallbackPenalty = (0.18*Tpd + 0.30*Tpc + 0.25*CF + 0.35*D) / max(1, AIFiles)
// Combined score: Score = clamp(LineScore - FallbackPenalty, 0, 1)
//
// Buckets:
//
//	High:   Score >= 0.75
//	Medium: 0.45 <= Score < 0.75
//	Low:    Score < 0.45
//
// Thresholds may be tuned as the evaluation corpus grows.
func CommitEvidence(files []FileAttributionOutput) (level string, fallbackCount int) {
	var exactLines, normLines, modLines int
	var aiFiles int
	var tpd, tpc, cf, del int

	for _, f := range files {
		if f.PrimaryEvidence == EvidenceNone {
			continue
		}
		aiFiles++
		exactLines += f.AIExactLines
		normLines += f.AIFormattedLines
		modLines += f.AIModifiedLines

		switch f.PrimaryEvidence {
		case EvidenceProviderTouch:
			tpd++
			fallbackCount++
		case EvidenceProviderCoarse:
			tpc++
			fallbackCount++
		case EvidenceCarryForward:
			cf++
			fallbackCount++
		case EvidenceDeletion:
			del++
			fallbackCount++
		}
	}

	aiLines := exactLines + normLines + modLines
	if aiFiles == 0 && aiLines == 0 {
		return "", 0
	}

	denom := aiLines
	if denom < 1 {
		denom = 1
	}
	lineScore := (1.00*float64(exactLines) + 0.85*float64(normLines) + 0.55*float64(modLines)) / float64(denom)

	fileDenom := aiFiles
	if fileDenom < 1 {
		fileDenom = 1
	}
	fallbackPenalty := (0.18*float64(tpd) + 0.30*float64(tpc) + 0.25*float64(cf) + 0.35*float64(del)) / float64(fileDenom)

	score := lineScore - fallbackPenalty
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}

	switch {
	case score >= 0.75:
		level = "High"
	case score >= 0.45:
		level = "Medium"
	default:
		level = "Low"
	}
	return level, fallbackCount
}

// EvidenceExplanation returns a short sentence explaining the evidence level.
func EvidenceExplanation(level string, fallbackCount int) string {
	switch level {
	case "High":
		return "all files matched by direct line comparison"
	case "Medium":
		if fallbackCount == 1 {
			return "1 file attributed without direct line evidence"
		}
		return fmt.Sprintf("%d files attributed without direct line evidence", fallbackCount)
	case "Low":
		if fallbackCount == 1 {
			return "most files attributed via indirect provider signals"
		}
		return "most files attributed via indirect provider signals"
	default:
		return ""
	}
}
