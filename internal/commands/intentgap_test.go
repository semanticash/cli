package commands

import (
	"bytes"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/service"
)

// These tests cover the command's shared result renderer.

func TestRenderAnalyzeResult_Uploaded(t *testing.T) {
	var buf bytes.Buffer
	res := &service.IntentGapUploadResult{
		Status:   service.UploadStatusUploaded,
		PRNumber: 42,
		UploadID: "u-new",
	}
	if err := renderAnalyzeResult(&buf, false, res); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "PR #42") || !strings.Contains(got, "u-new") {
		t.Errorf("uploaded render missing PR # and upload_id: %q", got)
	}
	if !strings.Contains(got, "analysis recorded") {
		t.Errorf("render should say 'analysis recorded'; got: %q", got)
	}
}

func TestRenderAnalyzeResult_Duplicate(t *testing.T) {
	var buf bytes.Buffer
	res := &service.IntentGapUploadResult{
		Status:   service.UploadStatusDuplicate,
		PRNumber: 42,
		UploadID: "u-existing",
	}
	if err := renderAnalyzeResult(&buf, false, res); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "analysis already recorded") {
		t.Errorf("duplicate render should say 'analysis already recorded'; got: %q", got)
	}
}

func TestRenderAnalyzeResult_Skipped(t *testing.T) {
	var buf bytes.Buffer
	res := &service.IntentGapUploadResult{
		Status: service.UploadStatusSkipped,
		Reason: "no open PR for branch \"feat/x\"",
	}
	if err := renderAnalyzeResult(&buf, false, res); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "Skipped: no open PR for branch \"feat/x\"") {
		t.Errorf("skipped render should include the reason verbatim; got: %q", got)
	}
}

// Recording an errored row still returns a non-zero analysis result.
func TestRenderAnalyzeResult_AnalysisErroredEvenOnUploadSuccess(t *testing.T) {
	var buf bytes.Buffer
	res := &service.IntentGapUploadResult{
		Status:         service.UploadStatusUploaded,
		Analysis:       service.AnalysisErrored,
		AnalysisReason: "llm_unavailable",
		PRNumber:       42,
		UploadID:       "u-errored",
	}
	err := renderAnalyzeResult(&buf, false, res)
	if err == nil {
		t.Fatalf("analyzer-errored result should produce a non-zero exit error")
	}
	if !strings.Contains(err.Error(), "llm_unavailable") {
		t.Errorf("error should carry the sanitized reason code; got: %v", err)
	}
	if !strings.Contains(buf.String(), "errored") {
		t.Errorf("stdout should mention the errored state; got: %q", buf.String())
	}
}

// Upload errors return a non-zero exit code with the original reason.
func TestRenderAnalyzeResult_Error(t *testing.T) {
	var buf bytes.Buffer
	res := &service.IntentGapUploadResult{
		Status: service.UploadStatusError,
		Reason: "status 500: boom",
	}
	err := renderAnalyzeResult(&buf, false, res)
	if err == nil {
		t.Fatalf("error status should produce a non-nil error so the exit code is non-zero")
	}
	if !strings.Contains(err.Error(), "status 500") {
		t.Errorf("error message should surface the reason; got: %v", err)
	}
}

// Quiet mode suppresses normal output without suppressing errors.
func TestRenderAnalyzeResult_QuietSuppressesSuccessOutput(t *testing.T) {
	var buf bytes.Buffer
	res := &service.IntentGapUploadResult{
		Status:   service.UploadStatusUploaded,
		PRNumber: 42,
		UploadID: "u-new",
	}
	if err := renderAnalyzeResult(&buf, true, res); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("quiet should suppress success output; got: %q", buf.String())
	}
}

// Unknown statuses fail instead of producing misleading output.
func TestRenderAnalyzeResult_UnknownStatus(t *testing.T) {
	var buf bytes.Buffer
	res := &service.IntentGapUploadResult{Status: "weird"}
	err := renderAnalyzeResult(&buf, false, res)
	if err == nil {
		t.Fatalf("unknown status should produce an error; got nil")
	}
}

// The command is discoverable from the root help output.
func TestIntentGapAnalyze_RegisteredOnRoot(t *testing.T) {
	root := NewRootCmd()
	for _, c := range root.Commands() {
		if c.Name() == "intent-gap" {
			return
		}
	}
	t.Fatal("intent-gap command not registered on root")
}

func TestIntentGapAnalyze_BaseFlag(t *testing.T) {
	cmd := newIntentGapAnalyzeCmd(&RootOptions{})
	flag := cmd.Flags().Lookup("base")
	if flag == nil {
		t.Fatal("analyze command should expose --base")
	}
	if flag.DefValue != "" {
		t.Fatalf("--base default = %q, want auto-detect", flag.DefValue)
	}
}
