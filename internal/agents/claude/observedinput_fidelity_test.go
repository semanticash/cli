package claude

import (
	"encoding/json"
	"testing"

	"github.com/semanticash/cli/internal/observedinput"
)

func TestObservedInputFileFidelity(t *testing.T) {
	for _, tc := range []struct {
		name, metadata, delivered, transformation, extent string
	}{
		{"full text", `{"file":{"filePath":"/work/plan.md","content":"plan","startLine":1,"numLines":1,"totalLines":1}}`, "plan", "none", "complete"},
		{"partial text", `{"file":{"filePath":"/work/plan.md","content":"plan","startLine":2,"numLines":1,"totalLines":3}}`, "2: plan", "extracted", "partial"},
		{"unknown range", `{"file":{"filePath":"/work/plan.md","content":"plan"}}`, "plan", "none", "unknown"},
		{"missing body", `{"file":{"filePath":"/work/plan.md"}}`, "", "unknown", "unknown"},
		{"original bytes", `{"file":{"filePath":"/work/plan.md","base64":"cGxhbg==","originalSize":4}}`, "", "none", "complete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := observedinput.ObservedInput{DeliveryID: "result"}
			r := toolResultRepresentation(json.RawMessage(tc.metadata), tc.delivered, &o, func([]byte) string { return "hash" })
			if o.InputSource.Kind != "file" || o.InputSource.Locator != "/work/plan.md" || r.Transformation != tc.transformation || r.Extent != tc.extent {
				t.Fatalf("source=%+v representation=%+v", o.InputSource, r)
			}
		})
	}
}

func TestAttachmentSourceSurvivesMissingBody(t *testing.T) {
	o := observedinput.ObservedInput{DeliveryID: "attachment"}
	r := attachmentRepresentation(&rawAttachment{Filename: "/work/plan.md"}, &o, func([]byte) string { return "hash" })
	if o.InputSource.Kind != "file" || o.InputSource.Locator != "/work/plan.md" || r.Extent != "unknown" || r.Transformation != "unknown" {
		t.Fatalf("source=%+v representation=%+v", o.InputSource, r)
	}
}

func TestReportedSourceBytesPreservesZeroAndAbsence(t *testing.T) {
	for _, tc := range []struct {
		metadata string
		present  bool
	}{
		{`{"url":"https://example.org/docs","result":"summary"}`, false},
		{`{"url":"https://example.org/docs","result":"summary","bytes":0}`, true},
	} {
		o := observedinput.ObservedInput{DeliveryID: "result"}
		r := toolResultRepresentation(json.RawMessage(tc.metadata), "summary", &o, func([]byte) string { return "hash" })
		if (r.ReportedSourceBytes != nil) != tc.present {
			t.Fatalf("reported bytes = %v for %s", r.ReportedSourceBytes, tc.metadata)
		}
	}
}
