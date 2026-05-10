package commands

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/semanticash/cli/internal/service/handoff"
	"github.com/spf13/cobra"
)

// NewHandoffCmd builds the `semantica handoff` command. The
// initial release ships `--write`, which assembles the bundle on
// disk. A subsequent release will add a `continue` subcommand that
// launches the user's preferred agent against the bundle directly;
// until then users start a fresh session manually.
func NewHandoffCmd(rootOpts *RootOptions) *cobra.Command {
	var write bool

	cmd := &cobra.Command{
		Use:   "handoff",
		Short: "Prepare a session handoff for a fresh agent session",
		Long: "Assemble a redacted, provenance-rich markdown bundle from " +
			"the current Semantica capture session and write it to " +
			"`.semantica/handoff.md`. The bundle is file-first by design: it " +
			"is never echoed back into the originating session.\n\n" +
			"Usage: `semantica handoff --write`. Bare `semantica handoff` " +
			"prints this help.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !write {
				return cmd.Help()
			}
			return runHandoffWrite(cmd, rootOpts.RepoPath)
		},
	}

	cmd.Flags().BoolVar(&write, "write", false, "Write a handoff bundle to .semantica/handoff.md")
	return cmd
}

// runHandoffWrite is the shared implementation behind both
// `semantica handoff --write` (user-facing) and
// `semantica skills handoff` (hidden, invoked by the
// semantica-handoff SKILL.md body). Keeping the two surfaces on the
// same helper ensures the skill backing command and terminal command
// return identical output and errors.
func runHandoffWrite(cmd *cobra.Command, repoPath string) error {
	svc := handoff.NewService()
	res, err := svc.Write(cmd.Context(), handoff.Input{
		RepoPath: repoPath,
	})
	switch {
	case errors.Is(err, handoff.ErrNoSession):
		return fmt.Errorf("no agent session active for this repo. " +
			"open your agent here, work for a turn, then retry")
	case errors.Is(err, handoff.ErrAmbiguousSession):
		return fmt.Errorf("multiple agent sessions active for this repo. " +
			"close all but one and retry")
	case err != nil:
		return err
	}

	out := cmd.OutOrStdout()
	displayPath := res.Path
	if rel, relErr := filepath.Rel(repoPath, res.Path); relErr == nil && rel != "" {
		displayPath = rel
	}
	_, _ = fmt.Fprintf(out, "Handoff saved to %s.\n", displayPath)
	_, _ = fmt.Fprintln(out, "Exit this session, start a fresh one in this repo, and ask the agent to read the file above.")
	return nil
}
