package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/util"
	"github.com/spf13/cobra"
)

func NewAgentsCmd(rootOpts *RootOptions) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:     "agents",
		Aliases: []string{"agent"},
		Short:   "Manage AI agent hooks",
		Long:    "Show detected agents and toggle which ones have hooks installed.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			repoRoot, semDir, err := resolveRepo(rootOpts.RepoPath)
			if err != nil {
				return err
			}

			allProviders := hooks.ListProviders()
			out := cmd.OutOrStdout()

			// JSON mode: just print status.
			if asJSON {
				return printAgentsJSON(ctx, out, repoRoot, allProviders)
			}

			// Non-interactive: print status table.
			if !isTerminal() {
				return printAgentsTable(ctx, out, repoRoot, allProviders)
			}

			// Build options: only detected agents, pre-select installed ones.
			var detected []hooks.HookProvider
			for _, p := range allProviders {
				if p.IsAvailable() {
					detected = append(detected, p)
				}
			}

			if len(detected) == 0 {
				_, _ = fmt.Fprintln(out, "No supported AI coding agents detected on this machine.")
				return nil
			}

			options := make([]huh.Option[string], len(detected))
			for i, p := range detected {
				installed := p.AreHooksInstalled(ctx, repoRoot)
				options[i] = huh.NewOption(p.DisplayName(), p.Name()).Selected(installed)
			}

			var selected []string

			selectTheme := huh.ThemeFunc(func(isDark bool) *huh.Styles {
				s := huh.ThemeCharm(isDark)
				green := lipgloss.Color("#02BA84")
				s.Focused.SelectedPrefix = lipgloss.NewStyle().Foreground(green).SetString("[•] ")
				s.Focused.UnselectedPrefix = lipgloss.NewStyle().SetString("[ ] ")
				s.Blurred.SelectedPrefix = lipgloss.NewStyle().Foreground(green).SetString("[•] ")
				s.Blurred.UnselectedPrefix = lipgloss.NewStyle().SetString("[ ] ")
				return s
			})

			form := huh.NewForm(
				huh.NewGroup(
					huh.NewMultiSelect[string]().
						Title("Select AI agents to capture (Installed agents are pre-selected)").
						Description("space: toggle | enter: confirm").
						Options(options...).
						Height(len(options) + 2).
						Validate(func(s []string) error {
							if len(s) == 0 {
								return errors.New("please select at least one agent")
							}
							return nil
						}).
						Value(&selected),
				),
			).WithTheme(selectTheme)

			if err := form.Run(); err != nil {
				if errors.Is(err, huh.ErrUserAborted) {
					_, _ = fmt.Fprintln(out, "Aborted by the user.")
					return nil
				}
				// TTY error - fall back to status table.
				return printAgentsTable(ctx, out, repoRoot, allProviders)
			}

			// Diff: figure out what to install and what to uninstall.
			selectedSet := make(map[string]bool, len(selected))
			for _, n := range selected {
				selectedSet[n] = true
			}

			var installed, removed []string
			for _, p := range detected {
				wasInstalled := p.AreHooksInstalled(ctx, repoRoot)
				wantInstalled := selectedSet[p.Name()]

				if wantInstalled && !wasInstalled {
					if _, err := p.InstallHooks(ctx, repoRoot, hooks.ManagedCommand); err != nil {
						fmt.Fprintf(os.Stderr, "warning: install %s: %v\n", p.Name(), err)
						continue
					}
					installed = append(installed, p.Name())
				} else if !wantInstalled && wasInstalled {
					if err := p.UninstallHooks(ctx, repoRoot); err != nil {
						fmt.Fprintf(os.Stderr, "warning: uninstall %s: %v\n", p.Name(), err)
						continue
					}
					removed = append(removed, p.Name())
				}
			}

			// Update settings with final state.
			settings, err := util.ReadSettings(semDir)
			if err != nil {
				return fmt.Errorf("read settings: %w", err)
			}
			settings.Providers = mergeProviders(
				removeProviders(settings.Providers, removed),
				installed,
			)
			if err := util.WriteSettings(semDir, settings); err != nil {
				return fmt.Errorf("write settings: %w", err)
			}

			// Print summary.
			if len(installed) > 0 {
				_, _ = fmt.Fprintf(out, "Installed: %s\n", strings.Join(installed, ", "))
			}
			if len(removed) > 0 {
				_, _ = fmt.Fprintf(out, "Removed: %s\n", strings.Join(removed, ", "))
			}
			if len(installed) == 0 && len(removed) == 0 {
				_, _ = fmt.Fprintln(out, "No changes.")
			}

			if len(installed) > 0 || len(removed) > 0 {
				_, _ = fmt.Fprintln(out)
				_, _ = fmt.Fprintln(out, "Note: If any agents are already running, restart or reload them")
				_, _ = fmt.Fprintln(out, "      for Semantica to start capturing.")
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")

	return cmd
}

// --- output helpers ---

type agentStatus struct {
	Name      string `json:"name"`
	Display   string `json:"display_name"`
	Detected  bool   `json:"detected"`
	Installed bool   `json:"installed"`
}

func collectStatuses(ctx context.Context, repoRoot string, providers []hooks.HookProvider) []agentStatus {
	statuses := make([]agentStatus, len(providers))
	for i, p := range providers {
		statuses[i] = agentStatus{
			Name:      p.Name(),
			Display:   p.DisplayName(),
			Detected:  p.IsAvailable(),
			Installed: p.AreHooksInstalled(ctx, repoRoot),
		}
	}
	return statuses
}

func printAgentsJSON(ctx context.Context, out io.Writer, repoRoot string, providers []hooks.HookProvider) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(collectStatuses(ctx, repoRoot, providers))
}

func printAgentsTable(ctx context.Context, out io.Writer, repoRoot string, providers []hooks.HookProvider) error {
	for _, s := range collectStatuses(ctx, repoRoot, providers) {
		status := "not detected"
		if s.Installed {
			status = "hooks installed"
		} else if s.Detected {
			status = "detected"
		}
		_, _ = fmt.Fprintf(out, "%-20s %s\n", s.Display, status)
	}
	return nil
}

// --- helpers ---

func resolveRepo(repoPath string) (repoRoot, semDir string, err error) {
	if repoPath == "" {
		repoPath = "."
	}
	repo, err := git.OpenRepo(repoPath)
	if err != nil {
		return "", "", err
	}
	root := repo.Root()
	return root, filepath.Join(root, ".semantica"), nil
}

// mergeProviders adds new names to existing, deduplicating.
func mergeProviders(existing, added []string) []string {
	seen := make(map[string]bool, len(existing))
	for _, n := range existing {
		seen[n] = true
	}
	result := append([]string{}, existing...)
	for _, n := range added {
		if !seen[n] {
			result = append(result, n)
			seen[n] = true
		}
	}
	return result
}

// removeProviders removes names from existing.
func removeProviders(existing, removed []string) []string {
	drop := make(map[string]bool, len(removed))
	for _, n := range removed {
		drop[n] = true
	}
	var result []string
	for _, n := range existing {
		if !drop[n] {
			result = append(result, n)
		}
	}
	return result
}
