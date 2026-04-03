package llm

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/redact"
)

func sampleSlackWebhook() string {
	return "https://hooks.slack.com/" +
		"services/" +
		"T01234567/" +
		"B01234567/" +
		"xyzXYZ1234567890abcdefgh"
}

// captureProvider records the prompt it receives, for verifying redaction.
type captureProvider struct {
	captured string
}

func (c *captureProvider) runText(_ context.Context, _, prompt string) (string, error) {
	c.captured = prompt
	return "mock response", nil
}

func TestGenerateText_RedactsSecretInPrompt(t *testing.T) {
	cap := &captureProvider{}

	// Temporarily replace providers with our capture provider.
	origProviders := providers
	providers = []llmProvider{{
		name:    "test",
		model:   "test",
		find:    func() string { return "/usr/bin/true" },
		runText: cap.runText,
	}}
	defer func() { providers = origProviders }()

	prompt := "Analyze this diff:\n+slack = " + sampleSlackWebhook() + "\n+normal code here"

	_, err := GenerateText(context.Background(), prompt)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(cap.captured, "hooks.slack.com/services/T01234567") {
		t.Error("secret was not redacted before reaching provider")
	}
	if !strings.Contains(cap.captured, "[REDACTED]") {
		t.Error("expected [REDACTED] in prompt sent to provider")
	}
	if !strings.Contains(cap.captured, "normal code here") {
		t.Error("non-secret content was incorrectly removed")
	}
}

func TestGenerateText_SafePromptUnchanged(t *testing.T) {
	cap := &captureProvider{}

	origProviders := providers
	providers = []llmProvider{{
		name:    "test",
		model:   "test",
		find:    func() string { return "/usr/bin/true" },
		runText: cap.runText,
	}}
	defer func() { providers = origProviders }()

	prompt := "Analyze this diff:\n+func main() { fmt.Println(\"hello\") }\n"

	_, err := GenerateText(context.Background(), prompt)
	if err != nil {
		t.Fatal(err)
	}

	if cap.captured != prompt {
		t.Errorf("safe prompt was modified:\n  got:  %q\n  want: %q", cap.captured, prompt)
	}
}

func TestGenerate_RedactsSecretInPrompt(t *testing.T) {
	origProviders := providers
	cap2 := &captureProvider{}
	cap2Fn := func(_ context.Context, _, prompt string) (string, error) {
		cap2.captured = prompt
		return `{"title":"test","intent":"test","outcome":"test","learnings":[],"friction":[],"open_items":[],"keywords":[]}`, nil
	}
	providers = []llmProvider{{
		name:    "test",
		model:   "test",
		find:    func() string { return "/usr/bin/true" },
		runText: cap2Fn,
	}}
	defer func() { providers = origProviders }()

	prompt := "Summarize:\n-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA0Z3VS5JJcds3xfn/ygWyF8PbnGcY5unA67hFdJBEEH6kMRMD\n-----END RSA PRIVATE KEY-----\n"

	_, err := Generate(context.Background(), prompt)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(cap2.captured, "MIIEpAIBAAK") {
		t.Error("private key was not redacted before reaching provider in Generate()")
	}
	if !strings.Contains(cap2.captured, "[REDACTED]") {
		t.Error("expected [REDACTED] in prompt sent to provider")
	}
}

func TestGenerateText_FailClosed_OnDetectorInitError(t *testing.T) {
	cleanup := redact.ForceInitError(fmt.Errorf("forced detector failure"))
	defer cleanup()

	origProviders := providers
	providers = []llmProvider{{
		name:  "test",
		model: "test",
		find:  func() string { return "/usr/bin/true" },
		runText: func(_ context.Context, _, _ string) (string, error) {
			t.Fatal("provider should not be called when redaction fails")
			return "", nil
		},
	}}
	defer func() { providers = origProviders }()

	_, err := GenerateText(context.Background(), "any prompt content")
	if err == nil {
		t.Fatal("expected error when redactor init fails")
	}
	if !strings.Contains(err.Error(), "egress redaction failed") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestGenerate_FailClosed_OnDetectorInitError(t *testing.T) {
	cleanup := redact.ForceInitError(fmt.Errorf("forced detector failure"))
	defer cleanup()

	origProviders := providers
	providers = []llmProvider{{
		name:  "test",
		model: "test",
		find:  func() string { return "/usr/bin/true" },
		runText: func(_ context.Context, _, _ string) (string, error) {
			t.Fatal("provider should not be called when redaction fails")
			return "", nil
		},
	}}
	defer func() { providers = origProviders }()

	_, err := Generate(context.Background(), "any prompt content")
	if err == nil {
		t.Fatal("expected error when redactor init fails")
	}
	if !strings.Contains(err.Error(), "egress redaction failed") {
		t.Errorf("unexpected error message: %v", err)
	}
}
