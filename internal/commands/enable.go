package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/mattn/go-isatty"

	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/service"
	"github.com/semanticash/cli/internal/util"
	"github.com/semanticash/cli/internal/version"
	"github.com/spf13/cobra"
)

var errAborted = errors.New("aborted")

// handleAbort prints a consistent cancellation message and returns nil when
// the error is a user abort (Ctrl+C, Esc, empty selection). Returns the
// original error otherwise. Use at the top of a cobra RunE handler so
// interactive commands exit cleanly without stack-trace-style output.
func handleAbort(out io.Writer, err error) (bool, error) {
	if err == nil {
		return false, nil
	}
	if errors.Is(err, errAborted) || errors.Is(err, huh.ErrUserAborted) {
		_, _ = fmt.Fprintln(out, "Aborted by the user.")
		return true, nil
	}
	return false, err
}

func NewEnableCmd(rootOpts *RootOptions) *cobra.Command {
	var (
		force     bool
		asJSON    bool
		providers []string
	)
	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Enables Semantica in the current git repository",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			repoPath := rootOpts.RepoPath
			if repoPath == "" {
				repoPath = "."
			}

			// Skip the interactive provider prompt when the repo is
			// already enabled - Enable() will return an error anyway.
			var selected []string
			if force || !util.IsEnabledAt(repoPath) {
				var err error
				selected, err = resolveProviders(providers)
				if err != nil {
					if errors.Is(err, errAborted) {
						_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Aborted by the user.")
						return nil
					}
					return err
				}
			}

			svc, err := service.NewEnableService(service.EnableServiceOptions{
				RepoPath: repoPath,
			})
			if err != nil {
				return err
			}

			var res *service.EnableResult
			out := cmd.OutOrStdout()
			action := func() {
				res, err = svc.Enable(ctx, service.EnableOptions{
					Force:     force,
					Providers: selected,
				})
			}
			if spinErr := runWithOptionalSpinner(out, asJSON, "Enabling Semantica...", action); spinErr != nil {
				action()
			}
			if err != nil {
				return err
			}

			res.WorkspaceTierTitle = lookupWorkspaceTierTitle(ctx)
			if update := lookupCLIUpdate(ctx); update != nil {
				res.UpdateAvailable = true
				res.LatestVersion = update.LatestVersion
				res.UpdateDownloadURL = update.DownloadURL
			}

			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}

			if isTerminalWriter(out) {
				_, _ = lipgloss.Fprintln(out, renderEnableCard(res))
				return nil
			}

			_, _ = fmt.Fprint(out, renderEnablePlain(res))

			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "Reinitialize Semantica state if already enabled")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")
	cmd.Flags().StringSliceVar(&providers, "providers", nil, "Agents to install hooks for (e.g. claude-code,cursor)")

	return cmd
}

// resolveProviders determines which provider hooks to install.
//
// If explicit names are given via --providers, those are returned as-is.
// Otherwise, detection runs:
//   - 0 detected  -> warn, return nil (enable proceeds without provider hooks)
//   - 1 detected  -> auto-select silently
//   - 2+ detected -> interactive multi-select (falls back to all if not a TTY)
func resolveProviders(explicit []string) ([]string, error) {
	if len(explicit) > 0 {
		return explicit, nil
	}

	available := hooks.ListAvailableProviders()

	switch len(available) {
	case 0:
		fmt.Fprintln(os.Stderr, "No supported AI coding agents detected on this machine.")
		fmt.Fprintln(os.Stderr, "Semantica will be enabled without provider hooks.")
		fmt.Fprintln(os.Stderr, "Run `semantica enable --providers <name>` later to add them.")
		return []string{}, nil

	case 1:
		name := available[0].Name()
		fmt.Fprintf(os.Stderr, "Detected %s - installing hooks.\n", available[0].DisplayName())
		return []string{name}, nil

	default:
		if !isTerminal() {
			// Non-interactive: install all detected.
			names := make([]string, len(available))
			for i, p := range available {
				names[i] = p.Name()
			}
			return names, nil
		}

		selected, err := promptProviderSelection(available)
		if err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return nil, errAborted
			}
			// TTY unavailable (e.g. sandboxed env) - fall back to all.
			names := make([]string, len(available))
			for i, p := range available {
				names[i] = p.Name()
			}
			return names, nil
		}
		return selected, nil
	}
}

// promptProviderSelection shows an interactive multi-select for provider hooks.
// All detected agents start selected - user deselects what they don't want.
func promptProviderSelection(available []hooks.HookProvider) ([]string, error) {
	options := make([]huh.Option[string], len(available))
	for i, p := range available {
		options[i] = huh.NewOption(p.DisplayName(), p.Name()).Selected(true)
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
				Title("Select AI agents to capture (Detected agents are auto-selected)").
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
			return nil, err
		}
		return nil, fmt.Errorf("provider selection: %w", err)
	}

	return selected, nil
}

func isTerminal() bool {
	return isInteractiveTerminal(os.Stdin)
}

func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return isInteractiveTerminal(f)
}

func isInteractiveTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

