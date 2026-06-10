package service

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
	"time"

	"github.com/semanticash/cli/internal/llm"
	"github.com/semanticash/cli/internal/util"
)

// initEnabledRepo sets up a temp git repo with the .semantica state
// the upload path expects: enabled marker, settings.json with
// intent_gap enabled and connected_repo_id set, and a single commit
// so HEAD resolves to a real SHA.
func initEnabledRepo(t *testing.T, opts settingsOpts) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	run("git", "init", "-q", "-b", opts.Branch)
	run("git", "config", "user.email", "test@example.com")
	run("git", "config", "user.name", "Test")
	run("git", "commit", "--allow-empty", "-q", "-m", "init")

	semDir := filepath.Join(dir, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := util.Settings{
		Enabled:          opts.SemanticaEnabled,
		Connected:        opts.Connected,
		ConnectedRepoID:  opts.ConnectedRepoID,
		IntentGapEnabled: opts.IntentGapEnabled,
	}
	if err := util.WriteSettings(semDir, s); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		canonical = filepath.Clean(dir)
	}
	return canonical
}

type settingsOpts struct {
	Branch           string
	SemanticaEnabled bool
	Connected        bool
	ConnectedRepoID  string
	IntentGapEnabled *bool
}

func mustBool(v bool) *bool { return &v }

// orchestratorStubServer routes discovery + upload calls to handlers
// the tests configure, so each scenario can pin the wire shapes it
// needs without standing up the real API.
func orchestratorStubServer(t *testing.T, discoveryBody string, discoveryStatus, uploadStatus int, uploadBody string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/prs/by-head-branch"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(discoveryStatus)
			_, _ = w.Write([]byte(discoveryBody))
		case strings.HasSuffix(r.URL.Path, "/intent_gap/findings"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(uploadStatus)
			_, _ = w.Write([]byte(uploadBody))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return httptest.NewServer(mux)
}

// installedRegistry returns a writer registry whose first writer has
// a non-empty Find() so the orchestrator records a real provider name.
func installedRegistry() *llm.WriterRegistry {
	return llm.NewWriterRegistry(&stubInstalledWriter{name: "claude_code", model: "claude-opus-4-7", binPath: "/fake/claude"})
}

type stubInstalledWriter struct{ name, model, binPath string }

func (w *stubInstalledWriter) Name() string  { return w.name }
func (w *stubInstalledWriter) Model() string { return w.model }
func (w *stubInstalledWriter) Find() string  { return w.binPath }
func (w *stubInstalledWriter) Generate(context.Context, string, string) (string, error) {
	return "", nil
}

// Disabled intent-gap setting is a clean skip; the orchestrator never
// reaches the network.
func TestIntentGapUploadService_SkipsWhenSettingDisabled(t *testing.T) {
	repo := initEnabledRepo(t, settingsOpts{
		Branch:           "main",
		SemanticaEnabled: true,
		Connected:        true,
		ConnectedRepoID:  "11111111-2222-3333-4444-555555555555",
		IntentGapEnabled: mustBool(false),
	})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:    "http://should-not-be-called",
		Token:       "tok",
		LLMRegistry: installedRegistry(),
		DeviceID:    "dev-1",
		Now:         func() time.Time { return time.Unix(0, 0) },
	})
	got, err := svc.Run(context.Background(), repo)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != UploadStatusSkipped {
		t.Errorf("Status = %s, want skipped", got.Status)
	}
	if !strings.Contains(got.Reason, "intent_gap.enabled is false") {
		t.Errorf("Reason = %q, want it to mention the disabled setting", got.Reason)
	}
}

