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
  semantica set trailers disabled            Disable attribution & diagnostics trailers
  semantica set intent-gap enabled           Enable background intent-gap analysis
  semantica set intent-gap disabled          Disable background intent-gap analysis`,
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

			implSummaryStatus := "disabled"
			if util.IsImplementationSummaryEnabled(semDir) {
				implSummaryStatus = "enabled"
			}
			_, _ = fmt.Fprintf(out, "  Auto-implementation-summary: %s\n", implSummaryStatus)

			trailerStatus := "disabled"
			if util.TrailersEnabled(semDir) {
				trailerStatus = "enabled"
			}
			_, _ = fmt.Fprintf(out, "  Git Trailers:    %s\n", trailerStatus)

			intentGapStatus := "disabled"
			if util.IntentGapEnabled(semDir) {
				intentGapStatus = "enabled"
			}
			_, _ = fmt.Fprintf(out, "  Intent-gap:      %s\n", intentGapStatus)

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
	cmd.AddCommand(newSetAutoImplementationSummaryCmd(rootOpts))
	cmd.AddCommand(newSetTrailersCmd(rootOpts))
	cmd.AddCommand(newSetIntentGapCmd(rootOpts))

	return cmd
}

func newSetIntentGapCmd(rootOpts *RootOptions) *cobra.Command {
	return &cobra.Command{
		Use:       "intent-gap <enabled|disabled>",
		Short:     "Enable or disable background intent-gap analysis at push time",
		Long:      "When enabled, Semantica checks at push time whether the current branch has an open PR and records the intent-gap trigger for background analysis. The pre-push hook itself stays non-blocking. Off by default.",
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

			enabling := false
			switch args[0] {
			case "enabled", "true", "on":
				s.IntentGapEnabled = &boolTrue
				enabling = true
				_, _ = fmt.Fprintln(out, "Intent-gap analysis: enabled")
			case "disabled", "false", "off":
				s.IntentGapEnabled = &boolFalse
				_, _ = fmt.Fprintln(out, "Intent-gap analysis: disabled")
			default:
				return fmt.Errorf("invalid value: %q (use enabled/disabled)", args[0])
			}

			if err := util.WriteSettings(semDir, s); err != nil {
				return fmt.Errorf("write settings: %w", err)
			}

			// Upgraded repos may not have the pre-push hook yet. Refreshing it
			// here makes the setting effective without requiring re-enable.
			// Disabling leaves the hook installed; the service checks the setting.
			if enabling {
				if err := repo.InstallSemanticaHook(cmd.Context(), git.HookInstallOptions{
					Name:       "pre-push",
					Subcommand: "pre-push",
					PassArgs:   true,
				}); err != nil {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
						"semantica: warning: failed to install pre-push hook (%v); intent-gap will only run after `semantica enable` refreshes hooks\n", err)
				}
			}
			return nil
		},
	}
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

func newSetAutoImplementationSummaryCmd(rootOpts *RootOptions) *cobra.Command {
	return &cobra.Command{
		Use:       "auto-implementation-summary <enabled|disabled>",
		Short:     "Enable or disable auto-generated titles and summaries for cross-repo implementations",
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
				s.Automations.ImplementationSummary.Enabled = true
				_, _ = fmt.Fprintln(out, "Auto-implementation-summary: enabled")
			case "disabled", "false", "off":
				s.Automations.ImplementationSummary.Enabled = false
				_, _ = fmt.Fprintln(out, "Auto-implementation-summary: disabled")
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
