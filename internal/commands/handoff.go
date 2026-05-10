package commands

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/service/handoff"
	"github.com/spf13/cobra"
)

// NewHandoffCmd builds the `semantica handoff` command tree.
// `--write` assembles the bundle on disk; the `continue`
// subcommand launches a fresh agent session preloaded with the
// bundle path so the user can pick up where they left off without
// retyping the original prompt.
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
			"prints this help. After writing, `semantica handoff continue` " +
			"launches a fresh agent session preloaded with the bundle.",
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
	cmd.AddCommand(newHandoffContinueCmd(rootOpts))
	return cmd
}

// newHandoffContinueCmd wires `semantica handoff continue`. It
// reads `.semantica/handoff.md`, picks an agent (the original
// session's provider by default, --agent override otherwise),
// and either execs the agent's CLI with a fixed starter prompt
// or prints the command for the user to run manually.
func newHandoffContinueCmd(rootOpts *RootOptions) *cobra.Command {
	var agent string
	var printOnly bool
	cmd := &cobra.Command{
		Use:   "continue",
		Short: "Launch a fresh agent session preloaded with the handoff bundle",
		Long: "Read the handoff bundle written by `semantica handoff --write` " +
			"and either spawn the matching agent's CLI with a starter prompt " +
			"or print the equivalent command for manual launch.\n\n" +
			"Defaults to the agent that produced the bundle. Override with " +
			"--agent <name> to switch agents during handoff. Pass --print to " +
			"see the command without spawning anything.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHandoffContinue(cmd, rootOpts.RepoPath, agent, printOnly)
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "agent to launch (default: same as the original session's provider)")
	cmd.Flags().BoolVar(&printOnly, "print", false, "print the launch command instead of spawning the agent")
	return cmd
}

// continueExecutor is the test seam for launching the agent binary.
// Unix replaces the current process with the agent. Windows returns a
// clear error if this path is reached; users can still use --print.
var continueExecutor = defaultExecutor

func runHandoffContinue(cmd *cobra.Command, repoPath, agentOverride string, printOnly bool) error {
	repo, err := git.OpenRepo(repoPath)
	if err != nil {
		return fmt.Errorf("open repo: %w", err)
	}
	repoRoot := repo.Root()

	bundlePath := filepath.Join(repoRoot, ".semantica", handoff.HandoffFilename)
	body, err := os.ReadFile(bundlePath)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w at %s; run `semantica handoff --write` first",
			handoff.ErrBundleMissing, bundlePath)
	}
	if err != nil {
		return fmt.Errorf("read handoff bundle: %w", err)
	}

	provider := agentOverride
	if provider == "" {
		provider = handoff.ProviderFromBundle(body)
	}
	if provider == "" {
		return fmt.Errorf("could not determine which agent to launch from %s; pass --agent <name>",
			bundlePath)
	}

	spec, err := handoff.BuildLaunchSpec(provider, bundlePath, printOnly)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if !spec.Spawn {
		_, _ = fmt.Fprintln(out, spec.Message)
		return nil
	}

	_, _ = fmt.Fprintln(out, spec.Message)
	return continueExecutor(repoRoot, spec)
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