// No open PR for the pushed branch is a skip (not an error). The CLI
// just records the reason; doctor surfaces it.
func TestIntentGapUploadService_SkipsWhenNoOpenPR(t *testing.T) {
	srv := orchestratorStubServer(t,
		`{"error":false,"message":"ok","payload":{"pull_requests":[]}}`,
		http.StatusOK, http.StatusOK, "")
	defer srv.Close()

	repo := initEnabledRepo(t, settingsOpts{
		Branch:           "feat/x",
		SemanticaEnabled: true,
		Connected:        true,
		ConnectedRepoID:  "11111111-2222-3333-4444-555555555555",
		IntentGapEnabled: mustBool(true),
	})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:    srv.URL,
		Token:       "tok",
		LLMRegistry: installedRegistry(),
		DeviceID:    "dev-1",
		Now:         func() time.Time { return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC) },
	})
	got, err := svc.Run(context.Background(), repo)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != UploadStatusSkipped {
		t.Errorf("Status = %s, want skipped", got.Status)
	}
	if !strings.Contains(got.Reason, "no open PR") {
		t.Errorf("Reason = %q, want it to mention no open PR", got.Reason)
	}
}

// Ambiguous PR list is also a skip; the message tells the user how
// many matches there were so doctor can surface a useful note.
func TestIntentGapUploadService_SkipsWhenAmbiguousPR(t *testing.T) {
	srv := orchestratorStubServer(t,
		`{"error":false,"message":"ok","payload":{"pull_requests":[{"pr_number":42,"state":"open"},{"pr_number":57,"state":"open"}]}}`,
		http.StatusOK, http.StatusOK, "")
	defer srv.Close()

	repo := initEnabledRepo(t, settingsOpts{
		Branch:           "feat/x",
		SemanticaEnabled: true,
		Connected:        true,
		ConnectedRepoID:  "11111111-2222-3333-4444-555555555555",
		IntentGapEnabled: mustBool(true),
	})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:    srv.URL,
		Token:       "tok",
		LLMRegistry: installedRegistry(),
		DeviceID:    "dev-1",
		Now:         func() time.Time { return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC) },
	})
	got, err := svc.Run(context.Background(), repo)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != UploadStatusSkipped {
		t.Errorf("Status = %s, want skipped", got.Status)
	}
	if !strings.Contains(got.Reason, "2 open PRs") {
		t.Errorf("Reason = %q, want it to mention 2 matches", got.Reason)
	}
}

// Happy path: 201 from the upload endpoint flips Status to uploaded
// and the orchestrator surfaces the upload_id for downstream callers.
func TestIntentGapUploadService_Uploaded(t *testing.T) {
	srv := orchestratorStubServer(t,
		`{"error":false,"message":"ok","payload":{"pull_requests":[{"pr_number":42,"state":"open","head_sha":"deadbeef","head_branch":"feat/x"}]}}`,
		http.StatusOK,
		http.StatusCreated,
		`{"error":false,"message":"ok","payload":{"upload_id":"u-new","received_at":"2026-06-09T12:00:00Z"}}`)
	defer srv.Close()

	repo := initEnabledRepo(t, settingsOpts{
		Branch:           "feat/x",
		SemanticaEnabled: true,
		Connected:        true,
		ConnectedRepoID:  "11111111-2222-3333-4444-555555555555",
		IntentGapEnabled: mustBool(true),
	})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:    srv.URL,
		Token:       "tok",
		LLMRegistry: installedRegistry(),
		DeviceID:    "dev-1",
		Now:         func() time.Time { return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC) },
	})
	got, err := svc.Run(context.Background(), repo)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != UploadStatusUploaded {
		t.Errorf("Status = %s, want uploaded", got.Status)
	}
	if got.UploadID != "u-new" {
		t.Errorf("UploadID = %q, want u-new", got.UploadID)
	}
	if got.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42", got.PRNumber)
	}
	if got.Provider != "claude_code" {
		t.Errorf("Provider = %q, want claude_code", got.Provider)
	}
}

// 200 (idempotent duplicate) flips Status to duplicate so the caller
// can render "already uploaded" rather than misleading the user into
// thinking a new row landed.
func TestIntentGapUploadService_Duplicate(t *testing.T) {
	srv := orchestratorStubServer(t,
		`{"error":false,"message":"ok","payload":{"pull_requests":[{"pr_number":42,"state":"open","head_sha":"deadbeef","head_branch":"feat/x"}]}}`,
		http.StatusOK,
		http.StatusOK,
		`{"error":false,"message":"ok","payload":{"upload_id":"u-existing","received_at":"2026-06-08T12:00:00Z"}}`)
	defer srv.Close()

	repo := initEnabledRepo(t, settingsOpts{
		Branch:           "feat/x",
		SemanticaEnabled: true,
		Connected:        true,
		ConnectedRepoID:  "11111111-2222-3333-4444-555555555555",
		IntentGapEnabled: mustBool(true),
	})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:    srv.URL,
		Token:       "tok",
		LLMRegistry: installedRegistry(),
		DeviceID:    "dev-1",
		Now:         func() time.Time { return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC) },
	})
	got, err := svc.Run(context.Background(), repo)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != UploadStatusDuplicate {
		t.Errorf("Status = %s, want duplicate", got.Status)
	}
	if got.UploadID != "u-existing" {
		t.Errorf("UploadID = %q, want u-existing", got.UploadID)
	}
}

