package hooks

import (
	"context"
	"testing"

	"github.com/semanticash/cli/internal/toolsnap"
)

// Ref release leaves cleanup to maintenance while the coordination lock is busy.
func TestReleaseGroupRefsDefersUnderLock(t *testing.T) {
	home := t.TempDir()
	w := newToolWindowWorld(t, home, "repo")
	ctx := context.Background()

	rc, err := toolsnap.ResolveRepoContext(ctx, w.repoPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := toolsnap.OpenStore(ctx, rc, w.semDir)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := toolsnap.OpenRegistry(w.semDir)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.CaptureBefore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ref := toolsnap.SnapshotRef("main", "g-x", "tu-x")
	if err := store.CreateRef(ctx, ref, snap.TreeHash); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = reg.WithCoordinationLock(context.Background(), func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	releaseGroupRefs(ctx, reg, store, map[string]string{ref: snap.TreeHash})
	close(release)
	<-done

	refs, err := store.ListRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := refs[ref]; !ok {
		t.Error("ref was deleted despite lock contention; should defer to maintenance")
	}
}
