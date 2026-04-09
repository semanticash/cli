package commands

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/semanticash/cli/internal/service"
	"github.com/semanticash/cli/internal/service/implementations"
	"github.com/semanticash/cli/internal/util"
	"github.com/spf13/cobra"
)

func NewSuggestCmd(rootOpts *RootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "suggest",
		Short: "AI-powered commit and PR suggestions",
	}

	cmd.AddCommand(newSuggestCommitCmd(rootOpts))
	cmd.AddCommand(newSuggestPRCmd(rootOpts))
	cmd.AddCommand(newSuggestImplementationsCmd())

	return cmd
}

func newSuggestCommitCmd(rootOpts *RootOptions) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "commit",
		Short: "Generate a concise commit message from your changes",
		Long: `Analyzes your diff and recent AI session context to suggest
a concise commit message. Most suggestions are a single sentence, but
broader changes may use two short adjacent sentences on the same line. Copies it to the clipboard
automatically.

Requires at least one supported LLM CLI: Claude Code, Cursor, Gemini CLI, or Copilot.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc := service.NewSuggestService()
			var res *service.SuggestResult
			var err error
			out := cmd.OutOrStdout()

			action := func() {
				res, err = svc.Suggest(cmd.Context(), service.SuggestInput{
					RepoPath: rootOpts.RepoPath,
				})
			}
			if spinErr := runWithOptionalSpinner(out, asJSON, "Generating commit message...", action); spinErr != nil {
				action()
			}
			if err != nil {
				return err
			}

			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}

			_, _ = fmt.Fprintln(out, res.Message)

			if err := copyToClipboard(res.Message); err != nil {
				// Non-fatal: print the message anyway, just skip clipboard.
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Could not copy to clipboard:", err)
			} else {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Copied to clipboard.")
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")

	return cmd
}

func newSuggestPRCmd(rootOpts *RootOptions) *cobra.Command {
	var (
		asJSON bool
		copy   bool
		base   string
	)

	cmd := &cobra.Command{
		Use:   "pr",
		Short: "Generate a pull request title and description",
		Long: `Analyzes your branch diff against the base branch and generates
a PR title and body. Aligns with .github/pull_request_template.md if present.

Requires at least one supported LLM CLI: Claude Code, Cursor, Gemini CLI, or Copilot.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc := service.NewSuggestPRService()
			var res *service.SuggestPRResult
			var err error
			out := cmd.OutOrStdout()

			action := func() {
				res, err = svc.SuggestPR(cmd.Context(), service.SuggestPRInput{
					RepoPath: rootOpts.RepoPath,
					Base:     base,
				})
			}
			if spinErr := runWithOptionalSpinner(out, asJSON, "Generating PR description...", action); spinErr != nil {
				action()
			}
			if err != nil {
				return err
			}

			errOut := cmd.ErrOrStderr()

			if res.Dirty {
				_, _ = fmt.Fprintln(errOut, "Warning: working tree has uncommitted changes (not included in suggestion)")
			}

			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}

			_, _ = fmt.Fprintln(out, res.Title)
			_, _ = fmt.Fprintln(out)
			_, _ = fmt.Fprintln(out, res.Body)

			if copy {
				text := res.Title + "\n\n" + res.Body
				if err := copyToClipboard(text); err != nil {
					_, _ = fmt.Fprintln(errOut, "Could not copy to clipboard:", err)
				} else {
					_, _ = fmt.Fprintln(errOut, "Copied to clipboard.")
				}
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&copy, "copy", false, "Copy result to clipboard")
	cmd.Flags().StringVar(&base, "base", "", "Base branch (default: auto-detect)")

	return cmd
}

