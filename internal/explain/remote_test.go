package explain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/service"
	"github.com/semanticash/cli/internal/util"
)

// --- fetchRemoteProvenance: HTTP-branch tests ---
//
// Each test stands up an httptest server with a known response and
// asserts the returned (text, fallback) pair. These tests cover
// the four reachable HTTP outcomes plus the malformed-body case:
//
//   200 + playbook    -> text non-empty, fallback ""
//   200 + empty body  -> not_in_remote
//   404               -> not_in_remote
//   500               -> remote_unavailable
//   401               -> remote_unavailable
//   network error     -> remote_unavailable
//   malformed body    -> remote_unavailable

func TestFetchRemoteProvenance_HitReturnsFormattedText(t *testing.T) {
	playbook := mustMarshal(t, service.NarrativeResultJSON{
		Title:     "Auth handler refactor",
		Intent:    "Decouple auth.",
		Outcome:   "Middleware composed cleanly.",
		Learnings: []string{"Smaller interfaces help"},
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/repos/repo-123/commits/abcdef0123/playbook" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("missing/wrong Authorization header: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":   false,
			"message": "Commit playbook",
			"payload": map[string]any{
				"commit_sha":     "abcdef0123",
				"commit_subject": "feat: add Handle",
				"playbook":       json.RawMessage(playbook),
				"generated_at":   "2026-05-08T12:00:00Z",
			},
		})
	}))
	defer srv.Close()

	text, fallback := fetchRemoteProvenance(context.Background(),
		srv.URL, "repo-123", "abcdef0123", "test-token")

	if fallback != "" {
		t.Errorf("hit should leave fallback empty, got %q", fallback)
	}
	for _, want := range []string{
		"Commit abcdef01 - feat: add Handle",
		"[Playbook] Auth handler refactor",
		"Intent:",
		"Decouple auth.",
		"Outcome:",
		"Middleware composed cleanly.",
		"Learnings:",
		"  - Smaller interfaces help",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("formatted text missing %q\n--- text ---\n%s", want, text)
		}
	}
}

func TestFetchRemoteProvenance_EmptyPayloadIsNotInRemote(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":   false,
			"message": "Commit playbook",
			"payload": map[string]any{
				"commit_sha": "abcdef0123",
				"playbook":   json.RawMessage(`null`),
			},
		})
	}))
	defer srv.Close()

	text, fallback := fetchRemoteProvenance(context.Background(),
		srv.URL, "repo-123", "abcdef0123", "test-token")
	if text != "" {
		t.Errorf("expected empty text for empty payload; got %q", text)
	}
	if fallback != FallbackNotInRemote {
		t.Errorf("fallback = %q, want %q", fallback, FallbackNotInRemote)
	}
}

func TestFetchRemoteProvenance_NotFoundIsNotInRemote(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Commit not found", http.StatusNotFound)
	}))
	defer srv.Close()

	text, fallback := fetchRemoteProvenance(context.Background(),
		srv.URL, "repo-123", "abcdef0123", "test-token")
	if text != "" {
		t.Errorf("expected empty text on 404; got %q", text)
	}
	if fallback != FallbackNotInRemote {
		t.Errorf("fallback = %q, want %q", fallback, FallbackNotInRemote)
	}
}

func TestFetchRemoteProvenance_ServerErrorIsRemoteUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, fallback := fetchRemoteProvenance(context.Background(),
		srv.URL, "repo-123", "abcdef0123", "test-token")
	if fallback != FallbackRemoteUnavailable {
		t.Errorf("fallback = %q, want %q", fallback, FallbackRemoteUnavailable)
	}
}

func TestFetchRemoteProvenance_UnauthorizedIsRemoteUnavailable(t *testing.T) {
	// 401 mid-call (token rejected by API even though present
	// locally) is "tried, didn't work" - remote_unavailable so
	// the SKILL.md body says capture may exist but couldn't be
	// fetched, not "Semantica was not queried."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, fallback := fetchRemoteProvenance(context.Background(),
		srv.URL, "repo-123", "abcdef0123", "test-token")
	if fallback != FallbackRemoteUnavailable {
		t.Errorf("fallback = %q, want %q", fallback, FallbackRemoteUnavailable)
	}
}

func TestFetchRemoteProvenance_NetworkErrorIsRemoteUnavailable(t *testing.T) {
	// Endpoint that immediately closes connections so the request
	// transport surfaces an error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // start it then close before the call

	_, fallback := fetchRemoteProvenance(context.Background(),
		srv.URL, "repo-123", "abcdef0123", "test-token")
	if fallback != FallbackRemoteUnavailable {
		t.Errorf("fallback = %q, want %q", fallback, FallbackRemoteUnavailable)
	}
}

func TestFetchRemoteProvenance_MalformedBodyIsRemoteUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	_, fallback := fetchRemoteProvenance(context.Background(),
		srv.URL, "repo-123", "abcdef0123", "test-token")
	if fallback != FallbackRemoteUnavailable {
		t.Errorf("fallback = %q, want %q", fallback, FallbackRemoteUnavailable)
	}
}

func TestFetchRemoteProvenance_BadPlaybookJSONIsRemoteUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":   false,
			"message": "Commit playbook",
			"payload": map[string]any{
				"commit_sha": "abcdef0123",
				"playbook":   json.RawMessage(`"not an object"`),
			},
		})
	}))
	defer srv.Close()

	_, fallback := fetchRemoteProvenance(context.Background(),
		srv.URL, "repo-123", "abcdef0123", "test-token")
	if fallback != FallbackRemoteUnavailable {
		t.Errorf("fallback = %q, want %q", fallback, FallbackRemoteUnavailable)
	}
}

