package service

import "testing"

func TestSanitizeCommitMessageRemovesBackticks(t *testing.T) {
	input := "Replaces silent `_ =` discards with `t.Fatalf` in hook tests"
	got := sanitizeCommitMessage(input)
	want := "Replaces silent _ = discards with t.Fatalf in hook tests"

	if got != want {
		t.Fatalf("sanitizeCommitMessage() = %q, want %q", got, want)
	}
}

func TestSanitizeCommitMessageStripsCodeFences(t *testing.T) {
	input := "```Fixes hook tests```\n\nRemoves ignored setup failures"
	got := sanitizeCommitMessage(input)
	want := "Fixes hook tests Removes ignored setup failures"

	if got != want {
		t.Fatalf("sanitizeCommitMessage() = %q, want %q", got, want)
	}
}

func TestSanitizeCommitMessageFlattensMultilineOutput(t *testing.T) {
	input := "\n\nFixes hook tests.\n\nExplains extra details in a body."
	got := sanitizeCommitMessage(input)
	want := "Fixes hook tests. Explains extra details in a body."

	if got != want {
		t.Fatalf("sanitizeCommitMessage() = %q, want %q", got, want)
	}
}

func TestSanitizeCommitMessageCollapsesMultilineSentence(t *testing.T) {
	input := "Fixes hook tests.\nExplains extra details\nacross two lines.\n\n"
	got := sanitizeCommitMessage(input)
	want := "Fixes hook tests. Explains extra details across two lines."

	if got != want {
		t.Fatalf("sanitizeCommitMessage() = %q, want %q", got, want)
	}
}

func TestSanitizeCommitMessageLimitsToTwoSentences(t *testing.T) {
	input := "Extracts executable resolution into shared util. Expands CI matrix. Adds routing checks to mesh."
	got := sanitizeCommitMessage(input)
	want := "Extracts executable resolution into shared util. Expands CI matrix."

	if got != want {
		t.Fatalf("sanitizeCommitMessage() = %q, want %q", got, want)
	}
}
