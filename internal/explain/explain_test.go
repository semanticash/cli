package explain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/redact"
)

// --- IsSafeRef table ---

func TestIsSafeRef(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		want bool
	}{
		{"HEAD literal", "HEAD", true},
		{"short SHA", "abc1234", true},
		{"long SHA", "abcdef0123456789abcdef0123456789abcdef01", true},
		{"simple branch", "main", true},
		{"branch with slashes", "feature/foo-bar", true},
		{"tag with dots", "v1.2.3", true},

		// Positive cases that resemble SHAs but are actually valid
		// short branch / tag names: the SHA regex does not match
		// (too short, mixed case) but the branch / tag regex does.
		{"short branch resembling SHA", "abc", true},
		{"uppercase branch resembling SHA", "ABCDEF0", true},

		{"empty", "", false},
		{"leading dash", "-rf", false},
		{"double dot traversal", "feature/..bad", false},
		{"leading slash", "/etc/passwd", false},
		{"contains space", "main branch", false},
		{"contains semicolon", "main;rm", false},
		{"contains backtick", "main`x`", false},
		{"contains pipe", "a|b", false},
		{"contains dollar", "$ref", false},
		{"contains shell expansion", "$(rm -rf)", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSafeRef(tc.ref); got != tc.want {
				t.Errorf("IsSafeRef(%q) = %v, want %v", tc.ref, got, tc.want)
			}
		})
	}
}

// --- Engine: git-only happy path ---