// No LLM CLI installed is a skip with a clear reason. The body would
// be empty anyway; recording "provider: unknown" would mislead operators.
func TestIntentGapUploadService_SkipsWhenNoProviderInstalled(t *testing.T) {
	srv := orchestratorStubServer(t,
		`{"error":false,"message":"ok","payload":{"pull_requests":[{"pr_number":42,"state":"open","head_sha":"deadbeef","head_branch":"feat/x"}]}}`,
		http.StatusOK, http.StatusOK, "")
	defer srv.Close()

	repo := initEnabledRepo(t, settingsOpts{
		Branch:           "feat/x",
		SemanticaEnabled: true,
		Connected:        true,
		ConnectedRepoID:  "11111111-2222-3333-4444-555555555555",
		IntentGapEnabled: mustBool(true),
	})

	emptyRegistry := llm.NewWriterRegistry(&stubInstalledWriter{name: "claude_code", binPath: ""})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:    srv.URL,
		Token:       "tok",
		LLMRegistry: emptyRegistry,
		DeviceID:    "dev-1",
		Now:         func() time.Time { return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC) },
	})
	got, err := svc.Run(context.Background(), repo)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != UploadStatusSkipped {
		t.Errorf("Status = %s, want skipped", got.Status)
	}
	if !strings.Contains(got.Reason, "LLM CLI") {
		t.Errorf("Reason = %q, want it to mention missing LLM CLI", got.Reason)
	}
}

// 5xx from upload surfaces as UploadStatusError (not Skipped). The
// caller logs it; nothing bubbles up to fail the push.
func TestIntentGapUploadService_ServerError(t *testing.T) {
	srv := orchestratorStubServer(t,
		`{"error":false,"message":"ok","payload":{"pull_requests":[{"pr_number":42,"state":"open","head_sha":"deadbeef","head_branch":"feat/x"}]}}`,
		http.StatusOK,
		http.StatusInternalServerError,
		`{"error":true,"message":"boom"}`)
	defer srv.Close()

	repo := initEnabledRepo(t, settingsOpts{
		Branch:           "feat/x",
		SemanticaEnabled: true,
		Connected:        true,
		ConnectedRepoID:  "11111111-2222-3333-4444-555555555555",
		IntentGapEnabled: mustBool(true),
	})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:    srv.URL,
		Token:       "tok",
		LLMRegistry: installedRegistry(),
		DeviceID:    "dev-1",
		Now:         func() time.Time { return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC) },
	})
	got, err := svc.Run(context.Background(), repo)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != UploadStatusError {
		t.Errorf("Status = %s, want error", got.Status)
	}
	if !strings.Contains(got.Reason, "500") {
		t.Errorf("Reason = %q, want it to mention HTTP status 500", got.Reason)
	}
}

// Repo not connected is a skip. New users may have semantica enabled
// but not connected to a workspace yet; this path keeps the hook
// silent rather than erroring on every push.
func TestIntentGapUploadService_SkipsWhenNotConnected(t *testing.T) {
	repo := initEnabledRepo(t, settingsOpts{
		Branch:           "main",
		SemanticaEnabled: true,
		Connected:        false,
		ConnectedRepoID:  "",
		IntentGapEnabled: mustBool(true),
	})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:    "http://should-not-be-called",
		Token:       "tok",
		LLMRegistry: installedRegistry(),
		DeviceID:    "dev-1",
		Now:         func() time.Time { return time.Unix(0, 0) },
	})
	got, err := svc.Run(context.Background(), repo)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != UploadStatusSkipped {
		t.Errorf("Status = %s, want skipped", got.Status)
	}
	if !strings.Contains(got.Reason, "not connected") {
		t.Errorf("Reason = %q, want it to mention connection", got.Reason)
	}
}

