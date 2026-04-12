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

func TestCommitConfidence_AllExact(t *testing.T) {
	files := []FileAttributionOutput{
		{PrimaryEvidence: EvidenceExact, AIExactLines: 50},
		{PrimaryEvidence: EvidenceExact, AIExactLines: 30},
		{PrimaryEvidence: EvidenceNone, HumanLines: 20},
	}
	level, fallback := CommitConfidence(files)
	if level != "High" {
		t.Errorf("level = %q, want High", level)
	}
	if fallback != 0 {
		t.Errorf("fallback = %d, want 0", fallback)
	}
}

func TestCommitConfidence_MixedWithSmallFallback(t *testing.T) {
	// 100 exact lines + 5 provider-touch lines -> High (95%+ strong).
	files := []FileAttributionOutput{
		{PrimaryEvidence: EvidenceExact, AIExactLines: 100},
		{PrimaryEvidence: EvidenceProviderTouch, AIModifiedLines: 5},
	}
	level, fallback := CommitConfidence(files)
	if level != "High" {
		t.Errorf("level = %q, want High (95%% strong)", level)
	}
	if fallback != 1 {
		t.Errorf("fallback = %d, want 1", fallback)
	}
}

func TestCommitConfidence_MediumWhenFallbackSignificant(t *testing.T) {
	// 60 exact + 40 provider-touch -> Medium (60% strong, 40% fallback).
	files := []FileAttributionOutput{
		{PrimaryEvidence: EvidenceExact, AIExactLines: 60},
		{PrimaryEvidence: EvidenceProviderTouch, AIModifiedLines: 40},
	}
	level, fallback := CommitConfidence(files)
	if level != "Medium" {
		t.Errorf("level = %q, want Medium", level)
	}
	if fallback != 1 {
		t.Errorf("fallback = %d, want 1", fallback)
	}
}

func TestCommitConfidence_LowWhenMostlyFallback(t *testing.T) {
	// 5 exact + 3 provider-touch + 3 provider-coarse + 3 carry-forward + 3 deletion.
	// LineScore = (5 + 0.55*12) / 17 = 11.6/17 = 0.682
	// FallbackPenalty = (0.18 + 0.30 + 0.25 + 0.35) / 5 = 0.216
	// Score = 0.682 - 0.216 = 0.466 -> Medium (just barely).
	// To get Low, need heavier fallback. Use all-fallback files:
	// 0 exact + 5 provider-coarse modified lines.
	// LineScore = 0.55*5/5 = 0.55
	// FallbackPenalty = 0.30*1/1 = 0.30
	// Score = 0.25 -> Low
	files := []FileAttributionOutput{
		{PrimaryEvidence: EvidenceProviderCoarse, AIModifiedLines: 5},
	}
	level, fallback := CommitConfidence(files)
	if level != "Low" {
		t.Errorf("level = %q, want Low", level)
	}
	if fallback != 1 {
		t.Errorf("fallback = %d, want 1", fallback)
	}
}

func TestCommitConfidence_HighIncludesNormalized(t *testing.T) {
	files := []FileAttributionOutput{
		{PrimaryEvidence: EvidenceExact, AIExactLines: 40},
		{PrimaryEvidence: EvidenceNormalized, AIFormattedLines: 10},
	}
	level, _ := CommitConfidence(files)
	if level != "High" {
		t.Errorf("level = %q, want High (normalized counts as high)", level)
	}
}

func TestCommitConfidence_SmallModifiedStaysHigh(t *testing.T) {
	// 998 exact + 2 modified -> High (99.8% strong).
	// This is the "1000 lines, 2 modified" scenario.
	files := []FileAttributionOutput{
		{PrimaryEvidence: EvidenceExact, AIExactLines: 998},
		{PrimaryEvidence: EvidenceModified, AIModifiedLines: 2},
	}
	level, fallback := CommitConfidence(files)
	if level != "High" {
		t.Errorf("level = %q, want High (2 modified out of 1000 should not drop confidence)", level)
	}
	if fallback != 0 {
		t.Errorf("fallback = %d, want 0 (modified is not fallback)", fallback)
	}
}

func TestCommitConfidence_NoAIFiles(t *testing.T) {
	files := []FileAttributionOutput{
		{PrimaryEvidence: EvidenceNone, HumanLines: 20},
		{PrimaryEvidence: EvidenceNone, HumanLines: 30},
	}
	level, fallback := CommitConfidence(files)
	if level != "" {
		t.Errorf("level = %q, want empty (no AI files)", level)
	}
	if fallback != 0 {
		t.Errorf("fallback = %d, want 0", fallback)
	}
}

func TestConfidenceExplanation(t *testing.T) {
	tests := []struct {
		level    string
		fallback int
		want     string
	}{
		{"High", 0, "all files matched by direct line comparison"},
		{"Medium", 1, "1 file attributed without direct line evidence"},
		{"Medium", 3, "3 files attributed without direct line evidence"},
		{"Low", 2, "most files attributed via indirect provider signals"},
		{"", 0, ""},
	}
	for _, tt := range tests {
		got := ConfidenceExplanation(tt.level, tt.fallback)
		if got != tt.want {
			t.Errorf("ConfidenceExplanation(%q, %d) = %q, want %q", tt.level, tt.fallback, got, tt.want)
		}
	}
}
