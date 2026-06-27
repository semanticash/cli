package commands

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/semanticash/cli/internal/explain"
	"github.com/semanticash/cli/internal/service"
	"github.com/semanticash/cli/internal/skills"
	"github.com/semanticash/cli/internal/version"
	"github.com/spf13/cobra"
)

// NewSkillsCmd builds the `semantica skills` command tree. The
// visible subcommands (`install`, `uninstall`) provision and remove
// SKILL.md files in detected agent skills directories; the
// hidden subcommands (`handoff`, plus future skill-specific commands)
// back the SKILL.md bodies themselves. Hidden subcommands stay
// reachable: cobra's hidden flag only affects help output, so SKILL.md
// invocations resolve as expected.
func NewSkillsCmd(rootOpts *RootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "Install and manage Semantica skills",
	}
	cmd.AddCommand(newSkillsInstallCmd())
	cmd.AddCommand(newSkillsUninstallCmd())
	cmd.AddCommand(newSkillsHandoffCmd(rootOpts))
	cmd.AddCommand(newSkillsExplainCmd(rootOpts))
	cmd.AddCommand(newSkillsIntentGapCmd(rootOpts))
	return cmd
}

// newSkillsInstallCmd wires `semantica skills install`. By default
// it fetches Semantica-authored skills from GitHub; --source points
// at a local checkout for development and offline installs.
func newSkillsInstallCmd() *cobra.Command {
	var source string
	var force bool
	cmd := &cobra.Command{
		Use:           "install",
		Short:         "Install Semantica skill files into detected agent skills directories",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			rep, err := skills.Install(cmd.Context(), skills.InstallOptions{
				Source:     source,
				CLIVersion: version.Version,
				Force:      force,
			})
			if err != nil {
				return err
			}
			renderSkillsReport(cmd, rep, "install")
			return nil
		},
	}
	cmd.Flags().StringVar(&source, "source", "", "path to a local skills source directory (default: fetch from semanticash/skills GitHub repo)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite edited or unmanaged destination files")
	return cmd
}

// newSkillsUninstallCmd wires `semantica skills uninstall`. It
// scans detected agent skills directories for Semantica-managed files
// and removes the ones whose hash matches what was installed. Edited
// Semantica-managed files are preserved unless --force is set.
// Marker-missing files are always preserved.
func newSkillsUninstallCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:           "uninstall",
		Short:         "Remove Semantica skill files from detected agent skills directories",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			rep, err := skills.Uninstall(force)
			if err != nil {
				return err
			}
			renderSkillsReport(cmd, rep, "uninstall")
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "remove edited Semantica-managed files")
	return cmd
}

// renderSkillsReport prints a deterministic per-(skill, target)
// summary so a terminal user (and any CI log) can see exactly
// what changed for which agent. Tallies appear at the bottom for
// at-a-glance.
func renderSkillsReport(cmd *cobra.Command, rep *skills.Report, verb string) {
	out := cmd.OutOrStdout()
	if len(rep.Actions) == 0 {
		_, _ = fmt.Fprintf(out, "No skills to %s.\n", verb)
		return
	}
	tally := map[skills.ActionKind]int{}
	for _, a := range rep.Actions {
		label := a.Skill
		if a.Target != "" {
			label = a.Skill + " (" + a.Target + ")"
		}
		switch a.Action {
		case skills.ActionSkipped:
			_, _ = fmt.Fprintf(out, "  skipped  %s - %s\n", label, a.Reason)
		case skills.ActionForced:
			_, _ = fmt.Fprintf(out, "  forced   %s - %s\n", label, a.Reason)
		default:
			_, _ = fmt.Fprintf(out, "  %-8s %s\n", string(a.Action), label)
		}
		tally[a.Action]++
	}
	_, _ = fmt.Fprintln(out, summarizeTally(tally))
}

// summarizeTally builds a one-line summary like "1 installed, 2
// updated, 1 skipped" with stable ordering across runs.
func summarizeTally(tally map[skills.ActionKind]int) string {
	type pair struct {
		k skills.ActionKind
		n int
	}
	var pairs []pair
	for k, n := range tally {
		pairs = append(pairs, pair{k, n})
	}
	sort.Slice(pairs, func(i, j int) bool { return string(pairs[i].k) < string(pairs[j].k) })
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, fmt.Sprintf("%d %s", p.n, p.k))
	}
	return joinWithCommas(parts)
}

func joinWithCommas(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += ", " + p
	}
	return out
}

// newSkillsExplainCmd is the hidden backing command for the
// semantica-explain SKILL.md body. The skill body invokes this
// command with a single ref argument and parses the JSON it
// emits. Exit codes follow the skill command contract: zero whenever
// structured JSON is produced (including the blocked and
// not-found modes), non-zero only when the command itself failed
// before producing output.
func newSkillsExplainCmd(rootOpts *RootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "explain <ref>",
		Short:         "Backing command for the semantica-explain skill (hidden)",
		Args:          cobra.ExactArgs(1),
		Hidden:        true,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc := explain.NewService()
			out, err := svc.Explain(cmd.Context(), explain.Input{
				RepoPath: rootOpts.RepoPath,
				Ref:      args[0],
			})
			if err != nil {
				return err
			}
			body, err := json.Marshal(out)
			if err != nil {
				return fmt.Errorf("marshal explain output: %w", err)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(body))
			return nil
		},
	}
	return cmd
}

// newSkillsIntentGapCmd is the hidden backing command for the
// semantica-intent-gap SKILL.md body. It mirrors what
// `semantica intent-gap analyze --upload` does for terminal users:
// runs the upload service in the foreground and prints the same
// one-line status. --base mirrors the user-facing flag.
func newSkillsIntentGapCmd(rootOpts *RootOptions) *cobra.Command {
	var base string
	cmd := &cobra.Command{
		Use:           "intent-gap",
		Short:         "Backing command for the semantica-intent-gap skill (hidden)",
		Args:          cobra.NoArgs,
		Hidden:        true,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc := service.NewIntentGapUploadService(service.IntentGapUploadDeps{BaseRef: base})
			// Keep skill output aligned with
			// `semantica intent-gap analyze --upload`.
			res, err := svc.Run(cmd.Context(), rootOpts.RepoPath, service.RunOptions{Upload: true})
			if err != nil {
				return err
			}
			return renderAnalyzeResult(cmd.OutOrStdout(), false, res)
		},
	}
	cmd.Flags().StringVar(&base, "base", "", "Base branch or ref (default: auto-detect)")
	return cmd
}

// newSkillsHandoffCmd is the hidden backing command for the
// semantica-handoff SKILL.md body. It is a thin wrapper around the
// same writer that `semantica handoff --write` runs, producing the
// same two-line stdout. The SKILL.md body invokes this command
// once, prints stdout verbatim, and stops.
//
// --from mirrors the user-facing flag so skill and terminal
// invocations keep the same source-selection behavior.
func newSkillsHandoffCmd(rootOpts *RootOptions) *cobra.Command {
	var from string
	cmd := &cobra.Command{
		Use:           "handoff",
		Short:         "Backing command for the semantica-handoff skill (hidden)",
		Args:          cobra.NoArgs,
		Hidden:        true,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHandoffWrite(cmd, rootOpts.RepoPath, from)
		},
	}
	cmd.Flags().StringVar(&from, "from", "",
		"source provider for the bundle (claude-code, cursor, gemini-cli, "+
			"copilot, kiro-cli, kiro-ide); bypasses the active session")
	return cmd
}
