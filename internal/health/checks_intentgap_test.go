package health

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/util"
)

// writeSemDir creates local settings for health-check tests.
func writeSemDir(t *testing.T, root string) string {
	t.Helper()
	semDir := filepath.Join(root, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := util.WriteSettings(semDir, util.Settings{
		Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	return semDir
}

// Doctor stays silent when the repository has no Semantica state.
func TestCheckIntentGap_NoSemDirIsSilent(t *testing.T) {
	dir := t.TempDir()
	got := checkIntentGap(Options{RepoPath: dir})
	if len(got) != 0 {
		t.Errorf("expected no checks for a repo with no .semantica/, got %d:\n%#v", len(got), got)
	}
}

// Missing activity is a normal initial state.
func TestCheckIntentGap_NoActivityLogIsOK(t *testing.T) {
	dir := t.TempDir()
	writeSemDir(t, dir)

	got := checkIntentGap(Options{RepoPath: dir})
	last := findCheck(got, "last_activity")
	if last == nil {
		t.Fatalf("expected a last_activity check")
	}
	if last.Status != StatusOK {
		t.Errorf("last_activity status = %s, want ok", last.Status)
	}
	if !strings.Contains(last.Message, "no recorded") {
		t.Errorf("expected 'no recorded' message; got %q", last.Message)
	}
}

// Successful activity is reported with its original details.
func TestCheckIntentGap_SuccessfulUploadReportsAsOK(t *testing.T) {
	dir := t.TempDir()
	semDir := writeSemDir(t, dir)
	util.AppendActivityLog(semDir, "intent-gap uploaded PR #42 upload_id=u-new")

	got := checkIntentGap(Options{RepoPath: dir})
	last := findCheck(got, "last_activity")
	if last == nil {
		t.Fatalf("expected a last_activity check")
	}
	if last.Status != StatusOK {
		t.Errorf("status = %s, want ok", last.Status)
	}
	if !strings.Contains(last.Message, "uploaded PR #42") {
		t.Errorf("message should surface the upload line; got %q", last.Message)
	}
}

// Upload errors include a manual retry action.
func TestCheckIntentGap_UploadErrorReportsAsWarn(t *testing.T) {
	dir := t.TempDir()
	semDir := writeSemDir(t, dir)
	util.AppendActivityLog(semDir, "intent-gap upload error PR #42: status 500: boom")

	got := checkIntentGap(Options{RepoPath: dir})
	last := findCheck(got, "last_activity")
	if last == nil {
		t.Fatalf("expected a last_activity check")
	}
	if last.Status != StatusWarn {
		t.Errorf("status = %s, want warn", last.Status)
	}
	if !strings.Contains(last.Remediation, "intent-gap analyze") {
		t.Errorf("remediation should point at the analyze command; got %q", last.Remediation)
	}
}

// Expected skip outcomes remain informational.
func TestCheckIntentGap_SkipReasonReportsAsOK(t *testing.T) {
	dir := t.TempDir()
	semDir := writeSemDir(t, dir)
	util.AppendActivityLog(semDir, "intent-gap skipped: no open PR for branch \"feat/x\"")

	got := checkIntentGap(Options{RepoPath: dir})
	last := findCheck(got, "last_activity")
	if last == nil {
		t.Fatalf("expected a last_activity check")
	}
	if last.Status != StatusOK {
		t.Errorf("status = %s, want ok", last.Status)
	}
	if !strings.Contains(last.Message, "no open PR") {
		t.Errorf("message should include the skip reason; got %q", last.Message)
	}
}

// The most recent relevant activity determines status.
func TestCheckIntentGap_MostRecentLineWins(t *testing.T) {
	dir := t.TempDir()
	semDir := writeSemDir(t, dir)
	util.AppendActivityLog(semDir, "intent-gap upload error PR #42: status 500: boom")
	util.AppendActivityLog(semDir, "intent-gap uploaded PR #42 upload_id=u-new")

	got := checkIntentGap(Options{RepoPath: dir})
	last := findCheck(got, "last_activity")
	if last == nil {
		t.Fatalf("expected a last_activity check")
	}
	if last.Status != StatusOK {
		t.Errorf("recent success should override older error; got status %s", last.Status)
	}
}

// Non-intent-gap activity lines should not appear in the intent-gap
// doctor section.
func TestCheckIntentGap_IgnoresUnrelatedLines(t *testing.T) {
	dir := t.TempDir()
	semDir := writeSemDir(t, dir)
	util.AppendActivityLog(semDir, "post-commit warning: open db failed: x")

	got := checkIntentGap(Options{RepoPath: dir})
	last := findCheck(got, "last_activity")
	if last == nil {
		t.Fatalf("expected a last_activity check")
	}
	if !strings.Contains(last.Message, "no recorded") {
		t.Errorf("unrelated lines should be ignored; got %q", last.Message)
	}
}

// A recorded analyzer error remains a warning.
func TestCheckIntentGap_AnalyzerErroredUploadWarns(t *testing.T) {
	dir := t.TempDir()
	semDir := writeSemDir(t, dir)
	util.AppendActivityLog(semDir, "intent-gap analyzer failed PR #42: parse_failed")
	util.AppendActivityLog(semDir, "intent-gap analysis errored reason=parse_failed PR #42 upload=uploaded upload_id=u-1")

	got := checkIntentGap(Options{RepoPath: dir})
	last := findCheck(got, "last_activity")
	if last == nil {
		t.Fatalf("expected a last_activity check")
	}
	if last.Status != StatusWarn {
		t.Errorf("analyzer-errored upload should be warn; got %s", last.Status)
	}
	if !strings.Contains(last.Remediation, "intent-gap analyze") {
		t.Errorf("remediation should point at the analyze command; got %q", last.Remediation)
	}
}

func findCheck(checks []Check, id string) *Check {
	for i := range checks {
		if checks[i].ID == id {
			return &checks[i]
		}
	}
	return nil
}

// Text output uses the intended title and category order.
func TestRenderText_KnowsIntentGapCategory(t *testing.T) {
	if title, ok := categoryTitle["intent-gap"]; !ok || title == "" {
		t.Errorf("text renderer missing categoryTitle[\"intent-gap\"]; piped output will show the raw name")
	}
	if _, ok := categoryOrder["intent-gap"]; !ok {
		t.Errorf("text renderer missing categoryOrder[\"intent-gap\"]; piped output will sort the category past unknowns")
	}
}

// Reports containing intent-gap checks include the category heading.
func TestRenderText_IncludesIntentGapHeader(t *testing.T) {
	r := assemble([]Check{
		{Category: "intent-gap", ID: "last_activity", Status: StatusOK, Message: "last activity: intent-gap uploaded PR #42 upload_id=u-new"},
	})
	var buf strings.Builder
	if err := RenderText(&buf, r); err != nil {
		t.Fatalf("RenderText: %v", err)
	}
	if !strings.Contains(buf.String(), "Intent-gap") {
		t.Errorf("text output missing 'Intent-gap' header; got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "intent-gap uploaded PR #42") {
		t.Errorf("text output missing the check message; got:\n%s", buf.String())
	}
}
