package commands

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/semanticash/cli/internal/service"
	"github.com/semanticash/cli/internal/util"
)

func NewShowCmd(rootOpts *RootOptions) *cobra.Command {
	var (
		asJSON  bool
		asJSONL bool
	)

	cmd := &cobra.Command{
		Use:   "show [checkpoint_id]",
		Short: "Show details of a checkpoint",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if asJSON && asJSONL {
				return fmt.Errorf("flags --json and --jsonl are mutually exclusive")
			}

			checkpointID, err := resolveRef(cmd.Context(), rootOpts.RepoPath, args)
			if aborted, rerr := handleAbort(cmd.OutOrStdout(), err); aborted || rerr != nil {
				return rerr
			}

			svc := service.NewShowService()
			res, err := svc.ShowCheckpoint(cmd.Context(), service.ShowCheckpointInput{
				RepoPath:     rootOpts.RepoPath,
				CheckpointID: checkpointID,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()

			// JSON output: single object
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}

			// JSONL output: metadata line then files lines (still valid JSONL)
			if asJSONL {
				enc := json.NewEncoder(out)

				// Emit metadata (without huge file list), then emit each file as its own line
				meta := *res
				meta.Files = nil
				meta.FileCount = len(res.Files)
				if err := enc.Encode(meta); err != nil {
					return err
				}

				for _, f := range res.Files {
					if err := enc.Encode(f); err != nil {
						return err
					}
				}
				return nil
			}

			// Human output
			_, _ = fmt.Fprintf(out, "Checkpoint: %s\n", util.ShortID(res.CheckpointID))
			if res.CommitHash != "" {
				_, _ = fmt.Fprintf(out, "Commit:     %s\n", res.CommitHash)
			}
			_, _ = fmt.Fprintf(out, "Created:    %s\n", res.CreatedAt)
			_, _ = fmt.Fprintf(out, "Kind:       %s\n", res.Kind)

			trigger := strings.TrimSpace(res.Trigger)
			if trigger == "" {
				trigger = "-"
			}
			_, _ = fmt.Fprintf(out, "Trigger:    %s\n", trigger)

			if res.Bytes != nil {
				_, _ = fmt.Fprintf(out, "Bytes:      %d\n", *res.Bytes)
			} else {
				_, _ = fmt.Fprintf(out, "Bytes:      -\n")
			}

			if strings.TrimSpace(res.Message) != "" {
				_, _ = fmt.Fprintf(out, "Message:    %s\n", strings.TrimSpace(res.Message))
			}

			_, _ = fmt.Fprintf(out, "Manifest:   %s\n", res.ManifestHash)
			_, _ = fmt.Fprintf(out, "Files:      %d\n", res.FileCount)

			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&asJSONL, "jsonl", false, "Output as JSONL (metadata + one file per line)")

	return cmd
}
