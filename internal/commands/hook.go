package commands

import (
	"github.com/spf13/cobra"

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

	return cmd
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
			svc := service.NewCommitMsgHookService(rootOpts.RepoPath)
			return svc.Run(cmd.Context(), args[0])
		},
	}
	return cmd
}
