package service

import (
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

func TestRedactPushPayload_CommitSubjectWithSecret(t *testing.T) {
	p := &remotePushPayload{
		RemoteURL:     "https://github.com/org/repo.git",
		CommitHash:    "31533c04e617cdcc093ba44eff7c8e315488e62e",
		CommitSubject: "fix: update webhook " + sampleSlackWebhook(),
		Branch:        "main",
		AILines:       100,
		HumanLines:    10,
		TotalLines:    110,
		Files: []FileAttribution{
			{Path: "internal/service/worker.go", TotalLines: 50},
		},
	}

	if err := redactPushPayload(p); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(p.CommitSubject, "hooks.slack.com") {
		t.Errorf("secret in commit subject not redacted: %s", p.CommitSubject)
	}
	if !strings.Contains(p.CommitSubject, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in commit subject: %s", p.CommitSubject)
	}
}

func TestRedactPushPayload_PlaybookJSONWithSecret(t *testing.T) {
	p := &remotePushPayload{
		CommitHash:   "abc123",
		PlaybookJSON: []byte(`{"intent":"Added webhook integration with ` + sampleSlackWebhook() + `"}`),
	}

	if err := redactPushPayload(p); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(p.PlaybookJSON), "hooks.slack.com") {
		t.Errorf("secret in playbook JSON not redacted: %s", string(p.PlaybookJSON))
	}
}

func TestRedactPushPayload_SafeFieldsUnchanged(t *testing.T) {
	p := &remotePushPayload{
		RemoteURL:      "https://github.com/org/repo.git",
		CommitHash:     "31533c04e617cdcc093ba44eff7c8e315488e62e",
		CommitSubject:  "Adds carry-forward attribution",
		Branch:         "feat/check-runs",
		AILines:        1617,
		HumanLines:     11,
		TotalLines:     1628,
		FilesTotal:     9,
		FilesAITouched: 5,
		Providers:      []string{"claude_code"},
		Files: []FileAttribution{
			{Path: "internal/service/attribution.go", TotalLines: 374},
			{Path: "internal/service/attribution_test.go", TotalLines: 1012},
		},
		Diagnostics: AttributionDiagnostics{
			EventsConsidered: 649,
			ExactMatches:     1586,
		},
	}

	origSubject := p.CommitSubject
	origBranch := p.Branch
	origHash := p.CommitHash

	if err := redactPushPayload(p); err != nil {
		t.Fatal(err)
	}

	if p.CommitSubject != origSubject {
		t.Errorf("safe commit subject changed: %q -> %q", origSubject, p.CommitSubject)
	}
	if p.Branch != origBranch {
		t.Errorf("branch changed: %q -> %q", origBranch, p.Branch)
	}
	if p.CommitHash != origHash {
		t.Errorf("commit hash changed: %q -> %q", origHash, p.CommitHash)
	}
	if p.AILines != 1617 || p.HumanLines != 11 {
		t.Error("numeric fields changed")
	}
	if len(p.Files) != 2 || p.Files[0].Path != "internal/service/attribution.go" {
		t.Error("file paths changed")
	}
	if p.Diagnostics.EventsConsidered != 649 {
		t.Error("diagnostics changed")
	}
}

func TestRedactPushPayload_FailClosed_OnDetectorInitError(t *testing.T) {
	cleanup := redact.ForceInitError(fmt.Errorf("forced detector failure"))
	defer cleanup()

	p := &remotePushPayload{
		CommitHash:    "abc123",
		CommitSubject: "some commit message",
	}

	err := redactPushPayload(p)
	if err == nil {
		t.Fatal("expected error when redactor init fails")
	}
	if !strings.Contains(err.Error(), "forced detector failure") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSanitizeURL_InPushPayload(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "userinfo_stripped",
			input:    "https://user:ghp_token123@github.com/org/repo.git",
			expected: "https://github.com/org/repo.git",
		},
		{
			name:     "query_stripped",
			input:    "https://github.com/org/repo.git?token=secret",
			expected: "https://github.com/org/repo.git",
		},
		{
			name:     "fragment_stripped",
			input:    "https://github.com/org/repo.git#access_token=x",
			expected: "https://github.com/org/repo.git",
		},
		{
			name:     "ssh_unchanged",
			input:    "git@github.com:org/repo.git",
			expected: "git@github.com:org/repo.git",
		},
		{
			name:     "clean_https_unchanged",
			input:    "https://github.com/org/repo.git",
			expected: "https://github.com/org/repo.git",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redact.SanitizeURL(tc.input)
			if got != tc.expected {
				t.Errorf("SanitizeURL(%q) = %q, want %q", tc.input, got, tc.expected)
			}
		})
	}
}
