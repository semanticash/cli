package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	"github.com/semanticash/cli/internal/util"
)

type ShowCheckpointInput struct {
	RepoPath     string
	CheckpointID string
}

type ShowCheckpointFile struct {
	Path string `json:"path"`
	Blob string `json:"blob"`
	Size int64  `json:"size"`
}

type ShowCheckpointResult struct {
	RepoRoot      string               `json:"repo_root"`
	CheckpointID  string               `json:"checkpoint_id"`
	CommitHash    string               `json:"commit_hash,omitempty"`
	CreatedAtUnix int64                `json:"created_at_unix"`
	CreatedAt     string               `json:"created_at"` // RFC3339
	Kind          string               `json:"kind"`
	Trigger       string               `json:"trigger,omitempty"`
	Message       string               `json:"message,omitempty"`
	ManifestHash  string               `json:"manifest_hash"`
	Bytes         *int64               `json:"bytes,omitempty"`
	FileCount     int                  `json:"file_count"`
	Files         []ShowCheckpointFile `json:"files"`
}

type ShowService struct{}

func NewShowService() *ShowService { return &ShowService{} }

func (s *ShowService) ShowCheckpoint(ctx context.Context, in ShowCheckpointInput) (*ShowCheckpointResult, error) {
	if in.CheckpointID == "" {
		return nil, fmt.Errorf("checkpoint id is required")
	}

	repo, err := git.OpenRepo(in.RepoPath)
	if err != nil {
		return nil, err
	}
	repoRoot := repo.Root()

	semDir := filepath.Join(repoRoot, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")
	objectsDir := filepath.Join(semDir, "objects")

	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("semantica is not enabled in this repo. run `semantica enable` first")
	}
	if !util.IsEnabled(semDir) {
		return nil, fmt.Errorf("semantica is disabled. run `semantica enable` to re-enable")
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return nil, err
	}
	defer func() { _ = sqlstore.Close(h) }()

	// Resolve prefix / short ID to full checkpoint UUID.
	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, repoRoot)
	if err != nil {
		return nil, fmt.Errorf("repository not found for path %s", repoRoot)
	}
	resolvedID, err := sqlstore.ResolveCheckpointID(ctx, h.Queries, repoRow.RepositoryID, in.CheckpointID)
	if err != nil {
		return nil, err
	}
	in.CheckpointID = resolvedID

	cp, err := h.Queries.GetCheckpointByID(ctx, in.CheckpointID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("checkpoint not found: %s", in.CheckpointID)
		}
		return nil, err
	}

	// Parse nullable fields
	trigger := ""
	if cp.Trigger.Valid {
		trigger = cp.Trigger.String
	}
	message := ""
	if cp.Message.Valid {
		message = cp.Message.String
	}

	var bytesPtr *int64
	if cp.SizeBytes.Valid {
		v := cp.SizeBytes.Int64
		bytesPtr = &v
	}

	// Read manifest blob
	store, err := blobs.NewStore(objectsDir)
	if err != nil {
		return nil, fmt.Errorf("init blob store: %w", err)
	}
	if !cp.ManifestHash.Valid {
		return nil, fmt.Errorf("checkpoint %s has no manifest", in.CheckpointID)
	}
	manifestBytes, err := store.Get(ctx, cp.ManifestHash.String)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", cp.ManifestHash.String, err)
	}

	var manifest blobs.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}

	files := make([]ShowCheckpointFile, 0, len(manifest.Files))
	for _, f := range manifest.Files {
		files = append(files, ShowCheckpointFile{
			Path: f.Path,
			Blob: f.Blob,
			Size: f.Size,
		})
	}

	// Look up linked commit hash
	var commitHash string
	if links, linkErr := h.Queries.GetCommitLinksByCheckpoint(ctx, in.CheckpointID); linkErr == nil && len(links) > 0 {
		commitHash = links[0].CommitHash
	}

	createdAt := time.UnixMilli(cp.CreatedAt).Format(time.RFC3339)

	return &ShowCheckpointResult{
		CommitHash:    commitHash,
		RepoRoot:      repoRoot,
		CheckpointID:  cp.CheckpointID,
		CreatedAtUnix: cp.CreatedAt,
		CreatedAt:     createdAt,
		Kind:          cp.Kind,
		Trigger:       trigger,
		Message:       message,
		ManifestHash:  cp.ManifestHash.String,
		Bytes:         bytesPtr,
		FileCount:     len(files),
		Files:         files,
	}, nil
}
