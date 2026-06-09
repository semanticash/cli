package service

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/util"
)

// PrePushService handles git's pre-push hook.
//
// git invokes pre-push with the remote name + URL as argv and one
// "<local-ref> <local-sha> <remote-ref> <remote-sha>" line per pushed
// ref on stdin. The hook stays NON-blocking by contract (any error
// inside the service must not fail the push) so settings reads, file
// I/O, and command spawn errors are all logged-and-swallowed.
type PrePushService struct{}

func NewPrePushService() *PrePushService { return &PrePushService{} }

// PrePushResult records what the hook decided for tests and doctor output.
type PrePushResult struct {
	RepoRoot      string
	CurrentBranch string
	// Triggered is true when the current branch appeared in the pushed
	// refs AND the intent-gap setting was on. False covers every
	// no-op reason (Semantica disabled, intent-gap disabled, branch
	// not pushed, etc.).
	Triggered bool
	// Reason gives a one-line human-readable explanation, mirrored into
	// the activity log so `semantica doctor` can surface the last
	// trigger decision without re-running the hook.
	Reason string
}

// HandlePrePush is the hook entry point.
//
// Contract:
//   - Always returns nil (NEVER block the push).
//   - Decisions land in PrePushResult and the activity log.
//   - When triggered, follow-up analysis runs outside the blocking hook path.
func (s *PrePushService) HandlePrePush(ctx context.Context, repoPath string, stdin io.Reader) (*PrePushResult, error) {
	repo, err := git.OpenRepo(repoPath)
	if err != nil {
		return &PrePushResult{Reason: fmt.Sprintf("open repo failed: %v", err)}, nil
	}
	repoRoot := repo.Root()
	semDir := filepath.Join(repoRoot, ".semantica")

	res := &PrePushResult{RepoRoot: repoRoot}

	// Gate 1: Semantica disabled? Hooks always silent on disabled repos.
	if !util.IsEnabled(semDir) {
		res.Reason = "semantica not enabled"
		return res, nil
	}

	// Gate 2: intent-gap setting off.
	if !util.IntentGapEnabled(semDir) {
		res.Reason = "intent_gap.enabled is false"
		return res, nil
	}

	// Gate 3: current branch isn't in the pushed-ref list.
	//
	// Per the trigger contract, the hook only fires when the user
	// pushes the branch they are currently checked out on. Pushing a
	// different ref (e.g. `git push origin other-branch:other-branch`
	// while sitting on main) is intentionally ignored so the analysis
	// matches the working-copy state the developer was just looking
	// at.
	branch, branchErr := repo.CurrentBranch(ctx)
	if branchErr != nil || branch == "" {
		res.Reason = fmt.Sprintf("current branch unavailable: %v", branchErr)
		return res, nil
	}
	res.CurrentBranch = branch

	pushed, parseErr := parsePushedRefs(stdin)
	if parseErr != nil {
		res.Reason = fmt.Sprintf("parse pre-push stdin failed: %v", parseErr)
		util.AppendActivityLog(semDir, "pre-push: %s", res.Reason)
		return res, nil
	}

	matched := false
	for _, ref := range pushed {
		if shortRefName(ref.LocalRef) == branch {
			matched = true
			break
		}
	}
	if !matched {
		res.Reason = fmt.Sprintf("current branch %q not in pushed refs", branch)
		return res, nil
	}

	// All gates passed. Record the decision for doctor and future upload work.
	res.Triggered = true
	res.Reason = fmt.Sprintf("intent-gap trigger on branch %q (push to be analyzed)", branch)
	util.AppendActivityLog(semDir, "pre-push: %s", res.Reason)
	return res, nil
}

// PushedRef is one parsed line of pre-push stdin.
type PushedRef struct {
	LocalRef  string
	LocalSHA  string
	RemoteRef string
	RemoteSHA string
}

// parsePushedRefs reads git's pre-push stdin protocol. Each line is
//
//	<local-ref> SP <local-sha> SP <remote-ref> SP <remote-sha>
//
// where a deleted ref uses the zero SHA. We don't filter deletions
// here - the caller's branch-match check naturally excludes them
// because a delete pushes a different ref name than the current
// branch.
func parsePushedRefs(r io.Reader) ([]PushedRef, error) {
	if r == nil {
		return nil, nil
	}
	var refs []PushedRef
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 4 {
			return nil, fmt.Errorf("malformed pre-push line: %q", line)
		}
		refs = append(refs, PushedRef{
			LocalRef:  fields[0],
			LocalSHA:  fields[1],
			RemoteRef: fields[2],
			RemoteSHA: fields[3],
		})
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read pre-push stdin: %w", err)
	}
	return refs, nil
}

// shortRefName strips the refs/heads/ prefix from a git ref so it can
// be compared to the value `git rev-parse --abbrev-ref HEAD` returns
// (the same form GitHub webhooks store in pull_requests.head_branch).
// Other ref namespaces (refs/tags/,
// refs/remotes/, etc.) flow through unchanged because they cannot
// match a branch name.
func shortRefName(ref string) string {
	const prefix = "refs/heads/"
	if strings.HasPrefix(ref, prefix) {
		return ref[len(prefix):]
	}
	return ref
}
