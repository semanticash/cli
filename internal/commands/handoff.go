package commands

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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

	// Interactive path: ask whether to launch a fresh agent right now.
	// Pressing Enter (or typing y/yes) chains directly into `handoff
	// continue`, which on success replaces this process with the
	// agent CLI. Non-interactive callers (skill body, pipes, CI) skip
	// the prompt and get the manual hint below.
	if confirmContinueNow(cmd) {
		return runHandoffContinue(cmd, repoPath, "", false)
	}

	_, _ = fmt.Fprintln(out, "Run `semantica handoff continue` when you're ready.")
	return nil
}

// isTerminalWriterFn is the stdout-TTY gate, exposed as a package
// var so tests can simulate a TTY-like stdout without owning a
// real pty. The stdin gate intentionally stays in-line: it has to
// observe the actual reader the test passes, since the whole point
// of the stdin gate is to refuse to prompt against non-TTY
// readers (pipes, bytes.Buffer) that would never deliver input.
var isTerminalWriterFn = isTerminalWriter

// confirmContinueNow prompts the user to chain directly into
// `handoff continue`. Returns true only when both stdout and stdin
// are TTYs (so we don't block on a piped stdin) and the user
// accepts. Treats Enter alone as accept since the prompt's default
// is Y; Ctrl-D is treated as decline by readContinueAnswer.
func confirmContinueNow(cmd *cobra.Command) bool {
	out := cmd.OutOrStdout()
	in := cmd.InOrStdin()
	if !isTerminalWriterFn(out) {
		return false
	}
	inFile, ok := in.(*os.File)
	if !ok || !isInteractiveTerminal(inFile) {
		return false
	}

	_, _ = fmt.Fprint(out, "Continue in a new session now? [Y/n] ")
	return readContinueAnswer(in)
}

// readContinueAnswer reads one line from r and reports whether it
// is an accept. A bare newline (the user pressed Enter) counts as
// accept so the prompt's [Y/n] default holds. Ctrl-D with nothing
// typed (empty result + EOF) is treated as decline. Ctrl-D is the
// universal terminal cancel and must not silently launch the
// follow-on agent. Non-EOF read errors also decline. Exposed at
// package scope so tests can pin the parsing rules without needing
// a real TTY.
func readContinueAnswer(r io.Reader) bool {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	// Ctrl-D with no input yields line == "". Anything the user
	// actually typed, including a bare Enter ("\n"), gives a
	// non-empty line. Only the latter should be treated as the
	// [Y/n] default-accept.
	if line == "" {
		return false
	}
	answer := strings.TrimSpace(strings.ToLower(line))
	return answer == "" || answer == "y" || answer == "yes"
}
