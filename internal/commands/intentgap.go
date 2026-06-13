package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/semanticash/cli/internal/service"
)

// NewIntentGapCmd creates the intent-gap command group.
func NewIntentGapCmd(rootOpts *RootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "intent-gap",
		Short: "Run and inspect intent-gap analysis",
	}

	cmd.AddCommand(newIntentGapAnalyzeCmd(rootOpts))

	return cmd
}

// newIntentGapAnalyzeCmd runs the pre-push analysis path in the foreground.
func newIntentGapAnalyzeCmd(rootOpts *RootOptions) *cobra.Command {
	var base string
	var quiet bool

	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Analyze the current PR with your installed AI agent and record findings",
		Long: `Resolves the open PR for the current branch, assembles a bundle
of commits and the cumulative diff, runs intent-gap analysis using your
installed AI CLI fallback chain (Claude Code, Codex, Cursor, Gemini CLI,
GitHub Copilot CLI, or Kiro CLI), and uploads the findings to the
connected workspace.

Useful when:
  - A PR was opened after the last push and no analysis has been recorded yet.
  - You want to re-run analysis without waiting for the next push.
  - The repository uses a non-standard default branch; pass --base explicitly.

Skip conditions (exit 0, reason in output):
  - Semantica or intent-gap not enabled in this repo.
  - Repo not connected to a workspace.
  - No open PR for the current branch (or more than one).
  - No AI CLI installed.

Non-zero exit:
  - The analyzer ran but failed (LLM unavailable mid-flight, output
    could not be parsed, output failed schema validation). An errored
    row is still recorded server-side so doctor and the dashboard see
    the failure.
  - The wire upload itself failed (network / server error).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc := service.NewIntentGapUploadService(service.IntentGapUploadDeps{BaseRef: base})
			res, err := svc.Run(cmd.Context(), rootOpts.RepoPath)
			if err != nil {
				return err
			}
			return renderAnalyzeResult(cmd.OutOrStdout(), quiet, res)
		},
	}

	cmd.Flags().StringVar(&base, "base", "", "Base branch or ref (default: auto-detect)")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress success/skip output; only error exit codes")

	return cmd
}

// renderAnalyzeResult renders the upload outcome. Analyzer and transport
// failures return errors even when an errored row was recorded successfully.
func renderAnalyzeResult(out interface{ Write(p []byte) (int, error) }, quiet bool, res *service.IntentGapUploadResult) error {
	// Report analysis failure separately from successful error recording.
	if res.Analysis == service.AnalysisErrored && res.Status != service.UploadStatusError {
		if !quiet {
			_, _ = fmt.Fprintf(out, "Intent-gap analysis errored (%s); errored row recorded for PR #%d\n",
				res.AnalysisReason, res.PRNumber)
		}
		return fmt.Errorf("intent-gap analysis errored: %s", res.AnalysisReason)
	}

	switch res.Status {
	case service.UploadStatusUploaded:
		if !quiet {
			_, _ = fmt.Fprintf(out, "Intent-gap analysis recorded for PR #%d (upload_id=%s)\n", res.PRNumber, res.UploadID)
		}
		return nil
	case service.UploadStatusDuplicate:
		if !quiet {
			_, _ = fmt.Fprintf(out, "Intent-gap analysis already recorded for PR #%d (upload_id=%s)\n", res.PRNumber, res.UploadID)
		}
		return nil
	case service.UploadStatusSkipped:
		if !quiet {
			_, _ = fmt.Fprintf(out, "Skipped: %s\n", res.Reason)
		}
		return nil
	case service.UploadStatusError:
		// Preserve a non-zero exit for scripts; details are in the activity log.
		return fmt.Errorf("intent-gap upload failed to record: %s", res.Reason)
	default:
		return fmt.Errorf("intent-gap analyze: unknown status %q", res.Status)
	}
}
