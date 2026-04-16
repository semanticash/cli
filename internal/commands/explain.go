package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/platform"
	"github.com/semanticash/cli/internal/llm"
	"github.com/semanticash/cli/internal/service"
	"github.com/semanticash/cli/internal/util"
	"github.com/spf13/cobra"
)

func NewExplainCmd(rootOpts *RootOptions) *cobra.Command {
	var (
		asJSON   bool
		generate bool
		force    bool
	)

	cmd := &cobra.Command{
		Use:   "explain [commit]",
		Short: "Explain what happened in a commit",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := resolveRef(cmd.Context(), rootOpts.RepoPath, args)
			if aborted, rerr := handleAbort(cmd.OutOrStdout(), err); aborted || rerr != nil {
				return rerr
			}

			svc := service.NewExplainService()
			var res *service.ExplainResult
			out := cmd.OutOrStdout()
			action := func() {
				res, err = svc.Explain(cmd.Context(), service.ExplainInput{
					RepoPath: rootOpts.RepoPath,
					Ref:      ref,
				})
			}
			if spinErr := runWithOptionalSpinner(out, asJSON, "Analyzing commit...", action); spinErr != nil {
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

			// Header
			subject := res.CommitSubject
			if subject == "" {
				subject = "(no subject)"
			}
			_, _ = fmt.Fprintf(out, "Commit %s - %s\n", util.ShortID(res.CommitHash), subject)
			_, _ = fmt.Fprintf(out, "%d files changed (+%d/-%d)\n", res.FilesChanged, res.LinesAdded, res.LinesDeleted)
			_, _ = fmt.Fprintln(out)

			// AI involvement
			_, _ = fmt.Fprintln(out, "AI involvement:")
			if res.SessionCount > 0 {
				_, _ = fmt.Fprintf(out, "  %d sessions (%d root, %d subagents)\n", res.SessionCount, res.RootSessions, res.Subagents)
			} else {
				_, _ = fmt.Fprintln(out, "  No agent sessions linked to this commit")
			}
			_, _ = fmt.Fprintf(out, "  %.1f%% AI-Attributed (%d AI / %d human)\n", res.AIPercentage, res.AILines, res.HumanLines)
			_, _ = fmt.Fprintf(out, "  %d of %d files contain AI-produced lines\n", res.FilesWithAI, res.FilesChanged)
			_, _ = fmt.Fprintln(out)

			// Session breakdown
			if len(res.Sessions) > 0 {
				_, _ = fmt.Fprintln(out, "Session breakdown:")
				for _, sess := range res.Sessions {
					id := util.ShortID(sess.SessionID)
					if sess.IsSubagent {
						_, _ = fmt.Fprintf(out, "  Subagent %s: %d steps, %d tool calls\n",
							id, sess.StepCount, sess.ToolCallCount)
					} else {
						tok := fmt.Sprintf("tok %s/%s",
							service.CompactTokens(sess.TokensIn),
							service.CompactTokens(sess.TokensOut))
						if sess.TokensCached > 0 {
							tok += fmt.Sprintf(" (+%s cached)", service.CompactTokens(sess.TokensCached))
						}
						_, _ = fmt.Fprintf(out, "  %s (%s): %d steps, %d tool calls, %s\n",
							id, sess.Provider,
							sess.StepCount, sess.ToolCallCount,
							tok)
					}
				}
				_, _ = fmt.Fprintln(out)
			}

			// Top edited files
			if len(res.TopFiles) > 0 {
				_, _ = fmt.Fprintln(out, "Top edited files:")
				for _, f := range res.TopFiles {
					_, _ = fmt.Fprintf(out, "  %s (+%d/-%d)\n", f.Path, f.Added, f.Deleted)
				}
			}

			// Show cached summary if available.
			if res.Summary != nil {
				renderSummary(out, res.Summary)
			}

			// Handle --generate: spawn background process if needed.
			if generate && (force || res.Summary == nil) {
				if err := spawnGenerateBackground(cmd, rootOpts.RepoPath, res); err != nil {
					return err
				}
				shortRef := util.ShortID(res.CommitHash)
				_, _ = fmt.Fprintln(out)
				_, _ = fmt.Fprintf(out, "Generating playbook for commit %s in the background...\n", shortRef)
				_, _ = fmt.Fprintf(out, "Run `semantica explain %s` again later to see the result.\n", shortRef)
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&generate, "generate", false, "Generate a narrative explanation using an LLM")
	cmd.Flags().BoolVar(&force, "force", false, "Force regeneration of the playbook (use with --generate)")

	return cmd
}

func renderSummary(out io.Writer, s *service.NarrativeResultJSON) {
	_, _ = fmt.Fprintln(out)
	if s.Title != "" {
		_, _ = fmt.Fprintf(out, "[Playbook] %s\n", s.Title)
	} else {
		_, _ = fmt.Fprintln(out, "[Playbook]")
	}
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintf(out, "Intent:\n%s\n", s.Intent)
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintf(out, "Outcome:\n%s\n", s.Outcome)
	if len(s.Learnings) > 0 {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, "Learnings:")
		for _, l := range s.Learnings {
			_, _ = fmt.Fprintf(out, "  - %s\n", l)
		}
	}
	if len(s.Friction) > 0 {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, "Friction:")
		for _, f := range s.Friction {
			_, _ = fmt.Fprintf(out, "  - %s\n", f)
		}
	}
	if len(s.OpenItems) > 0 {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, "Open items:")
		for _, o := range s.OpenItems {
			_, _ = fmt.Fprintf(out, "  - %s\n", o)
		}
	}
}

