package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	"github.com/semanticash/cli/internal/util"
)

type RewindInput struct {
	RepoPath      string
	CheckpointID  string
	NoSafety      bool
	Exact         bool // delete files not present in checkpoint set
	SafetyMessage string
}

type RewindResult struct {
	RepoRoot           string `json:"repo_root"`
	CheckpointID       string `json:"checkpoint_id"`
	SafetyCheckpointID string `json:"safety_checkpoint_id,omitempty"`
	FilesRestored      int    `json:"files_restored"`
	FilesDeleted       int    `json:"files_deleted"`
}

type RewindService struct{}

func NewRewindService() *RewindService { return &RewindService{} }

func (s *RewindService) Rewind(ctx context.Context, in RewindInput) (*RewindResult, error) {
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

	// Open DB
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

	// Load checkpoint row
	cp, err := h.Queries.GetCheckpointByID(ctx, in.CheckpointID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("checkpoint not found: %s", in.CheckpointID)
		}
		return nil, fmt.Errorf("get checkpoint: %w", err)
	}

	// Safety checkpoint (default)
	var safetyCheckpointID string
	if !in.NoSafety {
		msg := in.SafetyMessage
		if msg == "" {
			msg = fmt.Sprintf("Safety checkpoint before rewind to %s", in.CheckpointID)
		}

		cps := NewCheckpointService()
		safetyRes, err := cps.Create(ctx, CreateCheckpointInput{
			RepoPath: repoRoot,
			Kind:     CheckpointAuto,
			Trigger:  "rewind_safety",
			Message:  msg,
		})
		if err != nil {
			return nil, fmt.Errorf("create safety checkpoint: %w", err)
		}
		safetyCheckpointID = safetyRes.CheckpointID
	}

	// Read manifest blob
	blobStore, err := blobs.NewStore(objectsDir)
	if err != nil {
		return nil, fmt.Errorf("init blob store: %w", err)
	}

	if !cp.ManifestHash.Valid {
		return nil, fmt.Errorf("checkpoint %s has no manifest", in.CheckpointID)
	}
	manifestBytes, err := blobStore.Get(ctx, cp.ManifestHash.String)
	if err != nil {
		return nil, fmt.Errorf("read manifest blob (%s): %w", cp.ManifestHash.String, err)
	}

	var manifest blobs.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}

	// Restore files (overwrite)
	want := make(map[string]struct{}, len(manifest.Files))
	restored := 0

	for _, f := range manifest.Files {
		want[f.Path] = struct{}{}

		var content []byte
		if !f.IsSymlink {
			var err error
			content, err = blobStore.Get(ctx, f.Blob)
			if err != nil {
				return nil, fmt.Errorf("read file blob for %s (%s): %w", f.Path, f.Blob, err)
			}
		}
		if err := repo.RestoreFile(f.Path, content, f.Mode, f.IsSymlink, f.LinkTarget); err != nil {
			return nil, err
		}
		restored++
	}

	// Optional: delete files not in checkpoint set (Exact mode)
	deleted := 0
	if in.Exact {
		current, err := repo.ListFilesFromGit(ctx)
		if err != nil {
			return nil, fmt.Errorf("list current files: %w", err)
		}
		for _, p := range current {
			if _, ok := want[p]; ok {
				continue
			}
			// Don't ever delete Semantica internals even if listed somehow.
			if p == ".semantica" || strings.HasPrefix(p, ".semantica"+string(filepath.Separator)) {
				continue
			}
			if err := repo.RemoveFile(p); err != nil {
				return nil, err
			}
			deleted++
		}
	}

	return &RewindResult{
		RepoRoot:           repoRoot,
		CheckpointID:       in.CheckpointID,
		SafetyCheckpointID: safetyCheckpointID,
		FilesRestored:      restored,
		FilesDeleted:       deleted,
	}, nil
}
