package commands

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/providers"
	"github.com/semanticash/cli/internal/service"
)

func NewWorkerCmd(rootOpts *RootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "worker",
		Short:  "Internal background worker commands",
		Hidden: true,
	}

	cmd.AddCommand(NewWorkerRunCmd(rootOpts))
	cmd.AddCommand(NewWorkerDrainCmd(rootOpts))
	cmd.AddCommand(NewWorkerRetryCmd(rootOpts))
	return cmd
}

// NewWorkerRetryCmd retries a terminally failed checkpoint.
func NewWorkerRetryCmd(rootOpts *RootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "retry <checkpoint-id>",
		Short: "Retry a terminally failed checkpoint",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := git.OpenRepo(rootOpts.RepoPath)
			if err != nil {
				return err
			}
			svc := service.NewWorkerService(providers.NewHookRegistry())
			id, err := svc.ResolveAndRetryCheckpoint(cmd.Context(), repo.Root(), args[0])
			if err != nil {
				return err
			}
			cmd.Printf("checkpoint %s retried\n", id)
			return nil
		},
	}
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
			svc := service.NewWorkerService(providers.NewHookRegistry())
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

// NewWorkerDrainCmd drains pending markers across active repositories.
func NewWorkerDrainCmd(rootOpts *RootOptions) *cobra.Command {
	var (
		lingerSeconds int
		logFilePath   string
	)

	cmd := &cobra.Command{
		Use:    "drain",
		Short:  "Drain pending post-commit markers across all active repositories",
		Hidden: true,
		// Keep output to the top-level wrapper's single error line.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Linux and Windows pass --log-file because their launcher
			// backends do not redirect process output. Per-repository
			// logs temporarily override this destination for each job.
			if logFilePath != "" {
				cleanup, err := service.RedirectWorkerLog(logFilePath)
				if err != nil {
					return err
				}
				defer func() { _ = cleanup() }()
			}

			linger := time.Duration(lingerSeconds) * time.Second
			if lingerSeconds < 0 {
				linger = service.DefaultDrainLinger
			}
			err := service.DrainUntilStable(
				cmd.Context(),
				linger,
				service.DefaultMarkerRunner(providers.NewHookRegistry()),
			)
			// Recover tool windows on every drain wake-up.
			service.SweepToolWindows(cmd.Context())
			return err
		},
	}

	cmd.Flags().IntVar(
		&lingerSeconds,
		"linger",
		int(service.DefaultDrainLinger/time.Second),
		"seconds to wait after an empty pass before committing to exit; "+
			"negative values use the built-in default",
	)
	cmd.Flags().StringVar(
		&logFilePath,
		"log-file",
		"",
		"redirect worker stdout/stderr to this file in append mode; "+
			"used by Linux systemd and Windows Task Scheduler launcher "+
			"backends, ignored when unset",
	)

	return cmd
}
