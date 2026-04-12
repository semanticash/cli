package commands

import (
	"encoding/json"
	"fmt"

	"github.com/semanticash/cli/internal/service"
	"github.com/semanticash/cli/internal/util"
	"github.com/spf13/cobra"
)

func NewBlameCmd(rootOpts *RootOptions) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "blame [ref]",
		Short: "Show AI attribution for a commit or checkpoint",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := resolveRef(cmd.Context(), rootOpts.RepoPath, args)
			if err != nil {
				return err
			}

			svc := service.NewAttributionService()
			var res *service.AttributionResult
			out := cmd.OutOrStdout()
			action := func() {
				res, err = svc.Blame(cmd.Context(), service.BlameInput{
					RepoPath: rootOpts.RepoPath,
					Ref:      ref,
				})
			}
			if spinErr := runWithOptionalSpinner(out, asJSON, "Computing attribution...", action); spinErr != nil {
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

			if res.CommitHash != "" {
				_, _ = fmt.Fprintf(out, "Commit:       %s\n", res.CommitHash)
			}
			if res.CheckpointID != "" {
				_, _ = fmt.Fprintf(out, "Checkpoint:   %s\n", util.ShortID(res.CheckpointID))
			}
			_, _ = fmt.Fprintf(out, "AI Exact:     %d lines\n", res.AIExactLines)
			_, _ = fmt.Fprintf(out, "AI Formatted: %d lines\n", res.AIFormattedLines)
			_, _ = fmt.Fprintf(out, "AI Modified:  %d lines\n", res.AIModifiedLines)
			_, _ = fmt.Fprintf(out, "Human:        %d lines\n", res.HumanLines)
			_, _ = fmt.Fprintf(out, "Total:        %d lines\n", res.TotalLines)
			_, _ = fmt.Fprintf(out, "AI %%:         %.1f%%\n", res.AIPercentage)
			_, _ = fmt.Fprintf(out, "AI touched:   %d / %d files\n", res.FilesAITouched, res.FilesTotal)

			nCreated := len(res.FilesCreated)
			nEdited := len(res.FilesEdited)
			nDeleted := len(res.FilesDeleted)

			if nCreated > 0 || nEdited > 0 || nDeleted > 0 {
				_, _ = fmt.Fprintf(out, "Created:      %d\n", nCreated)
				_, _ = fmt.Fprintf(out, "Edited:       %d\n", nEdited)
				_, _ = fmt.Fprintf(out, "Deleted:      %d\n", nDeleted)
			}

			if res.Diagnostics.Note != "" && (res.AIPercentage == 0 || res.Diagnostics.NormalizedMatches > 0 || res.Diagnostics.ModifiedMatches > 0) {
				_, _ = fmt.Fprintf(out, "Note:         %s\n", res.Diagnostics.Note)
			}
			_, _ = fmt.Fprintf(out, "Events:       %d considered, %d assistant, %d with tools, %d payloads loaded\n",
				res.Diagnostics.EventsConsidered, res.Diagnostics.EventsAssistant,
				res.Diagnostics.AIToolEvents, res.Diagnostics.PayloadsLoaded)
			if res.AILines > 0 {
				_, _ = fmt.Fprintf(out, "Matching:     %d exact, %d normalized, %d modified\n",
					res.Diagnostics.ExactMatches, res.Diagnostics.NormalizedMatches, res.Diagnostics.ModifiedMatches)
			}

			// Factual notes about weaker attribution methods used.
			var notes []string
			if res.FallbackCount > 0 {
				notes = append(notes, fmt.Sprintf("%d file(s) attributed using weaker fallback signals.", res.FallbackCount))
			}
			for _, f := range res.Files {
				if f.EvidenceClass == "carry_forward" {
					notes = append(notes, "Attribution includes historical carry-forward.")
					break
				}
			}
			for _, f := range res.Files {
				if f.EvidenceClass == "deletion" {
					notes = append(notes, "Some file attribution is inferred from deletion events.")
					break
				}
			}
			if len(notes) > 0 {
				_, _ = fmt.Fprintln(out, "Notes:")
				for _, n := range notes {
					_, _ = fmt.Fprintf(out, "  %s\n", n)
				}
			}

			if nCreated > 0 {
				_, _ = fmt.Fprintln(out)
				_, _ = fmt.Fprintln(out, "Files created:")
				for _, f := range res.FilesCreated {
					label := "human"
					if f.AI {
						label = "ai"
					}
					_, _ = fmt.Fprintf(out, "  + %-60s [%s]\n", f.Path, label)
				}
			}
			if nEdited > 0 {
				_, _ = fmt.Fprintln(out)
				_, _ = fmt.Fprintln(out, "Files edited:")
				for _, f := range res.FilesEdited {
					label := "human"
					if f.AI {
						label = "ai"
					}
					_, _ = fmt.Fprintf(out, "  ~ %-60s [%s]\n", f.Path, label)
				}
			}
			if nDeleted > 0 {
				_, _ = fmt.Fprintln(out)
				_, _ = fmt.Fprintln(out, "Files deleted:")
				for _, f := range res.FilesDeleted {
					label := "human"
					if f.AI {
						label = "ai"
					}
					_, _ = fmt.Fprintf(out, "  - %-60s [%s]\n", f.Path, label)
				}
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Output full result as JSON (includes per-file breakdown)")

	return cmd
}
