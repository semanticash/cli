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
	if prompt := ensurePromptCandidate(ctx, target, nil, PromptCandidate{}); prompt != nil {
		t.Fatalf("empty candidate = %+v, want no prompt", prompt)
	}
	if prompt := ensurePromptCandidate(ctx, target, nil, PromptCandidate{EventID: "prompt", Hash: "missing"}); prompt != nil {
		t.Fatalf("unresolvable candidate = %+v, want no prompt", prompt)
	}

	source, err := blobs.NewStore(filepath.Join(t.TempDir(), "source"))
	if err != nil {
		t.Fatal(err)
	}
	hash, _, err := source.Put(ctx, []byte("prompt"))
	if err != nil {
		t.Fatal(err)
	}
	prompt := ensurePromptCandidate(ctx, target, source, PromptCandidate{EventID: "prompt", Hash: hash})
	if prompt == nil || prompt.EventID != "prompt" || prompt.PayloadHash != hash || !target.Exists(hash) {
		t.Fatalf("propagated prompt = %+v, exists=%v", prompt, target.Exists(hash))
	}
}
