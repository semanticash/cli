package commands

import (
	"fmt"
	"path/filepath"

	"github.com/semanticash/cli/internal/auth"
	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/util"
	"github.com/spf13/cobra"
)

func NewSetCmd(rootOpts *RootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set",
		Short: "View or update Semantica settings",
		Long: `View current settings or update them with subcommands.

Examples:
  semantica set                              Show current settings
  semantica set auto-playbook enabled        Enable auto-playbook
  semantica set auto-playbook disabled       Disable auto-playbook
  semantica set trailers enabled             Enable attribution & diagnostics trailers
  semantica set trailers disabled            Disable attribution & diagnostics trailers`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return cmd.Help()
			}

			repo, err := git.OpenRepo(rootOpts.RepoPath)
			if err != nil {
				return err
			}
			semDir := filepath.Join(repo.Root(), ".semantica")

			if !util.IsEnabled(semDir) {
				return fmt.Errorf("semantica is not enabled. Run `semantica enable` first")
			}

			s, err := util.ReadSettings(semDir)
			if err != nil {
				return fmt.Errorf("read settings: %w", err)
			}

			out := cmd.OutOrStdout()

			_, _ = fmt.Fprintln(out, "Semantica settings:")
			_, _ = fmt.Fprintln(out)

			playbookStatus := "disabled"
			if util.IsPlaybookEnabled(semDir) {
				playbookStatus = "enabled"
			}
			_, _ = fmt.Fprintf(out, "  Auto-playbook:   %s\n", playbookStatus)

			trailerStatus := "disabled"
			if util.TrailersEnabled(semDir) {
				trailerStatus = "enabled"
			}
			_, _ = fmt.Fprintf(out, "  Git Trailers:    %s\n", trailerStatus)

			connectedStatus := "no"
			if s.Connected {
				connectedStatus = "yes"
			}
			_, _ = fmt.Fprintf(out, "  Connected:       %s\n", connectedStatus)
			_, _ = fmt.Fprintf(out, "  Endpoint:        %s\n", auth.EffectiveEndpoint())

			if len(s.Providers) > 0 {
				_, _ = fmt.Fprintln(out)
				_, _ = fmt.Fprintln(out, "Providers (hooks installed):")
				for _, name := range s.Providers {
					_, _ = fmt.Fprintf(out, "    %s\n", name)
				}
			}

			return nil
		},
	}

	cmd.AddCommand(newSetAutoPlaybookCmd(rootOpts))
	cmd.AddCommand(newSetTrailersCmd(rootOpts))

	return cmd
}

func newSetTrailersCmd(rootOpts *RootOptions) *cobra.Command {
	return &cobra.Command{
		Use:       "trailers <enabled|disabled>",
		Short:     "Enable or disable attribution and diagnostics commit trailers",
		Long:      "Controls whether Semantica-Attribution and Semantica-Diagnostics trailers are appended to commits. Semantica-Checkpoint is always included.",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"enabled", "disabled", "on", "off", "true", "false"},
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := git.OpenRepo(rootOpts.RepoPath)
			if err != nil {
				return err
			}
			semDir := filepath.Join(repo.Root(), ".semantica")

			if !util.IsEnabled(semDir) {
				return fmt.Errorf("semantica is not enabled. Run `semantica enable` first")
			}

			s, err := util.ReadSettings(semDir)
			if err != nil {
				return fmt.Errorf("read settings: %w", err)
			}

			out := cmd.OutOrStdout()
			boolTrue, boolFalse := true, false

			switch args[0] {
			case "enabled", "true", "on":
				s.Trailers = &boolTrue
				_, _ = fmt.Fprintln(out, "Git Trailers: enabled")
			case "disabled", "false", "off":
				s.Trailers = &boolFalse
				_, _ = fmt.Fprintln(out, "Git Trailers: disabled (checkpoint-only)")
			default:
				return fmt.Errorf("invalid value: %q (use enabled/disabled)", args[0])
			}

			if err := util.WriteSettings(semDir, s); err != nil {
				return fmt.Errorf("write settings: %w", err)
			}
			return nil
		},
	}
}

func newSetAutoPlaybookCmd(rootOpts *RootOptions) *cobra.Command {
	return &cobra.Command{
		Use:       "auto-playbook <enabled|disabled>",
		Short:     "Enable or disable auto-playbook generation after each commit",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"enabled", "disabled", "on", "off", "true", "false"},
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := git.OpenRepo(rootOpts.RepoPath)
			if err != nil {
				return err
			}
			semDir := filepath.Join(repo.Root(), ".semantica")

			if !util.IsEnabled(semDir) {
				return fmt.Errorf("semantica is not enabled. Run `semantica enable` first")
			}

			s, err := util.ReadSettings(semDir)
			if err != nil {
				return fmt.Errorf("read settings: %w", err)
			}

			if s.Automations == nil {
				s.Automations = &util.Automations{}
			}

			out := cmd.OutOrStdout()

			switch args[0] {
			case "enabled", "true", "on":
				s.Automations.Playbook.Enabled = true
				_, _ = fmt.Fprintln(out, "Auto-playbook: enabled")
			case "disabled", "false", "off":
				s.Automations.Playbook.Enabled = false
				_, _ = fmt.Fprintln(out, "Auto-playbook: disabled")
			default:
				return fmt.Errorf("invalid value: %q (use enabled/disabled)", args[0])
			}

			if err := util.WriteSettings(semDir, s); err != nil {
				return fmt.Errorf("write settings: %w", err)
			}
			return nil
		},
	}
}
