package toolsnap

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/platform"
)

// TurnBaseline identifies the worktree and starting snapshot across hook processes.
type TurnBaseline struct {
	Repository  RepoContext `json:"repository"`
	Directories []string    `json:"directories"`
	Snapshot    Snapshot    `json:"snapshot"`
	StartedAt   time.Time   `json:"started_at"`
	FinishedAt  time.Time   `json:"finished_at"`
}

// TurnChange compares a committed boundary or the final worktree with its baseline.
type TurnChange struct {
	Commit     string      `json:"commit,omitempty"`
	BeforeTree string      `json:"before_tree"`
	Tree       string      `json:"tree"`
	Files      []FileDelta `json:"files"`
}

// TurnObservation records repository changes, not authorship or execution completion.
type TurnObservation struct {
	State      string       `json:"state"`
	Reason     string       `json:"reason,omitempty"`
	Head       string       `json:"head,omitempty"`
	Changes    []TurnChange `json:"changes,omitempty"`
	StartedAt  time.Time    `json:"started_at"`
	FinishedAt time.Time    `json:"finished_at"`
}

// ValidateTurnSubject checks worktree identity without taking another snapshot.
func ValidateTurnSubject(ctx context.Context, baseline TurnBaseline) error {
	_, err := baseline.check(ctx)
	return err
}

// FreezeTurnSubject resolves identity without capturing worktree content.
func FreezeTurnSubject(ctx context.Context, path string) (TurnBaseline, error) {
	b := TurnBaseline{StartedAt: time.Now().UTC()}
	rc, err := ResolveRepoContext(ctx, path)
	if err != nil {
		return b, err
	}
	b.Repository = rc
	for _, p := range []string{rc.WorktreeRoot, rc.GitDir, rc.CommonDir} {
		id, err := platform.FileIdentity(p)
		if err != nil {
			return b, err
		}
		b.Directories = append(b.Directories, id)
	}
	return b, nil
}

func (b TurnBaseline) check(ctx context.Context) (RepoContext, error) {
	now, err := FreezeTurnSubject(ctx, b.Repository.WorktreeRoot)
	if err != nil {
		return RepoContext{}, err
	}
	a, n := b.Repository, now.Repository
	if a.WorktreeRoot != n.WorktreeRoot || a.GitDir != n.GitDir || a.CommonDir != n.CommonDir || a.ObjectFormat != n.ObjectFormat || strings.Join(b.Directories, ",") != strings.Join(now.Directories, ",") {
		return RepoContext{}, fmt.Errorf("worktree_identity_changed")
	}
	return n, nil
}

// CaptureTurnBaseline uses the same worktree representation as tool snapshots.
func CaptureTurnBaseline(ctx context.Context, storage string, b *TurnBaseline) error {
	if err := supportedTurnIndex(ctx, b.Repository); err != nil {
		return err
	}
	s, err := OpenStore(ctx, b.Repository, storage)
	if err != nil {
		return err
	}
	b.Snapshot, err = s.CaptureBefore(ctx)
	if err != nil {
		return err
	}
	if _, err := b.check(ctx); err != nil {
		return err
	}
	if err := s.EnsureRef(ctx, WorkspaceFreezeRef("turn-start"), b.Snapshot.TreeHash); err != nil {
		return err
	}
	b.FinishedAt = time.Now().UTC()
	return nil
}

// ObserveTurnEnd captures even when provider completion is unknown.
func ObserveTurnEnd(ctx context.Context, storage string, b TurnBaseline) (out TurnObservation) {
	out = TurnObservation{State: "unknown", StartedAt: time.Now().UTC()}
	defer func() { out.FinishedAt = time.Now().UTC() }()
	if err := observeTurnEnd(ctx, storage, b, &out); err != nil {
		out.State, out.Reason, out.Changes = "unknown", err.Error(), nil
	}
	return out
}

func observeTurnEnd(ctx context.Context, storage string, b TurnBaseline, out *TurnObservation) error {
	rc, err := b.check(ctx)
	if err != nil {
		return err
	}
	if err := supportedTurnIndex(ctx, rc); err != nil {
		return err
	}
	s, err := OpenStore(ctx, rc, storage)
	if err != nil {
		return err
	}
	anchor := rc.HeadAnchor()
	post, err := s.capture(ctx, &anchor)
	if err != nil {
		return err
	}
	boundaries, err := turnCommitBoundaries(ctx, b.Snapshot.HeadHash, rc)
	if err != nil {
		return err
	}
	boundaries = append(boundaries, TurnChange{Tree: post.TreeHash})
	previous := b.Repository.HeadTree
	if previous == "" && len(boundaries) > 1 {
		previous, err = s.emptyTree(ctx)
		if err != nil {
			return err
		}
	}
	changed := false
	for i := range boundaries {
		boundary := &boundaries[i]
		if boundary.Commit == "" {
			previous = b.Snapshot.TreeHash
		}
		boundary.BeforeTree = previous
		files, _, truncated, err := s.DeltaBetweenTrees(ctx, previous, boundary.Tree)
		if err != nil {
			return err
		}
		if truncated {
			return fmt.Errorf("delta_truncated")
		}
		if previous != boundary.Tree && len(files) == 0 {
			return fmt.Errorf("changed_tree_without_file_evidence")
		}
		boundary.Files = files
		changed = changed || len(files) > 0
		if err := s.EnsureRef(ctx, WorkspaceFreezeRef(fmt.Sprintf("turn-end-%d", i)), boundary.Tree); err != nil {
			return err
		}
		previous = boundary.Tree
	}
	now, err := b.check(ctx)
	if err != nil {
		return err
	}
	if now.HeadCommit != rc.HeadCommit {
		return fmt.Errorf("head_changed_during_observation")
	}
	out.State, out.Head, out.Changes = "unchanged", rc.HeadCommit, boundaries
	if changed {
		out.State = "changed"
	}
	return nil
}

// turnCommitBoundaries follows linear, reachable history without consulting reflogs.
func turnCommitBoundaries(ctx context.Context, before string, rc RepoContext) ([]TurnChange, error) {
	if before == rc.HeadCommit {
		return nil, nil
	}
	if rc.HeadCommit == "" {
		return nil, fmt.Errorf("commit_history_discontinuous")
	}
	rev := rc.HeadCommit
	if before != "" {
		rev = before + ".." + rev
	}
	out, err := gitOutput(ctx, rc.WorktreeRoot, "log", "--no-show-signature", "--no-decorate", "--first-parent", "--reverse", "--format=%H %T %P", rev, "--")
	if err != nil {
		return nil, err
	}
	previous := before
	var result []TurnChange
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 && (previous != "" || len(f) != 2) {
			return nil, fmt.Errorf("commit_history_discontinuous")
		}
		if len(f) == 3 && f[2] != previous {
			return nil, fmt.Errorf("commit_history_discontinuous")
		}
		result = append(result, TurnChange{Commit: f[0], Tree: f[1]})
		previous = f[0]
	}
	if previous != rc.HeadCommit {
		return nil, fmt.Errorf("commit_history_discontinuous")
	}
	return result, nil
}

func supportedTurnIndex(ctx context.Context, rc RepoContext) error {
	out, err := gitOutput(ctx, rc.WorktreeRoot, "ls-files", "-v", "--stage", "-z")
	if err != nil {
		return err
	}
	for _, entry := range strings.Split(out, "\x00") {
		if entry == "" {
			continue
		}
		meta, _, ok := strings.Cut(entry, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 4 || f[0] != "H" || f[3] != "0" || f[1] == gitlinkMode {
			return fmt.Errorf("unsupported_index_state")
		}
	}
	return nil
}
