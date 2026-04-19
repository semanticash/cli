package builder

import (
	"strings"
	"testing"
)

// --- TruncateWithEllipsis ---

func TestTruncateWithEllipsis_ShortInputUnchanged(t *testing.T) {
	if got := TruncateWithEllipsis("hello", 200); got != "hello" {
		t.Errorf("TruncateWithEllipsis = %q, want hello", got)
	}
}

func TestTruncateWithEllipsis_ExactlyMaxUnchanged(t *testing.T) {
	s := strings.Repeat("a", 200)
	if got := TruncateWithEllipsis(s, 200); got != s {
		t.Errorf("TruncateWithEllipsis with len==max should return input unchanged")
	}
}

func TestTruncateWithEllipsis_OverflowAddsEllipsis(t *testing.T) {
	s := strings.Repeat("a", 250)
	got := TruncateWithEllipsis(s, 200)
	if len(got) != 200+len("...") {
		t.Errorf("truncated length = %d, want 203", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncated output should end with '...', got %q", got)
	}
}

// Whitespace is never normalized by this variant; the caller gets
// exactly what they passed in (up to the length cap).
func TestTruncateWithEllipsis_PreservesWhitespace(t *testing.T) {
	got := TruncateWithEllipsis("  hello\nworld  ", 200)
	if got != "  hello\nworld  " {
		t.Errorf("TruncateWithEllipsis = %q, want the input verbatim", got)
	}
}

// --- TruncateClean ---

func TestTruncateClean_TrimsLeadingAndTrailing(t *testing.T) {
	if got := TruncateClean("  hello  ", 200); got != "hello" {
		t.Errorf("TruncateClean = %q, want hello", got)
	}
}

func TestTruncateClean_NewlineToSpace(t *testing.T) {
	if got := TruncateClean("hello\nworld", 200); got != "hello world" {
		t.Errorf("TruncateClean = %q, want 'hello world'", got)
	}
}

// Carriage returns are dropped with no substitution. This means
// "a\rb" becomes "ab", not "a b". The asymmetry with '\n' is
// intentional and preserved from the original agentcopilot.Truncate.
func TestTruncateClean_CarriageReturnStripped(t *testing.T) {
	if got := TruncateClean("hello\rworld", 200); got != "helloworld" {
		t.Errorf("TruncateClean = %q, want 'helloworld' (no space)", got)
	}
}

// Combining newline and carriage return exercises both rules at
// once. Locked in so a future refactor cannot make them symmetric
// without the test failing.
func TestTruncateClean_NewlineAndCarriageReturn(t *testing.T) {
	got := TruncateClean("  hello\nworld\rand more  ", 200)
	want := "hello worldand more"
	if got != want {
		t.Errorf("TruncateClean = %q, want %q", got, want)
	}
}

func TestTruncateClean_OverflowNoEllipsis(t *testing.T) {
	s := strings.Repeat("a", 250)
	got := TruncateClean(s, 200)
	if len(got) != 200 {
		t.Errorf("truncated length = %d, want 200", len(got))
	}
	if strings.HasSuffix(got, "...") {
		t.Errorf("TruncateClean must not append ellipsis; got %q", got)
	}
}

// --- Redact ---

// Empty input is returned unchanged without invoking the redactor,
// so call sites do not have to guard for the empty case themselves.
func TestRedact_EmptyPassthrough(t *testing.T) {
	if got := Redact(""); got != "" {
		t.Errorf("Redact('') = %q, want empty", got)
	}
}

// A value containing a GitHub token must be redacted (the token
// substring should not appear in the output).
func TestRedact_TokenScrubbed(t *testing.T) {
	in := "curl -H 'Authorization: Bearer ghp_1234567890abcdef1234567890abcdef12345678' /"
	got := Redact(in)
	if strings.Contains(got, "ghp_1234567890abcdef") {
		t.Errorf("Redact did not scrub the token: %q", got)
	}
	if got == in {
		t.Errorf("Redact returned the input unchanged; expected a difference")
	}
}

// Plain strings with no secrets pass through unchanged.
func TestRedact_CleanInputUnchanged(t *testing.T) {
	in := "go test ./..."
	if got := Redact(in); got != in {
		t.Errorf("Redact(clean) = %q, want input unchanged", got)
	}
}
