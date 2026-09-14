package service

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
)

// RunStandalone honors short capture retries without an external launcher.
// Each Run releases the repository lock and checkpoint lease before waiting.
func (s *WorkerService) RunStandalone(ctx context.Context, in WorkerInput) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := s.Run(ctx, in)
		var scheduled *ErrRetryScheduled
		if !errors.As(err, &scheduled) {
			return err
		}
		if !isCaptureRetry(ctx, in.RepoRoot, scheduled) {
			return err
		}
		delay := time.Until(scheduled.At)
		if delay > captureRetryDelay {
			return err
		}
		timer := time.NewTimer(max(delay, 0))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func isCaptureRetry(ctx context.Context, repoRoot string, scheduled *ErrRetryScheduled) bool {
	h, err := sqlstore.Open(ctx, filepath.Join(repoRoot, ".semantica", "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		return false
	}
	defer func() { _ = sqlstore.Close(h) }()
	cp, err := h.Queries.GetCheckpointByID(ctx, scheduled.CheckpointID)
	if err != nil || cp.Status != "pending" || cp.LastError.String != "capture_not_settled" {
		return false
	}
	r, err := readCheckpointCapture(ctx, h, cp)
	return err == nil && r != nil && r.Result.Status == "pending" && scheduled.At.UnixMilli() <= r.Deadline
}
