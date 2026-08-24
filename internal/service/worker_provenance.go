package service

import (
	"context"
	"time"

	"github.com/semanticash/cli/internal/auth"
	"github.com/semanticash/cli/internal/provenance"
	"github.com/semanticash/cli/internal/util"
)

const (
	provenanceSyncBatchSize = 50
	provenanceSyncTurnLimit = 200
	provenanceSyncTimeLimit = 2 * time.Minute
)

// syncProvenanceResult summarizes a post-completion upload run.
type syncProvenanceResult struct {
	Processed  int
	Uploaded   int
	Failed     int
	AuthFailed bool
}

// syncProvenanceFn lets tests assert the drain watermark without
// constructing a full workerContext.
var syncProvenanceFn = syncProvenance

// drainPackagedProvenance uploads pending turns after checkpoint completion.
func drainPackagedProvenance(ctx context.Context, semDir, repoRoot string) syncProvenanceResult {
	if !util.IsConnected(semDir) {
		return syncProvenanceResult{}
	}
	return syncProvenanceFn(ctx, repoRoot, 0)
}

// syncProvenance uploads pending turns in bounded batches.
// A zero watermark includes manifests regardless of creation time.
func syncProvenance(ctx context.Context, repoRoot string, watermarkTs int64) syncProvenanceResult {
	endpoint := auth.EffectiveEndpoint()
	token, tokenErr := auth.AccessToken(ctx)
	if tokenErr != nil {
		wlog("worker: sync-provenance: auth failed: %v\n", tokenErr)
		return syncProvenanceResult{}
	}

	drainCtx, cancel := context.WithTimeout(ctx, provenanceSyncTimeLimit)
	defer cancel()

	return drainProvenanceBatches(drainCtx, provenanceSyncBatchSize, provenanceSyncTurnLimit,
		func(batchCtx context.Context, limit int) syncProvenanceResult {
			return syncProvenanceBatch(batchCtx, repoRoot, endpoint, &token, watermarkTs, limit)
		})
}

func syncProvenanceBatch(
	ctx context.Context,
	repoRoot, endpoint string,
	token *string,
	watermarkTs int64,
	limit int,
) syncProvenanceResult {
	var out syncProvenanceResult
	results, err := provenance.SyncAndUpload(ctx, repoRoot, endpoint, *token, watermarkTs, limit, nil)
	if err != nil {
		wlog("worker: sync-provenance: %v\n", err)
		return out
	}

	// Capture auth failures before refresh-and-retry can replace the
	// initial result batch.
	for _, r := range results {
		if r.Err != nil && provenance.IsUnauthorized(r.Err) {
			out.AuthFailed = true
			break
		}
	}

	// On 401, refresh the token and retry the full batch once. If refresh
	// or retry fails, report the initial batch instead of dropping it.
	if out.AuthFailed && *token != "" && !auth.IsAPIKeyAuth() {
		refreshed, refreshErr := auth.ForceRefresh(ctx)
		if refreshErr != nil {
			wlog("worker: sync-provenance: refresh after 401 failed: %v\n", refreshErr)
		} else {
			*token = refreshed
			retryResults, retryErr := provenance.SyncAndUpload(ctx, repoRoot, endpoint, refreshed, watermarkTs, limit, nil)
			if retryErr != nil {
				wlog("worker: sync-provenance: retry after refresh: %v\n", retryErr)
			} else {
				results = retryResults
			}
		}
	}

	for _, r := range results {
		if r.Err != nil {
			wlog("worker: sync-provenance: turn %s upload failed: %v\n", util.ShortID(r.TurnID), r.Err)
			out.Failed++
		} else if r.Uploaded {
			wlog("worker: sync-provenance: turn %s uploaded\n", util.ShortID(r.TurnID))
			out.Uploaded++
		}
	}
	out.Processed = len(results)
	return out
}

func drainProvenanceBatches(
	ctx context.Context,
	batchSize, turnLimit int,
	upload func(context.Context, int) syncProvenanceResult,
) syncProvenanceResult {
	var total syncProvenanceResult
	for total.Processed < turnLimit && ctx.Err() == nil {
		limit := min(batchSize, turnLimit-total.Processed)
		batch := upload(ctx, limit)
		total.Processed += batch.Processed
		total.Uploaded += batch.Uploaded
		total.Failed += batch.Failed
		total.AuthFailed = total.AuthFailed || batch.AuthFailed

		// A failed turn may return to the packaged state. Stop here so it is
		// retried by a later sync instead of consuming retries immediately.
		if batch.Failed > 0 || batch.Processed < limit {
			break
		}
	}
	return total
}
