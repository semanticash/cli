package intentgap

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeRepo is the smallest GitRepo stand-in the assembler tests need.
// Per-method outputs are pinned per test.
type fakeRepo struct {
	defaultBaseRef string
	defaultBaseErr error
	mergeBaseFn    func(a, b string) (string, error)
	commitsFn      func(base, head string, limit int) ([]CommitMetaBetween, error)
	diffFn         func(base, head string) ([]byte, error)
	countFn        func(base, head string) (int, error)
}

func (f *fakeRepo) DefaultBaseRef(context.Context) (string, error) {
	return f.defaultBaseRef, f.defaultBaseErr
}
func (f *fakeRepo) MergeBase(_ context.Context, a, b string) (string, error) {
	return f.mergeBaseFn(a, b)
}
func (f *fakeRepo) CommitSummariesBetween(_ context.Context, base, head string, limit int) ([]CommitMetaBetween, error) {
	return f.commitsFn(base, head, limit)
}
func (f *fakeRepo) DiffBetween(_ context.Context, base, head string) ([]byte, error) {
	return f.diffFn(base, head)
}
func (f *fakeRepo) CountCommitsBetween(_ context.Context, base, head string) (int, error) {
	if f.countFn != nil {
		return f.countFn(base, head)
	}
	rows, err := f.commitsFn(base, head, 0)
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}

func openerFor(r *fakeRepo) GitRepoOpener {
	return func(string) (GitRepo, error) { return r, nil }
}

