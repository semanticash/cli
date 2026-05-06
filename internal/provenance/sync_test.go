package provenance

import (
	"errors"
	"strings"
	"testing"
)

func TestFormatLoadOrRedactReason_RedactErrorHasStablePrefix(t *testing.T) {
	got := formatLoadOrRedactReason("prompt", "abc12345deadbeef", &redactError{err: errors.New("init failed")})
	const want = "redaction failed: prompt: init failed"
	if got != want {
		t.Errorf("formatLoadOrRedactReason(redact) = %q, want %q", got, want)
	}
}

func TestFormatLoadOrRedactReason_RedactErrorAcrossKinds(t *testing.T) {
	cases := []string{"prompt", "step_provenance", "bundle"}
	for _, kind := range cases {
		t.Run(kind, func(t *testing.T) {
			got := formatLoadOrRedactReason(kind, "abc12345", &redactError{err: errors.New("apply failed")})
			wantPrefix := "redaction failed: " + kind + ": "
			if !strings.HasPrefix(got, wantPrefix) {
				t.Errorf("formatLoadOrRedactReason(%s) = %q, want prefix %q", kind, got, wantPrefix)
			}
		})
	}
}

func TestFormatLoadOrRedactReason_LoadErrorIsNotConflatedWithRedaction(t *testing.T) {
	got := formatLoadOrRedactReason("step_provenance", "deadbeef12345678", &loadError{err: errors.New("not found")})
	if strings.HasPrefix(got, "redaction failed:") {
		t.Errorf("load error should not use redaction-failed prefix, got %q", got)
	}
	if !strings.Contains(got, "deadbeef") {
		t.Errorf("expected hash prefix in load-error reason, got %q", got)
	}
}

func TestFormatLoadOrRedactReason_TruncatesHashTo8Chars(t *testing.T) {
	got := formatLoadOrRedactReason("step_provenance", "0123456789abcdef", &loadError{err: errors.New("not found")})
	if !strings.Contains(got, "01234567 ") {
		t.Errorf("expected 8-char hash prefix in reason, got %q", got)
	}
	if strings.Contains(got, "0123456789") {
		t.Errorf("hash should be truncated to 8 chars, got %q", got)
	}
}

func TestFormatLoadOrRedactReason_ShortHashNotTruncated(t *testing.T) {
	got := formatLoadOrRedactReason("prompt", "abc", &loadError{err: errors.New("not found")})
	if !strings.Contains(got, " abc ") {
		t.Errorf("short hash should be passed through, got %q", got)
	}
}

func TestFormatLoadOrRedactReason_UntaggedErrorFallback(t *testing.T) {
	got := formatLoadOrRedactReason("prompt", "abc12345", errors.New("something else"))
	if strings.HasPrefix(got, "redaction failed:") {
		t.Errorf("untagged error should not use redaction-failed prefix, got %q", got)
	}
}

func TestRedactionFailedReason_StablePrefix(t *testing.T) {
	cases := []struct {
		kind string
		want string
	}{
		{"prompt", "redaction failed: prompt: boom"},
		{"step_provenance", "redaction failed: step_provenance: boom"},
		{"bundle", "redaction failed: bundle: boom"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			got := redactionFailedReason(tc.kind, errors.New("boom"))
			if got != tc.want {
				t.Errorf("redactionFailedReason(%q, boom) = %q, want %q", tc.kind, got, tc.want)
			}
		})
	}
}

func TestFormatLoadOrRedactReason_RoutesRedactErrorsThroughHelper(t *testing.T) {
	wrapped := errors.New("apply failed")
	got := formatLoadOrRedactReason("prompt", "abc12345", &redactError{err: wrapped})
	want := redactionFailedReason("prompt", wrapped)
	if got != want {
		t.Errorf("formatLoadOrRedactReason redact path = %q, want %q (must equal redactionFailedReason output)", got, want)
	}
}
