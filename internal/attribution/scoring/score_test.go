package scoring

import (
	"strings"
	"testing"
)

func TestScoreFiles_ExactMatch(t *testing.T) {
	diff := ParseDiff([]byte(strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"--- /dev/null",
		"+++ b/main.go",
		"@@ -0,0 +1,2 @@",
		"+package main",
		"+func main() {}",
		"",
	}, "\n")))

	aiLines := map[string]map[string]struct{}{
		"main.go": {"package main": {}, "func main() {}": {}},
	}

	scores, stats := ScoreFiles(diff, aiLines, nil, map[string]string{"main.go": "claude_code"}, nil)

	if len(scores) != 1 {
		t.Fatalf("scores = %d, want 1", len(scores))
	}
	if scores[0].TotalLines != 2 {
		t.Errorf("TotalLines = %d, want 2", scores[0].TotalLines)
	}
	if scores[0].ExactLines != 2 {
		t.Errorf("ExactLines = %d, want 2", scores[0].ExactLines)
	}
	if stats.ExactMatches != 2 {
		t.Errorf("ExactMatches = %d, want 2", stats.ExactMatches)
	}
}

// TestScoreFiles_ProviderTouchOnly verifies the headline-exclusion
// contract for provider-touch-only files: non-blank added lines
// are counted in ProviderOnlyLines, not ModifiedLines.
func TestScoreFiles_ProviderTouchOnly(t *testing.T) {
	diff := ParseDiff([]byte(strings.Join([]string{
		"diff --git a/handler.go b/handler.go",
		"--- /dev/null",
		"+++ b/handler.go",
		"@@ -0,0 +1,2 @@",
		"+package api",
		"+func Handle() {}",
		"",
	}, "\n")))

	// No aiLines for handler.go, but provider touched it.
	scores, stats := ScoreFiles(diff, nil, map[string]string{"handler.go": "cursor"}, nil, nil)

	if len(scores) != 1 {
		t.Fatalf("scores = %d, want 1", len(scores))
	}
	if scores[0].ProviderOnlyLines != 2 {
		t.Errorf("ProviderOnlyLines = %d, want 2", scores[0].ProviderOnlyLines)
	}
	if scores[0].ModifiedLines != 0 {
		t.Errorf("ModifiedLines = %d, want 0 (provider-only lines must not become ModifiedLines)",
			scores[0].ModifiedLines)
	}
	if scores[0].ExactLines != 0 {
		t.Errorf("ExactLines = %d, want 0", scores[0].ExactLines)
	}
	if stats.ProviderOnlyMatches != 2 {
		t.Errorf("ProviderOnlyMatches = %d, want 2", stats.ProviderOnlyMatches)
	}
	if stats.ModifiedMatches != 0 {
		t.Errorf("ModifiedMatches = %d, want 0", stats.ModifiedMatches)
	}
	// Provider attribution is still recorded for the file, but
	// in the provider-only sidecar map so consumers can split
	// line-level vs provider-only per agent.
	if scores[0].ProviderOnlyLinesByProvider["cursor"] != 2 {
		t.Errorf("ProviderOnlyLinesByProvider[cursor] = %d, want 2",
			scores[0].ProviderOnlyLinesByProvider["cursor"])
	}
	if scores[0].ProviderLines["cursor"] != 0 {
		t.Errorf("ProviderLines[cursor] = %d, want 0 (provider-only kept out of line-level map)",
			scores[0].ProviderLines["cursor"])
	}
}

func TestScoreFiles_NormalizedMatch(t *testing.T) {
	diff := ParseDiff([]byte(strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"--- /dev/null",
		"+++ b/main.go",
		"@@ -0,0 +1,1 @@",
		"+func foo() {",
		"",
	}, "\n")))

	// Captured AI output has no space before the brace; the diff does.
	aiLines := map[string]map[string]struct{}{
		"main.go": {"func foo(){": {}},
	}

	scores, stats := ScoreFiles(diff, aiLines, nil, map[string]string{"main.go": "claude_code"}, nil)

	if scores[0].FormattedLines != 1 {
		t.Errorf("FormattedLines = %d, want 1", scores[0].FormattedLines)
	}
	if stats.NormalizedMatches != 1 {
		t.Errorf("NormalizedMatches = %d, want 1", stats.NormalizedMatches)
	}
}

