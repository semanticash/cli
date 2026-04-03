package redact

import (
	"strings"
	"sync"
	"testing"
)

// resetForCorpus resets the detector singleton for corpus tests.
func resetForCorpus(t *testing.T) {
	t.Helper()
	detector = nil
	initOnce = sync.Once{}
	initErr = nil
	newDetectorFn = defaultNewDetector
}

func TestCorpus(t *testing.T) {
	resetForCorpus(t)

	type corpusEntry struct {
		name         string
		input        string
		shouldRedact bool // true = expect [REDACTED] in output
	}

	corpus := []corpusEntry{
		{
			name:         "slack_webhook",
			input:        "https://hooks.slack.com/" + "services/" + "T01234567/" + "B01234567/" + "xyzXYZ1234567890abcdefgh",
			shouldRedact: true,
		},
		{
			name:         "private_key_block",
			input:        "some config\n-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA0Z3VS5JJcds3xfn/ygWyF8PbnGcY5unA67hFdJBEEH6kMRMD\n-----END RSA PRIVATE KEY-----\nmore text",
			shouldRedact: true,
		},
		{
			name:         "generic_api_key_sk_prefix",
			input:        "+api_key = sk-1234567890abcdef1234567890abcdef",
			shouldRedact: true,
		},
		{
			name:         "ec_private_key",
			input:        "config\n-----BEGIN EC PRIVATE KEY-----\nMHQCAQEEIBkg4LVWM9nuwNSk3yByxZpoDxbmNGcXnmz7KtPH0TAntXnZ1LMIhJw\n-----END EC PRIVATE KEY-----\nend",
			shouldRedact: true,
		},

		{
			name:         "commit_sha",
			input:        "31533c04e617cdcc093ba44eff7c8e315488e62e",
			shouldRedact: false,
		},
		{
			name:         "uuid",
			input:        "838e1f2e-d825-405e-872b-81cc87f516be",
			shouldRedact: false,
		},
		{
			name:         "file_path",
			input:        "internal/service/attribution.go",
			shouldRedact: false,
		},
		{
			name:         "github_url_no_auth",
			input:        "https://github.com/semanticash/cli.git",
			shouldRedact: false,
		},
		{
			name:         "ssh_remote",
			input:        "git@github.com:semanticash/cli.git",
			shouldRedact: false,
		},
		{
			name:         "version_string",
			input:        "v0.1.4",
			shouldRedact: false,
		},
		{
			name:         "placeholder_key",
			input:        "YOUR_API_KEY_HERE",
			shouldRedact: false,
		},
		{
			name:         "example_token",
			input:        "example-token-123",
			shouldRedact: false,
		},
		{
			name:         "hex_digest",
			input:        "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
			shouldRedact: false,
		},
		{
			name:         "go_code_snippet",
			input:        "func main() {\n\tfmt.Println(\"hello world\")\n\tos.Exit(0)\n}",
			shouldRedact: false,
		},
		{
			name:         "diff_header",
			input:        "diff --git a/internal/service/worker.go b/internal/service/worker.go",
			shouldRedact: false,
		},
		{
			name:         "diff_hunk",
			input:        "@@ -464,30 +464,45 @@ func pushAttribution(...) {",
			shouldRedact: false,
		},
		{
			name:         "commit_subject",
			input:        "Adds carry-forward attribution to hook and worker paths",
			shouldRedact: false,
		},
		{
			name:         "semantica_trailer",
			input:        "Semantica-Attribution: 99% claude_code (1617/1628 lines)",
			shouldRedact: false,
		},
		{
			name:         "json_config_structure",
			input:        `{"enabled": true, "version": 1, "providers": ["claude-code"]}`,
			shouldRedact: false,
		},
		{
			name:         "postgres_url_localhost",
			input:        "postgres://user:password@localhost:5432/mydb",
			shouldRedact: false, // Gitleaks does not flag localhost DB URLs by default
		},
	}

	for _, tc := range corpus {
		t.Run(tc.name, func(t *testing.T) {
			got, err := String(tc.input)
			if err != nil {
				t.Fatalf("String() error: %v", err)
			}
			hasRedaction := strings.Contains(got, "[REDACTED]")
			if tc.shouldRedact && !hasRedaction {
				t.Errorf("expected redaction but output is unchanged: %q", got)
			}
			if !tc.shouldRedact && hasRedaction {
				t.Errorf("unexpected redaction (false positive): input=%q output=%q", tc.input, got)
			}
		})
	}
}

