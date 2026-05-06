package health

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/auth"
)

func TestAssemble_AllOK(t *testing.T) {
	r := assemble([]Check{
		{Category: "a", Status: StatusOK},
		{Category: "b", Status: StatusOK},
	})
	if r.Result != StatusOK {
		t.Errorf("Result = %q, want ok", r.Result)
	}
	if r.Summary.OK != 2 || r.Summary.Warn != 0 || r.Summary.Fail != 0 {
		t.Errorf("Summary = %+v, want {ok:2 warn:0 fail:0}", r.Summary)
	}
}

func TestAssemble_WarnDominatesOK(t *testing.T) {
	r := assemble([]Check{
		{Status: StatusOK},
		{Status: StatusWarn},
		{Status: StatusOK},
	})
	if r.Result != StatusWarn {
		t.Errorf("Result = %q, want warn", r.Result)
	}
}

func TestAssemble_FailDominatesAll(t *testing.T) {
	r := assemble([]Check{
		{Status: StatusOK},
		{Status: StatusWarn},
		{Status: StatusFail},
		{Status: StatusWarn},
	})
	if r.Result != StatusFail {
		t.Errorf("Result = %q, want fail", r.Result)
	}
	if r.Summary.OK != 1 || r.Summary.Warn != 2 || r.Summary.Fail != 1 {
		t.Errorf("Summary = %+v, want {ok:1 warn:2 fail:1}", r.Summary)
	}
}

func TestAssemble_SchemaVersionPinned(t *testing.T) {
	r := assemble(nil)
	if r.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", r.SchemaVersion)
	}
	if SchemaVersion != 1 {
		t.Errorf("package SchemaVersion constant = %d, want 1 (bumping requires API consumer review)", SchemaVersion)
	}
}

func TestExitCode_Mapping(t *testing.T) {
	cases := []struct {
		status Status
		want   int
	}{
		{StatusOK, 0},
		{StatusWarn, 1},
		{StatusFail, 2},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			r := Report{Result: tc.status}
			if got := r.ExitCode(); got != tc.want {
				t.Errorf("ExitCode for %q = %d, want %d", tc.status, got, tc.want)
			}
		})
	}
}

func TestCheckBinary_NotOnPath(t *testing.T) {
	checks := checkBinary(Options{
		LookPath: func(string) (string, error) { return "", exec.ErrNotFound },
	}, nil)

	var pathCheck *Check
	for i := range checks {
		if checks[i].ID == "path_resolves" {
			pathCheck = &checks[i]
			break
		}
	}
	if pathCheck == nil {
		t.Fatal("missing path_resolves check")
	}
	if pathCheck.Status != StatusFail {
		t.Errorf("path_resolves status = %q, want fail", pathCheck.Status)
	}
	if pathCheck.Remediation == "" {
		t.Error("expected remediation hint when binary is missing from PATH")
	}
}

func TestCheckBinary_SelfMatch(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "semantica")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	checks := checkBinary(Options{
		LookPath:     func(string) (string, error) { return bin, nil },
		DoctorBinary: bin,
	}, []string{bin})

	for _, c := range checks {
		if c.ID == "self_match" {
			if c.Status != StatusOK {
				t.Errorf("self_match status = %q msg=%q, want ok", c.Status, c.Message)
			}
			return
		}
	}
	t.Error("missing self_match check")
}

func TestCheckBinary_SelfMismatch(t *testing.T) {
	tmp := t.TempDir()
	pathBin := filepath.Join(tmp, "path_semantica")
	docBin := filepath.Join(tmp, "doctor_semantica")
	for _, p := range []string{pathBin, docBin} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	checks := checkBinary(Options{
		LookPath:     func(string) (string, error) { return pathBin, nil },
		DoctorBinary: docBin,
	}, []string{pathBin})

	for _, c := range checks {
		if c.ID == "self_match" {
			if c.Status != StatusFail {
				t.Errorf("self_match status = %q, want fail", c.Status)
			}
			if !strings.Contains(c.Message, pathBin) || !strings.Contains(c.Message, docBin) {
				t.Errorf("self_match message should mention both paths, got %q", c.Message)
			}
			if c.Remediation == "" {
				t.Error("expected remediation when self_match fails")
			}
			return
		}
	}
	t.Error("missing self_match check")
}

