package service

import (
	"context"

	"github.com/semanticash/cli/internal/auth"
	"github.com/semanticash/cli/internal/provenance"
	"github.com/semanticash/cli/internal/util"
)

// syncProvenanceResult summarizes one syncProvenance call without
// requiring callers to parse worker logs.
//
// Fields mirror the UploadResult batch reported by syncProvenance:
//
//   - Processed = len(reported results); total turns the upload pipeline
//     attempted in the final reportable batch. If a 401 retry succeeds,
//     this counts the retry batch. If refresh or retry fails, this counts
//     the initial batch.
//   - Uploaded = sum of r.Uploaded == true in the reported batch.
//   - Failed = sum of r.Err != nil in the reported batch. Missing-blob
//     skips (UploadResult{Action: ActionFail, Err: "skipped: missing
//     local blobs"}) count as failures.
//   - AuthFailed = any r.Err in the initial batch was IsUnauthorized.
//     This remains true even when refresh and retry later succeed.
type syncProvenanceResult struct {
	Processed  int
	Uploaded   int
	Failed     int
	AuthFailed bool
}

// syncProvenanceFn lets tests assert the drain watermark without
// constructing a full workerContext.
var syncProvenanceFn = syncProvenance

// drainAllPackagedProvenance is the post-completion provenance drain.
// It uses watermark=0 so manifests packaged after the checkpoint
// timestamp are included. It skips unconnected repos.
func drainAllPackagedProvenance(ctx context.Context, semDir, repoRoot string) syncProvenanceResult {
	if !util.IsConnected(semDir) {
		return syncProvenanceResult{}
	}
	return syncProvenanceFn(ctx, repoRoot, 0)
}

// syncProvenance prepares and uploads packaged provenance manifests.
// Pass watermarkTs=0 to drain all packaged manifests.
func syncProvenance(ctx context.Context, repoRoot string, watermarkTs int64) syncProvenanceResult {
	var out syncProvenanceResult

	endpoint := auth.EffectiveEndpoint()
	token, tokenErr := auth.AccessToken(ctx)
	if tokenErr != nil {
		wlog("worker: sync-provenance: auth failed: %v\n", tokenErr)
		return out
	}

	results, err := provenance.SyncAndUpload(ctx, repoRoot, endpoint, token, watermarkTs, 50, nil)
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
	if out.AuthFailed && token != "" && !auth.IsAPIKeyAuth() {
		refreshed, refreshErr := auth.ForceRefresh(ctx)
		if refreshErr != nil {
			wlog("worker: sync-provenance: refresh after 401 failed: %v\n", refreshErr)
		} else {
			retryResults, retryErr := provenance.SyncAndUpload(ctx, repoRoot, endpoint, refreshed, watermarkTs, 50, nil)
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
