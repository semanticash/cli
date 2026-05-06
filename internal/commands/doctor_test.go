package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/health"
)

func TestResolveDoctorRepo_ExplicitPathInsideGitRepo(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := resolveDoctorRepo(root)
	if got == "" {
		t.Fatal("expected non-empty repo path for an explicit git repo")
	}
	wantCanonical, _ := filepath.EvalSymlinks(root)
	gotCanonical, _ := filepath.EvalSymlinks(got)
	if gotCanonical != wantCanonical {
		t.Errorf("resolveDoctorRepo(%q) = %q, want %q", root, got, root)
	}
}

func TestResolveDoctorRepo_DefaultEmptyWalksFromCwd(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	got := resolveDoctorRepo("")
	if got == "" {
		t.Fatal("expected default doctor invocation to resolve cwd's git repo, got empty")
	}
	wantCanonical, _ := filepath.EvalSymlinks(root)
	gotCanonical, _ := filepath.EvalSymlinks(got)
	if gotCanonical != wantCanonical {
		t.Errorf("resolveDoctorRepo(\"\") = %q, want cwd-resolved root %q", got, root)
	}
}

func TestResolveDoctorRepo_NonGitDirReturnsEmpty(t *testing.T) {
	plain := t.TempDir()
	t.Chdir(plain)

	got := resolveDoctorRepo(plain)
	if got == plain {
		t.Errorf("expected non-git dir %q to not resolve as its own repo root", plain)
	}
	_ = got
}

func TestRenderDoctorCard_ContainsCategoriesAndChecks(t *testing.T) {
	r := healthReportFixture()
	out := renderDoctorCard(r)

	want := []string{
		"Semantica doctor",
		"Binary",
		"Launcher",
		"Hooks",
		"Capture state",
		"resolved to /usr/local/bin/semantica",
		"service running",
		"workspace connected",
		"Result:",
	}
	for _, w := range want {
		if !strings.Contains(stripANSI(out), w) {
			t.Errorf("renderDoctorCard output missing %q\n--- output ---\n%s", w, out)
		}
	}
}

func TestRenderDoctorCard_ShowsRemediationForFailures(t *testing.T) {
	r := healthReportFixture()
	out := stripANSI(renderDoctorCard(r))
	if !strings.Contains(out, "remove the stale binary") {
		t.Errorf("expected fail remediation text in card, got:\n%s", out)
	}
}

func TestRenderDoctorCard_ResultLineMatchesSeverity(t *testing.T) {
	cases := []struct {
		name   string
		report health.Report
		want   string
	}{
		{
			name: "ok",
			report: health.Report{
				Result:  health.StatusOK,
				Summary: health.Summary{OK: 3},
				Checks:  []health.Check{{Category: "binary", Status: health.StatusOK, Message: "x"}},
			},
			want: "Result: ok",
		},
		{
			name: "warn-with-summary",
			report: health.Report{
				Result:  health.StatusWarn,
				Summary: health.Summary{OK: 1, Warn: 2},
				Checks:  []health.Check{{Category: "state", Status: health.StatusWarn, Message: "x"}},
			},
			want: "Result: warn (2 warnings)",
		},
		{
			name: "fail-with-summary",
			report: health.Report{
				Result:  health.StatusFail,
				Summary: health.Summary{OK: 0, Warn: 1, Fail: 1},
				Checks:  []health.Check{{Category: "state", Status: health.StatusFail, Message: "x"}},
			},
			want: "Result: fail (1 issue, 1 warning)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := stripANSI(renderDoctorCard(tc.report))
			if !strings.Contains(out, tc.want) {
				t.Errorf("expected result line %q in card, got:\n%s", tc.want, out)
			}
		})
	}
}

func healthReportFixture() health.Report {
	checks := []health.Check{
		{Category: "binary", ID: "path_resolves", Status: health.StatusOK, Message: "resolved to /usr/local/bin/semantica"},
		{Category: "binary", ID: "self_match", Status: health.StatusFail, Message: "PATH binary differs from doctor's", Remediation: "remove the stale binary"},
		{Category: "launcher", ID: "status", Status: health.StatusOK, Message: "service running"},
		{Category: "hooks", ID: "provider:claude-code", Status: health.StatusOK, Message: "Claude Code: installed"},
		{Category: "state", ID: "connect", Status: health.StatusOK, Message: "workspace connected"},
	}
	return health.Report{
		SchemaVersion: 1,
		Result:        health.StatusFail,
		Summary:       health.Summary{OK: 4, Fail: 1},
		Checks:        checks,
	}
}

// TestNewDoctorCmd_DefaultsToCwdRepo asserts that when --repo is
// not passed, doctor inspects the current repo.
func TestNewDoctorCmd_DefaultsToCwdRepo(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	rootOpts := &RootOptions{}
	cmd := NewDoctorCmd(rootOpts)
	cmd.SetArgs([]string{"--json"})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)

	_ = cmd.ExecuteContext(context.Background())

	var report struct {
		Checks []struct {
			Category string `json:"category"`
			ID       string `json:"id"`
			Status   string `json:"status"`
			Message  string `json:"message"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("doctor output is not JSON: %v\n%s", err, stdout.String())
	}

	for _, c := range report.Checks {
		if strings.Contains(c.Message, "no repo path supplied") {
			t.Errorf("default doctor invocation skipped repo-scoped check %s/%s: %s",
				c.Category, c.ID, c.Message)
		}
	}

	var sawConnect bool
	for _, c := range report.Checks {
		if c.Category == "state" && c.ID == "connect" {
			sawConnect = true
			if strings.Contains(c.Message, "skipped") {
				t.Errorf("connect check was skipped: %s", c.Message)
			}
		}
	}
	if !sawConnect {
		t.Error("expected a state/connect check in default doctor output")
	}
}