func renderEnablePlain(res *service.EnableResult) string {
	var b strings.Builder

	b.WriteString("Semantica enabled\n")
	b.WriteString("Repo: " + res.RepoRoot + "\n")
	b.WriteString("Store: " + res.SemanticaDir + "\n")
	if res.WorkspaceTierTitle != "" {
		b.WriteString("Workspace tier: " + res.WorkspaceTierTitle + "\n")
	}
	b.WriteString("Hooks: " + hooksSummary(res.HooksInstalled) + "\n")

	if len(res.Providers) > 0 {
		b.WriteString("Agents: " + strings.Join(res.Providers, ", ") + "\n")
	}
	if res.UpdateAvailable && res.LatestVersion != "" {
		b.WriteString("\n")
		b.WriteString(renderUpgradePlain(res.LatestVersion))
	}

	if len(res.Providers) > 0 {
		b.WriteString("\n")
		b.WriteString("Note: If any agents are already running, restart or reload them\n")
		b.WriteString("      for Semantica to start capturing.\n")
	}

	b.WriteString("\n")
	b.WriteString("Tip: Run `semantica connect` to sync attribution to your dashboard.\n")

	return b.String()
}

func renderEnableCard(res *service.EnableResult) string {
	theme := enableCardTheme()

	boxStyle := theme.Focused.Card.
		UnsetBorderLeft().
		BorderStyle(lipgloss.NormalBorder()).
		BorderTop(true).
		BorderRight(true).
		BorderBottom(true).
		BorderLeft(true).
		Padding(0, 1)
	versionStyle := theme.Focused.Description
	labelStyle := lipgloss.NewStyle().Bold(true)
	valueStyle := lipgloss.NewStyle()
	subtleStyle := theme.Focused.Description
	bodyStyle := lipgloss.NewStyle()
	commandStyle := theme.Focused.SelectedOption.Bold(true)
	titleStyle := theme.Focused.SelectedOption.Bold(true)

	header := lipgloss.JoinHorizontal(lipgloss.Center,
		titleStyle.Render("Semantica"),
		" ",
		versionStyle.Render(version.Version),
	)

	rows := []string{
		enableCardRow(labelStyle, valueStyle, "Status", "Enabled in "+res.RepoRoot),
		enableCardRow(labelStyle, valueStyle, "Hooks", hooksSummary(res.HooksInstalled)),
	}
	rows = append(rows, enableCardRow(labelStyle, valueStyle, "Store", res.SemanticaDir))
	if res.WorkspaceTierTitle != "" {
		rows = append(rows, enableCardRow(labelStyle, valueStyle, "Workspace tier", res.WorkspaceTierTitle))
	}
	if len(res.Providers) > 0 {
		rows = append(rows, enableCardRow(labelStyle, valueStyle, "Agents", strings.Join(res.Providers, ", ")))
	}

	if len(res.Providers) > 0 {
		rows = append(rows, subtleStyle.Render("Note: If any agents are already running, restart or reload them for Semantica to start capturing."))
	}

	tip := lipgloss.JoinHorizontal(lipgloss.Top,
		labelStyle.Render("Tip:"),
		" ",
		bodyStyle.Render("Run "),
		commandStyle.Render("semantica connect"),
		bodyStyle.Render(" to sync attribution to your dashboard."),
	)

	sections := []string{
		header,
		subtleStyle.Render("Code, with provenance."),
		"",
		lipgloss.JoinVertical(lipgloss.Left, rows...),
	}
	if upgradeCard := renderUpgradeCard(theme, res.UpdateAvailable, res.LatestVersion); upgradeCard != "" {
		sections = append(sections, "", upgradeCard)
	}
	sections = append(sections, "", tip)

	content := lipgloss.JoinVertical(lipgloss.Left, sections...)

	return boxStyle.Render(content)
}

func enableCardRow(labelStyle, valueStyle lipgloss.Style, label, value string) string {
	return lipgloss.JoinHorizontal(lipgloss.Top,
		labelStyle.Render(label+":"),
		" ",
		valueStyle.Render(value),
	)
}

func hooksSummary(installed bool) string {
	if installed {
		return "pre-commit, post-commit, commit-msg installed"
	}
	return "installed with warnings"
}

func enableCardTheme() *huh.Styles {
	if isTerminalWriter(os.Stdout) {
		return huh.ThemeCharm(lipgloss.HasDarkBackground(os.Stdin, os.Stdout))
	}
	return huh.ThemeCharm(true)
}

func renderUpgradePlain(latestVersion string) string {
	var b strings.Builder

	b.WriteString("Update available\n")
	b.WriteString("  Version: " + latestVersion + "\n")
	b.WriteString("  Install: " + cliUpgradeCommand + "\n")

	return b.String()
}

func renderUpgradeCard(theme *huh.Styles, show bool, latestVersion string) string {
	if !show || latestVersion == "" {
		return ""
	}

	boxStyle := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		Padding(0, 1)
	titleStyle := theme.Focused.SelectedOption.Bold(true)
	labelStyle := lipgloss.NewStyle().Bold(true)
	valueStyle := lipgloss.NewStyle()
	subtleStyle := theme.Focused.Description

	content := lipgloss.JoinVertical(lipgloss.Left,
		titleStyle.Render("Update available"),
		subtleStyle.Render("A newer Semantica release is ready to install."),
		"",
		enableCardRow(labelStyle, valueStyle, "Version", latestVersion),
		lipgloss.JoinHorizontal(lipgloss.Top,
			labelStyle.Render("Install:"),
			" ",
			valueStyle.Render(cliUpgradeCommand),
		),
	)

	return boxStyle.Render(content)
}