// Suppress the unused import warning when no test in this file
// references json.
var _ = json.Marshal

// Skip paths in enabled repos write an activity-log entry so doctor
// can surface the last upload outcome without re-running the service.
func TestIntentGapUploadService_SkipsWriteActivityLog(t *testing.T) {
	repo := initEnabledRepo(t, settingsOpts{
		Branch:           "main",
		SemanticaEnabled: true,
		Connected:        true,
		ConnectedRepoID:  "11111111-2222-3333-4444-555555555555",
		IntentGapEnabled: mustBool(false),
	})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:    "http://should-not-be-called",
		Token:       "tok",
		LLMRegistry: installedRegistry(),
		DeviceID:    "dev-1",
		Now:         func() time.Time { return time.Unix(0, 0) },
	})
	if _, err := svc.Run(context.Background(), repo); err != nil {
		t.Fatalf("Run: %v", err)
	}

	logPath := filepath.Join(repo, ".semantica", "activity.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read activity.log: %v", err)
	}
	if !strings.Contains(string(data), "intent-gap skipped") {
		t.Errorf("expected 'intent-gap skipped' entry in activity.log; got:\n%s", data)
	}
	if !strings.Contains(string(data), "intent_gap.enabled is false") {
		t.Errorf("expected reason text in activity.log; got:\n%s", data)
	}
}

// A repo that never opted in should not get a .semantica directory
// just because the upload service decided to skip.
func TestIntentGapUploadService_NotEnabledDoesNotCreateSemDir(t *testing.T) {
	dir := t.TempDir()
	// Initialize git so OpenRepo succeeds, but do NOT create
	// .semantica/ - simulate a repo that never ran `semantica enable`.
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	run("git", "init", "-q", "-b", "main")
	run("git", "config", "user.email", "test@example.com")
	run("git", "config", "user.name", "Test")
	run("git", "commit", "--allow-empty", "-q", "-m", "init")

	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		canonical = filepath.Clean(dir)
	}

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:    "http://should-not-be-called",
		Token:       "tok",
		LLMRegistry: installedRegistry(),
		DeviceID:    "dev-1",
		Now:         func() time.Time { return time.Unix(0, 0) },
	})
	got, err := svc.Run(context.Background(), canonical)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != UploadStatusSkipped {
		t.Errorf("Status = %s, want skipped", got.Status)
	}

	if _, err := os.Stat(filepath.Join(canonical, ".semantica")); err == nil {
		t.Errorf(".semantica directory should not have been created for a repo that was not enabled")
	}
}

// Upload-time HTTP error path also writes to the activity log so a
// failed server round-trip is surfaceable by doctor.
func TestIntentGapUploadService_UploadErrorWritesActivityLog(t *testing.T) {
	srv := orchestratorStubServer(t,
		`{"error":false,"message":"ok","payload":{"pull_requests":[{"pr_number":42,"state":"open","head_sha":"deadbeef","head_branch":"feat/x"}]}}`,
		http.StatusOK,
		http.StatusInternalServerError,
		`{"error":true,"message":"boom"}`)
	defer srv.Close()

	repo := initEnabledRepo(t, settingsOpts{
		Branch:           "feat/x",
		SemanticaEnabled: true,
		Connected:        true,
		ConnectedRepoID:  "11111111-2222-3333-4444-555555555555",
		IntentGapEnabled: mustBool(true),
	})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:    srv.URL,
		Token:       "tok",
		LLMRegistry: installedRegistry(),
		DeviceID:    "dev-1",
		Now:         func() time.Time { return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC) },
	})
	if _, err := svc.Run(context.Background(), repo); err != nil {
		t.Fatalf("Run: %v", err)
	}

	logPath := filepath.Join(repo, ".semantica", "activity.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read activity.log: %v", err)
	}
	if !strings.Contains(string(data), "intent-gap upload error") {
		t.Errorf("expected 'intent-gap upload error' entry; got:\n%s", data)
	}
}
