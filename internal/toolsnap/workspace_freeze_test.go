package toolsnap

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFreezeWorkspace_CapturesImmutableTreeAndPublishesRef(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	writeFile(t, root, "a.txt", "frozen content\n")

	fz, err := reg.FreezeWorkspace(ctx, s, "obs-1")
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if fz.Ref != WorkspaceFreezeRef("obs-1") {
		t.Errorf("ref = %q, want %q", fz.Ref, WorkspaceFreezeRef("obs-1"))
	}
	if !validHash(fz.TreeHash, s.repo.ObjectFormat) {
		t.Errorf("tree hash = %q, not a valid object id", fz.TreeHash)
	}
	if got := strings.TrimSpace(mustGit(t, s, ctx, "rev-parse", fz.Ref)); got != fz.TreeHash {
		t.Errorf("ref target = %q, want %q", got, fz.TreeHash)
	}

	writeFile(t, root, "a.txt", "changed after freeze\n")
	if got := mustGit(t, s, ctx, "show", fz.TreeHash+":a.txt"); got != "frozen content\n" {
		t.Errorf("frozen a.txt = %q, want the pre-mutation content", got)
	}
}

func TestFreezeWorkspace_RefProtectsTreeUntilReleased(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	writeFile(t, root, "a.txt", "protected freeze\n")
	fz, err := reg.FreezeWorkspace(ctx, s, "obs-keep")
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}

	// The ref keeps aged objects reachable through maintenance.
	backdateObjects(t, s, DefaultPruneGrace+time.Hour)
	report, err := s.Maintain(ctx, reg, 0)
	if err != nil {
		t.Fatalf("maintain: %v", err)
	}
	if !report.PruneRan {
		t.Fatalf("report = %+v, want prune to run", report)
	}
	if _, err := s.git(ctx, "cat-file", "-e", fz.TreeHash); err != nil {
		t.Fatalf("frozen tree pruned while ref present: %v", err)
	}
	if got := strings.TrimSpace(mustGit(t, s, ctx, "rev-parse", fz.Ref)); got != fz.TreeHash {
		t.Fatalf("freeze ref removed by maintenance: %q", got)
	}

	// Releasing the ref makes the tree reclaimable.
	if err := s.DeleteRef(ctx, fz.Ref, fz.TreeHash); err != nil {
		t.Fatalf("release ref: %v", err)
	}
	backdateObjects(t, s, DefaultPruneGrace+time.Hour)
	if _, err := s.Maintain(ctx, reg, 0); err != nil {
		t.Fatalf("maintain after release: %v", err)
	}
	if _, err := s.git(ctx, "cat-file", "-e", fz.TreeHash); err == nil {
		t.Fatalf("frozen tree survived after ref release")
	}
}

func TestValidRefName_WorkspaceFreezeNamespace(t *testing.T) {
	cases := []struct {
		ref  string
		want bool
	}{
		{WorkspaceFreezeRef("obs-1"), true},
		{WorkspaceFreezeRef("a/b"), true},          // slash sanitized to one component
		{workspaceFreezeRefPrefix + "/a/b", false}, // multi-component freeze rejected
		{refPrefix + "/main/g/tu/pre", true},
		{"refs/heads/main", false},
		{"refs/semantica/other/x", false},
		{workspaceFreezeRefPrefix, false}, // prefix only, no component
	}
	for _, c := range cases {
		if got := validRefName(c.ref); got != c.want {
			t.Errorf("validRefName(%q) = %v, want %v", c.ref, got, c.want)
		}
	}
}

func TestCreateRef_RejectsForeignNamespace(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()
	snap, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRef(ctx, "refs/heads/evil", snap.TreeHash); err == nil {
		t.Fatal("CreateRef accepted a ref outside the toolsnap namespaces")
	}
}

func TestListWorkspaceFreezeRefs_DiscoverableAndSeparate(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	writeFile(t, root, "a.txt", "one\n")
	f1, err := reg.FreezeWorkspace(ctx, s, "obs-1")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "a.txt", "two\n")
	f2, err := reg.FreezeWorkspace(ctx, s, "obs-2")
	if err != nil {
		t.Fatal(err)
	}

	freezes, err := s.ListWorkspaceFreezeRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if freezes[f1.Ref] != f1.TreeHash || freezes[f2.Ref] != f2.TreeHash || len(freezes) != 2 {
		t.Fatalf("freeze refs = %v, want both observations discoverable", freezes)
	}
	windows, err := s.ListRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := windows[f1.Ref]; ok || len(windows) != 0 {
		t.Fatalf("tool-window refs = %v, want no freeze refs", windows)
	}
}

// A structurally disallowed ref under the freeze namespace fails the listing
// closed, so corruption cannot look like an empty backlog.
func TestListWorkspaceFreezeRefs_FailsClosedOnMalformed(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	writeFile(t, root, "a.txt", "one\n")
	fz, err := reg.FreezeWorkspace(ctx, s, "obs-1")
	if err != nil {
		t.Fatal(err)
	}
	// Seed a valid object at a nested (structurally disallowed) freeze ref,
	// bypassing CreateRef's validation.
	if _, err := s.git(ctx, "update-ref", workspaceFreezeRefPrefix+"/nested/deep", fz.TreeHash); err != nil {
		t.Fatalf("seed nested ref: %v", err)
	}
	if _, err := s.ListWorkspaceFreezeRefs(ctx); err == nil {
		t.Fatal("ListWorkspaceFreezeRefs ignored a disallowed nested ref; want fail closed")
	}
}

func TestDeleteRef_RejectsForeignRefAndBadTarget(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	reg, err := OpenRegistry(filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	writeFile(t, root, "a.txt", "keep\n")
	fz, err := reg.FreezeWorkspace(ctx, s, "obs-1")
	if err != nil {
		t.Fatal(err)
	}

	foreign := "refs/semantica/other/x"
	if _, err := s.git(ctx, "update-ref", foreign, fz.TreeHash); err != nil {
		t.Fatalf("seed foreign ref: %v", err)
	}
	if err := s.DeleteRef(ctx, foreign, fz.TreeHash); err == nil {
		t.Fatal("DeleteRef accepted a ref outside the toolsnap namespaces")
	}
	if got := strings.TrimSpace(mustGit(t, s, ctx, "rev-parse", foreign)); got != fz.TreeHash {
		t.Fatalf("foreign ref was disturbed: %q", got)
	}
	if err := s.DeleteRef(ctx, fz.Ref, "not-a-hash"); err == nil {
		t.Fatal("DeleteRef accepted an invalid target hash")
	}
	if got := strings.TrimSpace(mustGit(t, s, ctx, "rev-parse", fz.Ref)); got != fz.TreeHash {
		t.Fatalf("freeze ref disturbed by rejected delete: %q", got)
	}
	if err := s.DeleteRef(ctx, fz.Ref, fz.TreeHash); err != nil {
		t.Fatalf("valid delete: %v", err)
	}
}

func mustGit(t *testing.T, s *Store, ctx context.Context, args ...string) string {
	t.Helper()
	out, err := s.git(ctx, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}
