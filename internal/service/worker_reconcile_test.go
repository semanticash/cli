package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/providers"
	"github.com/semanticash/cli/internal/store/blobs"
)

func TestReconcileActiveSessions_ScopedToLockedRepo(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	repoA, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(repoA, "vendor-repo")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	repoC, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	regPath, err := broker.DefaultRegistryPath()
	if err != nil {
		t.Fatal(err)
	}
	bh, err := broker.Open(ctx, regPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{repoA, nested, repoC} {
		if err := broker.Register(ctx, bh, r, r); err != nil {
			t.Fatalf("register %s: %v", r, err)
		}
	}
	if err := broker.Close(bh); err != nil {
		t.Fatal(err)
	}

	states := []*hooks.CaptureState{
		{SessionID: "s-owned", Provider: "claude-code", TranscriptRef: "t", Timestamp: 1, CWD: repoA},
		{SessionID: "s-nested", Provider: "claude-code", TranscriptRef: "t", Timestamp: 1, CWD: nested},
		{SessionID: "s-other", Provider: "claude-code", TranscriptRef: "t", Timestamp: 1, CWD: repoC},
		{SessionID: "s-no-cwd", Provider: "claude-code", TranscriptRef: "t", Timestamp: 1},
		{SessionID: "s-orphan", StateKey: "s-orphan.orphan.1", Provider: "claude-code",
			TranscriptRef: "t", Timestamp: 1, CWD: repoA, OrphanedAt: 5},
	}
	for _, st := range states {
		if err := hooks.SaveCaptureState(st); err != nil {
			t.Fatal(err)
		}
	}

	var captured []string
	orig := reconcileCapture
	reconcileCapture = func(ctx context.Context, provider hooks.HookProvider, event *hooks.Event, bh *broker.Handle, bs *blobs.Store, repoRoot string) (bool, error) {
		captured = append(captured, event.SessionID)
		if !sameReconcilePath(repoRoot, repoA) {
			t.Errorf("capture scoped to %s, want %s", repoRoot, repoA)
		}
		return true, nil
	}
	t.Cleanup(func() { reconcileCapture = orig })

	reconcileActiveSessions(ctx, providers.NewHookRegistry(), repoA)

	if len(captured) != 1 || captured[0] != "s-owned" {
		t.Fatalf("captured %v, want only [s-owned]: nested, other-repo, and "+
			"cwd-less sessions must not reconcile under this lock", captured)
	}
}