func TestScoreFiles_GroupOverlap_ModifiedLines(t *testing.T) {
	// Group of 3 lines: first matches exactly, other two don't.
	// Because the group has overlap, the non-matching lines become AI-Modified.
	diff := ParseDiff([]byte(strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"--- /dev/null",
		"+++ b/main.go",
		"@@ -0,0 +1,3 @@",
		"+package main",
		"+import fmt",
		"+func main() {}",
		"",
	}, "\n")))

	aiLines := map[string]map[string]struct{}{
		"main.go": {"package main": {}},
	}

	scores, stats := ScoreFiles(diff, aiLines, nil, map[string]string{"main.go": "claude_code"}, nil)

	if scores[0].ExactLines != 1 {
		t.Errorf("ExactLines = %d, want 1", scores[0].ExactLines)
	}
	if scores[0].ModifiedLines != 2 {
		t.Errorf("ModifiedLines = %d, want 2", scores[0].ModifiedLines)
	}
	if stats.ExactMatches != 1 {
		t.Errorf("ExactMatches = %d, want 1", stats.ExactMatches)
	}
	if stats.ModifiedMatches != 2 {
		t.Errorf("ModifiedMatches = %d, want 2", stats.ModifiedMatches)
	}
}

func TestScoreFiles_HumanLines_NoOverlap(t *testing.T) {
	diff := ParseDiff([]byte(strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"--- /dev/null",
		"+++ b/main.go",
		"@@ -0,0 +1,2 @@",
		"+package main",
		"+func humanOnly() {}",
		"",
	}, "\n")))

	// No AI lines for this file.
	scores, _ := ScoreFiles(diff, nil, nil, nil, nil)

	if scores[0].HumanLines != 2 {
		t.Errorf("HumanLines = %d, want 2", scores[0].HumanLines)
	}
	if scores[0].ExactLines != 0 || scores[0].ModifiedLines != 0 {
		t.Error("expected no AI matches")
	}
}

// TestScoreFiles_MultiProviderPerLineAttribution guards the per-line
// provider attribution path: when LineProviders carries different
// providers for different lines in the same file, ProviderLines
// reflects the per-line ownership instead of crediting every match
// to the per-file fileProvider value. The fileProvider in the input
// intentionally points at a different provider so the expected
// counts come only from per-line ownership.
func TestScoreFiles_MultiProviderPerLineAttribution(t *testing.T) {
	diff := ParseDiff([]byte(strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"--- /dev/null",
		"+++ b/main.go",
		"@@ -0,0 +1,3 @@",
		"+package main",
		"+func main() {}",
		"+// added by codex",
		"",
	}, "\n")))

	aiLines := map[string]map[string]struct{}{
		"main.go": {"package main": {}, "func main() {}": {}, "// added by codex": {}},
	}
	lineProviders := map[string]map[string]map[string]struct{}{
		"main.go": {
			"package main":      {"claude_code": {}},
			"func main() {}":    {"claude_code": {}},
			"// added by codex": {"codex": {}},
		},
	}
	// fileProvider intentionally points at codex while per-line data
	// assigns two lines to claude_code, so the expected counts come
	// from line ownership instead of the file-level fallback.
	scores, _ := ScoreFiles(diff, aiLines, nil, map[string]string{"main.go": "codex"}, lineProviders)

	if len(scores) != 1 {
		t.Fatalf("scores = %d, want 1", len(scores))
	}
	if scores[0].ExactLines != 3 {
		t.Errorf("ExactLines = %d, want 3", scores[0].ExactLines)
	}
	if got := scores[0].ProviderLines["claude_code"]; got != 2 {
		t.Errorf("ProviderLines[claude_code] = %d, want 2", got)
	}
	if got := scores[0].ProviderLines["codex"]; got != 1 {
		t.Errorf("ProviderLines[codex] = %d, want 1", got)
	}
}

// TestScoreFiles_SharedLineCreditsBothProviders covers the case
// where two providers each emitted the same line (e.g. an identical
// import statement): both providers get credit for the single
// matched diff line. Per-provider counts can therefore sum to more
// than ExactLines for a file - they measure involvement per line,
// not exclusive ownership.
func TestScoreFiles_SharedLineCreditsBothProviders(t *testing.T) {
	diff := ParseDiff([]byte(strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"--- /dev/null",
		"+++ b/main.go",
		"@@ -0,0 +1,1 @@",
		"+import \"fmt\"",
		"",
	}, "\n")))

	aiLines := map[string]map[string]struct{}{
		"main.go": {`import "fmt"`: {}},
	}
	lineProviders := map[string]map[string]map[string]struct{}{
		"main.go": {
			`import "fmt"`: {"claude_code": {}, "codex": {}},
		},
	}
	scores, _ := ScoreFiles(diff, aiLines, nil, nil, lineProviders)

	if len(scores) != 1 {
		t.Fatalf("scores = %d, want 1", len(scores))
	}
	if scores[0].ExactLines != 1 {
		t.Errorf("ExactLines = %d, want 1 (one matched line)", scores[0].ExactLines)
	}
	if got := scores[0].ProviderLines["claude_code"]; got != 1 {
		t.Errorf("ProviderLines[claude_code] = %d, want 1", got)
	}
	if got := scores[0].ProviderLines["codex"]; got != 1 {
		t.Errorf("ProviderLines[codex] = %d, want 1 (shared line credits both)", got)
	}
}

