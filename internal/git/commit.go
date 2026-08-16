package git

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

func (r *Repo) HeadCommitHash(ctx context.Context) (string, error) {
	cmd := r.gitCmd(ctx, "rev-parse", "HEAD")

	out, err := cmd.Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return "", fmt.Errorf("git rev-parse HEAD failed: %w: %s", err, string(ee.Stderr))
		}
		return "", fmt.Errorf("git rev-parse HEAD failed: %w", err)
	}

	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", fmt.Errorf("empty HEAD sha")
	}
	return sha, nil
}

// StagedTree writes the index as a tree object and returns its hash.
func (r *Repo) StagedTree(ctx context.Context) (string, error) {
	out, err := r.gitCmd(ctx, "write-tree").Output()
	if err != nil {
		return "", fmt.Errorf("git write-tree failed: %w", err)
	}
	tree := strings.TrimSpace(string(out))
	if tree == "" {
		return "", fmt.Errorf("empty staged tree")
	}
	return tree, nil
}

// HeadOrEmpty returns the current HEAD commit, or "" on an unborn branch.
func (r *Repo) HeadOrEmpty(ctx context.Context) (string, error) {
	return r.revParseOrEmpty(ctx, "HEAD")
}

// CommitTree returns the tree hash recorded by a commit.
func (r *Repo) CommitTree(ctx context.Context, ref string) (string, error) {
	out, err := r.gitCmd(ctx, "rev-parse", "--verify", ref+"^{tree}").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse tree failed for %s: %w", ref, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// FirstParentOrEmpty returns a commit's first parent, or "" for a root commit.
func (r *Repo) FirstParentOrEmpty(ctx context.Context, ref string) (string, error) {
	return r.revParseOrEmpty(ctx, ref+"^")
}

// revParseOrEmpty returns "" when a revision does not exist. Other Git errors
// are returned to the caller.
func (r *Repo) revParseOrEmpty(ctx context.Context, rev string) (string, error) {
	out, err := r.gitCmd(ctx, "rev-parse", "--verify", "--quiet", rev).Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok && ee.ExitCode() == 1 {
			return "", nil
		}
		return "", fmt.Errorf("git rev-parse %s failed: %w", rev, err)
	}
	return strings.TrimSpace(string(out)), nil
}
