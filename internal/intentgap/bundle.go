package intentgap

import (
	"context"
	"fmt"
)

// Bundle contains the local changes and captured prompts analyzed for a PR.
type Bundle struct {
	RepoRoot string
	BaseRef  string
	BaseSHA  string
	HeadSHA  string
	Commits  []BundleCommit
	// Diff is the cumulative unified diff, capped at MaxBundleDiffBytes.
	Diff []byte
	// Turns contains captured user prompts linked to commits in the PR range.
	Turns []BundleTurn
	// AgentActions contains the assistant tool-use records linked to
	// the same window as Turns. These are mechanical evidence only:
	// what tool touched what path, without semantic intent claims.
	AgentActions []BundleAgentAction
	Truncated    BundleTruncation
}

// BundleCommit identifies one commit in merge-base..HEAD.
type BundleCommit struct {
	Hash    string
	Subject string
}

// BundleTruncation records input omitted by bundle size limits.
type BundleTruncation struct {
	DiffBytesDropped    int
	CommitsDropped      int
	TurnsDropped        int
	AgentActionsDropped int
}

// Bundle size limits keep analyzer input bounded.
const (
	// MaxBundleDiffBytes caps the cumulative diff at 96 KiB.
	MaxBundleDiffBytes = 96 * 1024
	// MaxBundleCommits caps the commit list sent to the analyzer.
	MaxBundleCommits = 100
	// MaxBundleTurns caps captured prompts, retaining the most recent entries.
	MaxBundleTurns = 200
	// MaxBundleAgentActions caps captured agent actions, retaining the
	// most recent entries. This bounds analyzer input and payload reads.
	MaxBundleAgentActions = 500
)

// BundleAssembler builds analyzer input for a repository revision.
type BundleAssembler interface {
	Assemble(ctx context.Context, in BundleInput) (Bundle, error)
}

// BundleInput identifies the repository range to assemble.
type BundleInput struct {
	RepoRoot string
	// Base is the ref to diff against. Empty means "auto-detect".
	Base    string
	HeadSHA string
}

// GitBundleAssembler combines Git history with captured prompts.
type GitBundleAssembler struct {
	gitRepoOpener GitRepoOpener
	turnLoader    TurnLoader
}

// GitRepoOpener opens the Git operations required by bundle assembly.
type GitRepoOpener func(repoPath string) (GitRepo, error)

// GitRepo is the subset of git.Repo the assembler consumes.
type GitRepo interface {
	DefaultBaseRef(ctx context.Context) (string, error)
	MergeBase(ctx context.Context, a, b string) (string, error)
	DiffBetween(ctx context.Context, base, head string) ([]byte, error)
	CommitSummariesBetween(ctx context.Context, base, head string, limit int) ([]CommitMetaBetween, error)
	CountCommitsBetween(ctx context.Context, base, head string) (int, error)
}

// CommitMetaBetween describes a commit in the analyzed range.
type CommitMetaBetween struct {
	Hash    string
	Subject string
}

// NewGitBundleAssembler constructs a Git-backed bundle assembler.
func NewGitBundleAssembler(opener GitRepoOpener, turns TurnLoader) *GitBundleAssembler {
	if turns == nil {
		turns = NoopTurnLoader{}
	}
	return &GitBundleAssembler{gitRepoOpener: opener, turnLoader: turns}
}

// Assemble builds a bounded bundle for the requested revision range.
func (a *GitBundleAssembler) Assemble(ctx context.Context, in BundleInput) (Bundle, error) {
	if a.gitRepoOpener == nil {
		return Bundle{}, fmt.Errorf("bundle assembler: no GitRepoOpener wired")
	}
	if in.RepoRoot == "" || in.HeadSHA == "" {
		return Bundle{}, fmt.Errorf("bundle assembler: RepoRoot and HeadSHA are required")
	}

	repo, err := a.gitRepoOpener(in.RepoRoot)
	if err != nil {
		return Bundle{}, fmt.Errorf("open repo: %w", err)
	}

	baseRef := in.Base
	if baseRef == "" {
		baseRef, err = repo.DefaultBaseRef(ctx)
		if err != nil {
			return Bundle{}, fmt.Errorf("resolve default base: %w", err)
		}
	}

	mergeBase, err := repo.MergeBase(ctx, baseRef, in.HeadSHA)
	if err != nil {
		return Bundle{}, fmt.Errorf("merge-base %s %s: %w", baseRef, in.HeadSHA, err)
	}

	// Count separately so truncation reports every omitted commit.
	commits, err := repo.CommitSummariesBetween(ctx, mergeBase, in.HeadSHA, MaxBundleCommits)
	if err != nil {
		return Bundle{}, fmt.Errorf("list commits %s..%s: %w", mergeBase, in.HeadSHA, err)
	}
	total, err := repo.CountCommitsBetween(ctx, mergeBase, in.HeadSHA)
	if err != nil {
		return Bundle{}, fmt.Errorf("count commits %s..%s: %w", mergeBase, in.HeadSHA, err)
	}
	dropped := 0
	if total > len(commits) {
		dropped = total - len(commits)
	}
	bundleCommits := make([]BundleCommit, len(commits))
	for i, c := range commits {
		bundleCommits[i] = BundleCommit(c)
	}

	diff, err := repo.DiffBetween(ctx, mergeBase, in.HeadSHA)
	if err != nil {
		return Bundle{}, fmt.Errorf("diff %s..%s: %w", mergeBase, in.HeadSHA, err)
	}
	diffBytesDropped := 0
	if len(diff) > MaxBundleDiffBytes {
		diffBytesDropped = len(diff) - MaxBundleDiffBytes
		diff = diff[:MaxBundleDiffBytes]
	}

	commitHashes := make([]string, len(bundleCommits))
	for i, c := range bundleCommits {
		commitHashes[i] = c.Hash
	}
	turns, turnsErr := a.turnLoader.LoadTurnsForCommits(ctx, commitHashes)
	if turnsErr != nil {
		// Missing captures return an empty result; loader failures stop analysis.
		return Bundle{}, turnsErr
	}
	turnsDropped := 0
	if len(turns) > MaxBundleTurns {
		// Drop oldest first so the most recent intent context survives.
		turnsDropped = len(turns) - MaxBundleTurns
		turns = turns[turnsDropped:]
	}

	return Bundle{
		RepoRoot: in.RepoRoot,
		BaseRef:  baseRef,
		BaseSHA:  mergeBase,
		HeadSHA:  in.HeadSHA,
		Commits:  bundleCommits,
		Diff:     diff,
		Turns:    turns,
		Truncated: BundleTruncation{
			DiffBytesDropped: diffBytesDropped,
			CommitsDropped:   dropped,
			TurnsDropped:     turnsDropped,
		},
	}, nil
}
