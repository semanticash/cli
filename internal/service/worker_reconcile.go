package service

import (
	"context"
	"errors"
	"io/fs"
	"time"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/store/blobs"
)

// reconcileActiveSessions flushes only sessions that still have capture state.
func reconcileActiveSessions(ctx context.Context) {
	states, err := hooks.LoadActiveCaptureStates()
	if err != nil || len(states) == 0 {
		return
	}

	// Skip the per-session loop if this process cannot write capture
	// state. The files stay on disk for the next unrestricted worker.
	if err := hooks.CaptureDirWritable(); err != nil {
		if errors.Is(err, fs.ErrPermission) {
			wlog("worker: reconcile: capture directory not writable from this lineage; deferring %d session(s) to next unrestricted worker run\n", len(states))
			return
		}
		wlog("worker: reconcile: capture directory probe failed: %v; deferring %d session(s)\n", err, len(states))
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

	var blobStore *blobs.Store
	if objDir, err := broker.GlobalObjectsDir(); err == nil {
		if bs, err := blobs.NewStore(objDir); err != nil {
			wlog("worker: reconcile: global blob store: %v (attribution will degrade)\n", err)
		} else {
			blobStore = bs
		}
	}

	for _, state := range states {
		provider := hooks.GetProvider(state.Provider)
		if provider == nil {
			continue
		}
		event := &hooks.Event{
			SessionID:     state.SessionID,
			TranscriptRef: state.TranscriptRef,
			Timestamp:     time.Now().UnixMilli(),
		}
		if err := hooks.CaptureAndRoute(ctx, provider, event, bh, blobStore); err != nil {
			wlog("worker: reconcile %s/%s: %v\n", state.Provider, state.SessionID, err)
		}
	}
}
