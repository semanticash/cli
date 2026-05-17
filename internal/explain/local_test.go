package explain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/semanticash/cli/internal/redact"
	"github.com/semanticash/cli/internal/service"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// --- formatProvenance: pure-function rendering ---

func TestFormatProvenance_FullResultRendersAllSections(t *testing.T) {
	res := &service.ExplainResult{
		CommitHash:     "abcdef0123456789",
		CommitSubject:  "feat: add auth handler",
		FilesChanged:   3,
		LinesAdded:     150,
		LinesDeleted:   20,
		AIPercentage:   85.5,
		AILines:        128,
		HumanLines:     22,
		FilesWithAI:    3,
		FilesHumanOnly: 0,
		SessionCount:   2,
		RootSessions:   1,
		Subagents:      1,
		TopFiles: []service.FileDelta{
			{Path: "src/auth.go", Added: 50, Deleted: 10},
			{Path: "src/handler.go", Added: 45, Deleted: 5},
		},
		Summary: &service.NarrativeResultJSON{
			Title:     "Auth handler refactor",
			Intent:    "Decouple auth from request parsing.",
			Outcome:   "Middleware now composes cleanly.",
			Learnings: []string{"Smaller interfaces help"},
			Friction:  []string{"Tests took two passes to stabilize"},
			OpenItems: []string{"Add rate-limit retry"},
		},
	}

	out := formatProvenance(res)

	for _, want := range []string{
		"Commit abcdef01 - feat: add auth handler",
		"3 files changed (+150/-20)",
		"AI involvement:",
		"2 sessions (1 root, 1 subagent)", // 1 subagent -> singular

		"85.5% AI-Attributed (128 AI / 22 human)",
		"3 of 3 files contain AI-produced lines",
		"Top edited files:",
		"src/auth.go (+50/-10)",
		"src/handler.go (+45/-5)",
		"[Playbook] Auth handler refactor",
		"Intent:",
		"Decouple auth from request parsing.",
		"Outcome:",
		"Middleware now composes cleanly.",
		"Learnings:",
		"  - Smaller interfaces help",
		"Friction:",
		"  - Tests took two passes to stabilize",
		"Open items:",
		"  - Add rate-limit retry",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("formatted output missing %q\n--- output ---\n%s", want, out)
		}
	}
	if strings.HasSuffix(out, "\n") {
		t.Errorf("formatted output should be trimmed of trailing newline; got:\n%s", out)
	}
}

func TestFormatProvenance_ProvidersLine(t *testing.T) {
	single := &service.ExplainResult{
		CommitHash:    "abc12345",
		CommitSubject: "feat: x",
		SessionCount:  1,
		RootSessions:  1,
		Sessions: []service.SessionSummary{
			{SessionID: "s1", Provider: "claude_code"},
		},
	}
	out := formatProvenance(single)
	if !strings.Contains(out, "Provider: claude_code") {
		t.Errorf("single-provider line missing:\n%s", out)
	}

	multi := &service.ExplainResult{
		CommitHash:    "abc12345",
		CommitSubject: "feat: x",
		SessionCount:  3,
		RootSessions:  3,
		Sessions: []service.SessionSummary{
			{SessionID: "s1", Provider: "codex"},
			{SessionID: "s2", Provider: "claude_code"},
			{SessionID: "s3", Provider: "codex"}, // duplicate folded
		},
	}
	out = formatProvenance(multi)
	if !strings.Contains(out, "Providers: claude_code, codex") {
		t.Errorf("multi-provider line missing or unsorted:\n%s", out)
	}

	none := &service.ExplainResult{
		CommitHash:    "abc12345",
		CommitSubject: "feat: x",
		SessionCount:  0,
	}
	out = formatProvenance(none)
	if strings.Contains(out, "Provider") {
		t.Errorf("providers line should be omitted when no sessions; got:\n%s", out)
	}
}