func TestFixture_SafeDiff(t *testing.T) {
	resetForCorpus(t)

	diff := `diff --git a/internal/service/attribution.go b/internal/service/attribution.go
index 7fc6336..2cb7ddb 100644
--- a/internal/service/attribution.go
+++ b/internal/service/attribution.go
@@ -152,6 +152,11 @@ func (s *AttributionService) AttributeCommit(ctx context.Context, in Attribution
+	bs, err := blobs.NewStore(objectsDir)
+	if err != nil {
+		return nil, fmt.Errorf("init blob store: %w", err)
+	}

 	events, err := h.Queries.ListEventsInWindow(ctx, sqldb.ListEventsInWindowParams{
 		RepositoryID: cp.RepositoryID,
-		AfterTs:      afterTs,
+		AfterTs:      0,
 		UpToTs:       cp.CreatedAt,
 	})
`
	got, err := String(diff)
	if err != nil {
		t.Fatal(err)
	}
	if got != diff {
		t.Errorf("safe diff was modified by redaction")
	}
}

func TestFixture_MixedDiff(t *testing.T) {
	resetForCorpus(t)

	webhook := "https://hooks.slack.com/" + "services/" + "T01234567/B01234567/xyzXYZ1234567890abcdefgh"
	diff := `diff --git a/config.go b/config.go
+	repoRoot := repo.Root()
+	semDir := filepath.Join(repoRoot, ".semantica")
+	checkpointID := "838e1f2e-d825-405e-872b-81cc87f516be"
+	// This webhook should be redacted:
+	slackURL := "` + webhook + `"
+	commitHash := "31533c04e617cdcc093ba44eff7c8e315488e62e"
+	-----BEGIN RSA PRIVATE KEY-----
+	MIIEpAIBAAKCAQEA0Z3VS5JJcds3xfn/ygWyF8PbnGcY5unA67hFdJBEEH6kMRMD
+	-----END RSA PRIVATE KEY-----
`
	got, err := String(diff)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(got, "hooks.slack.com/services/T01234567") {
		t.Error("Slack webhook not redacted in mixed diff")
	}
	if strings.Contains(got, "MIIEpAIBAAK") {
		t.Error("private key not redacted in mixed diff")
	}

	if !strings.Contains(got, "838e1f2e-d825-405e-872b-81cc87f516be") {
		t.Error("UUID was incorrectly redacted")
	}
	if !strings.Contains(got, "31533c04e617cdcc093ba44eff7c8e315488e62e") {
		t.Error("commit SHA was incorrectly redacted")
	}
	if !strings.Contains(got, "repoRoot := repo.Root()") {
		t.Error("Go code was incorrectly redacted")
	}
	if !strings.Contains(got, "filepath.Join") {
		t.Error("Go stdlib call was incorrectly redacted")
	}
}

func TestFixture_PromptContent(t *testing.T) {
	resetForCorpus(t)

	prompt := `You are a PR author. Given a diff, commit subjects, and an optional PR template, write a pull request title and body.

<change_summary>
Files changed: 9
Lines added: 1628
Lines deleted: 84
Representative files:
- modified internal/service/attribution.go (+374 -62)
- modified internal/service/attribution_test.go (+1012 -2)
- modified internal/service/hook_commit_msg_test.go (+207 -1)
</change_summary>

<commit_subjects>
Adds carry-forward attribution to hook and worker paths
Fixes session detail lookup and --all flag
Adds trailers on/off toggle
</commit_subjects>

<pr_template>
## Summary
## Test plan
- [ ] Unit tests pass
- [ ] Lint passes
</pr_template>`

	got, err := String(prompt)
	if err != nil {
		t.Fatal(err)
	}
	if got != prompt {
		t.Error("safe prompt was modified by redaction")
	}
}
