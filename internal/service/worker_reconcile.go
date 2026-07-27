package service

import (
	"context"
	"errors"
	"io/fs"
	"time"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/platform"
	"github.com/semanticash/cli/internal/store/blobs"
)

// reconcileCapture is a seam for scoped-routing tests.
var reconcileCapture = hooks.CaptureAndRouteForRepo

// reconcileActiveSessions replays state owned by repoRoot.
// Other and unowned states remain pending for their proper capture path.
func reconcileActiveSessions(ctx context.Context, registry *hooks.Registry, repoRoot string) {
	all, err := hooks.LoadActiveCaptureStates()
	if err != nil || len(all) == 0 {
		return
	}

	// Leave capture state untouched when the directory is not writable.
	if err := hooks.CaptureDirWritable(); err != nil {
		if errors.Is(err, fs.ErrPermission) {
			wlog("worker: reconcile: capture directory not writable; deferring %d session(s); see `semantica doctor`\n", len(all))
			return
		}
		wlog("worker: reconcile: capture directory probe failed: %v; deferring %d session(s)\n", err, len(all))
		return
	}

	registryPath, err := broker.DefaultRegistryPath()
	if err != nil {
		return
	}
	bh, err := broker.Open(ctx, registryPath)
	if err != nil {
		return
	}
	defer func() { _ = broker.Close(bh) }()
	repos, err := broker.ListActiveRepos(ctx, bh)
	if err != nil {
		return
	}

	var states []*hooks.CaptureState
	var unowned, deferred int
	for _, state := range all {
		owner := deepestRepoOwner(state.CWD, repos)
		switch {
		case owner == nil:
			unowned++
		case !sameReconcilePath(owner.Path, repoRoot):
			deferred++
		default:
			states = append(states, state)
		}
	}
	if unowned > 0 {
		wlog("worker: reconcile: %d unowned capture state(s) (no working directory or no matching repository); see `semantica doctor`\n", unowned)
	}
	if deferred > 0 {
		wlog("worker: reconcile: deferring %d session(s) owned by other repositories\n", deferred)
	}
	if len(states) == 0 {
		return
	}

	var blobStore *blobs.Store
	if objDir, err := broker.GlobalObjectsDir(); err == nil {
		if bs, err := blobs.NewStore(objDir); err != nil {
			wlog("worker: reconcile: global blob store: %v (attribution will degrade)\n", err)
		} else {
			blobStore = bs
		}
	}

	for _, state := range states {
		provider := registry.Get(state.Provider)
		if provider == nil {
			continue
		}
		event := &hooks.Event{
			SessionID:     state.SessionID,
			TranscriptRef: state.TranscriptRef,
			Timestamp:     time.Now().UnixMilli(),
		}
		captured, err := reconcileCapture(ctx, provider, event, bh, blobStore, repoRoot)
		if err != nil {
			wlog("worker: reconcile %s/%s: %v\n", state.Provider, state.SessionID, err)
			continue
		}
		if !captured {
			wlog("worker: reconcile %s/%s: events route outside this repository; deferred to an unscoped capture\n",
				state.Provider, state.SessionID)
		}
	}
}

// deepestRepoOwner returns the deepest canonical repository containing cwd.
func deepestRepoOwner(cwd string, repos []broker.RegisteredRepo) *broker.RegisteredRepo {
	if cwd == "" {
		return nil
	}
	var best *broker.RegisteredRepo
	bestLen := -1
	for i := range repos {
		r := &repos[i]
		if !broker.PathBelongsToRepo(cwd, r.CanonicalPath) {
			continue
		}
		depth := len(platform.NormalizePathForCompare(r.CanonicalPath))
		if depth > bestLen {
			best, bestLen = r, depth
		}
	}
	return best
}

// sameReconcilePath reports path equality under broker normalization.
func sameReconcilePath(a, b string) bool {
	return broker.PathBelongsToRepo(a, b) && broker.PathBelongsToRepo(b, a)
}
