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

func TestCommitEvidence_AllExact(t *testing.T) {
	files := []FileAttributionOutput{
		{PrimaryEvidence: EvidenceExact, AIExactLines: 50},
		{PrimaryEvidence: EvidenceExact, AIExactLines: 30},
		{PrimaryEvidence: EvidenceNone, HumanLines: 20},
	}
	level, fallback := CommitEvidence(files)
	if level != "High" {
		t.Errorf("level = %q, want High", level)
	}
	if fallback != 0 {
		t.Errorf("fallback = %d, want 0", fallback)
	}
}

func TestCommitEvidence_MixedWithSmallFallback(t *testing.T) {
	// 100 exact lines + 5 provider-touch lines -> High (95%+ strong).
	files := []FileAttributionOutput{
		{PrimaryEvidence: EvidenceExact, AIExactLines: 100},
		{PrimaryEvidence: EvidenceProviderTouch, AIModifiedLines: 5},
	}
	level, fallback := CommitEvidence(files)
	if level != "High" {
		t.Errorf("level = %q, want High (95%% strong)", level)
	}
	if fallback != 1 {
		t.Errorf("fallback = %d, want 1", fallback)
	}
}

func TestCommitEvidence_MediumWhenFallbackSignificant(t *testing.T) {
	// 60 exact + 40 provider-touch -> Medium (60% strong, 40% fallback).
	files := []FileAttributionOutput{
		{PrimaryEvidence: EvidenceExact, AIExactLines: 60},
		{PrimaryEvidence: EvidenceProviderTouch, AIModifiedLines: 40},
	}
	level, fallback := CommitEvidence(files)
	if level != "Medium" {
		t.Errorf("level = %q, want Medium", level)
	}
	if fallback != 1 {
		t.Errorf("fallback = %d, want 1", fallback)
	}
}

func TestCommitEvidence_LowWhenMostlyFallback(t *testing.T) {
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
	level, fallback := CommitEvidence(files)
	if level != "Low" {
		t.Errorf("level = %q, want Low", level)
	}
	if fallback != 1 {
		t.Errorf("fallback = %d, want 1", fallback)
	}
}

func TestCommitEvidence_HighIncludesNormalized(t *testing.T) {
	files := []FileAttributionOutput{
		{PrimaryEvidence: EvidenceExact, AIExactLines: 40},
		{PrimaryEvidence: EvidenceNormalized, AIFormattedLines: 10},
	}
	level, _ := CommitEvidence(files)
	if level != "High" {
		t.Errorf("level = %q, want High (normalized counts as high)", level)
	}
}

func TestCommitEvidence_SmallModifiedStaysHigh(t *testing.T) {
	// 998 exact + 2 modified -> High (99.8% strong).
	// This is the "1000 lines, 2 modified" scenario.
	files := []FileAttributionOutput{
		{PrimaryEvidence: EvidenceExact, AIExactLines: 998},
		{PrimaryEvidence: EvidenceModified, AIModifiedLines: 2},
	}
	level, fallback := CommitEvidence(files)
	if level != "High" {
		t.Errorf("level = %q, want High (2 modified out of 1000 should not drop evidence)", level)
	}
	if fallback != 0 {
		t.Errorf("fallback = %d, want 0 (modified is not fallback)", fallback)
	}
}

func TestCommitEvidence_NoAIFiles(t *testing.T) {
	files := []FileAttributionOutput{
		{PrimaryEvidence: EvidenceNone, HumanLines: 20},
		{PrimaryEvidence: EvidenceNone, HumanLines: 30},
	}
	level, fallback := CommitEvidence(files)
	if level != "" {
		t.Errorf("level = %q, want empty (no AI files)", level)
	}
	if fallback != 0 {
		t.Errorf("fallback = %d, want 0", fallback)
	}
}

func TestEvidenceExplanation(t *testing.T) {
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
		got := EvidenceExplanation(tt.level, tt.fallback)
		if got != tt.want {
			t.Errorf("EvidenceExplanation(%q, %d) = %q, want %q", tt.level, tt.fallback, got, tt.want)
		}
	}
}