// spawnGenerateBackground builds the LLM prompt and spawns a detached
// _generate-playbook process that calls the LLM and persists the result.
func spawnGenerateBackground(cmd *cobra.Command, repoPath string, res *service.ExplainResult) error {
	repo, err := git.OpenRepo(repoPath)
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}
	diffBytes, err := repo.DiffForCommit(cmd.Context(), res.CommitHash)
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}

	ectx := buildExplainContext(res)
	entries := buildTranscriptEntries(res)
	prompt := llm.BuildUserPrompt(res.CommitHash, res.CommitSubject, ectx, entries, string(diffBytes))

	// Write input payload to temp file for the background process.
	input := generateInput{
		RepoPath:     repoPath,
		CheckpointID: res.CheckpointID,
		CommitHash:   res.CommitHash,
		Prompt:       prompt,
	}
	data, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("generate: marshal input: %w", err)
	}

	tmpFile, err := os.CreateTemp("", "semantica-generate-*.json")
	if err != nil {
		return fmt.Errorf("generate: create temp file: %w", err)
	}
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
		return fmt.Errorf("generate: write temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpFile.Name())
		return fmt.Errorf("generate: close temp file: %w", err)
	}

	// Re-exec ourselves with the hidden _generate-playbook command.
	self, err := os.Executable()
	if err != nil {
		_ = os.Remove(tmpFile.Name())
		return fmt.Errorf("generate: find executable: %w", err)
	}

	args := []string{"_generate-playbook", tmpFile.Name()}
	if repoPath != "" {
		args = append([]string{"--repo", repoPath}, args...)
	}

	proc := exec.Command(self, args...)
	platform.SetProcessGroup(proc)
	proc.Stdout = nil
	proc.Stderr = nil
	proc.Stdin = nil

	if err := proc.Start(); err != nil {
		_ = os.Remove(tmpFile.Name())
		return fmt.Errorf("generate: start background process: %w", err)
	}

	// Detach - don't wait for the child.
	go func() { _ = proc.Wait() }()

	return nil
}

// buildExplainContext maps an ExplainResult to the LLM's ExplainContext.
func buildExplainContext(res *service.ExplainResult) llm.ExplainContext {
	ectx := llm.ExplainContext{
		FilesChanged: res.FilesChanged,
		LinesAdded:   res.LinesAdded,
		LinesDeleted: res.LinesDeleted,
		AIPercentage: res.AIPercentage,
		AILines:      res.AILines,
		HumanLines:   res.HumanLines,
		SessionCount: res.SessionCount,
		RootSessions: res.RootSessions,
		Subagents:    res.Subagents,
	}
	for _, f := range res.TopFiles {
		ectx.TopFiles = append(ectx.TopFiles, struct {
			Path       string  `json:"path"`
			Added      int     `json:"added"`
			Deleted    int     `json:"deleted"`
			TotalLines int     `json:"total_lines"`
			AILines    int     `json:"ai_lines"`
			HumanLines int     `json:"human_lines"`
			AIPercent  float64 `json:"ai_percentage"`
		}{
			Path: f.Path, Added: f.Added, Deleted: f.Deleted,
			TotalLines: f.TotalLines, AILines: f.AILines,
			HumanLines: f.HumanLines, AIPercent: f.AIPercent,
		})
	}
	return ectx
}

// buildTranscriptEntries maps ExplainResult transcript to LLM entries.
func buildTranscriptEntries(res *service.ExplainResult) []llm.TranscriptEntry {
	var entries []llm.TranscriptEntry
	for _, t := range res.Transcript {
		entries = append(entries, llm.TranscriptEntry{
			Role:     t.Role,
			Summary:  t.Summary,
			ToolName: t.ToolName,
			FilePath: t.FilePath,
		})
	}
	return entries
}
