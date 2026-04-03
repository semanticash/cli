//go:build e2e

package e2e_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEnableCommitWorkerExplain(t *testing.T) {
	dir, env := initGitRepo(t)

	// Enable semantica.
	enableRepo(t, env, dir)

	// Verify core artifacts exist.
	semDir := filepath.Join(dir, ".semantica")
	if _, err := os.Stat(filepath.Join(semDir, "lineage.db")); err != nil {
		t.Fatalf("lineage.db not created: %v", err)
	}

	// Git hooks should be installed.
	for _, hook := range []string{"pre-commit", "post-commit", "commit-msg"} {
		hookPath := filepath.Join(dir, ".git", "hooks", hook)
		if _, err := os.Stat(hookPath); err != nil {
			t.Errorf("hook %s not installed: %v", hook, err)
		}
	}

	// Baseline checkpoint should exist.
	baseline := listCheckpoints(t, env, dir)
	if baseline.Count == 0 {
		t.Fatal("expected baseline checkpoint after enable")
	}

	// Make a commit (triggers hooks).
	hash := commitFile(t, env, dir, "main.go",
		"package main\n\nfunc main() {}\n", "add main.go")

	// Run worker synchronously.
	runWorker(t, env, dir, hash)

	// New auto checkpoint should exist with commit hash.
	cps := listCheckpoints(t, env, dir)
	if cps.Count < 2 {
		t.Fatalf("expected at least 2 checkpoints, got %d", cps.Count)
	}

	// Find the checkpoint matching our commit.
	var found *listCheckpoint
	for i, cp := range cps.Items {
		if cp.CommitHash == hash {
			found = &cps.Items[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no checkpoint found for commit %s", hash)
	}

	// Show checkpoint details.
	showOut := runSem(t, env, dir, "show", found.ID, "--json")
	var show showOutput
	if err := json.Unmarshal([]byte(showOut), &show); err != nil {
		t.Fatalf("parse show output: %v\n%s", err, showOut)
	}
	if show.ManifestHash == "" {
		t.Error("manifest_hash is empty")
	}
	if show.FileCount < 2 {
		t.Errorf("file_count = %d, want >= 2 (README.md + main.go)", show.FileCount)
	}

	// Explain the commit.
	explainOut := runSem(t, env, dir, "explain", hash, "--json")
	var explain explainOutput
	if err := json.Unmarshal([]byte(explainOut), &explain); err != nil {
		t.Fatalf("parse explain output: %v\n%s", err, explainOut)
	}
	if explain.FilesChanged == 0 {
		t.Error("explain: files_changed = 0, want > 0")
	}
	if explain.LinesAdded == 0 {
		t.Error("explain: lines_added = 0, want > 0")
	}
}