func TestCheckBinary_DoctorBinaryUnknown(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "semantica")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	_ = checkBinary(Options{
		LookPath: func(string) (string, error) { return bin, nil },
	}, []string{bin})
}

func TestRenderJSON_ShapeMatches(t *testing.T) {
	r := Report{
		SchemaVersion: 1,
		Result:        StatusWarn,
		Summary:       Summary{OK: 2, Warn: 1, Fail: 0},
		Checks: []Check{
			{Category: "binary", ID: "path_resolves", Status: StatusOK, Message: "ok"},
			{Category: "state", ID: "auth", Status: StatusWarn, Message: "expired", Remediation: "log in"},
		},
	}

	var buf bytes.Buffer
	if err := RenderJSON(&buf, r); err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}

	if got["schema_version"] != float64(1) {
		t.Errorf("schema_version = %v, want 1", got["schema_version"])
	}
	if got["result"] != "warn" {
		t.Errorf("result = %v, want warn", got["result"])
	}
	checks, _ := got["checks"].([]any)
	if len(checks) != 2 {
		t.Errorf("checks length = %d, want 2", len(checks))
	}
}

func TestRenderText_GroupsAndRemediation(t *testing.T) {
	r := Report{
		SchemaVersion: 1,
		Result:        StatusFail,
		Summary:       Summary{OK: 1, Warn: 1, Fail: 1},
		Checks: []Check{
			{Category: "state", ID: "auth", Status: StatusWarn, Message: "expired", Remediation: "log in"},
			{Category: "binary", ID: "path_resolves", Status: StatusOK, Message: "ok"},
			{Category: "binary", ID: "self_match", Status: StatusFail, Message: "mismatch", Remediation: "make install"},
		},
	}

	var buf bytes.Buffer
	if err := RenderText(&buf, r); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	binIdx := strings.Index(out, "Binary")
	stateIdx := strings.Index(out, "Capture state")
	if binIdx == -1 || stateIdx == -1 || binIdx >= stateIdx {
		t.Errorf("expected Binary before Capture state in output, got:\n%s", out)
	}
	if !strings.Contains(out, "remediation: make install") {
		t.Errorf("expected fail remediation in text output, got:\n%s", out)
	}
	if !strings.Contains(out, "remediation: log in") {
		t.Errorf("expected warn remediation in text output, got:\n%s", out)
	}
	if !strings.Contains(out, "Result: fail (1 issue, 1 warning)") {
		t.Errorf("expected pluralized result line, got:\n%s", out)
	}
}

func TestRenderText_OkResultNoSummary(t *testing.T) {
	r := Report{
		SchemaVersion: 1,
		Result:        StatusOK,
		Summary:       Summary{OK: 3},
		Checks:        []Check{{Category: "binary", ID: "x", Status: StatusOK, Message: "ok"}},
	}
	var buf bytes.Buffer
	if err := RenderText(&buf, r); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "Result: ok\n") {
		t.Errorf("expected bare `Result: ok` line on healthy report, got:\n%s", out)
	}
	if strings.Contains(out, "issue") || strings.Contains(out, "warning") {
		t.Errorf("ok result should not print issue/warning counts, got:\n%s", out)
	}
}

func TestClassifyAuth_DisconnectedNotAuthenticated_OK(t *testing.T) {
	got := classifyAuth(auth.AuthState{}, false)
	if got.Status != StatusOK {
		t.Errorf("status = %q, want ok (local-only mode)", got.Status)
	}
}

func TestClassifyAuth_ConnectedNotAuthenticated_Warns(t *testing.T) {
	got := classifyAuth(auth.AuthState{}, true)
	if got.Status != StatusWarn {
		t.Errorf("status = %q, want warn (connected workspace cannot authenticate)", got.Status)
	}
	if got.Remediation == "" {
		t.Error("expected remediation when connected workspace cannot authenticate")
	}
}

func TestClassifyAuth_AuthenticatedSession_OK(t *testing.T) {
	got := classifyAuth(auth.AuthState{
		Authenticated: true,
		Source:        "session",
		Email:         "user@example.com",
		EndpointMatch: true,
	}, true)
	if got.Status != StatusOK {
		t.Errorf("status = %q, want ok", got.Status)
	}
	if !strings.Contains(got.Message, "user@example.com") {
		t.Errorf("expected email in message, got %q", got.Message)
	}
}

