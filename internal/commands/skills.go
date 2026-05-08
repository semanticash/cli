package commands

import "github.com/spf13/cobra"

// NewSkillsCmd builds the `semantica skills` command tree. It
// groups the per-skill backing commands that SKILL.md bodies invoke
// (each hidden via `Hidden: true`). The parent stays hidden until
// visible install and uninstall commands are available, so
// `semantica --help` does not show an incomplete `skills` namespace.
//
// The hidden backing commands are still callable: cobra's hidden
// flag only affects help output, not path resolution. SKILL.md
// bodies invoke them by name.
func NewSkillsCmd(rootOpts *RootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "skills",
		Short:  "Manage Semantica skill installations",
		Hidden: true,
	}
	cmd.AddCommand(newSkillsHandoffCmd(rootOpts))
	return cmd
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
