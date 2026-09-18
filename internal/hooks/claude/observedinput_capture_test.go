package claude

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Capture preserves evidence from the requested transcript range.
func TestProviderCaptureObservedInputs(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "agents", "claude", "testdata", "observedinput", "captured_pdf_attachment.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, src, 0o644); err != nil {
		t.Fatal(err)
	}
	// Count lines to bound the batch (trailing newline yields no extra line).
	lines := 0
	for _, b := range src {
		if b == '\n' {
			lines++
		}
	}
	if len(src) > 0 && src[len(src)-1] != '\n' {
		lines++
	}
	p := New()
	batch, err := p.CaptureObservedInputs(context.Background(), path, 0, lines)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if len(batch.Turns) == 0 {
		t.Fatal("no evidence captured")
	}
	var haveRequest, haveAttachment bool
	for _, ev := range batch.Turns {
		if len(ev.Requests) > 0 {
			haveRequest = true
		}
		for _, o := range ev.Observations {
			if o.Acquisition == "attachment" {
				haveAttachment = true
			}
		}
	}
	if !haveRequest || !haveAttachment {
		t.Fatalf("expected request and attachment evidence: %+v", batch.Turns)
	}
	if len(batch.Contents) == 0 || batch.Ancestry == nil {
		t.Fatal("capture did not return content or ancestry")
	}
}

// A missing transcript must prevent offset advancement.
func TestProviderCaptureObservedInputs_MissingTranscriptFails(t *testing.T) {
	p := New()
	if _, err := p.CaptureObservedInputs(context.Background(), filepath.Join(t.TempDir(), "absent.jsonl"), 0, 3); err == nil {
		t.Fatal("expected error for missing transcript")
	}
}

// A range extending past EOF must fail.
func TestProviderCaptureObservedInputs_IncompleteRangeRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	// Two records, but the caller asks for four lines.
	if err := os.WriteFile(path, []byte("{\"type\":\"user\",\"uuid\":\"a\",\"message\":{\"content\":\"hi\"}}\n{\"type\":\"assistant\",\"uuid\":\"b\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New()
	if _, err := p.CaptureObservedInputs(context.Background(), path, 0, 4); err == nil {
		t.Fatal("expected error for range past end of transcript")
	}
}

// Offset resets return an empty batch without panicking.
func TestProviderCaptureObservedInputs_ResetRangeDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"user\",\"uuid\":\"a\",\"message\":{\"content\":\"hi\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New()
	batch, err := p.CaptureObservedInputs(context.Background(), path, 3, 1)
	if err != nil {
		t.Fatalf("reset range should be an empty success, got %v", err)
	}
	if len(batch.Turns) != 0 {
		t.Fatalf("expected empty batch on reset, got %+v", batch.Turns)
	}
}

// Blank lines preserve record positions without producing malformed gaps.
func TestProviderCaptureObservedInputs_BlankLinesPreservePositions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	// Blank line between two records; request UUID stays at absolute position 3.
	if err := os.WriteFile(path, []byte("{\"type\":\"user\",\"uuid\":\"a\",\"message\":{\"content\":\"one\"}}\n\n{\"type\":\"user\",\"uuid\":\"c\",\"message\":{\"content\":\"two\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New()
	batch, err := p.CaptureObservedInputs(context.Background(), path, 0, 3)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	var reqs int
	for _, ev := range batch.Turns {
		reqs += len(ev.Requests)
		for _, g := range ev.Gaps {
			if g.Reason == "malformed" {
				t.Fatalf("blank line produced a malformed gap: %+v", g)
			}
		}
	}
	if reqs != 2 {
		t.Fatalf("expected two requests across the blank line, got %d", reqs)
	}
}
