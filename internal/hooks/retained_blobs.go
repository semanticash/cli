package hooks

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/semanticash/cli/internal/agents/api"
	"github.com/semanticash/cli/internal/broker"
)

// captureBlobErrors preserves failures that provider builders otherwise omit.
// A retained capture must not advance after silently losing a payload hash.
type captureBlobErrors struct {
	store api.BlobPutter
	mu    sync.Mutex
	err   error
}

func (b *captureBlobErrors) Put(ctx context.Context, data []byte) (string, int64, error) {
	var hash string
	var size int64
	var err error
	if b.store == nil {
		err = fmt.Errorf("capture blob store unavailable")
	} else {
		hash, size, err = b.store.Put(ctx, data)
	}
	if err != nil {
		b.mu.Lock()
		b.err = errors.Join(b.err, err)
		b.mu.Unlock()
	}
	return hash, size, err
}

func (b *captureBlobErrors) failure() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

func buildRetainableHookEvents(ctx context.Context, emitter DirectHookEmitter, event *Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	if !broker.DurableRoutingEnabled() {
		return emitter.BuildHookEvents(ctx, event, bs)
	}
	tracked := &captureBlobErrors{store: bs}
	events, err := emitter.BuildHookEvents(ctx, event, tracked)
	return events, errors.Join(err, tracked.failure())
}

func readRetainableEvents(ctx context.Context, provider HookProvider, ref string, offset int, bs api.BlobPutter) ([]broker.RawEvent, int, error) {
	if !broker.DurableRoutingEnabled() {
		return provider.ReadFromOffset(ctx, ref, offset, bs)
	}
	tracked := &captureBlobErrors{store: bs}
	events, next, err := provider.ReadFromOffset(ctx, ref, offset, tracked)
	return events, next, errors.Join(err, tracked.failure())
}
