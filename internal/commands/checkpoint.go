package commands

import (
	"encoding/json"
	"fmt"

	"github.com/semanticash/cli/internal/service"
	"github.com/semanticash/cli/internal/util"
	"github.com/spf13/cobra"
)

func NewCheckpointCmd(rootOpts *RootOptions) *cobra.Command {
	var (
		message string
		auto    bool
		trigger string
		asJSON  bool
	)

	cmd := &cobra.Command{
		Use:   "checkpoint",
		Short: "Create a Semantica checkpoint",
		RunE: func(cmd *cobra.Command, args []string) error {
			kind := service.CheckpointManual
			if auto {
				kind = service.CheckpointAuto
			}

			svc := service.NewCheckpointService()
			var res *service.CreateCheckpointResult
			out := cmd.OutOrStdout()
			var err error
			action := func() {
				res, err = svc.Create(cmd.Context(), service.CreateCheckpointInput{
					RepoPath: rootOpts.RepoPath,
					Kind:     kind,
					Trigger:  trigger,
					Message:  message,
				})
			}
			if spinErr := runWithOptionalSpinner(out, asJSON, "Creating checkpoint...", action); spinErr != nil {
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

			_, _ = fmt.Fprintf(out,
				"Checkpoint created\n  id: %s\n  kind: %s\n  files: %d\n  bytes: %d\n  manifest: %s\n",
				util.ShortID(res.CheckpointID), kind, res.FileCount, res.TotalBytes, res.ManifestHash,
			)
			return nil
		},
	}

	cmd.Flags().StringVarP(&message, "message", "m", "", "Message for this checkpoint (manual checkpoints)")
	cmd.Flags().BoolVar(&auto, "auto", false, "Create an automatic checkpoint")
	cmd.Flags().StringVar(&trigger, "trigger", "", "Trigger label for auto checkpoints (e.g. agent_step, pre_commit, rewind_safety)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")

	return cmd
}
