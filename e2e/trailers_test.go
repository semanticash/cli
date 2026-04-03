//go:build e2e

package e2e_test

import (
	"strings"
	"testing"
)

func TestCommitAppendsCheckpointTrailer(t *testing.T) {
	dir, env := initGitRepo(t)
	enableRepo(t, env, dir)

	commitFile(t, env, dir, "hello.go",
		"package main\n\nfunc hello() {}\n", "add hello.go")

	body := runGit(t, env, dir, "log", "-1", "--format=%B")

	found := false
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "Semantica-Checkpoint:") {
			found = true
			value := strings.TrimSpace(strings.TrimPrefix(line, "Semantica-Checkpoint:"))
			// Value should be a non-empty UUID-like string.
			if len(value) < 8 {
				t.Errorf("trailer value too short: %q", value)
			}
			break
		}
	}
	if !found {
		t.Errorf("Semantica-Checkpoint trailer not found in commit message:\n%s", body)
	}
}

func TestNoTrailersWithoutEnable(t *testing.T) {
	dir, env := initGitRepo(t)
	// Deliberately NOT enabling semantica.

	commitFile(t, env, dir, "hello.go",
		"package main\n\nfunc hello() {}\n", "add hello.go")

	body := runGit(t, env, dir, "log", "-1", "--format=%B")
	if strings.Contains(body, "Semantica-") {
		t.Errorf("found Semantica trailer without enable:\n%s", body)
	}
}
