package commands

import (
	"fmt"
	"sort"

	"github.com/semanticash/cli/internal/skills"
	"github.com/semanticash/cli/internal/version"
	"github.com/spf13/cobra"
)

// NewSkillsCmd builds the `semantica skills` command tree. The
// visible subcommands (`install`, `uninstall`) provision and remove
// SKILL.md files at the agent's user-global skills directory; the
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
	return cmd
}

// newSkillsInstallCmd wires `semantica skills install --source
// <path>`. This release supports local source directories only;
// release tarball or git-clone sources can be added later. The install
// targets the Claude Code user-global skills directory; multi-agent
// targeting can be added after other loaders are verified.
func newSkillsInstallCmd() *cobra.Command {
	var source string
	var force bool
	cmd := &cobra.Command{
		Use:           "install",
		Short:         "Install Semantica skill files into the user-global skills directory",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if source == "" {
				return fmt.Errorf("--source is required (path to a directory laid out as <source>/<skill-name>/SKILL.md)")
			}
			rep, err := skills.Install(skills.InstallOptions{
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
	cmd.Flags().StringVar(&source, "source", "", "path to a local skills source directory")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite edited or unmanaged destination files")
	return cmd
}

// newSkillsUninstallCmd wires `semantica skills uninstall`. It
// scans the user-global skills directory for Semantica-managed
// files and removes the ones whose hash matches what was installed.
// Edited Semantica-managed files are preserved unless --force is set.
// Marker-missing files are always preserved.
func newSkillsUninstallCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:           "uninstall",
		Short:         "Remove Semantica skill files from the user-global skills directory",
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

// renderSkillsReport prints a deterministic per-skill summary so a
// terminal user (and any CI log) can see exactly what changed.
// Tallies appear at the bottom for at-a-glance.
func renderSkillsReport(cmd *cobra.Command, rep *skills.Report, verb string) {
	out := cmd.OutOrStdout()
	if len(rep.Actions) == 0 {
		_, _ = fmt.Fprintf(out, "No skills to %s.\n", verb)
		return
	}
	tally := map[skills.ActionKind]int{}
	for _, a := range rep.Actions {
		switch a.Action {
		case skills.ActionSkipped:
			_, _ = fmt.Fprintf(out, "  skipped  %s - %s\n", a.Skill, a.Reason)
		case skills.ActionForced:
			_, _ = fmt.Fprintf(out, "  forced   %s - %s\n", a.Skill, a.Reason)
		default:
			_, _ = fmt.Fprintf(out, "  %-8s %s\n", string(a.Action), a.Skill)
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

// newSkillsHandoffCmd is the hidden backing command for the
// semantica-handoff SKILL.md body. It is a thin wrapper around the
// same writer that `semantica handoff --write` runs, producing the
// same two-line stdout. The SKILL.md body invokes this command
// once, prints stdout verbatim, and stops.
func newSkillsHandoffCmd(rootOpts *RootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "handoff",
		Short:         "Backing command for the semantica-handoff skill (hidden)",
		Args:          cobra.NoArgs,
		Hidden:        true,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHandoffWrite(cmd, rootOpts.RepoPath)
		},
	}
	return cmd
}
