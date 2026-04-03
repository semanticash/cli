package commands

import (
	"encoding/json"
	"fmt"

	"github.com/semanticash/cli/internal/service"
	"github.com/spf13/cobra"
)

func NewDisableCmd(rootOpts *RootOptions) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "disable",
		Short: "Disables Semantica hooks in the current git repository",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			svc := service.NewDisableService()
			res, err := svc.Disable(ctx, rootOpts.RepoPath)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()

			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}

			_, _ = fmt.Fprintln(out, "Semantica disabled. Hooks will be no-ops. Run `semantica enable` to re-enable.")
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")

	return cmd
}