// Happy path: explicit base, all sizes under the limits.
func TestGitBundleAssembler_HappyPath(t *testing.T) {
	repo := &fakeRepo{
		mergeBaseFn: func(a, b string) (string, error) { return "merge-base-sha", nil },
		commitsFn: func(base, head string, limit int) ([]CommitMetaBetween, error) {
			return []CommitMetaBetween{
				{Hash: "c1", Subject: "first"},
				{Hash: "c2", Subject: "second"},
			}, nil
		},
		diffFn: func(base, head string) ([]byte, error) {
			return []byte("--- a\n+++ b\n"), nil
		},
	}
	a := NewGitBundleAssembler(openerFor(repo), nil, nil)

	b, err := a.Assemble(context.Background(), BundleInput{
		RepoRoot: "/tmp/r",
		Base:     "main",
		HeadSHA:  "head-sha",
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if b.BaseRef != "main" || b.BaseSHA != "merge-base-sha" || b.HeadSHA != "head-sha" {
		t.Errorf("ref/sha fields wrong: %+v", b)
	}
	if len(b.Commits) != 2 {
		t.Errorf("commits = %d, want 2", len(b.Commits))
	}
	if len(b.Diff) == 0 {
		t.Errorf("expected non-empty diff")
	}
	if b.Truncated.CommitsDropped != 0 || b.Truncated.DiffBytesDropped != 0 {
		t.Errorf("unexpected truncation: %+v", b.Truncated)
	}
}

// Empty Base triggers DefaultBaseRef resolution.
func TestGitBundleAssembler_AutoBaseRef(t *testing.T) {
	repo := &fakeRepo{
		defaultBaseRef: "origin/main",
		mergeBaseFn: func(a, b string) (string, error) {
			if a != "origin/main" {
				t.Errorf("MergeBase first arg = %q, want origin/main", a)
			}
			return "mb", nil
		},
		commitsFn: func(base, head string, limit int) ([]CommitMetaBetween, error) { return nil, nil },
		diffFn:    func(base, head string) ([]byte, error) { return nil, nil },
	}
	a := NewGitBundleAssembler(openerFor(repo), nil, nil)

	b, err := a.Assemble(context.Background(), BundleInput{
		RepoRoot: "/tmp/r",
		HeadSHA:  "head-sha",
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if b.BaseRef != "origin/main" {
		t.Errorf("BaseRef = %q, want origin/main", b.BaseRef)
	}
}

// DefaultBaseRef failure surfaces as a clear error before merge-base.
func TestGitBundleAssembler_DefaultBaseRefError(t *testing.T) {
	repo := &fakeRepo{
		defaultBaseErr: errors.New("no candidates"),
	}
	a := NewGitBundleAssembler(openerFor(repo), nil, nil)

	_, err := a.Assemble(context.Background(), BundleInput{
		RepoRoot: "/tmp/r",
		HeadSHA:  "head-sha",
	})
	if err == nil {
		t.Fatalf("expected error when DefaultBaseRef fails")
	}
	if !strings.Contains(err.Error(), "resolve default base") {
		t.Errorf("error should mention base resolution; got: %v", err)
	}
}

// Commit count over the cap is reported using the real total from
// CountCommitsBetween, not just (returned-len) which would
// underreport. Motivating case: a 500-commit PR with a 100-commit
// cap must report 400 dropped, not 1.
func TestGitBundleAssembler_CommitTruncationReportsRealTotal(t *testing.T) {
	capped := make([]CommitMetaBetween, MaxBundleCommits)
	for i := range capped {
		capped[i] = CommitMetaBetween{Hash: "c", Subject: "s"}
	}
	repo := &fakeRepo{
		mergeBaseFn: func(a, b string) (string, error) { return "mb", nil },
		commitsFn: func(base, head string, limit int) ([]CommitMetaBetween, error) {
			if limit != MaxBundleCommits {
				t.Errorf("commits limit = %d, want MaxBundleCommits", limit)
			}
			return capped, nil
		},
		diffFn:  func(base, head string) ([]byte, error) { return nil, nil },
		countFn: func(base, head string) (int, error) { return 500, nil },
	}
	a := NewGitBundleAssembler(openerFor(repo), nil, nil)

	b, err := a.Assemble(context.Background(), BundleInput{RepoRoot: "/tmp/r", Base: "main", HeadSHA: "h"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(b.Commits) != MaxBundleCommits {
		t.Errorf("commits = %d, want %d", len(b.Commits), MaxBundleCommits)
	}
	if got, want := b.Truncated.CommitsDropped, 500-MaxBundleCommits; got != want {
		t.Errorf("CommitsDropped = %d, want %d (real total minus cap)", got, want)
	}
}

// Under the cap: dropped count is zero.
func TestGitBundleAssembler_NoTruncationWhenUnderCap(t *testing.T) {
	repo := &fakeRepo{
		mergeBaseFn: func(a, b string) (string, error) { return "mb", nil },
		commitsFn: func(base, head string, limit int) ([]CommitMetaBetween, error) {
			return []CommitMetaBetween{{Hash: "c1"}, {Hash: "c2"}}, nil
		},
		diffFn:  func(base, head string) ([]byte, error) { return nil, nil },
		countFn: func(base, head string) (int, error) { return 2, nil },
	}
	a := NewGitBundleAssembler(openerFor(repo), nil, nil)
	b, err := a.Assemble(context.Background(), BundleInput{RepoRoot: "/tmp/r", Base: "main", HeadSHA: "h"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if b.Truncated.CommitsDropped != 0 {
		t.Errorf("CommitsDropped = %d, want 0", b.Truncated.CommitsDropped)
	}
}

// Diff size over the cap is recorded the same way. The analyzer can
// note "diff truncated; N bytes dropped" in its data_gaps rather than
// claiming full coverage.
func TestGitBundleAssembler_DiffTruncationRecorded(t *testing.T) {
	bigDiff := make([]byte, MaxBundleDiffBytes+1024)
	for i := range bigDiff {
		bigDiff[i] = 'x'
	}
	repo := &fakeRepo{
		mergeBaseFn: func(a, b string) (string, error) { return "mb", nil },
		commitsFn:   func(base, head string, limit int) ([]CommitMetaBetween, error) { return nil, nil },
		diffFn:      func(base, head string) ([]byte, error) { return bigDiff, nil },
	}
	a := NewGitBundleAssembler(openerFor(repo), nil, nil)

	b, err := a.Assemble(context.Background(), BundleInput{RepoRoot: "/tmp/r", Base: "main", HeadSHA: "h"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(b.Diff) != MaxBundleDiffBytes {
		t.Errorf("Diff len = %d, want %d", len(b.Diff), MaxBundleDiffBytes)
	}
	if b.Truncated.DiffBytesDropped != 1024 {
		t.Errorf("DiffBytesDropped = %d, want 1024", b.Truncated.DiffBytesDropped)
	}
}

// Required inputs are validated before git operations run.
func TestGitBundleAssembler_RequiresRepoRootAndHead(t *testing.T) {
	repo := &fakeRepo{}
	a := NewGitBundleAssembler(openerFor(repo), nil, nil)

	if _, err := a.Assemble(context.Background(), BundleInput{HeadSHA: "h"}); err == nil {
		t.Errorf("expected error when RepoRoot is missing")
	}
	if _, err := a.Assemble(context.Background(), BundleInput{RepoRoot: "/tmp/r"}); err == nil {
		t.Errorf("expected error when HeadSHA is missing")
	}
}

// No GitRepoOpener wired in is a programming error; reporting it as a
// clear error beats a nil-pointer panic at runtime.
func TestGitBundleAssembler_NilOpener(t *testing.T) {
	a := NewGitBundleAssembler(nil, nil, nil)
	_, err := a.Assemble(context.Background(), BundleInput{RepoRoot: "/tmp/r", HeadSHA: "h"})
	if err == nil {
		t.Fatalf("expected error from nil opener")
	}
	if !strings.Contains(err.Error(), "GitRepoOpener") {
		t.Errorf("error should mention the missing opener; got: %v", err)
	}
}

// stubActionLoader returns a canned list of actions. Used by the
// retention-cap and pass-through tests below.
type stubActionLoader struct {
	actions []BundleAgentAction
	err     error
}

func (s *stubActionLoader) LoadActionsForCommits(context.Context, []string) ([]BundleAgentAction, error) {
	return s.actions, s.err
}

// Actions surface on the bundle when the loader returns less than
// the cap, with no truncation reported.
func TestGitBundleAssembler_AgentActionsPassThrough(t *testing.T) {
	repo := &fakeRepo{
		mergeBaseFn: func(a, b string) (string, error) { return "mb", nil },
		commitsFn:   func(base, head string, limit int) ([]CommitMetaBetween, error) { return nil, nil },
		diffFn:      func(base, head string) ([]byte, error) { return nil, nil },
	}
	actions := []BundleAgentAction{
		{ActionID: "a1", TurnID: "t1", ToolName: "Edit", FilePath: "a.go"},
		{ActionID: "a2", TurnID: "t1", ToolName: "Edit", FilePath: "b.go"},
	}
	a := NewGitBundleAssembler(openerFor(repo), nil, &stubActionLoader{actions: actions})
	b, err := a.Assemble(context.Background(), BundleInput{RepoRoot: "/tmp/r", Base: "main", HeadSHA: "h"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(b.AgentActions) != 2 {
		t.Errorf("AgentActions len = %d, want 2", len(b.AgentActions))
	}
	if b.Truncated.AgentActionsDropped != 0 {
		t.Errorf("AgentActionsDropped = %d, want 0", b.Truncated.AgentActionsDropped)
	}
}

// When the loader returns more actions than the cap, the assembler
// drops the oldest entries (the prefix) so the most recent activity
// survives. The truncation count reflects exactly how many entries
// were dropped.
func TestGitBundleAssembler_AgentActionsCappedKeepsMostRecent(t *testing.T) {
	repo := &fakeRepo{
		mergeBaseFn: func(a, b string) (string, error) { return "mb", nil },
		commitsFn:   func(base, head string, limit int) ([]CommitMetaBetween, error) { return nil, nil },
		diffFn:      func(base, head string) ([]byte, error) { return nil, nil },
	}
	over := MaxBundleAgentActions + 3
	actions := make([]BundleAgentAction, over)
	for i := range actions {
		actions[i] = BundleAgentAction{
			ActionID: "a" + string(rune('A'+i%26)),
			TurnID:   "t1",
			ToolName: "Edit",
			TS:       int64(i),
		}
	}
	a := NewGitBundleAssembler(openerFor(repo), nil, &stubActionLoader{actions: actions})
	b, err := a.Assemble(context.Background(), BundleInput{RepoRoot: "/tmp/r", Base: "main", HeadSHA: "h"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(b.AgentActions) != MaxBundleAgentActions {
		t.Errorf("kept len = %d, want %d", len(b.AgentActions), MaxBundleAgentActions)
	}
	if b.Truncated.AgentActionsDropped != 3 {
		t.Errorf("AgentActionsDropped = %d, want 3", b.Truncated.AgentActionsDropped)
	}
	if b.AgentActions[0].TS != 3 {
		t.Errorf("oldest kept TS = %d, want 3 (dropped 3 oldest)", b.AgentActions[0].TS)
	}
}

// A loader failure stops bundle assembly rather than returning a
// bundle with incomplete action evidence.
func TestGitBundleAssembler_AgentActionLoaderErrorStopsAssembly(t *testing.T) {
	repo := &fakeRepo{
		mergeBaseFn: func(a, b string) (string, error) { return "mb", nil },
		commitsFn:   func(base, head string, limit int) ([]CommitMetaBetween, error) { return nil, nil },
		diffFn:      func(base, head string) ([]byte, error) { return nil, nil },
	}
	wantErr := errors.New("loader failed")
	a := NewGitBundleAssembler(openerFor(repo), nil, &stubActionLoader{err: wantErr})
	if _, err := a.Assemble(context.Background(), BundleInput{RepoRoot: "/tmp/r", Base: "main", HeadSHA: "h"}); !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
}
