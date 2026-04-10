package commands

import (
	"github.com/spf13/cobra"

	"github.com/semanticash/cli/internal/service"
)

func NewWorkerCmd(rootOpts *RootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "worker",
		Short:  "Internal background worker commands",
		Hidden: true,
	}

	cmd.AddCommand(NewWorkerRunCmd(rootOpts))
	return cmd
}

func NewWorkerRunCmd(rootOpts *RootOptions) *cobra.Command {
	var (
		checkpointID string
		commitHash   string
		repoRoot     string
	)

	cmd := &cobra.Command{
		Use:    "run",
		Short:  "Complete a pending checkpoint (blobs, manifest, agent ingest)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc := service.NewWorkerService()
			return svc.Run(cmd.Context(), service.WorkerInput{
				CheckpointID: checkpointID,
				CommitHash:   commitHash,
				RepoRoot:     repoRoot,
			})
		},
	}

	cmd.Flags().StringVar(&checkpointID, "checkpoint", "", "checkpoint ID to complete (required)")
	cmd.Flags().StringVar(&commitHash, "commit", "", "commit hash (for logging)")
	cmd.Flags().StringVar(&repoRoot, "repo", "", "repository root path (required)")
	if err := cmd.MarkFlagRequired("checkpoint"); err != nil {
		panic(err)
	}
	if err := cmd.MarkFlagRequired("repo"); err != nil {
		panic(err)
	}

	return cmd
}
