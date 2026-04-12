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

	scores, stats := ScoreFiles(diff, aiLines, nil, map[string]string{"main.go": "claude_code"})

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
	scores, stats := ScoreFiles(diff, nil, map[string]string{"handler.go": "cursor"}, nil)

	if len(scores) != 1 {
		t.Fatalf("scores = %d, want 1", len(scores))
	}
	if scores[0].ModifiedLines != 2 {
		t.Errorf("ModifiedLines = %d, want 2", scores[0].ModifiedLines)
	}
	if scores[0].ExactLines != 0 {
		t.Errorf("ExactLines = %d, want 0", scores[0].ExactLines)
	}
	if stats.ModifiedMatches != 2 {
		t.Errorf("ModifiedMatches = %d, want 2", stats.ModifiedMatches)
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

	// AI wrote "func foo(){" (no space before brace), diff has "func foo() {".
	aiLines := map[string]map[string]struct{}{
		"main.go": {"func foo(){": {}},
	}

	scores, stats := ScoreFiles(diff, aiLines, nil, map[string]string{"main.go": "claude_code"})

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

	scores, stats := ScoreFiles(diff, aiLines, nil, map[string]string{"main.go": "claude_code"})

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
	scores, _ := ScoreFiles(diff, nil, nil, nil)

	if scores[0].HumanLines != 2 {
		t.Errorf("HumanLines = %d, want 2", scores[0].HumanLines)
	}
	if scores[0].ExactLines != 0 || scores[0].ModifiedLines != 0 {
		t.Error("expected no AI matches")
	}
}