func TestFormatProvenance_SubagentPluralization(t *testing.T) {
	res := &service.ExplainResult{
		CommitHash:    "abc12345",
		CommitSubject: "feat: x",
		SessionCount:  3,
		RootSessions:  1,
		Subagents:     2,
	}
	out := formatProvenance(res)
	if !strings.Contains(out, "3 sessions (1 root, 2 subagents)") {
		t.Errorf("plural subagent missing:\n%s", out)
	}
}

func TestFormatProvenance_SingleSessionAndSubagent(t *testing.T) {
	res := &service.ExplainResult{
		CommitHash:    "abc12345",
		CommitSubject: "feat: x",
		SessionCount:  1,
		RootSessions:  0,
		Subagents:     1,
	}
	out := formatProvenance(res)
	if !strings.Contains(out, "1 session (0 root, 1 subagent)") {
		t.Errorf("singular session/subagent missing:\n%s", out)
	}
}

func TestFormatProvenance_NoSummarySectionWhenAbsent(t *testing.T) {
	res := &service.ExplainResult{
		CommitHash:    "abc12345",
		CommitSubject: "feat: x",
		FilesChanged:  1,
		AILines:       10,
		HumanLines:    5,
		AIPercentage:  66.7,
		SessionCount:  1,
		RootSessions:  1,
	}
	out := formatProvenance(res)
	if strings.Contains(out, "[Playbook]") {
		t.Errorf("output should not include playbook section when Summary is nil:\n%s", out)
	}
	if strings.Contains(out, "Intent:") {
		t.Errorf("output should not include Intent section when Summary is nil:\n%s", out)
	}
}

func TestFormatProvenance_EmptyTopFilesOmitsSection(t *testing.T) {
	res := &service.ExplainResult{
		CommitHash:    "abc12345",
		CommitSubject: "feat: x",
		SessionCount:  1,
	}
	out := formatProvenance(res)
	if strings.Contains(out, "Top edited files:") {
		t.Errorf("output should not include Top edited files when empty:\n%s", out)
	}
}

func TestFormatProvenance_NoSubjectGetsPlaceholder(t *testing.T) {
	res := &service.ExplainResult{
		CommitHash:   "abc12345",
		SessionCount: 1,
	}
	out := formatProvenance(res)
	if !strings.Contains(out, "Commit abc12345 - (no subject)") {
		t.Errorf("missing (no subject) placeholder:\n%s", out)
	}
}

// --- localProvenance: miss conditions ---
//
// These tests exercise filesystem and "not in db" miss branches.
// The populated lineage.db path is covered by
// TestExplain_LocalProvenanceHit below.

func TestLocalProvenance_MissOnNoSemanticaDir(t *testing.T) {
	repo := t.TempDir()
	if got := localProvenance(context.Background(), repo, "abcdef0"); got != "" {
		t.Errorf("expected empty result; got:\n%s", got)
	}
}

