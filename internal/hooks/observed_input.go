package hooks

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/observedinput"
	"github.com/semanticash/cli/internal/provenance"
)

// ErrRetentionIncomplete requires callers to preserve the turn and offset for retry.
var ErrRetentionIncomplete = errors.New("observed-input retention incomplete")

// providerSessionID returns the provider's session identity from the batch's events.
func providerSessionID(events []broker.RawEvent, _ *CaptureState) string {
	for _, e := range events {
		if e.ProviderSessionID != "" {
			return e.ProviderSessionID
		}
	}
	return ""
}

// ObservedInputBatch holds normalized evidence from one transcript batch.
type ObservedInputBatch struct {
	Turns      []observedinput.Evidence
	Contents   map[string][]byte
	CallOwners map[string]string
	Ancestry   map[string]string
}

func (b ObservedInputBatch) empty() bool {
	return len(b.Turns) == 0 && len(b.Ancestry) == 0 && len(b.CallOwners) == 0
}

// ObservedInputCapturer normalizes transcript records in [startOffset, endOffset).
type ObservedInputCapturer interface {
	CaptureObservedInputs(ctx context.Context, transcriptRef string, startOffset, endOffset int) (ObservedInputBatch, error)
}

// providerSessionResolver identifies sessions for batches without events.
type providerSessionResolver interface {
	ProviderSessionForTranscript(transcriptRef string) string
}

// resolveProviderSession prefers the identity carried on the batch's events and
// falls back to deriving it from the transcript reference.
func resolveProviderSession(provider HookProvider, events []broker.RawEvent, transcriptRef string) string {
	if id := providerSessionID(events, nil); id != "" {
		return id
	}
	if r, ok := provider.(providerSessionResolver); ok {
		return r.ProviderSessionForTranscript(transcriptRef)
	}
	return ""
}

// retainSubagentObserved stores input evidence in the launch repository.
// It reports whether the caller must preserve the offset for retry.
func retainSubagentObserved(ctx context.Context, provider HookProvider, bh *broker.Handle, transcriptRef, cwd string, events []broker.RawEvent, startOffset, endOffset int) (retry bool) {
	launchRepo := ""
	if target, terr := resolveToolWindowTarget(ctx, bh, cwd); terr == nil && target != nil {
		launchRepo = target.repoPath
	}
	return retainObservedInputs(ctx, provider, transcriptRef, launchRepo, resolveProviderSession(provider, events, transcriptRef), startOffset, endOffset)
}

// retainObservedInputs stores normalized evidence, ancestry, and call mappings.
// It reports whether the caller must preserve the offset for retry.
func retainObservedInputs(ctx context.Context, provider HookProvider, transcriptRef, targetRepo, providerSessionID string, startOffset, endOffset int) (retry bool) {
	capturer, ok := provider.(ObservedInputCapturer)
	if !ok {
		return false // provider does not produce observed-input evidence
	}
	batch, err := capturer.CaptureObservedInputs(ctx, transcriptRef, startOffset, endOffset)
	if err != nil {
		slog.Warn("observed-input capture failed", "transcript", transcriptRef, "err", err)
		return true
	}
	if batch.empty() {
		return false
	}
	if targetRepo == "" {
		// Preserve the offset until the evidence has a known destination.
		slog.Warn("observed-input owning repository unresolved; retaining for retry", "transcript", transcriptRef)
		return true
	}
	err = provenance.PersistObservedInputs(ctx, targetRepo, provider.Name(), providerSessionID, time.Now().UnixMilli(),
		batch.Turns, batch.Contents, batch.CallOwners, batch.Ancestry)
	if err != nil {
		if !errors.Is(err, provenance.ErrObservedInputRetry) {
			slog.Warn("observed-input persist failed", "repo", targetRepo, "err", err)
		}
		return true // incomplete retention: do not advance the offset
	}
	return false
}
