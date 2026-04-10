package commands

import (
	"encoding/json"
	"fmt"

	"github.com/semanticash/cli/internal/service"
	"github.com/spf13/cobra"
)

func NewTidyCmd(rootOpts *RootOptions) *cobra.Command {
	var (
		asJSON bool
		apply  bool
	)

	cmd := &cobra.Command{
		Use:   "tidy",
		Short: "Clean up stale local state",
		Long: `Performs safe housekeeping on transient Semantica state.

Cleans up:
  - Stale broker registry entries (repos whose .semantica was deleted)
  - Abandoned capture state files (older than 24h with missing transcript)
  - Pending checkpoints that never completed (marked as failed)
  - Orphan playbook FTS index rows

Does NOT delete checkpoints, sessions, events, or blob objects.

By default runs in dry-run mode. Use --apply to perform changes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc := service.NewTidyService()
			res, err := svc.Tidy(cmd.Context(), service.TidyInput{
				RepoPath: rootOpts.RepoPath,
				Apply:    apply,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()

			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}

			if res.DryRun {
				_, _ = fmt.Fprintln(out, "Dry run (use --apply to perform changes)")
				_, _ = fmt.Fprintln(out)
			}

			total := res.BrokerEntriesPruned + res.CaptureStatesRemoved +
				res.CheckpointsMarked + res.ImplStale + res.ImplConflicts +
				res.ImplFailedObs + res.ImplObsPruned

			if total == 0 {
				_, _ = fmt.Fprintln(out, "Nothing to clean up.")
				return nil
			}

			// Group actions by category.
			categories := []struct {
				name  string
				cat   string
				count int
				verb  string
			}{
				{"Broker entries", "broker", res.BrokerEntriesPruned, "pruned"},
				{"Capture states", "capture", res.CaptureStatesRemoved, "removed"},
				{"Pending checkpoints", "checkpoint", res.CheckpointsMarked, "marked failed"},
				{"Implementations", "implementation", res.ImplStale + res.ImplConflicts + res.ImplFailedObs + res.ImplObsPruned, "findings"},
			}

			for _, c := range categories {
				if c.count == 0 {
					continue
				}
				_, _ = fmt.Fprintf(out, "%s: %d %s\n", c.name, c.count, c.verb)
				for _, a := range res.Actions {
					if a.Category == c.cat {
						_, _ = fmt.Fprintf(out, "  %s - %s\n", a.ID, a.Detail)
					}
				}
			}

				if res.Errors > 0 {
					_, _ = fmt.Fprintf(out, "\n%d action(s) failed\n", res.Errors)
				}

			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&apply, "apply", false, "Perform changes (default is dry-run)")

	return cmd
}