func TestLocalProvenance_MissOnDisabledSemantica(t *testing.T) {
	repo := t.TempDir()
	semDir := filepath.Join(repo, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Drop a `disabled` marker so util.IsEnabled returns false.
	if err := os.WriteFile(filepath.Join(semDir, "disabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := localProvenance(context.Background(), repo, "abcdef0"); got != "" {
		t.Errorf("expected empty result for disabled repo; got:\n%s", got)
	}
}

func TestLocalProvenance_MissOnLineageDBAbsent(t *testing.T) {
	repo := t.TempDir()
	semDir := filepath.Join(repo, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// `enabled` marker present, but no lineage.db file.
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := localProvenance(context.Background(), repo, "abcdef0"); got != "" {
		t.Errorf("expected empty result when lineage.db is absent; got:\n%s", got)
	}
}

// TestExplain_LocalProvenanceHit exercises the local-provenance hit
// path against a populated lineage.db. Miss conditions and formatting
// are covered by narrower tests above.
func TestExplain_LocalProvenanceHit(t *testing.T) {
	ctx := context.Background()
	dir := setupProvenanceFixture(t, "feat: add Handle")

	svc := NewService()
	out, err := svc.Explain(ctx, Input{RepoPath: dir, Ref: "HEAD"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if out.Mode != ModeProvenance {
		t.Fatalf("expected mode=%q, got %q\nhuman_text:\n%s\ndiff_excerpt:\n%s",
			ModeProvenance, out.Mode, out.HumanText, out.DiffExcerpt)
	}
	if out.HumanText == "" {
		t.Fatal("provenance hit returned empty human_text")
	}
	for _, want := range []string{
		"feat: add Handle", // commit subject
		"AI involvement:",  // formatter section
		"AI-Attributed",    // attribution percentage line
	} {
		if !strings.Contains(out.HumanText, want) {
			t.Errorf("human_text missing %q\n--- human_text ---\n%s", want, out.HumanText)
		}
	}
	// Provenance mode should never carry diff_excerpt or
	// fallback_reason; those are git-only territory.
	if out.DiffExcerpt != "" {
		t.Errorf("provenance hit leaked diff_excerpt: %q", out.DiffExcerpt)
	}
	if out.FallbackReason != "" {
		t.Errorf("provenance hit set fallback_reason=%q (should be empty)", out.FallbackReason)
	}
}

// TestExplain_LocalProvenanceSecretInCommitSubjectIsRedacted ensures
// provenance-mode human_text passes through the same redactor used
// for git-only diff_excerpt. A commit subject containing a
// secret-shaped value must not reach the agent unredacted.
func TestExplain_LocalProvenanceSecretInCommitSubjectIsRedacted(t *testing.T) {
	ctx := context.Background()
	const webhook = "https://hooks.slack.com/" +
		"services/T01234567/B01234567/xyzXYZ1234567890abcdefgh"
	subject := "feat: revoke " + webhook
	dir := setupProvenanceFixture(t, subject)

	svc := NewService()
	out, err := svc.Explain(ctx, Input{RepoPath: dir, Ref: "HEAD"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if out.Mode != ModeProvenance {
		t.Fatalf("expected mode=%q, got %q\nhuman_text:\n%s",
			ModeProvenance, out.Mode, out.HumanText)
	}
	for _, fragment := range []string{
		webhook,                              // full token
		"hooks.slack.com/services/T01234567", // host + workspace
		"T01234567/B01234567",                // workspace + channel
		"xyzXYZ1234567890",                   // tail of secret
	} {
		if strings.Contains(out.HumanText, fragment) {
			t.Errorf("webhook fragment %q leaked into human_text:\n%s",
				fragment, out.HumanText)
		}
	}
	if !strings.Contains(out.HumanText, "[REDACTED]") {
		t.Errorf("expected [REDACTED] token in human_text:\n%s", out.HumanText)
	}
}

// TestExplain_LocalProvenanceRedactionFailureIsBlocked confirms the
// fail-closed contract: when the redactor fails on the formatted
// provenance text, the service returns mode=blocked rather than
// either falling through to git-only (silently dropping the
// provenance hit) or leaking unredacted text.
func TestExplain_LocalProvenanceRedactionFailureIsBlocked(t *testing.T) {
	ctx := context.Background()
	dir := setupProvenanceFixture(t, "feat: add Handle")

	cleanup := redact.ForceInitError(errors.New("forced redactor failure"))
	defer cleanup()

	svc := NewService()
	out, err := svc.Explain(ctx, Input{RepoPath: dir, Ref: "HEAD"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if out.Mode != ModeBlocked || out.Reason != ReasonRedactionFailed {
		t.Errorf("got mode=%q reason=%q, want blocked / redaction_failed\nhuman_text:\n%s",
			out.Mode, out.Reason, out.HumanText)
	}
	if out.HumanText != "" {
		t.Errorf("blocked output leaked human_text: %q", out.HumanText)
	}
	if out.DiffExcerpt != "" {
		t.Errorf("blocked output leaked diff_excerpt: %q", out.DiffExcerpt)
	}
}

// setupProvenanceFixture builds a temp git repo with two commits
// (the second one adds edit.go with a one-line function), a
// fully-populated .semantica/lineage.db, and the agent_event +
// checkpoint + commit_link rows the user-facing explain service
// needs to credit AI activity. Returns the canonical repo path so
// the caller can pass it to Service.Explain. Setup mirrors the
// established attribution-test pattern: two commits + checkpoint
// linked to the second commit so attribution has a time window
// to walk for AI events.
func setupProvenanceFixture(t *testing.T, commitSubject string) string {
	t.Helper()
	ctx := context.Background()
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
	if err := os.WriteFile(filepath.Join(dir, "edit.go"), []byte("package edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd("add", "edit.go")
	gitCmd("commit", "-m", "initial")

	if err := os.WriteFile(filepath.Join(dir, "edit.go"),
		[]byte("package edit\nfunc Handle() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd("add", "edit.go")
	gitCmd("commit", "-m", commitSubject)
	headHash := gitCmd("rev-parse", "HEAD")

	semDir := filepath.Join(dir, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	bs, err := blobs.NewStore(filepath.Join(semDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(semDir, "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}

	repoID := uuid.NewString()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID,
		RootPath:     dir,
		CreatedAt:    50_000,
		EnabledAt:    50_000,
	}); err != nil {
		t.Fatal(err)
	}

	src, err := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID:     uuid.NewString(),
		RepositoryID: repoID,
		Provider:     "claude_code",
		SourceKey:    "/fake/source.jsonl",
		LastSeenAt:   50_000,
		CreatedAt:    50_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID:         uuid.NewString(),
		ProviderSessionID: "test-session",
		RepositoryID:      repoID,
		Provider:          "claude_code",
		SourceID:          src.SourceID,
		StartedAt:         50_000,
		LastSeenAt:        50_000,
	})
	if err != nil {
		t.Fatal(err)
	}

	payload := fmt.Sprintf(
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"%s/edit.go","content":"package edit\\nfunc Handle() {}\\n"}}]}}`,
		filepath.ToSlash(dir))
	payloadHash, _, err := bs.Put(ctx, []byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID:      uuid.NewString(),
		SessionID:    sess.SessionID,
		RepositoryID: repoID,
		Ts:           150_000,
		Kind:         "assistant",
		Role:         sqlstore.NullStr("assistant"),
		ToolUses:     sql.NullString{Valid: true, String: `{"content_types":["tool_use"],"tools":[{"name":"Write","file_path":"edit.go","file_op":"write"}]}`},
		PayloadHash:  sqlstore.NullStr(payloadHash),
		Summary:      sqlstore.NullStr("Wrote edit.go"),
	}); err != nil {
		t.Fatal(err)
	}

	manifest := blobs.Manifest{
		Version:   1,
		CreatedAt: 200_000,
		Files:     []blobs.ManifestFile{{Path: "edit.go", Blob: "fakehash-edit", Size: 100}},
	}
	manifestRaw, _ := json.Marshal(manifest)
	manifestHash, _, err := bs.Put(ctx, manifestRaw)
	if err != nil {
		t.Fatal(err)
	}
	cpID := uuid.NewString()
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpID,
		RepositoryID: repoID,
		CreatedAt:    200_000,
		Kind:         "auto",
		Trigger:      sqlstore.NullStr("test"),
		Message:      sqlstore.NullStr("test cp"),
		ManifestHash: sqlstore.NullStr(manifestHash),
		SizeBytes:    sql.NullInt64{Int64: 100, Valid: true},
		Status:       "complete",
		CompletedAt:  sql.NullInt64{Int64: 200_000, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash:   headHash,
		RepositoryID: repoID,
		CheckpointID: cpID,
		LinkedAt:     200_000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertSessionCheckpoint(ctx, sqldb.InsertSessionCheckpointParams{
		SessionID:    sess.SessionID,
		CheckpointID: cpID,
	}); err != nil {
		t.Fatal(err)
	}

	if err := sqlstore.Close(h); err != nil {
		t.Fatalf("close lineage.db: %v", err)
	}
	return dir
}
