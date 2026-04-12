package service

import (
	"context"
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
