package commands

import (
	"time"

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
	cmd.AddCommand(NewWorkerDrainCmd(rootOpts))
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

// NewWorkerDrainCmd returns the hidden launchd entry point that
// drains pending markers across active repositories.
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
			// First action: redirect worker output to the requested
			// file when --log-file is set. Used by the Linux systemd
			// and Windows Task Scheduler launcher backends, where the
			// daemon manager does not redirect stdout/stderr at the OS
			// level. macOS launchd handles redirection via the plist's
			// StandardOutPath / StandardErrorPath, so the flag is a
			// no-op there but accepted for cross-platform parity.
			//
			// Per-job repo-local redirects (see service.redirectWlogToRepoLog)
			// compose with this: they save and restore wlogWriter
			// around each job, so pipeline output still lands in the
			// repo's .semantica/worker.log while everything outside a
			// job lands in --log-file.
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
			return service.DrainUntilStable(
				cmd.Context(),
				linger,
				service.DefaultMarkerRunner(),
			)
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
