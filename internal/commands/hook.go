package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/semanticash/cli/internal/providers"
	"github.com/semanticash/cli/internal/service"
)

func NewHookCmd(rootOpts *RootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "hook",
		Short:  "Internal hook entrypoints",
		Hidden: true,
	}

	cmd.AddCommand(NewHookPreCommitCmd(rootOpts))
	cmd.AddCommand(NewHookPostCommitCmd(rootOpts))
	cmd.AddCommand(NewHookCommitMsgCmd(rootOpts))
	cmd.AddCommand(NewHookPrePushCmd(rootOpts))
	cmd.AddCommand(NewHookIntentGapUploadCmd(rootOpts))

	return cmd
}

// NewHookIntentGapUploadCmd is the detached worker spawned by the
// pre-push hook. It is hidden because it is an internal entrypoint.
//
// Output is redirected by the spawning hook, and the command returns
// nil so worker failures never affect the parent hook's exit code.
// Errors that occur before .semantica is available are written to
// stderr, which the spawning hook captures in the worker log.
func NewHookIntentGapUploadCmd(rootOpts *RootOptions) *cobra.Command {
	return &cobra.Command{
		Use:    "intent-gap-upload",
		Short:  "Internal: analyze and record intent-gap findings",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc := service.NewIntentGapUploadService(service.IntentGapUploadDeps{})
			_, err := svc.Run(cmd.Context(), rootOpts.RepoPath)
			if err != nil {
				// The service logs once .semantica is available.
				// Before that, stderr is the only worker-log sink.
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "intent-gap-upload: %v\n", err)
			}
			return nil
		},
	}
}

// NewHookPrePushCmd handles git's pre-push hook without blocking the push.
// Decisions are logged for doctor instead of returned to git.
func NewHookPrePushCmd(rootOpts *RootOptions) *cobra.Command {
	return &cobra.Command{
		// Git passes remote name and URL as argv; accept extras so wrappers
		// can forward "$@" unchanged.
		Use:    "pre-push [remote_name] [remote_url]",
		Short:  "Handle git pre-push hook",
		Args:   cobra.ArbitraryArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc := service.NewPrePushService()
			_, err := svc.HandlePrePush(cmd.Context(), rootOpts.RepoPath, cmd.InOrStdin())
			// Keep Semantica's hook non-blocking; diagnostics are recorded by the service.
			_ = err
			return nil
		},
	}
}

func NewHookPreCommitCmd(rootOpts *RootOptions) *cobra.Command {
	return &cobra.Command{
		Use:    "pre-commit",
		Short:  "Handle git pre-commit hook",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc := service.NewPreCommitService()
			return svc.HandlePreCommit(cmd.Context(), rootOpts.RepoPath)
		},
	}
}

func NewHookPostCommitCmd(rootOpts *RootOptions) *cobra.Command {
	return &cobra.Command{
		Use:    "post-commit",
		Short:  "Handle git post-commit hook",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc := service.NewPostCommitService()
			_, err := svc.HandlePostCommit(cmd.Context(), rootOpts.RepoPath)
			return err
		},
	}
}

func NewHookCommitMsgCmd(rootOpts *RootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "commit-msg <commit_msg_file>",
		Short:  "Internal: Git commit-msg hook handler",
		Args:   cobra.MinimumNArgs(1),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc := service.NewCommitMsgHookService(rootOpts.RepoPath, providers.NewHookRegistry())
			return svc.Run(cmd.Context(), args[0])
		},
	}
	return cmd
}
