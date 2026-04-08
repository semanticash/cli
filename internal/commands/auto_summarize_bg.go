package commands

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/llm"
	"github.com/semanticash/cli/internal/service"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	"github.com/semanticash/cli/internal/util"
	"github.com/spf13/cobra"
)

// NewAutoPlaybookCmd creates the hidden _auto-playbook command.
// It is spawned as a detached background process by the worker when
// automations.playbook is enabled in settings.json.
func NewAutoPlaybookCmd() *cobra.Command {
	var (
		commitHash   string
		checkpointID string
		repoRoot     string
	)

	cmd := &cobra.Command{
		Use:    "_auto-playbook",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			// 1. Check if summary already exists (skip if so).
			if hasSummary(repoRoot, checkpointID) {
				fmt.Fprintf(os.Stderr, "auto-playbook: summary already exists for %s, skipping\n", checkpointID)
				return nil
			}

			// 2. Gather full explain context.
			svc := service.NewExplainService()
			res, err := svc.Explain(ctx, service.ExplainInput{
				RepoPath: repoRoot,
				Ref:      commitHash,
			})
			if err != nil {
				return fmt.Errorf("auto-playbook: explain: %w", err)
			}

			// 3. Get diff for prompt.
			repo, err := git.OpenRepo(repoRoot)
			if err != nil {
				return fmt.Errorf("auto-playbook: open repo: %w", err)
			}
			diffBytes, err := repo.DiffForCommit(ctx, res.CommitHash)
			if err != nil {
				return fmt.Errorf("auto-playbook: diff: %w", err)
			}

			// 4. Build LLM prompt.
			ectx := buildExplainContext(res)
			entries := buildTranscriptEntries(res)
			prompt := llm.BuildUserPrompt(
				res.CommitHash, res.CommitSubject,
				ectx, entries, string(diffBytes),
			)

			// 5. Call LLM.
			gen, err := llm.Generate(ctx, prompt)
			if err != nil {
				return fmt.Errorf("auto-playbook: llm: %w", err)
			}

			// 6. Save summary.
			if err := svc.SaveSummary(ctx, service.SaveSummaryInput{
				RepoPath:     repoRoot,
				CheckpointID: res.CheckpointID,
				Summary: &service.NarrativeResultJSON{
					Title:     gen.Narrative.Title,
					Intent:    gen.Narrative.Intent,
					Outcome:   gen.Narrative.Outcome,
					Learnings: gen.Narrative.Learnings,
					Friction:  gen.Narrative.Friction,
					OpenItems: gen.Narrative.OpenItems,
					Keywords:  gen.Narrative.Keywords,
				},
				Model: gen.Model,
			}); err != nil {
				return fmt.Errorf("auto-playbook: save: %w", err)
			}

			semDir := filepath.Join(repoRoot, ".semantica")
			util.AppendActivityLog(semDir, "Generated playbook for commit %s (checkpoint %s) by %s (%s)",
				util.ShortID(commitHash), checkpointID, gen.Provider, gen.Model)

			fmt.Fprintf(os.Stderr, "auto-playbook: summary saved for checkpoint %s\n", checkpointID)

			// 7. Re-push attribution with playbook_summary now populated.
			// This triggers backend rematerialization of any PR comments.
			if util.IsConnected(semDir) {
				service.RePushAttribution(ctx, repoRoot, commitHash, checkpointID)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&commitHash, "commit", "", "commit hash")
	cmd.Flags().StringVar(&checkpointID, "checkpoint", "", "checkpoint ID")
	cmd.Flags().StringVar(&repoRoot, "repo", "", "repository root path")
	if err := cmd.MarkFlagRequired("commit"); err != nil {
		panic(err)
	}
	if err := cmd.MarkFlagRequired("checkpoint"); err != nil {
		panic(err)
	}
	if err := cmd.MarkFlagRequired("repo"); err != nil {
		panic(err)
	}

	return cmd
}

// hasSummary returns true if a summary already exists for the given checkpoint.
func hasSummary(repoRoot, checkpointID string) bool {
	semDir := filepath.Join(repoRoot, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")

	ctx := context.Background()
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return false
	}
	defer func() { _ = sqlstore.Close(h) }()

	row, err := h.Queries.GetCheckpointSummary(ctx, checkpointID)
	if err != nil {
		return false
	}
	return row.SummaryJson != (sql.NullString{}) && row.SummaryJson.Valid
}
