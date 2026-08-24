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

// ErrMissingHistory indicates that a commit's parent object is unavailable.
var ErrMissingHistory = errors.New("parent commit object is unavailable (shallow history?)")

// CommitParent returns the first parent declared by a commit. A commit without
// a parent is a root; an unavailable or invalid parent returns an error.
func (r *Repo) CommitParent(ctx context.Context, commit string) (parent string, isRoot bool, err error) {
	if strings.TrimSpace(commit) == "" {
		return "", false, fmt.Errorf("commit parent: empty commit")
	}
	out, err := r.gitCmd(ctx, "cat-file", "commit", commit).Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return "", false, fmt.Errorf("git cat-file commit %s failed: %w: %s", commit, err, string(ee.Stderr))
		}
		return "", false, fmt.Errorf("git cat-file commit %s failed: %w", commit, err)
	}
	// Read the first parent directly from the commit headers. Unlike revision
	// traversal, this preserves the parent ID at a shallow boundary.
	var hasParent bool
	var firstParent string
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			break // end of headers
		}
		if p, ok := strings.CutPrefix(line, "parent "); ok {
			hasParent = true
			firstParent = strings.TrimSpace(p)
			break
		}
	}
	if !hasParent {
		return "", true, nil // genuine root: the object declares no parent
	}
	if !IsFullCommitHash(firstParent) {
		return "", false, fmt.Errorf("commit %s has a malformed parent header %q", commit, firstParent)
	}
	// A bare existence check reports a missing object with exit code 1.
	if _, err := r.gitCmd(ctx, "cat-file", "-e", firstParent).Output(); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return "", false, fmt.Errorf("verifying first parent %s of %s: %w", firstParent, commit, cerr)
		}
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			if ee.ExitCode() == 1 {
				return "", false, fmt.Errorf("first parent %s of %s: %w", firstParent, commit, ErrMissingHistory)
			}
			return "", false, fmt.Errorf("verifying first parent %s of %s: %w: %s", firstParent, commit, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", false, fmt.Errorf("verifying first parent %s of %s: %w", firstParent, commit, err)
	}
	// A parent header must name a commit, not merely an existing object.
	typeOut, err := r.gitCmd(ctx, "cat-file", "-t", firstParent).Output()
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return "", false, fmt.Errorf("typing first parent %s of %s: %w", firstParent, commit, cerr)
		}
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return "", false, fmt.Errorf("typing first parent %s of %s: %w: %s", firstParent, commit, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", false, fmt.Errorf("typing first parent %s of %s: %w", firstParent, commit, err)
	}
	if objType := strings.TrimSpace(string(typeOut)); objType != "commit" {
		return "", false, fmt.Errorf("first parent %s of %s is a %s, not a commit", firstParent, commit, objType)
	}
	return firstParent, false, nil
}

// EmptyTreeID computes the repository's empty-tree object ID for its object
// format (SHA-1 or SHA-256) without writing anything.
func (r *Repo) EmptyTreeID(ctx context.Context) (string, error) {
	cmd := r.gitCmd(ctx, "hash-object", "-t", "tree", "--stdin")
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return "", fmt.Errorf("git hash-object empty tree failed: %w: %s", err, string(ee.Stderr))
		}
		return "", fmt.Errorf("git hash-object empty tree failed: %w", err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", fmt.Errorf("empty tree id resolved to empty output")
	}
	return id, nil
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
