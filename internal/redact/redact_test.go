package redact

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/zricethezav/gitleaks/v8/detect"
)

func sampleSlackWebhook() string {
	return "https://hooks.slack.com/" +
		"services/" +
		"T00000000/" +
		"B00000000/" +
		"XXXXXXXXXXXXXXXXXXXXXXXX"
}

// resetDetector resets the singleton so each test gets a clean init.
func resetDetector(t *testing.T) {
	t.Helper()
	detector = nil
	initOnce = sync.Once{}
	initErr = nil
	newDetectorFn = defaultNewDetector
}

func TestRedact_GenericAPIKey(t *testing.T) {
	resetDetector(t)
	// generic-api-key rule matches sk- prefixed high-entropy strings.
	input := "+api_key = sk-1234567890abcdef1234567890abcdef"
	got, err := String(input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "sk-1234567890abcdef") {
		t.Errorf("API key not redacted: %s", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("expected [REDACTED] token in output: %s", got)
	}
}

func TestRedact_PrivateKey(t *testing.T) {
	resetDetector(t)
	input := `some text
-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEA0Z3VS5JJcds3xfn/ygWyF8PbnGcY5unA67hFdJBEEH6kMRMD
-----END RSA PRIVATE KEY-----
more text`
	got, err := String(input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "MIIEpAIBAAK") {
		t.Errorf("private key not redacted: %s", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("expected [REDACTED] token in output: %s", got)
	}
}

func TestRedact_SlackWebhook(t *testing.T) {
	resetDetector(t)
	input := "url: " + sampleSlackWebhook()
	got, err := String(input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "hooks.slack.com/services/T00000000") {
		t.Errorf("Slack webhook not redacted: %s", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("expected [REDACTED] token in output: %s", got)
	}
}

func TestRedact_SafeString(t *testing.T) {
	resetDetector(t)
	input := "func main() {\n\tfmt.Println(\"hello world\")\n}"
	got, err := String(input)
	if err != nil {
		t.Fatal(err)
	}
	if got != input {
		t.Errorf("safe string was modified:\n  got:  %q\n  want: %q", got, input)
	}
}

func TestRedact_MultipleSecrets(t *testing.T) {
	resetDetector(t)
	input := "+api_key = sk-1234567890abcdef1234567890abcdef\n+webhook = " + sampleSlackWebhook()
	got, err := String(input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "sk-1234567890abcdef") {
		t.Errorf("API key not redacted: %s", got)
	}
	if strings.Contains(got, "hooks.slack.com/services/T00000000") {
		t.Errorf("Slack webhook not redacted: %s", got)
	}
	count := strings.Count(got, "[REDACTED]")
	if count < 2 {
		t.Errorf("expected at least 2 [REDACTED] tokens, got %d: %s", count, got)
	}
}

func TestRedact_RepeatedSecret(t *testing.T) {
	resetDetector(t)
	secret := sampleSlackWebhook()
	input := fmt.Sprintf("primary=%s backup=%s", secret, secret)
	got, err := String(input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "hooks.slack.com/services/T00000000") {
		t.Errorf("repeated secret not fully redacted: %s", got)
	}
	count := strings.Count(got, "[REDACTED]")
	if count != 2 {
		t.Errorf("expected 2 [REDACTED] tokens for repeated secret, got %d: %s", count, got)
	}
}

func TestRedact_EmptyString(t *testing.T) {
	resetDetector(t)
	got, err := String("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestRedact_FilePaths(t *testing.T) {
	resetDetector(t)
	input := "modified: internal/service/attribution.go\ndeleted: docs/old-guide.md"
	got, err := String(input)
	if err != nil {
		t.Fatal(err)
	}
	if got != input {
		t.Errorf("file paths were modified:\n  got:  %q\n  want: %q", got, input)
	}
}

func TestRedact_InitFailure_ReturnsError(t *testing.T) {
	resetDetector(t)
	newDetectorFn = func() (*detect.Detector, error) {
		return nil, fmt.Errorf("forced init failure")
	}

	_, err := String("any content")
	if err == nil {
		t.Fatal("expected error when detector init fails")
	}
	if !strings.Contains(err.Error(), "forced init failure") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestBytes(t *testing.T) {
	resetDetector(t)
	input := []byte("url: " + sampleSlackWebhook())
	got, err := Bytes(input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "hooks.slack.com/services/T00000000") {
		t.Errorf("Bytes did not redact: %s", got)
	}
}

func TestBytes_NoOpReturnsSameSlice(t *testing.T) {
	resetDetector(t)
	input := []byte("func main() { fmt.Println(\"hello\") }")
	got, err := Bytes(input)
	if err != nil {
		t.Fatal(err)
	}
	if &got[0] != &input[0] {
		t.Error("expected returned slice to share backing array with input on no-op path")
	}
}

// --- SanitizeURL tests ---

func TestSanitizeURL_WithCredentials(t *testing.T) {
	input := "https://user:ghp_secrettoken@github.com/org/repo.git"
	got := SanitizeURL(input)
	if strings.Contains(got, "user") || strings.Contains(got, "ghp_secrettoken") {
		t.Errorf("credentials not stripped: %s", got)
	}
	if !strings.Contains(got, "github.com/org/repo.git") {
		t.Errorf("host/path lost: %s", got)
	}
}

func TestSanitizeURL_WithoutCredentials(t *testing.T) {
	input := "https://github.com/org/repo.git"
	got := SanitizeURL(input)
	if got != input {
		t.Errorf("URL modified: got %q, want %q", got, input)
	}
}

func TestSanitizeURL_SSHUrl(t *testing.T) {
	input := "git@github.com:org/repo.git"
	got := SanitizeURL(input)
	if got != input {
		t.Errorf("SSH URL modified: got %q, want %q", got, input)
	}
}

func TestSanitizeURL_QueryStringStripped(t *testing.T) {
	input := "https://github.com/org/repo.git?token=secret123"
	got := SanitizeURL(input)
	if strings.Contains(got, "token") || strings.Contains(got, "secret123") {
		t.Errorf("query string not stripped: %s", got)
	}
	if !strings.Contains(got, "github.com/org/repo.git") {
		t.Errorf("host/path lost: %s", got)
	}
}

func TestSanitizeURL_FragmentStripped(t *testing.T) {
	input := "https://github.com/org/repo.git#access_token=secret"
	got := SanitizeURL(input)
	if strings.Contains(got, "access_token") || strings.Contains(got, "secret") {
		t.Errorf("fragment not stripped: %s", got)
	}
}

func TestSanitizeURL_AllComponentsStripped(t *testing.T) {
	input := "https://user:pass@github.com/org/repo.git?token=abc#frag"
	got := SanitizeURL(input)
	if strings.Contains(got, "user") || strings.Contains(got, "pass") ||
		strings.Contains(got, "token") || strings.Contains(got, "frag") {
		t.Errorf("credentials/query/fragment not fully stripped: %s", got)
	}
	if got != "https://github.com/org/repo.git" {
		t.Errorf("got %q, want %q", got, "https://github.com/org/repo.git")
	}
}

func TestSanitizeURL_InvalidInput(t *testing.T) {
	input := "not a url at all"
	got := SanitizeURL(input)
	if got != input {
		t.Errorf("non-URL modified: got %q, want %q", got, input)
	}
}

// --- Span merging tests ---

func TestMergeSpans_Overlapping(t *testing.T) {
	spans := []span{{0, 10}, {5, 15}, {20, 30}}
	merged := mergeSpans(spans)
	if len(merged) != 2 {
		t.Fatalf("expected 2 merged spans, got %d: %v", len(merged), merged)
	}
	if merged[0].start != 0 || merged[0].end != 15 {
		t.Errorf("first span: got [%d,%d), want [0,15)", merged[0].start, merged[0].end)
	}
	if merged[1].start != 20 || merged[1].end != 30 {
		t.Errorf("second span: got [%d,%d), want [20,30)", merged[1].start, merged[1].end)
	}
}

func TestMergeSpans_Adjacent(t *testing.T) {
	spans := []span{{0, 10}, {10, 20}}
	merged := mergeSpans(spans)
	if len(merged) != 1 {
		t.Fatalf("expected 1 merged span, got %d: %v", len(merged), merged)
	}
	if merged[0].start != 0 || merged[0].end != 20 {
		t.Errorf("span: got [%d,%d), want [0,20)", merged[0].start, merged[0].end)
	}
}