func newSuggestImplementationsCmd() *cobra.Command {
	var (
		asJSON bool
		apply  bool
	)

	cmd := &cobra.Command{
		Use:   "implementations [implementation_id]",
		Short: "Suggest titles, summaries, and merge candidates for implementations",
		Long: `Without arguments: suggests titles for untitled implementations and
identifies merge candidates across all active/dormant implementations.

With an implementation ID: generates a title and summary
for that specific implementation.

All suggestions are advisory. Use --apply to write the suggested title and summary.`,
		Aliases: []string{"impl"},
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			svc := implementations.NewSuggestService()

			if len(args) == 1 {
				return suggestSingleImplementation(cmd, out, svc, args[0], asJSON, apply)
			}
			return suggestBatchImplementations(cmd, out, svc, asJSON)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&apply, "apply", false, "Apply the suggested title")

	return cmd
}

func suggestSingleImplementation(
	cmd *cobra.Command,
	out interface{ Write([]byte) (int, error) },
	svc *implementations.SuggestService,
	implID string,
	asJSON, apply bool,
) error {
	var res *implementations.SuggestResult
	var err error

	action := func() {
		res, err = svc.SuggestForImplementation(cmd.Context(), implID)
	}
	if spinErr := runWithOptionalSpinner(out, asJSON, "Generating suggestions...", action); spinErr != nil {
		action()
	}
	if err != nil {
		return err
	}

	// Apply the suggestion before output so --json --apply works correctly.
	if apply {
		if err := implementations.ApplySuggestion(cmd.Context(), implID, res.Title, res.Summary); err != nil {
			return fmt.Errorf("apply suggestion: %w", err)
		}
	}

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}

	_, _ = fmt.Fprintf(out, "Suggested title: %s\n", res.Title)
	_, _ = fmt.Fprintf(out, "Suggested summary: %s\n", res.Summary)

	if apply {
		_, _ = fmt.Fprintf(out, "\nTitle and summary applied.\n")
	}

	return nil
}

func suggestBatchImplementations(
	cmd *cobra.Command,
	out interface{ Write([]byte) (int, error) },
	svc *implementations.SuggestService,
	asJSON bool,
) error {
	var res *implementations.SuggestBatchResult
	var err error

	action := func() {
		res, err = svc.SuggestBatch(cmd.Context())
	}
	if spinErr := runWithOptionalSpinner(out, asJSON, "Analyzing implementations...", action); spinErr != nil {
		action()
	}
	if err != nil {
		return err
	}

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}

	if res.Truncated {
		_, _ = fmt.Fprintf(out, "Note: analyzed %d of %d implementations (oldest excluded)\n\n",
			res.Analyzed, res.Total)
	}

	if len(res.Titles) == 0 && len(res.Merges) == 0 {
		_, _ = fmt.Fprintln(out, "No suggestions.")
		return nil
	}

	if len(res.Titles) > 0 {
		_, _ = fmt.Fprintf(out, "Suggested titles:\n")
		for _, t := range res.Titles {
			_, _ = fmt.Fprintf(out, "  %s  %s\n", util.ShortID(t.ImplementationID), t.Title)
		}
	}

	if len(res.Merges) > 0 {
		_, _ = fmt.Fprintf(out, "\nMerge candidates:\n")
		for _, m := range res.Merges {
			_, _ = fmt.Fprintf(out, "  %s + %s  %s\n",
				util.ShortID(m.ImplementationA),
				util.ShortID(m.ImplementationB),
				m.Reason)
		}
	}

	return nil
}

// copyToClipboard copies text to the system clipboard.
func copyToClipboard(text string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("pbcopy")
	case "linux":
		if _, err := exec.LookPath("wl-copy"); err == nil {
			cmd = exec.Command("wl-copy")
		} else if _, err := exec.LookPath("xclip"); err == nil {
			cmd = exec.Command("xclip", "-selection", "clipboard")
		} else if _, err := exec.LookPath("xsel"); err == nil {
			cmd = exec.Command("xsel", "--clipboard", "--input")
		} else {
			return fmt.Errorf("no clipboard tool found (install wl-copy, xclip, or xsel)")
		}
	default:
		return fmt.Errorf("clipboard not supported on %s", runtime.GOOS)
	}
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}