// TestScoreFiles_LineProvidersFallsBackToFileProvider covers the
// nil-LineProviders path: when a caller passes nil for the per-line
// map (e.g. candidate rows that carry no per-line provider data),
// every matched line credits the per-file fileProvider value so
// attribution is preserved rather than silently dropped.
func TestScoreFiles_LineProvidersFallsBackToFileProvider(t *testing.T) {
	diff := ParseDiff([]byte(strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"--- /dev/null",
		"+++ b/main.go",
		"@@ -0,0 +1,2 @@",
		"+package main",
		"+func main() {}",
		"",
	}, "\n")))

	aiLines := map[string]map[string]struct{}{
		"main.go": {"package main": {}, "func main() {}": {}},
	}
	scores, _ := ScoreFiles(diff, aiLines, nil, map[string]string{"main.go": "claude_code"}, nil)

	if scores[0].ProviderLines["claude_code"] != 2 {
		t.Errorf("ProviderLines[claude_code] = %d, want 2 (fileProvider fallback when LineProviders is nil)",
			scores[0].ProviderLines["claude_code"])
	}
}

// TestScoreFiles_ModifiedLinesCreditHunkProviders verifies that
// tier-3 (modified) lines are credited to the providers that own the
// matched-tier neighbours inside the same hunk, not to the per-file
// fallback.
func TestScoreFiles_ModifiedLinesCreditHunkProviders(t *testing.T) {
	// Line 1 anchors ownership for the hunk; lines 2 and 3 are
	// modified-tier lines that inherit that hunk ownership.
	diff := ParseDiff([]byte(strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"--- /dev/null",
		"+++ b/main.go",
		"@@ -0,0 +1,3 @@",
		"+package main",
		"+import fmt",
		"+func main() {}",
		"",
	}, "\n")))

	aiLines := map[string]map[string]struct{}{
		"main.go": {"package main": {}},
	}
	lineProviders := map[string]map[string]map[string]struct{}{
		"main.go": {
			"package main": {"claude_code": {}},
		},
	}
	scores, _ := ScoreFiles(diff, aiLines, nil, map[string]string{"main.go": "codex"}, lineProviders)

	if len(scores) != 1 {
		t.Fatalf("scores = %d, want 1", len(scores))
	}
	if scores[0].ExactLines != 1 {
		t.Errorf("ExactLines = %d, want 1", scores[0].ExactLines)
	}
	if scores[0].ModifiedLines != 2 {
		t.Errorf("ModifiedLines = %d, want 2", scores[0].ModifiedLines)
	}
	if got := scores[0].ProviderLines["claude_code"]; got != 3 {
		t.Errorf("ProviderLines[claude_code] = %d, want 3 (1 exact + 2 modified credited to hunk owner)", got)
	}
	if got := scores[0].ProviderLines["codex"]; got != 0 {
		t.Errorf("ProviderLines[codex] = %d, want 0 (modified lines must not fall back to fileProvider when hunk has ownership)", got)
	}
}

// TestScoreFiles_ModifiedLinesFallBackWhenHunkHasNoOwnership covers
// the safety net for hunks where the matched-tier lines have no
// per-line provider data (older candidates, mixed sources). Tier-3
// lines in such hunks fall back to fileProvider so attribution is
// preserved rather than dropped.
func TestScoreFiles_ModifiedLinesFallBackWhenHunkHasNoOwnership(t *testing.T) {
	diff := ParseDiff([]byte(strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"--- /dev/null",
		"+++ b/main.go",
		"@@ -0,0 +1,2 @@",
		"+package main",
		"+other line",
		"",
	}, "\n")))

	aiLines := map[string]map[string]struct{}{
		"main.go": {"package main": {}},
	}
	// No lineProviders passed, simulating older candidates.
	scores, _ := ScoreFiles(diff, aiLines, nil, map[string]string{"main.go": "claude_code"}, nil)

	if scores[0].ModifiedLines != 1 {
		t.Errorf("ModifiedLines = %d, want 1", scores[0].ModifiedLines)
	}
	if got := scores[0].ProviderLines["claude_code"]; got != 2 {
		t.Errorf("ProviderLines[claude_code] = %d, want 2 (fallback credits per-file provider)", got)
	}
}
