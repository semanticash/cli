package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/intentgap"
	"github.com/semanticash/cli/internal/llm"
	"github.com/semanticash/cli/internal/util"
)

// initEnabledRepo sets up a temp git repo with the .semantica state
// the upload path expects: enabled marker, connected repo metadata, and
// a single commit so HEAD resolves to a real SHA.
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
		Enabled:         opts.SemanticaEnabled,
		Connected:       opts.Connected,
		ConnectedRepoID: opts.ConnectedRepoID,
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
}

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
	})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:    srv.URL,
		Token:       "tok",
		LLMRegistry: installedRegistry(),
		DeviceID:    "dev-1",
		Now:         func() time.Time { return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC) },
	})
	got, err := svc.Run(context.Background(), repo, RunOptions{Upload: true})
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
	})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:    srv.URL,
		Token:       "tok",
		LLMRegistry: installedRegistry(),
		DeviceID:    "dev-1",
		Now:         func() time.Time { return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC) },
	})
	got, err := svc.Run(context.Background(), repo, RunOptions{Upload: true})
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
	})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:    srv.URL,
		Token:       "tok",
		LLMRegistry: installedRegistry(),
		DeviceID:    "dev-1",
		Now:         func() time.Time { return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC) },
	})
	got, err := svc.Run(context.Background(), repo, RunOptions{Upload: true})
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
	})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:    srv.URL,
		Token:       "tok",
		LLMRegistry: installedRegistry(),
		DeviceID:    "dev-1",
		Now:         func() time.Time { return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC) },
	})
	got, err := svc.Run(context.Background(), repo, RunOptions{Upload: true})
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
	})

	emptyRegistry := llm.NewWriterRegistry(&stubInstalledWriter{name: "claude_code", binPath: ""})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:    srv.URL,
		Token:       "tok",
		LLMRegistry: emptyRegistry,
		DeviceID:    "dev-1",
		Now:         func() time.Time { return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC) },
	})
	got, err := svc.Run(context.Background(), repo, RunOptions{Upload: true})
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

// 5xx from upload surfaces as UploadStatusError (not Skipped). Run
// returns the transport outcome without returning an error.
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
	})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:    srv.URL,
		Token:       "tok",
		LLMRegistry: installedRegistry(),
		DeviceID:    "dev-1",
		Now:         func() time.Time { return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC) },
	})
	got, err := svc.Run(context.Background(), repo, RunOptions{Upload: true})
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

// Repo not connected is a skip. New users may have Semantica enabled
// before connecting the repo to a workspace.
func TestIntentGapUploadService_SkipsWhenNotConnected(t *testing.T) {
	repo := initEnabledRepo(t, settingsOpts{
		Branch:           "main",
		SemanticaEnabled: true,
		Connected:        false,
		ConnectedRepoID:  "",
	})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:    "http://should-not-be-called",
		Token:       "tok",
		LLMRegistry: installedRegistry(),
		DeviceID:    "dev-1",
		Now:         func() time.Time { return time.Unix(0, 0) },
	})
	got, err := svc.Run(context.Background(), repo, RunOptions{Upload: true})
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

// Keep json available to tests added under build-specific configurations.
var _ = json.Marshal

// Analyzer path tests.

// stubBundleAssembler returns a fixed bundle for orchestrator tests.
type stubBundleAssembler struct {
	bundle intentgap.Bundle
	err    error
	input  intentgap.BundleInput
	calls  int
}

func (s *stubBundleAssembler) Assemble(_ context.Context, in intentgap.BundleInput) (intentgap.Bundle, error) {
	s.calls++
	s.input = in
	return s.bundle, s.err
}

// stubAnalyzer returns a fixed analysis result and counts invocations so
// cache-hit tests can assert the analyzer was skipped on the second run.
type stubAnalyzer struct {
	result intentgap.AnalysisResult
	err    error
	calls  int
}

func (s *stubAnalyzer) Analyze(context.Context, intentgap.AnalysisInput) (intentgap.AnalysisResult, error) {
	s.calls++
	return s.result, s.err
}

func minimalBundle() intentgap.Bundle {
	return intentgap.Bundle{
		BaseRef: "main",
		BaseSHA: "merge-base-sha",
		HeadSHA: "head-sha",
		Diff:    []byte("--- a\n+++ b\n"),
	}
}

