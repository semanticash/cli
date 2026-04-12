package reporting

import (
	"testing"
)

func TestResolveFileEvidence_ExactWins(t *testing.T) {
	fs := FileScoreInput{ExactLines: 5, FormattedLines: 2, ModifiedLines: 1}
	got := ResolveFileEvidence(fs, TouchOriginLineLevel, false)
	if got != EvidenceExact {
		t.Errorf("got %q, want %q", got, EvidenceExact)
	}
}

func TestResolveFileEvidence_NormalizedWhenNoExact(t *testing.T) {
	fs := FileScoreInput{FormattedLines: 3, ModifiedLines: 1}
	got := ResolveFileEvidence(fs, TouchOriginLineLevel, false)
	if got != EvidenceNormalized {
		t.Errorf("got %q, want %q", got, EvidenceNormalized)
	}
}

func TestResolveFileEvidence_ModifiedWhenNoExactOrNormalized(t *testing.T) {
	fs := FileScoreInput{ModifiedLines: 4}
	got := ResolveFileEvidence(fs, TouchOriginProviderEdit, false)
	if got != EvidenceModified {
		t.Errorf("got %q, want %q", got, EvidenceModified)
	}
}

func TestResolveFileEvidence_ProviderTouchWhenZeroLines(t *testing.T) {
	fs := FileScoreInput{TotalLines: 3, HumanLines: 3}
	got := ResolveFileEvidence(fs, TouchOriginProviderEdit, false)
	if got != EvidenceProviderTouch {
		t.Errorf("got %q, want %q", got, EvidenceProviderTouch)
	}
}

func TestResolveFileEvidence_ProviderCoarseWhenZeroLines(t *testing.T) {
	fs := FileScoreInput{TotalLines: 3, HumanLines: 3}
	got := ResolveFileEvidence(fs, TouchOriginCoarse, false)
	if got != EvidenceProviderCoarse {
		t.Errorf("got %q, want %q", got, EvidenceProviderCoarse)
	}
}

func TestResolveFileEvidence_CarryForwardWhenZeroLines(t *testing.T) {
	fs := FileScoreInput{TotalLines: 5, HumanLines: 5}
	got := ResolveFileEvidence(fs, "", true)
	if got != EvidenceCarryForward {
		t.Errorf("got %q, want %q", got, EvidenceCarryForward)
	}
}

func TestResolveFileEvidence_DeletionWhenZeroLines(t *testing.T) {
	fs := FileScoreInput{}
	got := ResolveFileEvidence(fs, TouchOriginDeletion, false)
	if got != EvidenceDeletion {
		t.Errorf("got %q, want %q", got, EvidenceDeletion)
	}
}

func TestResolveFileEvidence_NoneWhenNoEvidence(t *testing.T) {
	fs := FileScoreInput{TotalLines: 5, HumanLines: 5}
	got := ResolveFileEvidence(fs, "", false)
	if got != EvidenceNone {
		t.Errorf("got %q, want %q", got, EvidenceNone)
	}
}

func TestResolveFileEvidence_ExactBeatsCarryForward(t *testing.T) {
	// File has exact lines AND is carry-forward -- exact wins.
	fs := FileScoreInput{ExactLines: 3}
	got := ResolveFileEvidence(fs, TouchOriginLineLevel, true)
	if got != EvidenceExact {
		t.Errorf("got %q, want %q", got, EvidenceExact)
	}
}

func TestResolveFileEvidence_LineLevelWithZeroScored(t *testing.T) {
	// TouchOriginLineLevel but no scored lines (edge case).
	fs := FileScoreInput{TotalLines: 3, HumanLines: 3}
	got := ResolveFileEvidence(fs, TouchOriginLineLevel, false)
	if got != EvidenceProviderTouch {
		t.Errorf("got %q, want %q (line-level with zero scored lines)", got, EvidenceProviderTouch)
	}
}

