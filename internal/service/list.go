package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/git"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"
)

type ListCheckpointsInput struct {
	RepoPath string
	Limit    int64
}

type ListedCheckpoint struct {
	ID           string
	CreatedAt    int64
	Kind         string
	Trigger      string
	Message      string
	ManifestHash string
	SizeBytes    *int64

	CommitHash    string
	CommitSubject string
}

type ListCheckpointsResult struct {
	RepoRoot string
	Items    []ListedCheckpoint
}

type ListService struct{}

func NewListService() *ListService { return &ListService{} }

func (s *ListService) ListCheckpoints(ctx context.Context, in ListCheckpointsInput) (*ListCheckpointsResult, error) {
	if in.Limit <= 0 {
		in.Limit = 20
	}

	repo, err := git.OpenRepo(in.RepoPath)
	if err != nil {
		return nil, err
	}
	repoRoot := repo.Root()

	semDir := filepath.Join(repoRoot, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("semantica is not enabled in this repo. run `semantica enable` first")
	}
	if !util.IsEnabled(semDir) {
		return nil, fmt.Errorf("semantica is disabled. run `semantica enable` to re-enable")
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.UserFacingOpenOptions())
	if err != nil {
		return nil, err
	}
	defer func() { _ = sqlstore.Close(h) }()

	// get repository_id by root path
	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, repoRoot)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &ListCheckpointsResult{RepoRoot: repoRoot, Items: nil}, nil
		}
		return nil, err
	}

	rows, err := h.Queries.ListCheckpointsWithCommit(ctx, sqldb.ListCheckpointsWithCommitParams{
		RepositoryID: repoRow.RepositoryID,
		Limit:        in.Limit,
	})
	if err != nil {
		return nil, err
	}

	// Resolve commit subjects in one git invocation. A per-row lookup
	// spawns one subprocess per record, which is noticeable on large local
	// histories. Best-effort: a batch failure omits subjects, matching the
	// previous behavior.
	uniqueHashes := make([]string, 0, len(rows))
	seenHashes := make(map[string]bool, len(rows))
	for _, r := range rows {
		if !r.CommitHash.Valid {
			continue
		}
		h := strings.TrimSpace(r.CommitHash.String)
		if h == "" || seenHashes[h] {
			continue
		}
		seenHashes[h] = true
		uniqueHashes = append(uniqueHashes, h)
	}
	subjects, err := repo.CommitSubjects(ctx, uniqueHashes)
	if err != nil {
		subjects = map[string]string{}
	}

	items := make([]ListedCheckpoint, 0, len(rows))
	for _, r := range rows {
		var trig, msg string
		if r.Trigger.Valid {
			trig = r.Trigger.String
		}
		if r.Message.Valid {
			msg = r.Message.String
		}

		// Suppress redundant auto commit message (it repeats created_at).
		if r.Kind == "auto" && (strings.TrimSpace(trig) == "commit" || strings.TrimSpace(trig) == "pre-commit") &&
			strings.HasPrefix(strings.TrimSpace(msg), "Auto checkpoint") {
			msg = ""
		}

		var sizePtr *int64
		if r.SizeBytes.Valid {
			v := r.SizeBytes.Int64
			sizePtr = &v
		}

		commitHash := ""
		if r.CommitHash.Valid {
			commitHash = strings.TrimSpace(r.CommitHash.String)
		}

		commitSubject := ""
		if commitHash != "" {
			commitSubject = subjects[commitHash]
		}

		items = append(items, ListedCheckpoint{
			ID:            r.CheckpointID,
			CreatedAt:     r.CreatedAt,
			Kind:          r.Kind,
			Trigger:       trig,
			Message:       msg,
			ManifestHash:  r.ManifestHash.String,
			SizeBytes:     sizePtr,
			CommitHash:    commitHash,
			CommitSubject: commitSubject,
		})
	}

	return &ListCheckpointsResult{
		RepoRoot: repoRoot,
		Items:    items,
	}, nil
}
