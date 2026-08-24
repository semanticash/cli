package health

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Doctor reports invalid receipts and exits non-zero.
func TestCheckCommitReceipts_WiredAndReportsFile(t *testing.T) {
	dir := t.TempDir()
	rdir := filepath.Join(dir, ".semantica", "commit-receipts")
	if err := os.MkdirAll(rdir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := strings.Repeat("a", 40) + ".json"
	if err := os.WriteFile(filepath.Join(rdir, name), []byte("{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := findCheck(t, checkCommitReceipts(Options{RepoPath: dir}), "commit_receipts")
	if c.Category != "worker" || c.Status != StatusFail {
		t.Errorf("commit_receipts check = %+v, want worker/StatusFail", c)
	}
	if !strings.Contains(c.Message, name) {
		t.Errorf("message must name the offending file %q: %s", name, c.Message)
	}
	if c.Remediation == "" {
		t.Error("a blocking receipt check must carry remediation")
	}

	report, err := Run(context.Background(), Options{RepoPath: dir})
	if err != nil {
		t.Fatal(err)
	}
	if rc := findCheck(t, report.Checks, "commit_receipts"); rc.Status != StatusFail {
		t.Errorf("Run omitted or downgraded commit_receipts: %+v", rc)
	}
	if report.ExitCode() == 0 {
		t.Error("a failing receipt check must yield a non-zero doctor exit code")
	}

	if got := checkCommitReceipts(Options{RepoPath: t.TempDir()}); len(got) != 0 {
		t.Errorf("clean repo = %+v, want no commit_receipts problems", got)
	}
}
