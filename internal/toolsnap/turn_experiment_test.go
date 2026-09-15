//go:build turnexperiment

package toolsnap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Observations use temporary stores without publishing evidence or attribution.
type turnSubject struct {
	RepositoryID string `json:"repository_id"`
	Path         string `json:"path"`
}

type turnSample struct {
	turnSubject
	State                string         `json:"state"`
	Reason               string         `json:"reason,omitempty"`
	Files                []string       `json:"files,omitempty"`
	Boundaries           []turnBoundary `json:"boundaries,omitempty"`
	StartTime            time.Duration  `json:"start_ns"`
	EndTime              time.Duration  `json:"end_ns"`
	StartSnapshotTime    time.Duration  `json:"start_snapshot_ns"`
	EndSnapshotDeltaTime time.Duration  `json:"end_snapshot_delta_ns"`
	before               Snapshot
	identity             RepoContext
	directories          []os.FileInfo
	storeDir             string
}

// Commit boundaries compare committed trees. The final worktree boundary
// compares with the original worktree baseline, including its existing dirt.
type turnBoundary struct {
	Commit string   `json:"commit,omitempty"`
	Tree   string   `json:"tree"`
	Files  []string `json:"files,omitempty"`
}

type observedTurn struct {
	SessionID   string
	TurnID      string
	Samples     []turnSample
	StartTime   time.Duration
	EndTime     time.Duration
	Concurrency int
}

// beginObservedTurn returns only after every baseline succeeded or failed.
// The caller may start its controlled workload after this function returns.
func beginObservedTurn(ctx context.Context, sessionID, turnID, storage string, subjects []turnSubject, concurrency int) (*observedTurn, error) {
	if sessionID == "" || turnID == "" {
		return nil, fmt.Errorf("session and turn identities are required")
	}
	if concurrency < 1 {
		return nil, fmt.Errorf("concurrency must be positive")
	}
	seen := make(map[string]bool)
	for _, s := range subjects {
		if s.RepositoryID == "" || !filepath.IsAbs(s.Path) || seen[s.Path] {
			return nil, fmt.Errorf("invalid or duplicate subject: %q", s.Path)
		}
		seen[s.Path] = true
	}
	t := &observedTurn{SessionID: sessionID, TurnID: turnID, Concurrency: concurrency}
	started := time.Now()
	for i, subject := range subjects {
		t.Samples = append(t.Samples, turnSample{turnSubject: subject, State: "unknown", storeDir: filepath.Join(storage, fmt.Sprint(i))})
	}
	// Freeze every worktree identity before starting any snapshot.
	observeTurnRepos(len(t.Samples), concurrency, func(i int) {
		s := &t.Samples[i]
		at := time.Now()
		if err := s.freezeIdentity(ctx); err != nil {
			s.Reason = err.Error()
		}
		s.StartTime = time.Since(at)
	})
	observeTurnRepos(len(t.Samples), concurrency, func(i int) {
		s := &t.Samples[i]
		at := time.Now()
		if s.Reason == "" {
			if err := s.captureBaseline(ctx); err != nil {
				s.Reason = err.Error()
			}
		}
		s.StartTime += time.Since(at)
	})
	t.StartTime = time.Since(started)
	return t, nil
}

// observeTurnRepos gives each index one owner and waits for all owners.
func observeTurnRepos(count, concurrency int, observe func(int)) {
	var wg sync.WaitGroup
	workers := min(count, concurrency)
	for worker := 0; worker < workers; worker++ {
		wg.Go(func() {
			for i := worker; i < count; i += workers {
				observe(i)
			}
		})
	}
	wg.Wait()
}

func (s *turnSample) freezeIdentity(ctx context.Context) error {
	rc, err := ResolveRepoContext(ctx, s.Path)
	if err != nil {
		return err
	}
	s.identity = rc
	for _, p := range []string{rc.WorktreeRoot, rc.GitDir, rc.CommonDir} {
		info, err := os.Stat(p)
		if err != nil {
			return err
		}
		s.directories = append(s.directories, info)
	}
	return nil
}

func (s *turnSample) captureBaseline(ctx context.Context) error {
	if err := turnSupportedIndex(ctx, s.identity); err != nil {
		return err
	}
	store, err := OpenStore(ctx, s.identity, s.storeDir)
	if err != nil {
		return err
	}
	started := time.Now()
	s.before, err = store.CaptureBefore(ctx)
	s.StartSnapshotTime = time.Since(started)
	if err != nil {
		return err
	}
	return s.checkIdentity(ctx)
}

// finishObservedTurn requires a matching terminal event and externally known
// quiescence. A turn-end notification alone cannot prove background work ended.
func finishObservedTurn(ctx context.Context, t *observedTurn, sessionID, turnID string, quiescent bool) {
	started := time.Now()
	observeTurnRepos(len(t.Samples), t.Concurrency, func(i int) {
		s := &t.Samples[i]
		at := time.Now()
		switch {
		case s.Reason != "":
		case sessionID != t.SessionID || turnID != t.TurnID:
			s.Reason = "completion_missing_or_mismatched"
		case !quiescent:
			s.Reason = "background_or_unknown_completion"
		default:
			if err := s.captureEnd(ctx); err != nil {
				s.State, s.Reason, s.Files, s.Boundaries = "unknown", err.Error(), nil, nil
			}
		}
		s.EndTime = time.Since(at)
	})
	t.EndTime = time.Since(started)
}

