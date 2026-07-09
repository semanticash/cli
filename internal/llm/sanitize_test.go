package llm

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

// Valid UTF-8 must pass through unchanged. If sanitizeUTF8 mutated a
// clean input it would corrupt every prompt Semantica sends.
func TestSanitizeUTF8_ValidPassesUnchanged(t *testing.T) {
	cases := []string{
		"",
		"plain ascii",
		"café",           // é as a single 2-byte sequence
		"世界",         // 世界
		"line1\nline2\n",
		"emoji \U0001F600 end", // multi-byte outside BMP
	}
	for _, in := range cases {
		if got := sanitizeUTF8(in); got != in {
			t.Errorf("valid utf-8 mutated: %q -> %q", in, got)
		}
	}
}

// A single stray byte in the middle of an otherwise-valid string is
// replaced with U+FFFD; the valid prefix and suffix are preserved.
// This is the exact failure mode a truncation-mid-rune bug produces.
func TestSanitizeUTF8_ReplacesInvalidByteMidString(t *testing.T) {
	// "café" is c(0x63) a(0x61) f(0x66) é(0xc3 0xa9). Truncating after
	// the first é-byte leaves an incomplete sequence at the tail.
	in := "caf\xc3"
	got := sanitizeUTF8(in)
	if !utf8.ValidString(got) {
		t.Fatalf("sanitizer output not valid utf-8: %q", got)
	}
	if !strings.HasPrefix(got, "caf") {
		t.Errorf("valid prefix lost: %q", got)
	}
	if !strings.ContainsRune(got, '�') {
		t.Errorf("invalid byte not replaced with U+FFFD: %q", got)
	}
}

// An all-invalid input must still return a valid UTF-8 string. This
// bounds the worst case so a broken renderer never crashes the writer.
func TestSanitizeUTF8_AllInvalidIsReplaced(t *testing.T) {
	got := sanitizeUTF8("\xff\xfe\xfd")
	if !utf8.ValidString(got) {
		t.Fatalf("sanitizer output not valid utf-8: %q", got)
	}
	if got == "\xff\xfe\xfd" {
		t.Errorf("invalid bytes leaked through unchanged: %q", got)
	}
}

// GenerateText must sanitize after redaction so writers only ever
// see valid UTF-8. Without this, an upstream renderer that produces
// a mid-rune truncation makes codex error out explicitly and claude
// stall until its shell timeout.
func TestGenerateText_SanitizesInvalidUTF8BeforeDispatch(t *testing.T) {
	cap := &captureWriter{name: "test", model: "test", response: "ok"}
	r := NewWriterRegistry(cap)

	dirty := "hello caf\xc3 world"
	if _, err := r.GenerateText(context.Background(), dirty); err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if !utf8.ValidString(cap.captured) {
		t.Errorf("writer received invalid utf-8: %q", cap.captured)
	}
	if !strings.Contains(cap.captured, "hello caf") || !strings.Contains(cap.captured, "world") {
		t.Errorf("valid substrings lost during sanitization: %q", cap.captured)
	}
}

// Generate has the same sanitization guarantee as GenerateText. This
// covers narrative callers that use the parsed-result path.
func TestGenerate_SanitizesInvalidUTF8BeforeDispatch(t *testing.T) {
	cap := &captureWriter{name: "test", model: "test", response: stubResult}
	r := NewWriterRegistry(cap)

	dirty := "summarize caf\xc3 diff"
	if _, err := r.Generate(context.Background(), dirty); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !utf8.ValidString(cap.captured) {
		t.Errorf("writer received invalid utf-8: %q", cap.captured)
	}
}

// A clean prompt is not mutated by the sanitize step. Guards against
// a regression where sanitizeUTF8 accidentally rewrites something
// harmless (e.g. an aggressive normalization pass added later).
func TestGenerateText_ValidUTF8PassesThroughSanitizer(t *testing.T) {
	cap := &captureWriter{name: "test", model: "test", response: "ok"}
	r := NewWriterRegistry(cap)

	prompt := "user says: café with 世界 emoji \U0001F600"
	if _, err := r.GenerateText(context.Background(), prompt); err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if cap.captured != prompt {
		t.Errorf("valid utf-8 mutated:\n  got:  %q\n  want: %q", cap.captured, prompt)
	}
}
