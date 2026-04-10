package implementations

import (
	"context"
	"fmt"
	"time"

	"github.com/semanticash/cli/internal/store/impldb"
	impldbgen "github.com/semanticash/cli/internal/store/impldb/db"
)

// ShouldAutoSummarizeOpts controls which checks ShouldAutoSummarize runs.
type ShouldAutoSummarizeOpts struct {
	// SkipInProgressCheck disables the duplicate-work guard. Set to true
	// when called from inside the background job itself, which already
	// owns the in-progress marker.
	SkipInProgressCheck bool
}

// ShouldAutoSummarize checks whether a background implementation summary
// should be generated for the given implementation. Returns (true, "") if
// generation should proceed, or (false, reason) if it should be skipped.
func ShouldAutoSummarize(ctx context.Context, h *impldb.Handle, implID string, opts ShouldAutoSummarizeOpts) (bool, string) {
	impl, err := h.Queries.GetImplementation(ctx, implID)
	if err != nil {
		return false, "implementation not found"
	}

	// Count repos.
	repoCount, err := h.Queries.CountReposForImplementation(ctx, implID)
	if err != nil {
		return false, fmt.Sprintf("count repos: %v", err)
	}
	if repoCount < 2 {
		return false, fmt.Sprintf("single-repo implementation (%d repos)", repoCount)
	}

	// Check manual override.
	meta := ReadImplementationMeta(impl.MetadataJson)
	if meta.IsManuallyEdited() {
		return false, "title or summary was manually set"
	}

	// Check in-progress guard (skipped when called from the background job itself).
	if !opts.SkipInProgressCheck && meta.IsGenerationInProgress() {
		return false, "generation already in progress"
	}

	// Check scope change.
	if meta.GeneratedRepoCount >= int(repoCount) {
		return false, fmt.Sprintf("scope unchanged (still %d repos)", repoCount)
	}

	return true, ""
}

// MarkGenerationInProgress sets the generation_in_progress_at marker in
// metadata_json. Called by the worker before spawning the background command.
func MarkGenerationInProgress(ctx context.Context, h *impldb.Handle, implID string) error {
	impl, err := h.Queries.GetImplementation(ctx, implID)
	if err != nil {
		return err
	}

	meta := ReadImplementationMeta(impl.MetadataJson)
	meta.GenerationInProgressAt = time.Now().UnixMilli()

	encoded, err := WriteImplementationMeta(meta)
	if err != nil {
		return err
	}

	return h.Queries.UpdateImplementationMetadata(ctx, impldbgen.UpdateImplementationMetadataParams{
		MetadataJson:     encoded,
		ImplementationID: implID,
	})
}

// ClearGenerationInProgress clears the in-progress marker. Called by the
// background command on completion (in a defer).
func ClearGenerationInProgress(ctx context.Context, h *impldb.Handle, implID string) {
	impl, err := h.Queries.GetImplementation(ctx, implID)
	if err != nil {
		return
	}

	meta := ReadImplementationMeta(impl.MetadataJson)
	meta.GenerationInProgressAt = 0

	encoded, err := WriteImplementationMeta(meta)
	if err != nil {
		return
	}

	_ = h.Queries.UpdateImplementationMetadata(ctx, impldbgen.UpdateImplementationMetadataParams{
		MetadataJson:     encoded,
		ImplementationID: implID,
	})
}
