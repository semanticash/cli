package commands

import (
	"bytes"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/service"
)

// The render helper is the function the analyze command's RunE
// delegates to, so these tests pin the actual production output
// shapes rather than a copy.

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
	if !strings.Contains(got, "upload recorded") {
		t.Errorf("render should say 'upload recorded' (transport-only); got: %q", got)
	}
	if strings.Contains(got, "analysis uploaded") || strings.Contains(got, "findings uploaded") {
		t.Errorf("render must not claim findings/analysis were uploaded while the service ships transport_only; got: %q", got)
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
	if !strings.Contains(got, "upload already recorded") {
		t.Errorf("duplicate render should say 'upload already recorded'; got: %q", got)
	}
}

func TestRenderAnalyzeResult_Skipped(t *testing.T) {
	var buf bytes.Buffer
	res := &service.IntentGapUploadResult{
		Status: service.UploadStatusSkipped,
		Reason: "intent_gap.enabled is false",
	}
	if err := renderAnalyzeResult(&buf, false, res); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "Skipped: intent_gap.enabled is false") {
		t.Errorf("skipped render should include the reason verbatim; got: %q", got)
	}
}

// Error status must return a non-nil error so cobra propagates a
// non-zero exit code; the reason flows through verbatim so scripts
// can grep for it.
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

// Quiet suppresses success/skip output. Error status is unaffected
// because the non-zero exit code is the whole point of -q for scripts.
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

// An unknown status is a programming error in this package, not a
// runtime condition; surface it as an error so the regression is
// loud at exit time rather than silently mis-rendered.
func TestRenderAnalyzeResult_UnknownStatus(t *testing.T) {
	var buf bytes.Buffer
	res := &service.IntentGapUploadResult{Status: "weird"}
	err := renderAnalyzeResult(&buf, false, res)
	if err == nil {
		t.Fatalf("unknown status should produce an error; got nil")
	}
}

// The command must appear in the root command tree under its
// `intent-gap` name so users can discover it via `semantica --help`.
func TestIntentGapAnalyze_RegisteredOnRoot(t *testing.T) {
	root := NewRootCmd()
	for _, c := range root.Commands() {
		if c.Name() == "intent-gap" {
			return
		}
	}
	t.Fatal("intent-gap command not registered on root")
}
