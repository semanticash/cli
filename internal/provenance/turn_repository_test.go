package provenance

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/semanticash/cli/internal/store/blobs"
)

func TestEnsurePromptCandidate(t *testing.T) {
	ctx := context.Background()
	target, err := blobs.NewStore(filepath.Join(t.TempDir(), "target"))
	if err != nil {
		t.Fatal(err)
	}
	if prompt, err := ensurePromptCandidate(ctx, target, nil, PromptCandidate{}); err != nil || prompt != nil {
		t.Fatalf("empty candidate = (%+v, %v), want no prompt", prompt, err)
	}
	if _, err := ensurePromptCandidate(ctx, target, nil, PromptCandidate{EventID: "prompt", Hash: "missing"}); err == nil {
		t.Fatal("missing source accepted")
	}

	source, err := blobs.NewStore(filepath.Join(t.TempDir(), "source"))
	if err != nil {
		t.Fatal(err)
	}
	hash, _, err := source.Put(ctx, []byte("prompt"))
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := ensurePromptCandidate(ctx, target, source, PromptCandidate{EventID: "prompt", Hash: hash})
	if err != nil {
		t.Fatal(err)
	}
	if prompt == nil || prompt.EventID != "prompt" || prompt.PayloadHash != hash || !target.Exists(hash) {
		t.Fatalf("propagated prompt = %+v, exists=%v", prompt, target.Exists(hash))
	}
}
