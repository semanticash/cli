package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/semanticash/cli/internal/llm"
	"github.com/semanticash/cli/internal/service"
	"github.com/semanticash/cli/internal/util"
	"github.com/spf13/cobra"
)

// generateInput is the payload written to the temp file by explain --generate.
type generateInput struct {
	RepoPath     string `json:"repo_path"`
	CheckpointID string `json:"checkpoint_id"`
	CommitHash   string `json:"commit_hash"`
	Prompt       string `json:"prompt"`
}

// NewGeneratePlaybookCmd creates the hidden _generate-playbook command.
// It is spawned as a detached background process by explain --generate.
func NewGeneratePlaybookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "_generate-playbook",
		Hidden: true,
		Args:   cobra.ExactArgs(1), // path to temp input file
		RunE: func(cmd *cobra.Command, args []string) error {
			inputPath := args[0]

			data, err := os.ReadFile(inputPath)
			if err != nil {
				return fmt.Errorf("read input: %w", err)
			}
			// Clean up temp file regardless of outcome.
			defer func() { _ = os.Remove(inputPath) }()

			var in generateInput
			if err := json.Unmarshal(data, &in); err != nil {
				return fmt.Errorf("parse input: %w", err)
			}

			gen, err := llm.Generate(cmd.Context(), in.Prompt)
			if err != nil {
				return err
			}

			svc := service.NewExplainService()
			if err := svc.SaveSummary(cmd.Context(), service.SaveSummaryInput{
				RepoPath:     in.RepoPath,
				CheckpointID: in.CheckpointID,
				Summary: &service.NarrativeResultJSON{
					Title:     gen.Narrative.Title,
					Intent:    gen.Narrative.Intent,
					Outcome:   gen.Narrative.Outcome,
					Learnings: gen.Narrative.Learnings,
					Friction:  gen.Narrative.Friction,
					OpenItems: gen.Narrative.OpenItems,
					Keywords:  gen.Narrative.Keywords,
				},
				Model: gen.Model,
			}); err != nil {
				return err
			}

			semDir := filepath.Join(in.RepoPath, ".semantica")
			util.AppendActivityLog(semDir, "Generated playbook for commit %s (checkpoint %s) by %s (%s)",
				util.ShortID(in.CommitHash), in.CheckpointID, gen.Provider, gen.Model)
			return nil
		},
	}
	return cmd
}
