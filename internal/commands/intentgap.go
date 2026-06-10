package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/semanticash/cli/internal/service"
)

// NewIntentGapCmd is the user-facing command tree for intent-gap
// upload tooling.
func NewIntentGapCmd(rootOpts *RootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "intent-gap",
		Short: "Record and inspect intent-gap uploads",
	}

	cmd.AddCommand(newIntentGapAnalyzeCmd(rootOpts))

	return cmd
}

// newIntentGapAnalyzeCmd runs the same upload path the pre-push hook
// spawns, but in the foreground so the user can see the outcome. Used
// to bridge the gap when a PR was opened after the most recent push,
// and as a manual trigger for diagnostics.
func newIntentGapAnalyzeCmd(rootOpts *RootOptions) *cobra.Command {
	var quiet bool

	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Record an intent-gap upload for the current HEAD",
		Long: `Resolves the open PR for the current branch and records an
intent-gap upload server-side. Today's upload is a transport-only
trigger - no local findings are generated yet; the row signals that
the lifecycle is alive so the rest of the pipeline can be exercised
end-to-end.

Useful when:
  - A PR was opened after the last push and no upload has been
    recorded yet.
  - You want to confirm that the upload path is healthy without
    waiting for the next push.

The command does not block on capture or settings failures: those
surface as a skip result with a human-readable reason. Network or
server errors return a non-zero exit code.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc := service.NewIntentGapUploadService(service.IntentGapUploadDeps{})
			res, err := svc.Run(cmd.Context(), rootOpts.RepoPath)
			if err != nil {
				return err
			}
			return renderAnalyzeResult(cmd.OutOrStdout(), quiet, res)
		},
	}

	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress success/skip output; only error exit codes")

	return cmd
}

// renderAnalyzeResult prints the analyze command's per-status output
// and returns a non-nil error only when the upload failed at the
// HTTP / envelope layer. Extracted so the rendering shape can be
// exercised in tests without standing up a full git repo + stub API.
func renderAnalyzeResult(out interface{ Write(p []byte) (int, error) }, quiet bool, res *service.IntentGapUploadResult) error {
	switch res.Status {
	case service.UploadStatusUploaded:
		if !quiet {
			fmt.Fprintf(out, "Intent-gap upload recorded for PR #%d (upload_id=%s)\n", res.PRNumber, res.UploadID)
		}
		return nil
	case service.UploadStatusDuplicate:
		if !quiet {
			fmt.Fprintf(out, "Intent-gap upload already recorded for PR #%d (upload_id=%s)\n", res.PRNumber, res.UploadID)
		}
		return nil
	case service.UploadStatusSkipped:
		if !quiet {
			fmt.Fprintf(out, "Skipped: %s\n", res.Reason)
		}
		return nil
	case service.UploadStatusError:
		// Non-zero exit lets `semantica intent-gap analyze` participate
		// in scripts the way other diagnostic commands do; the service
		// already wrote the reason to the activity log so doctor can
		// surface it later.
		return fmt.Errorf("intent-gap upload failed to record: %s", res.Reason)
	default:
		return fmt.Errorf("intent-gap analyze: unknown status %q", res.Status)
	}
}