func TestExplain_GitOnlyHappyPath(t *testing.T) {
	repo, hash := setupRepoWithCommit(t, "feat: add handler", "package main\nfunc Handle() {}\n")

	svc := NewService()
	out, err := svc.Explain(context.Background(), Input{RepoPath: repo, Ref: "HEAD"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if out.Mode != ModeGitOnly {
		t.Errorf("mode = %q, want %q", out.Mode, ModeGitOnly)
	}
	if out.FallbackReason != FallbackRemoteNotAttempted {
		t.Errorf("fallback_reason = %q, want %q",
			out.FallbackReason, FallbackRemoteNotAttempted)
	}
	if out.CommitMetadata == nil {
		t.Fatal("commit_metadata is nil")
	}
	if out.CommitMetadata.Hash != hash {
		t.Errorf("metadata hash = %q, want %q", out.CommitMetadata.Hash, hash)
	}
	if out.CommitMetadata.Subject != "feat: add handler" {
		t.Errorf("metadata subject = %q, want %q",
			out.CommitMetadata.Subject, "feat: add handler")
	}
	if !strings.Contains(out.DiffExcerpt, "func Handle() {}") {
		t.Errorf("diff_excerpt missing committed content:\n%s", out.DiffExcerpt)
	}
}

// --- Engine: not-found branches ---

func TestExplain_UnsafeRefIsNotFound(t *testing.T) {
	repo, _ := setupRepoWithCommit(t, "init", "package main\n")
	svc := NewService()
	out, err := svc.Explain(context.Background(), Input{RepoPath: repo, Ref: "main; rm -rf /"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if out.Mode != ModeNotFound || out.Reason != ReasonRefUnsafe {
		t.Errorf("got mode=%q reason=%q, want not-found / ref_unsafe", out.Mode, out.Reason)
	}
	if out.Message == "" {
		t.Errorf("not-found / ref_unsafe should populate Message")
	}
}

func TestExplain_UnresolvableRefIsNotFound(t *testing.T) {
	repo, _ := setupRepoWithCommit(t, "init", "package main\n")
	svc := NewService()
	out, err := svc.Explain(context.Background(), Input{RepoPath: repo, Ref: "feedface"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if out.Mode != ModeNotFound || out.Reason != ReasonRefNotResolvable {
		t.Errorf("got mode=%q reason=%q, want not-found / ref_not_resolvable",
			out.Mode, out.Reason)
	}
}

// --- Engine: blocked branch ---

func TestExplain_RedactionFailureIsBlocked(t *testing.T) {
	repo, _ := setupRepoWithCommit(t, "init", "package main\n")

	cleanup := redact.ForceInitError(errors.New("forced redactor failure"))
	defer cleanup()

	svc := NewService()
	out, err := svc.Explain(context.Background(), Input{RepoPath: repo, Ref: "HEAD"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if out.Mode != ModeBlocked || out.Reason != ReasonRedactionFailed {
		t.Errorf("got mode=%q reason=%q, want blocked / redaction_failed",
			out.Mode, out.Reason)
	}
	// blocked must not leak any diff content.
	if out.DiffExcerpt != "" {
		t.Errorf("blocked output leaked diff_excerpt: %q", out.DiffExcerpt)
	}
	if out.CommitMetadata != nil {
		// commit_metadata isn't sensitive on its own, but the
		// blocked branch is intentionally bare so future readers
		// don't have to think about whether each field is safe.
		t.Errorf("blocked output should not carry commit_metadata: %+v", out.CommitMetadata)
	}
}

// --- Engine: secret redaction inside the diff ---

func TestExplain_DiffSecretsAreRedacted(t *testing.T) {
	const slackWebhook = "https://hooks.slack.com/" +
		"services/T01234567/B01234567/xyzXYZ1234567890abcdefgh"
	const apiKey = "sk-1234567890abcdef1234567890abcdef"

	body := "package main\n" +
		"// webhook = " + slackWebhook + "\n" +
		"// api_key = " + apiKey + "\n"

	repo, _ := setupRepoWithCommit(t, "feat: add secrets", body)

	svc := NewService()
	out, err := svc.Explain(context.Background(), Input{RepoPath: repo, Ref: "HEAD"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if out.Mode != ModeGitOnly {
		t.Fatalf("expected git-only mode, got %q", out.Mode)
	}

	if strings.Contains(out.DiffExcerpt, slackWebhook) {
		t.Errorf("Slack webhook was not redacted from diff_excerpt:\n%s",
			out.DiffExcerpt)
	}
	if strings.Contains(out.DiffExcerpt, apiKey) {
		t.Errorf("API key was not redacted from diff_excerpt:\n%s",
			out.DiffExcerpt)
	}
	if !strings.Contains(out.DiffExcerpt, "[REDACTED]") {
		t.Errorf("expected at least one [REDACTED] token in diff_excerpt:\n%s",
			out.DiffExcerpt)
	}
}

// --- Engine: bound enforcement ---

func TestExplain_DiffIsTruncatedAtMaxBytes(t *testing.T) {
	// One commit whose diff blows past MaxDiffBytes by a comfortable
	// margin. Each line is ~64 bytes; 1000 of them is ~64 KB.
	var b strings.Builder
	for i := 0; i < 1000; i++ {
		b.WriteString("// long-padding-line-to-blow-past-the-12kb-bound-XXXXXXXXXXXXXXX\n")
	}
	repo, _ := setupRepoWithCommit(t, "feat: huge", b.String())

	svc := NewService()
	out, err := svc.Explain(context.Background(), Input{RepoPath: repo, Ref: "HEAD"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if out.Mode != ModeGitOnly {
		t.Fatalf("expected git-only mode, got %q", out.Mode)
	}
	if !strings.HasSuffix(out.DiffExcerpt, truncatedMarker) {
		t.Errorf("expected diff_excerpt to end with truncated marker:\n%s",
			truncatedMarker)
	}
	// Bound check: total length is at most MaxDiffBytes plus marker.
	if got := len(out.DiffExcerpt); got > MaxDiffBytes+len(truncatedMarker) {
		t.Errorf("diff_excerpt length = %d, max = %d (+marker %d)",
			got, MaxDiffBytes, len(truncatedMarker))
	}
}

// TestExplain_SecretSpanningTruncationBoundaryIsRedacted guards
// redact-before-truncate behavior for secrets crossing the byte cap.
// The cap is computed from the webhook's actual offset in the raw
// diff so it lands midway through the token, before the cut and after
// the cut. Plenty of post-webhook content is included so truncation
// still fires after the redactor compresses the webhook into
// [REDACTED].
func TestExplain_SecretSpanningTruncationBoundaryIsRedacted(t *testing.T) {
	const webhook = "https://hooks.slack.com/" +
		"services/T01234567/B01234567/xyzXYZ1234567890abcdefgh"

	preWebhookPad := strings.Repeat("a", 50)
	postWebhookPad := strings.Repeat("z", 500)
	body := "package main\n// " + preWebhookPad + " " + webhook + " " + postWebhookPad + "\n"

	repo, hash := setupRepoWithCommit(t, "feat: secret crossing truncation", body)

	// Read the raw diff so we can compute a cap that lands inside
	// the webhook bytes. This makes the test independent of git's
	// diff-framing layout (header lengths, hunk markers, etc.).
	r, err := git.OpenRepo(repo)
	if err != nil {
		t.Fatalf("git.OpenRepo: %v", err)
	}
	diff, err := r.DiffForCommit(context.Background(), hash)
	if err != nil {
		t.Fatalf("DiffForCommit: %v", err)
	}
	idx := bytes.Index(diff, []byte(webhook))
	if idx < 0 {
		t.Fatalf("setup error: webhook not in diff:\n%s", diff)
	}
	// Cap mid-webhook in the original diff: under truncate-first
	// the redactor would see only an unmatched prefix; under
	// redact-first the webhook is already [REDACTED] before the
	// cap applies.
	cap := idx + len(webhook)/2
	if cap <= idx || cap >= idx+len(webhook) {
		t.Fatalf("cap %d not strictly inside webhook [%d, %d)", cap, idx, idx+len(webhook))
	}
	withMaxDiffBytes(t, cap)

	svc := NewService()
	out, err := svc.Explain(context.Background(), Input{RepoPath: repo, Ref: "HEAD"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if out.Mode != ModeGitOnly {
		t.Fatalf("expected git-only mode, got %q", out.Mode)
	}
	if !strings.HasSuffix(out.DiffExcerpt, truncatedMarker) {
		t.Fatalf("setup did not exceed bound; truncation marker absent\nexcerpt:\n%s", out.DiffExcerpt)
	}
	for _, fragment := range []string{
		webhook,                              // full token
		"hooks.slack.com/services/T01234567", // host + workspace
		"T01234567/B01234567",                // workspace + channel
		"xyzXYZ1234567890",                   // tail of secret
		"https://hooks.slack.com",            // even the bare prefix
	} {
		if strings.Contains(out.DiffExcerpt, fragment) {
			t.Errorf("webhook fragment %q leaked into truncated diff_excerpt:\n%s",
				fragment, out.DiffExcerpt)
		}
	}
}

// --- JSON shape ---

func TestExplain_JSONShape_GitOnly(t *testing.T) {
	repo, _ := setupRepoWithCommit(t, "feat: shape", "package main\nfunc X() {}\n")

	svc := NewService()
	out, err := svc.Explain(context.Background(), Input{RepoPath: repo, Ref: "HEAD"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("json marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}

	want := map[string]bool{
		"mode":            true,
		"commit_metadata": true,
		"diff_excerpt":    true,
		"fallback_reason": true,
	}
	for k := range want {
		if _, ok := generic[k]; !ok {
			t.Errorf("git-only JSON missing %q: %s", k, body)
		}
	}
	for _, must := range []string{"human_text", "reason", "message"} {
		if _, ok := generic[must]; ok {
			t.Errorf("git-only JSON should omit %q: %s", must, body)
		}
	}
}

func TestExplain_JSONShape_NotFound(t *testing.T) {
	repo, _ := setupRepoWithCommit(t, "init", "package main\n")
	svc := NewService()
	out, _ := svc.Explain(context.Background(), Input{RepoPath: repo, Ref: "feedface"})
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("json marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	for _, must := range []string{"mode", "reason", "message"} {
		if _, ok := generic[must]; !ok {
			t.Errorf("not-found JSON missing %q: %s", must, body)
		}
	}
	for _, mustNot := range []string{"human_text", "commit_metadata", "diff_excerpt", "fallback_reason"} {
		if _, ok := generic[mustNot]; ok {
			t.Errorf("not-found JSON should omit %q: %s", mustNot, body)
		}
	}
}

func TestExplain_JSONShape_Blocked(t *testing.T) {
	repo, _ := setupRepoWithCommit(t, "init", "package main\n")
	cleanup := redact.ForceInitError(errors.New("forced"))
	defer cleanup()

	svc := NewService()
	out, _ := svc.Explain(context.Background(), Input{RepoPath: repo, Ref: "HEAD"})
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("json marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	for _, must := range []string{"mode", "reason", "message"} {
		if _, ok := generic[must]; !ok {
			t.Errorf("blocked JSON missing %q: %s", must, body)
		}
	}
	for _, mustNot := range []string{"human_text", "commit_metadata", "diff_excerpt", "fallback_reason"} {
		if _, ok := generic[mustNot]; ok {
			t.Errorf("blocked JSON should omit %q: %s", mustNot, body)
		}
	}
}

// --- Helpers ---

// withMaxDiffBytes overrides the package-level diff bound for the
// duration of t and restores the default afterward. Used to keep
// truncation-boundary fixtures small.
func withMaxDiffBytes(t *testing.T, n int) {
	t.Helper()
	orig := maxDiffBytes
	maxDiffBytes = n
	t.Cleanup(func() { maxDiffBytes = orig })
}

// setupRepoWithCommit creates a temp git repo with one commit that
// adds a single file. Returns the canonical repo path and the
// commit hash. Uses GIT_CONFIG_GLOBAL=/dev/null to insulate the
// test from the developer's git config.
func setupRepoWithCommit(t *testing.T, message, fileBody string) (repoPath, hash string) {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	gitCmd := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@test.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	gitCmd("init", ".")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(fileBody), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd("add", "main.go")
	gitCmd("commit", "-m", message)
	hash = gitCmd("rev-parse", "HEAD")
	return dir, hash
}
