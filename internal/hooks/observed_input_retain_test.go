package hooks

import (
	"context"
	"testing"

	"github.com/semanticash/cli/internal/observedinput"
)

// fakeCapturer is a provider that produces a fixed observed-input batch.
type fakeCapturer struct {
	fakeProvider
	batch ObservedInputBatch
	err   error
	calls int
}

func (f *fakeCapturer) CaptureObservedInputs(ctx context.Context, _ string, _, _ int) (ObservedInputBatch, error) {
	f.calls++
	return f.batch, f.err
}

// An unresolved destination preserves captured evidence for retry.
func TestRetainObservedInputs_UnresolvedRepoPreservesRetry(t *testing.T) {
	fc := &fakeCapturer{batch: ObservedInputBatch{Turns: []observedinput.Evidence{{}}}}
	fc.name = "claude_code"
	if !retainObservedInputs(context.Background(), fc, "/t.jsonl", "", "psess", 0, 1) {
		t.Fatal("expected retry when the owning repository is unresolved")
	}
	if fc.calls != 1 {
		t.Fatalf("capture must be attempted even without a target repo, calls=%d", fc.calls)
	}
}

// Providers without observed-input capture require no retention.
func TestRetainObservedInputs_UnsupportedProviderIsNoOp(t *testing.T) {
	fp := &fakeProvider{name: "codex"}
	if retainObservedInputs(context.Background(), fp, "/t.jsonl", "", "psess", 0, 1) {
		t.Fatal("unsupported provider should be a no-op, not a retry")
	}
}

// An empty batch advances normally, but capture is still attempted.
func TestRetainObservedInputs_EmptyBatchAdvances(t *testing.T) {
	fc := &fakeCapturer{batch: ObservedInputBatch{}}
	fc.name = "claude_code"
	if retainObservedInputs(context.Background(), fc, "/t.jsonl", "", "psess", 0, 1) {
		t.Fatal("empty batch should not force a retry")
	}
	if fc.calls != 1 {
		t.Fatal("capture should be attempted before deciding the batch is empty")
	}
}
