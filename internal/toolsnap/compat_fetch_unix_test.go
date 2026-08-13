//go:build !windows

package toolsnap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installFetchMarker records any Git transport attempt through testfetch::.
func installFetchMarker(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "fetch-invoked")
	script := "#!/bin/sh\n: > " + marker + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "git-remote-testfetch"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

func assertNoFetch(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("transport helper was invoked: a fetch was attempted")
	}
}

// TestStoreConfigInjectionCannotEnableFetch verifies inherited Git config
// cannot add a promisor remote to the snapshot store.
func TestStoreConfigInjectionCannotEnableFetch(t *testing.T) {
	marker := installFetchMarker(t)

	globalCfg := filepath.Join(t.TempDir(), "gitconfig")
	configData := strings.Join([]string{
		"[extensions]",
		"\tpartialClone = snapshot-source",
		"[remote \"snapshot-source\"]",
		"\turl = testfetch::snapshot-source",
		"\tfetch = +refs/heads/*:refs/remotes/snapshot-source/*",
		"\tpromisor = true",
		"",
	}, "\n")
	if err := os.WriteFile(globalCfg, []byte(configData), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)

	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	// Git tries the promisor remote when this missing object is requested.
	bogus := strings.Repeat("ab", 20)
	if _, err := s.ReadBlob(ctx, bogus); err == nil {
		t.Fatal("bogus blob read succeeded")
	}
	assertNoFetch(t, marker)

	// Capture must also remain transport-free.
	writeFile(t, root, "a.txt", "config injection capture\n")
	if _, err := s.CaptureBefore(ctx); err != nil {
		t.Fatalf("capture under injected config: %v", err)
	}
	assertNoFetch(t, marker)
}

// TestStoreLocalConfigInjectionCannotEnableFetch verifies OpenStore removes
// fetch-capable settings from the store's local config.
func TestStoreLocalConfigInjectionCannotEnableFetch(t *testing.T) {
	marker := installFetchMarker(t)

	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	run(t, root, "git", "--git-dir", s.Dir, "config", "extensions.partialClone", "snapshot-source")
	run(t, root, "git", "--git-dir", s.Dir, "config", "remote.snapshot-source.url", "testfetch::snapshot-source")
	run(t, root, "git", "--git-dir", s.Dir, "config", "remote.snapshot-source.fetch", "+refs/heads/*:refs/remotes/snapshot-source/*")
	run(t, root, "git", "--git-dir", s.Dir, "config", "remote.snapshot-source.promisor", "true")

	rc, err := ResolveRepoContext(ctx, root)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	reopened, err := OpenStore(ctx, rc, filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatalf("reopen modified store: %v", err)
	}

	bogus := strings.Repeat("ab", 20)
	if _, err := reopened.ReadBlob(ctx, bogus); err == nil {
		t.Fatal("bogus blob read succeeded")
	}
	assertNoFetch(t, marker)

	cfg := run(t, root, "git", "--git-dir", reopened.Dir, "config", "--local", "--list")
	if strings.Contains(cfg, "remote.") || strings.Contains(strings.ToLower(cfg), "partialclone") {
		t.Errorf("fetch-enabling config survived sanitization:\n%s", cfg)
	}
}

// TestStoreIncludedConfigInjectionCannotEnableFetch verifies OpenStore
// removes an include that supplies promisor configuration.
func TestStoreIncludedConfigInjectionCannotEnableFetch(t *testing.T) {
	marker := installFetchMarker(t)

	includedPath := filepath.Join(t.TempDir(), "included-config")
	included := strings.Join([]string{
		"[extensions]",
		"\tpartialClone = snapshot-source",
		"[remote \"snapshot-source\"]",
		"\turl = testfetch::snapshot-source",
		"\tfetch = +refs/heads/*:refs/remotes/snapshot-source/*",
		"\tpromisor = true",
		"",
	}, "\n")
	if err := os.WriteFile(includedPath, []byte(included), 0o644); err != nil {
		t.Fatal(err)
	}

	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()
	run(t, root, "git", "--git-dir", s.Dir, "config", "include.path", includedPath)

	rc, err := ResolveRepoContext(ctx, root)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	reopened, err := OpenStore(ctx, rc, filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatalf("reopen store with include: %v", err)
	}

	bogus := strings.Repeat("ab", 20)
	if _, err := reopened.ReadBlob(ctx, bogus); err == nil {
		t.Fatal("bogus blob read succeeded")
	}
	assertNoFetch(t, marker)

	cfg := run(t, root, "git", "--git-dir", reopened.Dir, "config", "--local", "--list")
	if strings.Contains(strings.ToLower(cfg), "include") {
		t.Errorf("include survived sanitization:\n%s", cfg)
	}
}

// TestStoreConditionalIncludeInjectionCannotEnableFetch verifies OpenStore
// removes a matching includeIf entry before object access.
func TestStoreConditionalIncludeInjectionCannotEnableFetch(t *testing.T) {
	marker := installFetchMarker(t)

	includedPath := filepath.Join(t.TempDir(), "included-config")
	included := strings.Join([]string{
		"[remote \"snapshot-source\"]",
		"\turl = testfetch::snapshot-source",
		"\tfetch = +refs/heads/*:refs/remotes/snapshot-source/*",
		"\tpromisor = true",
		"",
	}, "\n")
	if err := os.WriteFile(includedPath, []byte(included), 0o644); err != nil {
		t.Fatal(err)
	}

	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()
	run(t, root, "git", "--git-dir", s.Dir, "config",
		`includeIf.gitdir:**/tool-snapshots.git.path`, includedPath)

	rc, err := ResolveRepoContext(ctx, root)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	reopened, err := OpenStore(ctx, rc, filepath.Join(root, ".semantica"))
	if err != nil {
		t.Fatalf("reopen store with conditional include: %v", err)
	}

	bogus := strings.Repeat("ab", 20)
	if _, err := reopened.ReadBlob(ctx, bogus); err == nil {
		t.Fatal("bogus blob read succeeded")
	}
	assertNoFetch(t, marker)

	cfg := run(t, root, "git", "--git-dir", reopened.Dir, "config", "--local", "--list")
	if strings.Contains(strings.ToLower(cfg), "include") {
		t.Errorf("conditional include survived sanitization:\n%s", cfg)
	}
}

// TestPartialCloneReadsNeverInvokeTransport verifies capture and missing-object
// reads do not contact a partial clone's promisor remote.
func TestPartialCloneReadsNeverInvokeTransport(t *testing.T) {
	marker := installFetchMarker(t)

	root := testRepo(t)
	historicalHash := strings.TrimSpace(run(t, root, "git", "rev-parse", "HEAD:a.txt"))
	writeFile(t, root, "a.txt", "superseding version\n")
	run(t, root, "git", "add", "a.txt")
	run(t, root, "git", "commit", "-q", "-m", "supersede")

	clone := partialClone(t, root)
	run(t, clone, "git", "remote", "set-url", "origin", "testfetch::unreachable")
	ctx := context.Background()

	rc, err := ResolveRepoContext(ctx, clone)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	s, err := OpenStore(ctx, rc, filepath.Join(clone, ".semantica"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	writeFile(t, clone, "a.txt", "partial clone edit\n")
	if _, err := s.CaptureBefore(ctx); err != nil {
		t.Fatalf("capture: %v", err)
	}
	if _, err := s.ReadBlob(ctx, historicalHash); err == nil {
		t.Fatal("promised blob read succeeded, want missing-object failure")
	}
	assertNoFetch(t, marker)
}
