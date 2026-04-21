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

// NewWorkerDrainCmd returns the hidden `semantica worker drain`
// subcommand that is the entry point invoked by the launchd agent
// (see internal/launcher). It discovers every active repository
// via the broker registry, processes every pending marker in each
// repository's .semantica/pending/ directory by running the
// standard worker pipeline per marker, and loops until the queue
// stays empty across a bounded idle linger.
//
// The command accepts a --linger flag mostly so tests can force
// zero-linger runs. Production invocations rely on the default.
func NewWorkerDrainCmd(rootOpts *RootOptions) *cobra.Command {
	var lingerSeconds int

	cmd := &cobra.Command{
		Use:    "drain",
		Short:  "Drain pending post-commit markers across all active repositories",
		Hidden: true,
		// Silence cobra's own error / usage output so errors from
		// DrainUntilStable surface as the one clean line produced
		// by the top-level wrapper rather than mixed with a cobra
		// usage block. Matches the pattern used by the other
		// hidden background commands.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
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

	return cmd
}