func (s *turnSample) checkIdentity(ctx context.Context) error {
	now, err := ResolveRepoContext(ctx, s.Path)
	if err != nil {
		return err
	}
	old := s.identity
	if now.WorktreeRoot != old.WorktreeRoot || now.GitDir != old.GitDir || now.CommonDir != old.CommonDir || now.ObjectFormat != old.ObjectFormat {
		return fmt.Errorf("worktree_identity_changed")
	}
	for i, p := range []string{now.WorktreeRoot, now.GitDir, now.CommonDir} {
		info, err := os.Stat(p)
		if err != nil {
			return err
		}
		if !os.SameFile(s.directories[i], info) {
			return fmt.Errorf("worktree_identity_changed")
		}
	}
	return nil
}

func (s *turnSample) captureEnd(ctx context.Context) error {
	if err := s.checkIdentity(ctx); err != nil {
		return err
	}
	rc, err := ResolveRepoContext(ctx, s.Path)
	if err != nil {
		return err
	}
	if err := turnSupportedIndex(ctx, rc); err != nil {
		return err
	}
	store, err := OpenStore(ctx, rc, s.storeDir)
	if err != nil {
		return err
	}
	started := time.Now()
	defer func() { s.EndSnapshotDeltaTime = time.Since(started) }()
	anchor := rc.HeadAnchor()
	post, err := store.capture(ctx, &anchor)
	if err != nil {
		return err
	}
	boundaries, err := s.commitBoundaries(ctx, rc)
	if err != nil {
		return err
	}
	boundaries = append(boundaries, turnBoundary{Tree: post.TreeHash})
	previous := s.identity.HeadTree
	if previous == "" && len(boundaries) > 1 {
		previous, err = store.emptyTree(ctx)
		if err != nil {
			return err
		}
	}
	files := make(map[string]bool)
	for i := range boundaries {
		boundary := &boundaries[i]
		if boundary.Commit == "" {
			previous = s.before.TreeHash
		}
		delta, _, truncated, err := store.DeltaBetweenTrees(ctx, previous, boundary.Tree)
		if err != nil {
			return err
		}
		if truncated {
			return fmt.Errorf("delta_truncated")
		}
		if previous != boundary.Tree && len(delta) == 0 {
			return fmt.Errorf("changed_tree_without_file_evidence")
		}
		for _, f := range delta {
			boundary.Files = append(boundary.Files, f.Path)
			files[f.Path] = true
		}
		previous = boundary.Tree
	}
	if err := s.checkIdentity(ctx); err != nil {
		return err
	}
	s.Boundaries = boundaries
	s.State = "unchanged"
	if len(files) > 0 {
		s.State = "changed"
		for path := range files {
			s.Files = append(s.Files, path)
		}
		sort.Strings(s.Files)
	}
	return nil
}

// commitBoundaries accepts only a linear continuation of the captured HEAD.
// Rewrites, merges, and commits no longer reachable at turn end remain unknown
// when HEAD movement exposes the discontinuity; this does not inspect reflogs.
func (s *turnSample) commitBoundaries(ctx context.Context, rc RepoContext) ([]turnBoundary, error) {
	if rc.HeadCommit == s.before.HeadHash {
		return nil, nil
	}
	if rc.HeadCommit == "" {
		return nil, fmt.Errorf("commit_history_discontinuous")
	}
	revision := rc.HeadCommit
	if s.before.HeadHash != "" {
		revision = s.before.HeadHash + ".." + rc.HeadCommit
	}
	out, err := gitOutput(ctx, rc.WorktreeRoot, "log", "--no-show-signature", "--no-decorate", "--first-parent", "--reverse", "--format=%H %T %P", revision, "--")
	if err != nil {
		return nil, err
	}
	previous := s.before.HeadHash
	var boundaries []turnBoundary
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 && !(previous == "" && len(fields) == 2) {
			return nil, fmt.Errorf("commit_history_discontinuous")
		}
		if len(fields) == 3 && fields[2] != previous {
			return nil, fmt.Errorf("commit_history_discontinuous")
		}
		boundaries = append(boundaries, turnBoundary{Commit: fields[0], Tree: fields[1]})
		previous = fields[0]
	}
	if previous != rc.HeadCommit {
		return nil, fmt.Errorf("commit_history_discontinuous")
	}
	return boundaries, nil
}

// Git status can omit flagged paths and submodule contents. These states are
// outside this experiment's supported regular-file/symlink worktree domain.
func turnSupportedIndex(ctx context.Context, rc RepoContext) error {
	out, err := gitOutput(ctx, rc.WorktreeRoot, "ls-files", "-v", "--stage", "-z")
	if err != nil {
		return err
	}
	for _, entry := range strings.Split(out, "\x00") {
		if entry == "" {
			continue
		}
		meta, _, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 4 || fields[0] != "H" || fields[3] != "0" || fields[1] == gitlinkMode {
			return fmt.Errorf("unsupported_index_state")
		}
	}
	return nil
}