func validFindingsJSON() json.RawMessage {
	return json.RawMessage(`[
		{
			"schema_version":"1",
			"finding_id":"f_0123456789abcdef",
			"kind":"deferred",
			"title":"Deferred validation",
			"confidence":"medium",
			"originally_requested_in":{"turn_id":"t-1","prompt_excerpt":"add validation","prompt_excerpt_hash":"h"},
			"trajectory_note":"added then removed",
			"current_state":{"file":"handler.go","line_range":[12,24],"summary":"removed"}
		}
	]`)
}

func TestAssembleBundle_PassesBaseRef(t *testing.T) {
	assembler := &stubBundleAssembler{bundle: minimalBundle()}
	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		BaseRef:         "origin/develop",
		BundleAssembler: assembler,
	})

	_, _, hadErrored, err := svc.assembleBundle(
		context.Background(),
		t.TempDir(),
		intentgap.UploadInput{RepositoryID: "repo-1", PRNumber: 42, HeadSHA: "head-1"},
		&intentgap.OpenPR{PRNumber: 42},
		t.TempDir(),
		func() time.Time { return time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC) },
	)
	if err != nil {
		t.Fatalf("assembleBundle: %v", err)
	}
	if hadErrored {
		t.Fatalf("assembleBundle reported errored result on a clean bundle")
	}
	if assembler.input.Base != "origin/develop" {
		t.Errorf("bundle base = %q, want origin/develop", assembler.input.Base)
	}
}

