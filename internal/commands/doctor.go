package commands

import (
	"fmt"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/health"
	"github.com/semanticash/cli/internal/version"
	"github.com/spf13/cobra"
)

// NewDoctorCmd builds the `semantica doctor` command. It runs a
// suite of read-only diagnostic checks against the local install
// and the current repo, then renders a report and exits with a
// status code matching the worst result (0 ok, 1 warn, 2 fail).
func NewDoctorCmd(rootOpts *RootOptions) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose Semantica capture-pipeline health",
		Long: "Run read-only diagnostics against the local Semantica install. " +
			"Doctor never repairs or modifies state. Exit codes: 0 ok, 1 warn, 2 fail.",
		Args: cobra.NoArgs,
		// Suppress cobra's automatic usage print on non-zero exit so
		// the diagnostic output is not buried.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			repoPath := resolveDoctorRepo(rootOpts.RepoPath)
			report, err := health.Run(cmd.Context(), health.Options{
				RepoPath: repoPath,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			switch {
			case asJSON:
				if err := health.RenderJSON(out, report); err != nil {
					return err
				}
			case isTerminalWriter(out):
				if _, err := fmt.Fprintln(out, renderDoctorCard(report)); err != nil {
					return err
				}
			default:
				if err := health.RenderText(out, report); err != nil {
					return err
				}
			}

			// Exit with the report's severity-based code. Returning
			// SilentExitError skips cobra's default error rendering
			// and lets Execute() set the code.
			if code := report.ExitCode(); code != 0 {
				return &doctorExitError{code: code}
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")
	return cmd
}

// renderDoctorCard formats the interactive terminal view.
func renderDoctorCard(r health.Report) string {
	theme := enableCardTheme()

	boxStyle := theme.Focused.Card.
		UnsetBorderLeft().
		BorderStyle(lipgloss.NormalBorder()).
		BorderTop(true).
		BorderRight(true).
		BorderBottom(true).
		BorderLeft(true).
		Padding(0, 1)
	subtleStyle := theme.Focused.Description
	titleStyle := theme.Focused.SelectedOption.Bold(true)
	categoryStyle := lipgloss.NewStyle().Bold(true)
	messageStyle := lipgloss.NewStyle()
	remediationStyle := subtleStyle.Italic(true)

	header := lipgloss.JoinHorizontal(lipgloss.Center,
		titleStyle.Render("Semantica doctor"),
		" ",
		subtleStyle.Render(version.Version),
	)

	sections := []string{
		header,
		subtleStyle.Render("Diagnose capture-pipeline health"),
	}

	for _, cat := range orderedCategoriesForCard(r.Checks) {
		sections = append(sections, "", categoryStyle.Render(categoryTitle(cat)))
		for _, c := range checksForCategory(r.Checks, cat) {
			line := lipgloss.JoinHorizontal(lipgloss.Top,
				renderStatusBadge(c.Status),
				" ",
				messageStyle.Render(c.Message),
			)
			sections = append(sections, "  "+line)
			if c.Remediation != "" {
				sections = append(sections, "      "+remediationStyle.Render("→ "+c.Remediation))
			}
		}
	}

	sections = append(sections, "", renderResultLine(r))

	return boxStyle.Render(lipgloss.JoinVertical(lipgloss.Left, sections...))
}

// renderStatusBadge maps a Status to a compact visual marker.
func renderStatusBadge(s health.Status) string {
	switch s {
	case health.StatusOK:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Render("✓")
	case health.StatusWarn:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Render("!")
	case health.StatusFail:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render("✗")
	default:
		return "·"
	}
}

func renderResultLine(r health.Report) string {
	labelStyle := lipgloss.NewStyle().Bold(true)
	switch r.Result {
	case health.StatusOK:
		valueStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
		return labelStyle.Render("Result: ") + valueStyle.Render("ok")
	case health.StatusWarn:
		valueStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Bold(true)
		return labelStyle.Render("Result: ") +
			valueStyle.Render("warn") +
			lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Render(" "+summaryParenthetical(r))
	case health.StatusFail:
		valueStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
		return labelStyle.Render("Result: ") +
			valueStyle.Render("fail") +
			lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render(" "+summaryParenthetical(r))
	default:
		return labelStyle.Render("Result: ") + string(r.Result)
	}
}

func summaryParenthetical(r health.Report) string {
	parts := []string{}
	if r.Summary.Fail > 0 {
		parts = append(parts, fmt.Sprintf("%d issue%s", r.Summary.Fail, plural(r.Summary.Fail)))
	}
	if r.Summary.Warn > 0 {
		parts = append(parts, fmt.Sprintf("%d warning%s", r.Summary.Warn, plural(r.Summary.Warn)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func categoryTitle(cat string) string {
	switch cat {
	case "binary":
		return "Binary"
	case "launcher":
		return "Launcher"
	case "hooks":
		return "Hooks"
	case "state":
		return "Capture state"
	case "events":
		return "Recent events"
	case "manifests":
		return "Manifests"
	default:
		return cat
	}
}

var doctorCategoryOrder = map[string]int{
	"binary":    0,
	"launcher":  1,
	"hooks":     2,
	"state":     3,
	"events":    4,
	"manifests": 5,
}

func orderedCategoriesForCard(checks []health.Check) []string {
	seen := map[string]struct{}{}
	cats := []string{}
	for _, c := range checks {
		if _, ok := seen[c.Category]; ok {
			continue
		}
		seen[c.Category] = struct{}{}
		cats = append(cats, c.Category)
	}
	sort.Slice(cats, func(i, j int) bool {
		oi, oki := doctorCategoryOrder[cats[i]]
		oj, okj := doctorCategoryOrder[cats[j]]
		if !oki {
			oi = 100
		}
		if !okj {
			oj = 100
		}
		if oi != oj {
			return oi < oj
		}
		return cats[i] < cats[j]
	})
	return cats
}

func checksForCategory(checks []health.Check, cat string) []health.Check {
	var out []health.Check
	for _, c := range checks {
		if c.Category == cat {
			out = append(out, c)
		}
	}
	return out
}

// doctorExitError carries a non-zero exit code without producing
// cobra's standard "Error:" line. Execute() inspects this type.
type doctorExitError struct {
	code int
}

func (e *doctorExitError) Error() string { return "" }
func (e *doctorExitError) ExitCode() int { return e.code }

// resolveDoctorRepo returns the repo root for repo-scoped checks
// such as hooks, git hooks, and settings.json. Outside a git repo it
// returns an empty string so those checks are skipped.
func resolveDoctorRepo(explicit string) string {
	repo, err := git.OpenRepo(explicit)
	if err != nil {
		return ""
	}
	return repo.Root()
}
