package commands

import (
	"encoding/json"
	"fmt"

	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/service"
	"github.com/semanticash/cli/internal/util"
	"github.com/spf13/cobra"
)

func NewRewindCmd(rootOpts *RootOptions) *cobra.Command {
	var (
		noSafety bool
		exact    bool
		asJSON   bool
	)

	cmd := &cobra.Command{
		Use:   "rewind [checkpoint_id]",
		Short: "Restore the working tree to a checkpoint",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			checkpointID, err := resolveRef(cmd.Context(), rootOpts.RepoPath, args)
			if err != nil {
				return err
			}

			repo, err := git.OpenRepo(rootOpts.RepoPath)
			if err != nil {
				return err
			}

			dirty, err := repo.IsDirty(cmd.Context())
			if err != nil {
				return err
			}

			if dirty && noSafety {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Warning: working tree has uncommitted changes and --no-safety was specified.")
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Changes may be lost.")
				_, _ = fmt.Fprintln(cmd.OutOrStdout())
			}

			svc := service.NewRewindService()
			var res *service.RewindResult
			out := cmd.OutOrStdout()
			action := func() {
				res, err = svc.Rewind(cmd.Context(), service.RewindInput{
					RepoPath:     rootOpts.RepoPath,
					CheckpointID: checkpointID,
					NoSafety:     noSafety,
					Exact:        exact,
				})
			}
			if spinErr := runWithOptionalSpinner(out, asJSON, "Restoring files...", action); spinErr != nil {
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

			_, _ = fmt.Fprintf(out, "Restored to checkpoint %s\n", util.ShortID(res.CheckpointID))
			if res.SafetyCheckpointID != "" {
				_, _ = fmt.Fprintf(out, "Safety checkpoint: %s\n", util.ShortID(res.SafetyCheckpointID))
			}
			_, _ = fmt.Fprintf(out, "Files restored: %d\n", res.FilesRestored)
			if exact {
				_, _ = fmt.Fprintf(out, "Files deleted: %d\n", res.FilesDeleted)
			} else {
				_, _ = fmt.Fprintln(out, "\nNote: extra untracked files were not removed. Use --exact to fully match the checkpoint.")
			}
			_, _ = fmt.Fprintln(out, "Review changes with: git status / git diff")
			return nil
		},
	}

	cmd.Flags().BoolVar(&noSafety, "no-safety", false, "Do not create a safety checkpoint before rewinding (dangerous)")
	cmd.Flags().BoolVar(&exact, "exact", false, "Also delete files not present in the checkpoint file set")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")

	return cmd
}