func TestClassifyAuth_AuthenticatedAPIKey_OK(t *testing.T) {
	got := classifyAuth(auth.AuthState{
		Authenticated: true,
		Source:        "api_key",
		EndpointMatch: true,
	}, true)
	if got.Status != StatusOK {
		t.Errorf("status = %q, want ok", got.Status)
	}
	if !strings.Contains(got.Message, "API_KEY") {
		t.Errorf("expected API_KEY in message, got %q", got.Message)
	}
}

func TestClassifyAuth_StorageError_Warns(t *testing.T) {
	got := classifyAuth(auth.AuthState{StorageError: "keyring locked"}, false)
	if got.Status != StatusWarn {
		t.Errorf("status = %q, want warn on storage error", got.Status)
	}
	if !strings.Contains(got.Message, "keyring locked") {
		t.Errorf("expected error detail in message, got %q", got.Message)
	}
}

func TestClassifyAuth_AccessExpiredButAuthStateOK_NoWarn(t *testing.T) {
	state := auth.AuthState{
		Authenticated: true,
		Source:        "session",
		Email:         "user@example.com",
		EndpointMatch: true,
	}
	got := classifyAuth(state, true)
	if got.Status == StatusWarn {
		t.Errorf("authenticated session must not warn just because some access token is expired upstream; got %+v", got)
	}
}

func TestCheckBinary_PathUniqueness_SingleBinaryNoWarn(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "semantica")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	checks := checkBinary(Options{
		LookPath:     func(string) (string, error) { return bin, nil },
		DoctorBinary: bin,
	}, []string{bin})

	for _, c := range checks {
		if c.ID == "path_uniqueness" {
			t.Errorf("expected no path_uniqueness check with a single binary, got: %+v", c)
		}
	}
}

func TestCheckBinary_PathUniqueness_MultipleBinariesWarn(t *testing.T) {
	tmp := t.TempDir()
	a := filepath.Join(tmp, "a", "semantica")
	b := filepath.Join(tmp, "b", "semantica")
	for _, p := range []string{a, b} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	checks := checkBinary(Options{
		LookPath:     func(string) (string, error) { return a, nil },
		DoctorBinary: a,
	}, []string{a, b})

	var sawWarn bool
	for _, c := range checks {
		if c.ID == "path_uniqueness" {
			sawWarn = true
			if c.Status != StatusWarn {
				t.Errorf("path_uniqueness status = %q, want warn", c.Status)
			}
			if !strings.Contains(c.Message, a) || !strings.Contains(c.Message, b) {
				t.Errorf("expected message to list both paths, got %q", c.Message)
			}
		}
	}
	if !sawWarn {
		t.Error("missing path_uniqueness warn for multiple binaries")
	}
}

func TestFindSemanticaOnPath_FindsExecutables(t *testing.T) {
	tmp := t.TempDir()
	dirA := filepath.Join(tmp, "a")
	dirB := filepath.Join(tmp, "b")
	for _, d := range []string{dirA, dirB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "semantica"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	pathEnv := dirA + string(os.PathListSeparator) + dirB
	got := findSemanticaOnPath(pathEnv)
	if len(got) != 2 {
		t.Errorf("expected 2 binaries, got %d: %v", len(got), got)
	}
}

func TestFindSemanticaOnPath_DedupesSymlink(t *testing.T) {
	tmp := t.TempDir()
	realDir := filepath.Join(tmp, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(realDir, "semantica")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(tmp, "link")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(linkDir, "semantica")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	pathEnv := linkDir + string(os.PathListSeparator) + realDir
	got := findSemanticaOnPath(pathEnv)
	if len(got) != 1 {
		t.Errorf("expected symlink + target to dedupe to 1 binary, got %d: %v", len(got), got)
	}
}

func TestFindSemanticaOnPath_SkipsNonExecutable(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "semantica"), []byte("not exec"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := findSemanticaOnPath(tmp)
	if len(got) != 0 {
		t.Errorf("expected non-executable file to be skipped, got %v", got)
	}
}

func TestRun_Integration_NoRepoPath(t *testing.T) {
	r, err := Run(t.Context(), Options{
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if r.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", r.SchemaVersion)
	}
	if len(r.Checks) == 0 {
		t.Error("expected at least one check in report")
	}
}