func TestCollectFileEvidence_MultipleClasses(t *testing.T) {
	fs := FileScoreInput{ExactLines: 3, ModifiedLines: 1}
	classes := CollectFileEvidence(fs, TouchOriginProviderEdit, true)

	has := make(map[EvidenceClass]bool)
	for _, c := range classes {
		has[c] = true
	}

	if !has[EvidenceExact] {
		t.Error("expected EvidenceExact in contributing classes")
	}
	if !has[EvidenceModified] {
		t.Error("expected EvidenceModified in contributing classes")
	}
	if !has[EvidenceProviderTouch] {
		t.Error("expected EvidenceProviderTouch in contributing classes")
	}
	if !has[EvidenceCarryForward] {
		t.Error("expected EvidenceCarryForward in contributing classes")
	}
	if has[EvidenceNormalized] {
		t.Error("should not have EvidenceNormalized (no formatted lines)")
	}
}

func TestCollectFileEvidence_NoneWhenEmpty(t *testing.T) {
	fs := FileScoreInput{TotalLines: 5, HumanLines: 5}
	classes := CollectFileEvidence(fs, "", false)

	if len(classes) != 1 || classes[0] != EvidenceNone {
		t.Errorf("got %v, want [none]", classes)
	}
}

func TestCommitEvidenceLabel_AllExact(t *testing.T) {
	files := []FileAttributionOutput{
		{PrimaryEvidence: EvidenceExact},
		{PrimaryEvidence: EvidenceExact},
		{PrimaryEvidence: EvidenceNone}, // human file, ignored
	}
	label, fallback := CommitEvidenceLabel(files)
	if label != "Strong evidence" {
		t.Errorf("label = %q, want Strong evidence", label)
	}
	if fallback != 0 {
		t.Errorf("fallback = %d, want 0", fallback)
	}
}

func TestCommitEvidenceLabel_MixedWithOneFallback(t *testing.T) {
	files := []FileAttributionOutput{
		{PrimaryEvidence: EvidenceExact},
		{PrimaryEvidence: EvidenceExact},
		{PrimaryEvidence: EvidenceProviderTouch}, // fallback
	}
	label, fallback := CommitEvidenceLabel(files)
	if label != "Mixed evidence" {
		t.Errorf("label = %q, want Mixed evidence", label)
	}
	if fallback != 1 {
		t.Errorf("fallback = %d, want 1", fallback)
	}
}

func TestCommitEvidenceLabel_LimitedWhenMajorityFallback(t *testing.T) {
	files := []FileAttributionOutput{
		{PrimaryEvidence: EvidenceExact},
		{PrimaryEvidence: EvidenceProviderTouch},
		{PrimaryEvidence: EvidenceProviderCoarse},
		{PrimaryEvidence: EvidenceCarryForward},
	}
	label, fallback := CommitEvidenceLabel(files)
	if label != "Limited evidence" {
		t.Errorf("label = %q, want Limited evidence", label)
	}
	if fallback != 3 {
		t.Errorf("fallback = %d, want 3", fallback)
	}
}

func TestCommitEvidenceLabel_StrongIncludesNormalized(t *testing.T) {
	files := []FileAttributionOutput{
		{PrimaryEvidence: EvidenceExact},
		{PrimaryEvidence: EvidenceNormalized},
	}
	label, _ := CommitEvidenceLabel(files)
	if label != "Strong evidence" {
		t.Errorf("label = %q, want Strong evidence (normalized counts as strong)", label)
	}
}

func TestCommitEvidenceLabel_ModifiedIsMixed(t *testing.T) {
	files := []FileAttributionOutput{
		{PrimaryEvidence: EvidenceExact},
		{PrimaryEvidence: EvidenceModified},
	}
	label, fallback := CommitEvidenceLabel(files)
	if label != "Mixed evidence" {
		t.Errorf("label = %q, want Mixed evidence", label)
	}
	if fallback != 0 {
		t.Errorf("fallback = %d, want 0 (modified is not fallback)", fallback)
	}
}

func TestCommitEvidenceLabel_NoAIFiles(t *testing.T) {
	files := []FileAttributionOutput{
		{PrimaryEvidence: EvidenceNone},
		{PrimaryEvidence: EvidenceNone},
	}
	label, fallback := CommitEvidenceLabel(files)
	if label != "" {
		t.Errorf("label = %q, want empty (no AI files)", label)
	}
	if fallback != 0 {
		t.Errorf("fallback = %d, want 0", fallback)
	}
}
