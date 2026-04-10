package implementations

import (
	"context"
	"fmt"
	"time"

	"github.com/semanticash/cli/internal/store/impldb"
	impldbgen "github.com/semanticash/cli/internal/store/impldb/db"
)

// Close sets an implementation's state to 'closed'. Idempotent.
func Close(ctx context.Context, implID string) error {
	h, err := openGlobalDB(ctx)
	if err != nil {
		return fmt.Errorf("open implementations db: %w", err)
	}
	defer func() { _ = impldb.Close(h) }()

	fullID, err := resolveImplID(ctx, h, implID)
	if err != nil {
		return err
	}

	impl, err := h.Queries.GetImplementation(ctx, fullID)
	if err != nil {
		return fmt.Errorf("implementation %s not found", implID)
	}

	if impl.State == "closed" {
		return nil // idempotent
	}

	return h.Queries.UpdateImplementationState(ctx, impldbgen.UpdateImplementationStateParams{
		State:            "closed",
		ClosedAt:         impldb.NullInt64(time.Now().UnixMilli()),
		ImplementationID: fullID,
	})
}
