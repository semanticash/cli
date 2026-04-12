package service

import (
	"context"

	"github.com/semanticash/cli/internal/auth"
	"github.com/semanticash/cli/internal/provenance"
	"github.com/semanticash/cli/internal/util"
)

// syncProvenance prepares and uploads packaged provenance manifests.
// Pass watermarkTs=0 to drain all packaged manifests.
func syncProvenance(ctx context.Context, repoRoot string, watermarkTs int64) {
	endpoint := auth.EffectiveEndpoint()
	token, tokenErr := auth.AccessToken(ctx)
	if tokenErr != nil {
		wlog("worker: sync-provenance: auth failed: %v\n", tokenErr)
		return
	}

	results, err := provenance.SyncAndUpload(ctx, repoRoot, endpoint, token, watermarkTs, 50, nil)
	if err != nil {
		wlog("worker: sync-provenance: %v\n", err)
		return
	}

	// On 401 for any result, refresh token and retry the full batch once.
	hasUnauth := false
	for _, r := range results {
		if r.Err != nil && provenance.IsUnauthorized(r.Err) {
			hasUnauth = true
			break
		}
	}
	if hasUnauth && token != "" && !auth.IsAPIKeyAuth() {
		refreshed, refreshErr := auth.ForceRefresh(ctx)
		if refreshErr != nil {
			wlog("worker: sync-provenance: refresh after 401 failed: %v\n", refreshErr)
			return
		}
		retryResults, retryErr := provenance.SyncAndUpload(ctx, repoRoot, endpoint, refreshed, watermarkTs, 50, nil)
		if retryErr != nil {
			wlog("worker: sync-provenance: retry after refresh: %v\n", retryErr)
			return
		}
		results = retryResults
	}

	for _, r := range results {
		if r.Err != nil {
			wlog("worker: sync-provenance: turn %s upload failed: %v\n", util.ShortID(r.TurnID), r.Err)
		} else if r.Uploaded {
			wlog("worker: sync-provenance: turn %s uploaded\n", util.ShortID(r.TurnID))
		}
	}
}
