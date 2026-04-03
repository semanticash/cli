package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

func EnsureRepository(ctx context.Context, q *sqldb.Queries, rootPath string) (string, error) {
	// Try fetch
	repo, err := q.GetRepositoryByRootPath(ctx, rootPath)
	if err == nil {
		return repo.RepositoryID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	// Insert
	id := uuid.NewString()
	now := time.Now().UnixMilli()

	if err := q.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: id,
		RootPath:     rootPath,
		CreatedAt:    now,
		EnabledAt:    now,
	}); err != nil {
		// If two enable calls race, retry fetch:
		repo, err := q.GetRepositoryByRootPath(ctx, rootPath)
		if err == nil {
			return repo.RepositoryID, nil
		}
		return "", err
	}

	return id, nil
}