// --- remoteProvenance: setup-check branches ---
//
// These tests exercise the not-attempted short-circuits without
// firing an HTTP request. Each test arranges one of the three
// missing-precondition cases and asserts the fallback comes back
// as remote_not_attempted.

func TestRemoteProvenance_NotAttemptedWhenSettingsMissing(t *testing.T) {
	repo := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SEMANTICA_API_KEY", "test-token")

	text, fallback := remoteProvenance(context.Background(), repo, "abcdef0")
	if text != "" {
		t.Errorf("expected empty text; got %q", text)
	}
	if fallback != FallbackRemoteNotAttempted {
		t.Errorf("fallback = %q, want %q", fallback, FallbackRemoteNotAttempted)
	}
}

func TestRemoteProvenance_NotAttemptedWhenNotConnected(t *testing.T) {
	repo := t.TempDir()
	semDir := filepath.Join(repo, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := util.WriteSettings(semDir, util.Settings{Enabled: true, Connected: false}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SEMANTICA_API_KEY", "test-token")

	_, fallback := remoteProvenance(context.Background(), repo, "abcdef0")
	if fallback != FallbackRemoteNotAttempted {
		t.Errorf("fallback = %q, want %q", fallback, FallbackRemoteNotAttempted)
	}
}

func TestRemoteProvenance_NotAttemptedWhenConnectedRepoIDMissing(t *testing.T) {
	repo := t.TempDir()
	semDir := filepath.Join(repo, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := util.WriteSettings(semDir, util.Settings{
		Enabled:   true,
		Connected: true,
		// ConnectedRepoID intentionally empty
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SEMANTICA_API_KEY", "test-token")

	_, fallback := remoteProvenance(context.Background(), repo, "abcdef0")
	if fallback != FallbackRemoteNotAttempted {
		t.Errorf("fallback = %q, want %q", fallback, FallbackRemoteNotAttempted)
	}
}

// Note on the auth-token-missing branch: that path is plumbing
// over auth.AccessToken, which is exhaustively tested in its own
// package. Reproducing it here cleanly requires neutralizing the
// OS keychain (macOS keychain returns real credentials even when
// XDG_CONFIG_HOME is empty), and the cross-package helper for
// that is package-private to internal/auth. The four "not
// attempted" reasons are covered by the three settings-state
// tests above plus the auth package's own coverage.

// --- Service.Explain end-to-end: remote API hit ---

// TestExplain_RemoteProvenanceHit guards the remote API wire-up.
// Setup:
//
//   - real git repo with one commit (no .semantica/lineage.db, so
//     layer 1 misses naturally)
//   - settings.json marking the repo connected to a workspace
//   - SEMANTICA_API_KEY for the auth check
//   - SEMANTICA_ENDPOINT pointing at an httptest server that
//     returns a populated playbook
//
// Asserts Service.Explain returns mode=provenance with the
// formatted playbook content. A regression that drops the remote
// branch from Service.Explain shows up here as mode=git-only.
func TestExplain_RemoteProvenanceHit(t *testing.T) {
	playbook := mustMarshal(t, service.NarrativeResultJSON{
		Title:   "Cross-team auth audit",
		Intent:  "Validate session policy.",
		Outcome: "Audit clean.",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/playbook") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":   false,
			"message": "Commit playbook",
			"payload": map[string]any{
				"commit_sha":     "abcdef0123",
				"commit_subject": "feat: rotate session policy",
				"playbook":       json.RawMessage(playbook),
			},
		})
	}))
	defer srv.Close()

	dir := initRepoForLayerTwoTest(t)
	semDir := filepath.Join(dir, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := util.WriteSettings(semDir, util.Settings{
		Enabled:         true,
		Connected:       true,
		ConnectedRepoID: "repo-abc",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SEMANTICA_API_KEY", "test-token")
	t.Setenv("SEMANTICA_ENDPOINT", srv.URL)

	out, err := NewService().Explain(context.Background(), Input{RepoPath: dir, Ref: "HEAD"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if out.Mode != ModeProvenance {
		t.Fatalf("expected mode=%q, got %q\nhuman_text:\n%s\ndiff_excerpt:\n%s",
			ModeProvenance, out.Mode, out.HumanText, out.DiffExcerpt)
	}
	for _, want := range []string{
		"Commit abcdef01 - feat: rotate session policy",
		"[Playbook] Cross-team auth audit",
		"Intent:",
		"Validate session policy.",
		"Outcome:",
		"Audit clean.",
	} {
		if !strings.Contains(out.HumanText, want) {
			t.Errorf("human_text missing %q\n--- human_text ---\n%s", want, out.HumanText)
		}
	}
	// Provenance mode never carries fallback_reason or
	// diff_excerpt; those are git-only territory.
	if out.FallbackReason != "" {
		t.Errorf("provenance hit set fallback_reason=%q (should be empty)", out.FallbackReason)
	}
	if out.DiffExcerpt != "" {
		t.Errorf("provenance hit leaked diff_excerpt: %q", out.DiffExcerpt)
	}
}

// initRepoForLayerTwoTest creates a temp git repo with a single
// commit. The remote API path doesn't need lineage.db - it just
// needs a resolvable HEAD so Service.Explain reaches the remote
// branch.
func initRepoForLayerTwoTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	gitCmd := func(args ...string) {
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
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitCmd("init", ".")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd("add", "main.go")
	gitCmd("commit", "-m", "initial")
	return dir
}

// mustMarshal marshals v or fatals the test. Callers wrap the
// result in json.RawMessage when feeding it into envelope payloads.
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}