// Findings are encoded and accepted as a new upload.
func TestIntentGapUploadService_AnalyzedWithFindings(t *testing.T) {
	srv := orchestratorStubServer(t,
		`{"error":false,"message":"ok","payload":{"pull_requests":[{"pr_number":42,"state":"open","head_sha":"deadbeef","head_branch":"feat/x"}]}}`,
		http.StatusOK,
		http.StatusCreated,
		`{"error":false,"message":"ok","payload":{"upload_id":"u-new","received_at":"2026-06-10T12:00:00Z"}}`)
	defer srv.Close()

	repo := initEnabledRepo(t, settingsOpts{
		Branch:           "feat/x",
		SemanticaEnabled: true,
		Connected:        true,
		ConnectedRepoID:  "11111111-2222-3333-4444-555555555555",
	})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:        srv.URL,
		Token:           "tok",
		LLMRegistry:     installedRegistry(),
		DeviceID:        "dev-1",
		Now:             func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) },
		BundleAssembler: &stubBundleAssembler{bundle: minimalBundle()},
		Analyzer: &stubAnalyzer{result: intentgap.AnalysisResult{
			Findings:              validFindingsJSON(),
			CoverageSummary:       json.RawMessage(`{"commits":1}`),
			Provider:              "claude_code",
			Model:                 "claude-opus-4-7",
			PromptTemplateVersion: intentgap.PromptTemplateVersion,
		}},
	})
	got, err := svc.Run(context.Background(), repo, RunOptions{Upload: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != UploadStatusUploaded {
		t.Errorf("Status = %s, want uploaded", got.Status)
	}
	if got.UploadID != "u-new" {
		t.Errorf("UploadID = %q, want u-new", got.UploadID)
	}
}

// An empty finding set is recorded as analyzed rather than skipped.
func TestIntentGapUploadService_AnalyzedEmpty(t *testing.T) {
	srv := orchestratorStubServer(t,
		`{"error":false,"message":"ok","payload":{"pull_requests":[{"pr_number":42,"state":"open","head_sha":"deadbeef","head_branch":"feat/x"}]}}`,
		http.StatusOK,
		http.StatusCreated,
		`{"error":false,"message":"ok","payload":{"upload_id":"u-empty","received_at":"2026-06-10T12:00:00Z"}}`)
	defer srv.Close()

	repo := initEnabledRepo(t, settingsOpts{
		Branch:           "feat/x",
		SemanticaEnabled: true,
		Connected:        true,
		ConnectedRepoID:  "11111111-2222-3333-4444-555555555555",
	})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:        srv.URL,
		Token:           "tok",
		LLMRegistry:     installedRegistry(),
		DeviceID:        "dev-1",
		Now:             func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) },
		BundleAssembler: &stubBundleAssembler{bundle: minimalBundle()},
		Analyzer: &stubAnalyzer{result: intentgap.AnalysisResult{
			Findings:              json.RawMessage(`[]`),
			CoverageSummary:       json.RawMessage(`{"commits":1}`),
			Provider:              "claude_code",
			Model:                 "claude-opus-4-7",
			PromptTemplateVersion: intentgap.PromptTemplateVersion,
		}},
	})
	got, err := svc.Run(context.Background(), repo, RunOptions{Upload: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != UploadStatusUploaded {
		t.Errorf("Status = %s, want uploaded (empty is analyzed, not skipped)", got.Status)
	}
}

// Lineage failures use a distinct reason code.
func TestIntentGapUploadService_LineageUnavailableUsesDedicatedReason(t *testing.T) {
	var captured []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/prs/by-head-branch"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"error":false,"message":"ok","payload":{"pull_requests":[{"pr_number":42,"state":"open","head_sha":"deadbeef","head_branch":"feat/x"}]}}`))
		case strings.HasSuffix(r.URL.Path, "/intent_gap/findings"):
			captured, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"error":false,"message":"ok","payload":{"upload_id":"u-lin","received_at":"2026-06-10T12:00:00Z"}}`))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	repo := initEnabledRepo(t, settingsOpts{
		Branch:           "feat/x",
		SemanticaEnabled: true,
		Connected:        true,
		ConnectedRepoID:  "11111111-2222-3333-4444-555555555555",
	})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:        srv.URL,
		Token:           "tok",
		LLMRegistry:     installedRegistry(),
		DeviceID:        "dev-1",
		Now:             func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) },
		BundleAssembler: &stubBundleAssembler{err: intentgap.ErrLineageUnavailable},
	})
	got, err := svc.Run(context.Background(), repo, RunOptions{Upload: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Analysis != AnalysisErrored {
		t.Errorf("Analysis = %s, want errored", got.Analysis)
	}
	if got.AnalysisReason != string(intentgap.ReasonLineageUnavailable) {
		t.Errorf("AnalysisReason = %q, want %q", got.AnalysisReason, string(intentgap.ReasonLineageUnavailable))
	}

	var parsed map[string]any
	if err := json.Unmarshal(captured, &parsed); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	cov, _ := parsed["coverage_summary"].(map[string]any)
	if cov["error_reason"] != string(intentgap.ReasonLineageUnavailable) {
		t.Errorf("wire error_reason = %v, want lineage_unavailable", cov["error_reason"])
	}
}

// Redaction failures cannot be reported as an empty successful analysis.
func TestIntentGapUploadService_RedactionFailureUsesDedicatedReason(t *testing.T) {
	var captured []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/prs/by-head-branch"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"error":false,"message":"ok","payload":{"pull_requests":[{"pr_number":42,"state":"open","head_sha":"deadbeef","head_branch":"feat/x"}]}}`))
		case strings.HasSuffix(r.URL.Path, "/intent_gap/findings"):
			captured, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"error":false,"message":"ok","payload":{"upload_id":"u-red","received_at":"2026-06-10T12:00:00Z"}}`))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	repo := initEnabledRepo(t, settingsOpts{
		Branch:           "feat/x",
		SemanticaEnabled: true,
		Connected:        true,
		ConnectedRepoID:  "11111111-2222-3333-4444-555555555555",
	})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:        srv.URL,
		Token:           "tok",
		LLMRegistry:     installedRegistry(),
		DeviceID:        "dev-1",
		Now:             func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) },
		BundleAssembler: &stubBundleAssembler{err: intentgap.ErrRedactionFailed},
	})
	got, err := svc.Run(context.Background(), repo, RunOptions{Upload: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Analysis != AnalysisErrored {
		t.Errorf("Analysis = %s, want errored", got.Analysis)
	}
	if got.AnalysisReason != string(intentgap.ReasonRedactionFailed) {
		t.Errorf("AnalysisReason = %q, want %q", got.AnalysisReason, string(intentgap.ReasonRedactionFailed))
	}

	var parsed map[string]any
	if err := json.Unmarshal(captured, &parsed); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	cov, _ := parsed["coverage_summary"].(map[string]any)
	if cov["error_reason"] != string(intentgap.ReasonRedactionFailed) {
		t.Errorf("wire error_reason = %v, want redaction_failed", cov["error_reason"])
	}
}

// Errored results retain the bundle's resolved base SHA.
func TestIntentGapUploadService_AnalyzerErrorPreservesBaseSHA(t *testing.T) {
	// Capture the request to verify base_sha.
	var capturedBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/prs/by-head-branch"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"error":false,"message":"ok","payload":{"pull_requests":[{"pr_number":42,"state":"open","head_sha":"deadbeef","head_branch":"feat/x"}]}}`))
		case strings.HasSuffix(r.URL.Path, "/intent_gap/findings"):
			capturedBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"error":false,"message":"ok","payload":{"upload_id":"u-x","received_at":"2026-06-10T12:00:00Z"}}`))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	repo := initEnabledRepo(t, settingsOpts{
		Branch:           "feat/x",
		SemanticaEnabled: true,
		Connected:        true,
		ConnectedRepoID:  "11111111-2222-3333-4444-555555555555",
	})

	bundle := minimalBundle()
	bundle.BaseSHA = "real-merge-base-sha"

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:        srv.URL,
		Token:           "tok",
		LLMRegistry:     installedRegistry(),
		DeviceID:        "dev-1",
		Now:             func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) },
		BundleAssembler: &stubBundleAssembler{bundle: bundle},
		Analyzer:        &stubAnalyzer{err: intentgap.ErrAnalyzerLLMUnavailable},
	})
	if _, err := svc.Run(context.Background(), repo, RunOptions{Upload: true}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(capturedBody, &parsed); err != nil {
		t.Fatalf("captured body not JSON: %v", err)
	}
	if got := parsed["base_sha"]; got != "real-merge-base-sha" {
		t.Errorf("errored body base_sha = %v, want %q (bundle's resolved merge-base)", got, "real-merge-base-sha")
	}
}

// Analyzer failures are recorded as errored uploads.
func TestIntentGapUploadService_AnalyzerErrorUploadsErroredRow(t *testing.T) {
	srv := orchestratorStubServer(t,
		`{"error":false,"message":"ok","payload":{"pull_requests":[{"pr_number":42,"state":"open","head_sha":"deadbeef","head_branch":"feat/x"}]}}`,
		http.StatusOK,
		http.StatusCreated,
		`{"error":false,"message":"ok","payload":{"upload_id":"u-errored","received_at":"2026-06-10T12:00:00Z"}}`)
	defer srv.Close()

	repo := initEnabledRepo(t, settingsOpts{
		Branch:           "feat/x",
		SemanticaEnabled: true,
		Connected:        true,
		ConnectedRepoID:  "11111111-2222-3333-4444-555555555555",
	})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:        srv.URL,
		Token:           "tok",
		LLMRegistry:     installedRegistry(),
		DeviceID:        "dev-1",
		Now:             func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) },
		BundleAssembler: &stubBundleAssembler{bundle: minimalBundle()},
		Analyzer:        &stubAnalyzer{err: intentgap.ErrAnalyzerLLMUnavailable},
	})
	got, err := svc.Run(context.Background(), repo, RunOptions{Upload: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != UploadStatusUploaded {
		t.Errorf("Status = %s, want uploaded (errored row uploaded successfully)", got.Status)
	}
	if got.UploadID != "u-errored" {
		t.Errorf("UploadID = %q, want u-errored", got.UploadID)
	}

	logPath := filepath.Join(repo, ".semantica", "activity.log")
	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "intent-gap analyzer failed") {
		t.Errorf("expected analyzer-failed activity log; got:\n%s", data)
	}
}

// Skip outcomes are recorded for local diagnostics.
func TestIntentGapUploadService_SkipsWriteActivityLog(t *testing.T) {
	srv := orchestratorStubServer(t,
		`{"error":false,"message":"ok","payload":{"pull_requests":[]}}`,
		http.StatusOK, http.StatusOK, "")
	defer srv.Close()

	repo := initEnabledRepo(t, settingsOpts{
		Branch:           "feat/x",
		SemanticaEnabled: true,
		Connected:        true,
		ConnectedRepoID:  "11111111-2222-3333-4444-555555555555",
	})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:    srv.URL,
		Token:       "tok",
		LLMRegistry: installedRegistry(),
		DeviceID:    "dev-1",
		Now:         func() time.Time { return time.Unix(0, 0) },
	})
	if _, err := svc.Run(context.Background(), repo, RunOptions{Upload: true}); err != nil {
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
	if !strings.Contains(string(data), "no open PR") {
		t.Errorf("expected reason text in activity.log; got:\n%s", data)
	}
}

// A skipped analysis does not create local Semantica state.
func TestIntentGapUploadService_NotEnabledDoesNotCreateSemDir(t *testing.T) {
	dir := t.TempDir()
	// Initialize Git without creating .semantica.
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
	got, err := svc.Run(context.Background(), canonical, RunOptions{Upload: true})
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

// Upload failures are recorded for local diagnostics.
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
	})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:    srv.URL,
		Token:       "tok",
		LLMRegistry: installedRegistry(),
		DeviceID:    "dev-1",
		Now:         func() time.Time { return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC) },
	})
	if _, err := svc.Run(context.Background(), repo, RunOptions{Upload: true}); err != nil {
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

// prLookupOnlyServer answers the PR-discovery call and fails the test
// on any other path. Local mode may discover PR context, but it does
// not upload findings.
func prLookupOnlyServer(t *testing.T, discoveryBody string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/prs/by-head-branch") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(discoveryBody))
			return
		}
		t.Errorf("unexpected request to %s during local-only run", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	return httptest.NewServer(mux)
}

// cacheFileForRepo finds the single cache file written by the local
// analysis run. Helpful when the head SHA is whatever the git fixture
// produced and is not known up front.
func cacheFileForRepo(t *testing.T, repoRoot string) string {
	t.Helper()
	dir := filepath.Join(repoRoot, ".semantica", "intent-gap")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one cache file under %s, found %d", dir, len(entries))
	}
	return filepath.Join(dir, entries[0].Name())
}

// Default mode (Upload=false) returns findings, writes a cache file,
// and never reaches the upload endpoint. PR discovery may still query
// the API to resolve the current PR.
func TestIntentGapUploadService_LocalModeReturnsFindingsWithoutUpload(t *testing.T) {
	srv := prLookupOnlyServer(t,
		`{"error":false,"message":"ok","payload":{"pull_requests":[{"pr_number":42,"state":"open","head_sha":"deadbeef","head_branch":"feat/x"}]}}`)
	defer srv.Close()

	repo := initEnabledRepo(t, settingsOpts{
		Branch:           "feat/x",
		SemanticaEnabled: true,
		Connected:        true,
		ConnectedRepoID:  "11111111-2222-3333-4444-555555555555",
	})

	analyzer := &stubAnalyzer{result: intentgap.AnalysisResult{
		Findings:              json.RawMessage("[]"),
		CoverageSummary:       json.RawMessage(`{"pr_commits_total":1}`),
		Provider:              "claude_code",
		Model:                 "claude-opus-4-7",
		PromptTemplateVersion: intentgap.PromptTemplateVersion,
	}}
	assembler := &stubBundleAssembler{bundle: minimalBundle()}
	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:        srv.URL,
		Token:           "tok",
		LLMRegistry:     installedRegistry(),
		DeviceID:        "dev-1",
		Now:             func() time.Time { return time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC) },
		BundleAssembler: assembler,
		Analyzer:        analyzer,
	})

	got, err := svc.Run(context.Background(), repo, RunOptions{Upload: false})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != UploadStatusAnalyzed {
		t.Errorf("Status = %s, want analyzed", got.Status)
	}
	if got.UploadID != "" {
		t.Errorf("UploadID populated in local-only mode: %q", got.UploadID)
	}
	if string(got.Findings) == "" {
		t.Errorf("Findings missing from local-mode result")
	}
	if analyzer.calls != 1 {
		t.Errorf("analyzer calls = %d, want 1", analyzer.calls)
	}
	if _, err := os.Stat(cacheFileForRepo(t, repo)); err != nil {
		t.Errorf("cache file not written: %v", err)
	}
}

// A second --upload run with identical inputs reuses the cached
// analysis: the analyzer is not invoked again, and the result is still
// uploaded.
func TestIntentGapUploadService_UploadReusesCachedAnalysis(t *testing.T) {
	discovery := `{"error":false,"message":"ok","payload":{"pull_requests":[{"pr_number":42,"state":"open","head_sha":"deadbeef","head_branch":"feat/x"}]}}`
	srv := orchestratorStubServer(t, discovery, http.StatusOK, http.StatusCreated,
		`{"error":false,"message":"ok","payload":{"upload_id":"u-1","received_at":"2026-06-27T12:00:00Z"}}`)
	defer srv.Close()

	repo := initEnabledRepo(t, settingsOpts{
		Branch:           "feat/x",
		SemanticaEnabled: true,
		Connected:        true,
		ConnectedRepoID:  "11111111-2222-3333-4444-555555555555",
	})

	analyzer := &stubAnalyzer{result: intentgap.AnalysisResult{
		Findings:              json.RawMessage("[]"),
		CoverageSummary:       json.RawMessage(`{"pr_commits_total":1}`),
		Provider:              "claude_code",
		Model:                 "claude-opus-4-7",
		PromptTemplateVersion: intentgap.PromptTemplateVersion,
	}}
	assembler := &stubBundleAssembler{bundle: minimalBundle()}
	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:        srv.URL,
		Token:           "tok",
		LLMRegistry:     installedRegistry(),
		DeviceID:        "dev-1",
		Now:             func() time.Time { return time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC) },
		BundleAssembler: assembler,
		Analyzer:        analyzer,
	})

	first, err := svc.Run(context.Background(), repo, RunOptions{Upload: true})
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if first.UsedCache {
		t.Errorf("first run should not report cache hit")
	}
	second, err := svc.Run(context.Background(), repo, RunOptions{Upload: true})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if !second.UsedCache {
		t.Errorf("second run should report UsedCache=true; got %+v", second)
	}
	if analyzer.calls != 1 {
		t.Errorf("analyzer calls = %d, want 1 (cache should skip the analyzer on the second run)", analyzer.calls)
	}
	// Bundle assembly runs every call because the cache key needs the
	// resolved BaseSHA. Cache hits skip only LLM analysis.
	if assembler.calls != 2 {
		t.Errorf("assembler calls = %d, want 2 (bundle must be assembled every run to resolve BaseSHA)", assembler.calls)
	}
}

// Changing --base between runs should invalidate the cache: the diff
// the analyzer saw is different, so a cached result against the
// default base cannot stand in for an analysis against a release
// branch.
func TestIntentGapUploadService_CacheMissOnBaseRefChange(t *testing.T) {
	discovery := `{"error":false,"message":"ok","payload":{"pull_requests":[{"pr_number":42,"state":"open","head_sha":"deadbeef","head_branch":"feat/x"}]}}`
	srv := orchestratorStubServer(t, discovery, http.StatusOK, http.StatusCreated,
		`{"error":false,"message":"ok","payload":{"upload_id":"u-1","received_at":"2026-06-27T12:00:00Z"}}`)
	defer srv.Close()

	repo := initEnabledRepo(t, settingsOpts{
		Branch:           "feat/x",
		SemanticaEnabled: true,
		Connected:        true,
		ConnectedRepoID:  "11111111-2222-3333-4444-555555555555",
	})

	analyzer := &stubAnalyzer{result: intentgap.AnalysisResult{
		Findings:              json.RawMessage("[]"),
		CoverageSummary:       json.RawMessage(`{"pr_commits_total":1}`),
		Provider:              "claude_code",
		Model:                 "claude-opus-4-7",
		PromptTemplateVersion: intentgap.PromptTemplateVersion,
	}}
	build := func(baseRef string) *IntentGapUploadService {
		return NewIntentGapUploadService(IntentGapUploadDeps{
			BaseRef:         baseRef,
			Endpoint:        srv.URL,
			Token:           "tok",
			LLMRegistry:     installedRegistry(),
			DeviceID:        "dev-1",
			Now:             func() time.Time { return time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC) },
			BundleAssembler: &stubBundleAssembler{bundle: minimalBundle()},
			Analyzer:        analyzer,
		})
	}

	if _, err := build("origin/main").Run(context.Background(), repo, RunOptions{Upload: true}); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if _, err := build("origin/release").Run(context.Background(), repo, RunOptions{Upload: true}); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if analyzer.calls != 2 {
		t.Errorf("analyzer calls = %d, want 2 (different --base must invalidate the cache)", analyzer.calls)
	}
}

// Changing the connected repository between runs should invalidate the
// cache: finding IDs are scoped to (repository_id, pr_number), so
// replaying findings stamped under repo-A as a record for repo-B would
// upload mismatched IDs.
func TestIntentGapUploadService_CacheMissOnRepositoryChange(t *testing.T) {
	discovery := `{"error":false,"message":"ok","payload":{"pull_requests":[{"pr_number":42,"state":"open","head_sha":"deadbeef","head_branch":"feat/x"}]}}`
	srv := orchestratorStubServer(t, discovery, http.StatusOK, http.StatusCreated,
		`{"error":false,"message":"ok","payload":{"upload_id":"u-1","received_at":"2026-06-27T12:00:00Z"}}`)
	defer srv.Close()

	build := func(connectedRepoID string) (*IntentGapUploadService, string) {
		repo := initEnabledRepo(t, settingsOpts{
			Branch:           "feat/x",
			SemanticaEnabled: true,
			Connected:        true,
			ConnectedRepoID:  connectedRepoID,
		})
		analyzer := &stubAnalyzer{result: intentgap.AnalysisResult{
			Findings:              json.RawMessage("[]"),
			CoverageSummary:       json.RawMessage(`{"pr_commits_total":1}`),
			Provider:              "claude_code",
			Model:                 "claude-opus-4-7",
			PromptTemplateVersion: intentgap.PromptTemplateVersion,
		}}
		svc := NewIntentGapUploadService(IntentGapUploadDeps{
			Endpoint:        srv.URL,
			Token:           "tok",
			LLMRegistry:     installedRegistry(),
			DeviceID:        "dev-1",
			Now:             func() time.Time { return time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC) },
			BundleAssembler: &stubBundleAssembler{bundle: minimalBundle()},
			Analyzer:        analyzer,
		})
		return svc, repo
	}

	// First run under repo-A populates the cache.
	svcA, repoA := build("11111111-1111-1111-1111-111111111111")
	if _, err := svcA.Run(context.Background(), repoA, RunOptions{Upload: true}); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	// Copy that cache file into a fresh repo whose settings point at a
	// different connected repository. Each repo has its own .semantica,
	// so the test stages the prior cache to verify the read-time
	// mismatch check rejects it.
	svcB, repoB := build("22222222-2222-2222-2222-222222222222")
	srcCache := cacheFileForRepo(t, repoA)
	dstDir := filepath.Join(repoB, ".semantica", "intent-gap")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stagedData, err := os.ReadFile(srcCache)
	if err != nil {
		t.Fatalf("read source cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dstDir, filepath.Base(srcCache)), stagedData, 0o644); err != nil {
		t.Fatalf("stage cache: %v", err)
	}

	// Capture analyzer call count before the second run, then assert it
	// advances. The two services own separate analyzer instances, so
	// this verifies the staged cache was rejected.
	depsB := svcB.deps
	priorCalls := depsB.Analyzer.(*stubAnalyzer).calls
	if _, err := svcB.Run(context.Background(), repoB, RunOptions{Upload: true}); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if depsB.Analyzer.(*stubAnalyzer).calls != priorCalls+1 {
		t.Errorf("repo-B analyzer should have run; calls before=%d after=%d", priorCalls, depsB.Analyzer.(*stubAnalyzer).calls)
	}
}

// stubTextWriter is an llm.Writer that returns a canned text response.
// The candidate-first pipeline runs through the WriterRegistry; a stub
// writer is the least-invasive way to exercise the default analyzer
// path without instantiating a real CLI.
type stubTextWriter struct {
	name, model, binPath, response string
	err                            error
}

func (w *stubTextWriter) Name() string  { return w.name }
func (w *stubTextWriter) Model() string { return w.model }
func (w *stubTextWriter) Find() string  { return w.binPath }
func (w *stubTextWriter) Generate(context.Context, string, string) (string, error) {
	return w.response, w.err
}

// With no analyzer injected, the service runs the candidate-first
// pipeline. The classifier's first call sees unparseable text, so the
// pipeline surfaces intent_classification_failed. This exercises both
// the default wiring and the classifier failure reason code in one run.
func TestIntentGapUploadService_DefaultAnalyzerIsCandidateFirstClassifierFailure(t *testing.T) {
	srv := orchestratorStubServer(t,
		`{"error":false,"message":"ok","payload":{"pull_requests":[{"pr_number":42,"state":"open","head_sha":"deadbeef","head_branch":"feat/x"}]}}`,
		http.StatusOK,
		http.StatusCreated,
		`{"error":false,"message":"ok","payload":{"upload_id":"u-classify-fail","received_at":"2026-06-27T12:00:00Z"}}`)
	defer srv.Close()

	repo := initEnabledRepo(t, settingsOpts{
		Branch:           "feat/x",
		SemanticaEnabled: true,
		Connected:        true,
		ConnectedRepoID:  "11111111-2222-3333-4444-555555555555",
	})

	// A bundle with a real captured turn drives the classifier through
	// its LLM call; the stub writer returns unparseable text so the
	// classifier fails.
	bundle := minimalBundle()
	bundle.Turns = []intentgap.BundleTurn{{
		TurnID:            "t-1",
		CommitHash:        "c1",
		PromptExcerpt:     "add input validation",
		PromptExcerptHash: "h-1",
	}}
	reg := llm.NewWriterRegistry(&stubTextWriter{
		name: "claude_code", model: "claude-opus-4-7", binPath: "/fake/claude",
		response: "definitely not JSON",
	})

	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint:        srv.URL,
		Token:           "tok",
		LLMRegistry:     reg,
		DeviceID:        "dev-1",
		Now:             func() time.Time { return time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC) },
		BundleAssembler: &stubBundleAssembler{bundle: bundle},
	})
	got, err := svc.Run(context.Background(), repo, RunOptions{Upload: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Analysis != AnalysisErrored {
		t.Fatalf("Analysis = %s, want errored", got.Analysis)
	}
	if got.AnalysisReason != string(intentgap.ReasonIntentClassificationFailed) {
		t.Errorf("AnalysisReason = %q, want %q", got.AnalysisReason, string(intentgap.ReasonIntentClassificationFailed))
	}
}

// The candidate-first pipeline stamps every analyzed body with
// PromptTemplateVersion. A no-turns bundle short-circuits the pipeline
// to an empty findings array without any LLM calls, keeping the test
// focused on the wire template version.
func TestIntentGapUploadService_DefaultAnalyzerStampsCandidateFirstTemplateVersion(t *testing.T) {
	var capturedBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/prs/by-head-branch"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"error":false,"message":"ok","payload":{"pull_requests":[{"pr_number":42,"state":"open","head_sha":"deadbeef","head_branch":"feat/x"}]}}`))
		case strings.HasSuffix(r.URL.Path, "/intent_gap/findings"):
			capturedBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"error":false,"message":"ok","payload":{"upload_id":"u-tpl","received_at":"2026-06-27T12:00:00Z"}}`))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	repo := initEnabledRepo(t, settingsOpts{
		Branch:           "feat/x",
		SemanticaEnabled: true,
		Connected:        true,
		ConnectedRepoID:  "11111111-2222-3333-4444-555555555555",
	})

	// No captured turns triggers the pipeline's fast path: empty
	// findings, no LLM calls, PromptTemplateVersion stamped.
	svc := NewIntentGapUploadService(IntentGapUploadDeps{
		Endpoint: srv.URL,
		Token:    "tok",
		LLMRegistry: llm.NewWriterRegistry(&stubTextWriter{
			name: "claude_code", model: "claude-opus-4-7", binPath: "/fake/claude",
			err: errors.New("must not be called on empty-turns fast path"),
		}),
		DeviceID:        "dev-1",
		Now:             func() time.Time { return time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC) },
		BundleAssembler: &stubBundleAssembler{bundle: minimalBundle()},
	})
	if _, err := svc.Run(context.Background(), repo, RunOptions{Upload: true}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(capturedBody, &parsed); err != nil {
		t.Fatalf("captured body not JSON: %v", err)
	}
	if got := parsed["prompt_template_version"]; got != intentgap.PromptTemplateVersion {
		t.Errorf("wire prompt_template_version = %v, want %q", got, intentgap.PromptTemplateVersion)
	}
	if got := parsed["prompt_template_version"]; got != "0.3.0-candidate-first-v1" {
		t.Errorf("wire prompt_template_version = %v, want 0.3.0-candidate-first-v1", got)
	}
}
