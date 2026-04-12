package reporting

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

// isFallbackEvidence returns true for evidence classes that represent
// weaker, non-line-level attribution paths.
func isFallbackEvidence(ec EvidenceClass) bool {
	switch ec {
	case EvidenceProviderTouch, EvidenceProviderCoarse, EvidenceCarryForward, EvidenceDeletion:
		return true
	}
	return false
}

// CommitEvidenceLabel derives a user-facing evidence label and fallback count
// from per-file evidence. The label is one of:
//   - "Strong evidence" -- all AI-attributed files have exact or normalized primary evidence
//   - "Mixed evidence" -- at least one AI-attributed file uses weaker evidence
//   - "Limited evidence" -- majority of AI-attributed files use fallback evidence
//
// Initial policy, subject to corpus calibration.
func CommitEvidenceLabel(files []FileAttributionOutput) (label string, fallbackCount int) {
	var aiFiles, strongFiles int

	for _, f := range files {
		if f.PrimaryEvidence == EvidenceNone {
			continue // not AI-attributed
		}
		aiFiles++
		if isFallbackEvidence(f.PrimaryEvidence) {
			fallbackCount++
		}
		if f.PrimaryEvidence == EvidenceExact || f.PrimaryEvidence == EvidenceNormalized {
			strongFiles++
		}
	}

	if aiFiles == 0 {
		return "", 0
	}

	switch {
	case strongFiles == aiFiles:
		label = "Strong evidence"
	case fallbackCount > aiFiles/2:
		label = "Limited evidence"
	default:
		label = "Mixed evidence"
	}
	return label, fallbackCount
}
