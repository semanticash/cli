package commands

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/service/implementations"
	"github.com/semanticash/cli/internal/store/impldb"
	"github.com/spf13/cobra"
)

// NewAutoImplementationSummaryCmd creates the hidden _auto-implementation-summary
// command. It is spawned as a detached background process by the worker when
// automations.implementation_summary is enabled and the implementation spans 2+ repos.
func NewAutoImplementationSummaryCmd() *cobra.Command {
	var implID string

	cmd := &cobra.Command{
		Use:    "_auto-implementation-summary",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			base, err := broker.GlobalBase()
			if err != nil {
				return fmt.Errorf("auto-impl-summary: resolve base: %w", err)
			}
			implPath := filepath.Join(base, "implementations.db")

			// The worker already created and migrated the DB before spawning
			// this process. Use OpenNoMigrate to skip redundant migration.
			h, err := impldb.OpenNoMigrate(ctx, implPath, impldb.DefaultOpenOptions())
			if err != nil {
				return fmt.Errorf("auto-impl-summary: open db: %w", err)
			}
			defer func() { _ = impldb.Close(h) }()

			// Clear in-progress marker on any exit path.
			defer implementations.ClearGenerationInProgress(ctx, h, implID)

			// Re-check skip conditions (safe against races with another worker).
			// Skip the in-progress check - this is the job that owns the marker.
			if ok, reason := implementations.ShouldAutoSummarize(ctx, h, implID, implementations.ShouldAutoSummarizeOpts{
				SkipInProgressCheck: true,
			}); !ok {
				_, _ = fmt.Fprintf(os.Stderr, "auto-impl-summary: skipping %s: %s\n", implID[:8], reason)
				return nil
			}

			// Generate suggestions via LLM.
			svc := implementations.NewSuggestService()
			res, err := svc.SuggestForImplementation(ctx, implID)
			if err != nil {
				return fmt.Errorf("auto-impl-summary: suggest: %w", err)
			}

			// Get current repo count for freshness tracking.
			repoCount, err := h.Queries.CountReposForImplementation(ctx, implID)
			if err != nil {
				return fmt.Errorf("auto-impl-summary: count repos: %w", err)
			}

			// Apply with auto source.
			if err := implementations.ApplySuggestion(ctx, implementations.ApplySuggestionInput{
				ImplementationID: implID,
				Title:            res.Title,
				Summary:          res.Summary,
				Source:           implementations.SourceAuto,
				RepoCount:        int(repoCount),
			}); err != nil {
				return fmt.Errorf("auto-impl-summary: apply: %w", err)
			}

			_, _ = fmt.Fprintf(os.Stderr, "auto-impl-summary: generated title for %s: %q (%d repos)\n",
				implID[:8], res.Title, repoCount)
			return nil
		},
	}

	cmd.Flags().StringVar(&implID, "impl", "", "Implementation ID")
	_ = cmd.MarkFlagRequired("impl")

	return cmd
}