// CommitEvidence walks AllEvidence so corroborating fallback classes
// (e.g. provider_touch on top of line-level modified) count toward the
// strength penalty even when a stronger class wins primary.
func TestCommitEvidence_FallbackCountFromAllEvidence(t *testing.T) {
	files := []FileAttributionOutput{
		{
			PrimaryEvidence: EvidenceModified,
			AIModifiedLines: 5,
			AllEvidence:     []EvidenceClass{EvidenceModified, EvidenceProviderTouch},
		},
	}
	level, fallback := CommitEvidence(files)
	if fallback != 1 {
		t.Errorf("fallback = %d, want 1 (provider_touch corroboration must count)", fallback)
	}
	// 5 modified lines + 1 provider_touch fallback -> score below 0.45 -> Low.
	if level == "High" {
		t.Errorf("level = %q, want a non-High label since fallback now applies", level)
	}
}

func TestCommitEvidence_NoDoubleCount(t *testing.T) {
	files := []FileAttributionOutput{
		{
			PrimaryEvidence: EvidenceModified,
			AIModifiedLines: 5,
			AllEvidence: []EvidenceClass{
				EvidenceModified,
				EvidenceProviderTouch,
				EvidenceCarryForward,
			},
		},
	}
	_, fallback := CommitEvidence(files)
	if fallback != 1 {
		t.Errorf("fallback = %d, want 1 (each file counts once across multiple fallback classes)", fallback)
	}
}

func TestCommitEvidence_PrimaryOnlyFallback_StillCounts(t *testing.T) {
	// Backward compat: when AllEvidence is empty (older callers), the
	// bucket selector falls back to PrimaryEvidence. A pure
	// provider_touch file must still count.
	files := []FileAttributionOutput{
		{PrimaryEvidence: EvidenceProviderTouch, AIModifiedLines: 0},
	}
	_, fallback := CommitEvidence(files)
	if fallback != 1 {
		t.Errorf("fallback = %d, want 1 (defense path for empty AllEvidence)", fallback)
	}
}

func TestCommitEvidence_LineLevelOnly_NoFallback(t *testing.T) {
	files := []FileAttributionOutput{
		{
			PrimaryEvidence: EvidenceExact,
			AIExactLines:    10,
			AllEvidence:     []EvidenceClass{EvidenceExact},
		},
	}
	level, fallback := CommitEvidence(files)
	if fallback != 0 {
		t.Errorf("fallback = %d, want 0 (no fallback class in AllEvidence)", fallback)
	}
	if level != "High" {
		t.Errorf("level = %q, want High", level)
	}
}

func TestSelectFallbackBucket_StrongestWins(t *testing.T) {
	// provider_touch is the strongest fallback in AllEvidence; the
	// bucket selector should pick it even when others are present.
	f := FileAttributionOutput{
		AllEvidence: []EvidenceClass{
			EvidenceModified,
			EvidenceCarryForward,
			EvidenceProviderTouch,
			EvidenceDeletion,
		},
	}
	got := selectFallbackBucket(f)
	if got != EvidenceProviderTouch {
		t.Errorf("got %q, want %q (provider_touch is strongest fallback)", got, EvidenceProviderTouch)
	}
}

func TestSelectFallbackBucket_NoneWhenNoFallback(t *testing.T) {
	f := FileAttributionOutput{
		AllEvidence: []EvidenceClass{EvidenceExact, EvidenceNormalized, EvidenceModified},
	}
	got := selectFallbackBucket(f)
	if got != EvidenceNone {
		t.Errorf("got %q, want EvidenceNone (no fallback class present)", got)
	}
}

func TestIsFallbackEvidence(t *testing.T) {
	cases := []struct {
		c    EvidenceClass
		want bool
	}{
		{EvidenceExact, false},
		{EvidenceNormalized, false},
		{EvidenceModified, false},
		{EvidenceProviderTouch, true},
		{EvidenceProviderCoarse, true},
		{EvidenceCarryForward, true},
		{EvidenceDeletion, true},
		{EvidenceNone, false},
	}
	for _, tc := range cases {
		if got := IsFallbackEvidence(tc.c); got != tc.want {
			t.Errorf("IsFallbackEvidence(%q) = %v, want %v", tc.c, got, tc.want)
		}
	}
}
