package toolsnap

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreWithExplicitCommonDirectoryRemainsBare(t *testing.T) {
	store := openTestStore(t, testRepo(t))
	cmd, err := store.gitCommand(context.Background(), nil, "rev-parse", "--is-bare-repository")
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(cmd.Env, "GIT_COMMON_DIR="+store.Dir)
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Fatalf("common directory changed bare-store semantics: %s, %v", out, err)
	}
}

func TestTurnSnapshotsWithLongStorePath(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := "unchanged"
		if changed {
			name = "commit_and_dirty"
		}
		t.Run(name, func(t *testing.T) { testLongStoreTurn(t, changed) })
	}
}

func testLongStoreTurn(t *testing.T, changed bool) {
	t.Helper()
	ctx := context.Background()
	root := testRepo(t)
	storage := t.TempDir()
	for len(storage) < 300 {
		storage = filepath.Join(storage, strings.Repeat("s", 64))
	}
	baseline, err := FreezeTurnSubject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := CaptureTurnBaseline(ctx, storage, &baseline); err != nil {
		t.Fatal(err)
	}
	if !changed {
		if got := ObserveTurnEnd(ctx, storage, baseline); got.State != "unchanged" {
			t.Fatalf("unchanged turn: %+v", got)
		}
		return
	}

	writeFile(t, root, "a.txt", "committed during turn\n")
	run(t, root, "git", "commit", "-qam", "inside turn")
	writeFile(t, root, "a.txt", "dirty after commit\n")
	writeFile(t, root, "new file.txt", "untracked\n")
	got := ObserveTurnEnd(ctx, storage, baseline)
	if got.State != "changed" || len(got.Changes) != 2 || got.Changes[0].Commit == "" {
		t.Fatalf("commit and dirty turn: %+v", got)
	}
	if len(got.Changes[0].Files) != 1 || len(got.Changes[1].Files) != 2 {
		t.Fatalf("missing file evidence: %+v", got.Changes)
	}
	store, err := OpenStoreForInspection(ctx, baseline.Repository, storage)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a.txt": "dirty after commit\n", "new file.txt": "untracked\n"}
	for _, file := range got.Changes[1].Files {
		blobs, _, err := store.batchReadBlobs(ctx, []string{file.AfterHash})
		if err != nil {
			t.Fatal(err)
		}
		if content, ok := want[file.Path]; !ok || string(blobs[file.AfterHash].content) != content {
			t.Fatalf("unexpected captured content for %q: %q", file.Path, blobs[file.AfterHash].content)
		}
	}
}
