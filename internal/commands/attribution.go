package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/semanticash/cli/internal/service"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	"github.com/semanticash/cli/internal/util"
	"github.com/spf13/cobra"
)

func NewBlameCmd(rootOpts *RootOptions) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "blame [ref]",
		Short: "Show AI attribution for a commit or lineage record",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := resolveRef(cmd.Context(), rootOpts.RepoPath, args)
			if aborted, rerr := handleAbort(cmd.OutOrStdout(), err); aborted || rerr != nil {
				return rerr
			}

			svc := service.NewAttributionServiceWithOpenOptions(sqlstore.UserFacingOpenOptions())
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
			incomplete := res.Capture != nil && res.Capture.Status != "complete"
			writeAttributionCounts(out, res)
			_, _ = fmt.Fprintf(out, "AI touched:   %d / %d files\n", res.FilesAITouched, res.FilesTotal)
			if agents := attributionAgentLabels(res.ProviderDetails); len(agents) > 0 {
				_, _ = fmt.Fprintf(out, "AI agent(s):  %s\n", strings.Join(agents, ", "))
			}

			nCreated := len(res.FilesCreated)
			nEdited := len(res.FilesEdited)
			nDeleted := len(res.FilesDeleted)

			if nCreated > 0 || nEdited > 0 || nDeleted > 0 {
				_, _ = fmt.Fprintf(out, "Created:      %d\n", nCreated)
				_, _ = fmt.Fprintf(out, "Edited:       %d\n", nEdited)
				_, _ = fmt.Fprintf(out, "Deleted:      %d\n", nDeleted)
			}

			_, _ = fmt.Fprintf(out, "Events:       %d considered, %d assistant, %d with tools, %d payloads loaded\n",
				res.Diagnostics.EventsConsidered, res.Diagnostics.EventsAssistant,
				res.Diagnostics.AIToolEvents, res.Diagnostics.PayloadsLoaded)
			if res.AILines > 0 {
				_, _ = fmt.Fprintf(out, "Matching:     %d exact, %d normalized, %d modified\n",
					res.Diagnostics.ExactMatches, res.Diagnostics.NormalizedMatches, res.Diagnostics.ModifiedMatches)
			}

			writeAttributionNotes(out, res)

			// AI files include the provider when known. Older or
			// incomplete records fall back to the plain [ai] tag.
			fileTag := func(f service.FileChange) string {
				if !f.AI {
					if incomplete {
						return "unattributed"
					}
					return "human"
				}
				if len(f.Providers) > 0 {
					return "ai:" + strings.Join(f.Providers, ",")
				}
				return "ai"
			}

			if nCreated > 0 {
				_, _ = fmt.Fprintln(out)
				_, _ = fmt.Fprintln(out, "Files created:")
				for _, f := range res.FilesCreated {
					_, _ = fmt.Fprintf(out, "  + %-60s [%s]\n", f.Path, fileTag(f))
				}
			}
			if nEdited > 0 {
				_, _ = fmt.Fprintln(out)
				_, _ = fmt.Fprintln(out, "Files edited:")
				for _, f := range res.FilesEdited {
					_, _ = fmt.Fprintf(out, "  ~ %-60s [%s]\n", f.Path, fileTag(f))
				}
			}
			if nDeleted > 0 {
				_, _ = fmt.Fprintln(out)
				_, _ = fmt.Fprintln(out, "Files deleted:")
				for _, f := range res.FilesDeleted {
					_, _ = fmt.Fprintf(out, "  - %-60s [%s]\n", f.Path, fileTag(f))
				}
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Output full result as JSON (includes per-file breakdown)")

	return cmd
}

func writeAttributionCounts(out io.Writer, res *service.AttributionResult) {
	_, _ = fmt.Fprintf(out, "AI Exact:     %d lines\n", res.AIExactLines)
	_, _ = fmt.Fprintf(out, "AI Formatted: %d lines\n", res.AIFormattedLines)
	_, _ = fmt.Fprintf(out, "AI Modified:  %d lines\n", res.AIModifiedLines)
	incomplete := res.Capture != nil && res.Capture.Status != "complete"
	if incomplete {
		_, _ = fmt.Fprintf(out, "Unattributed: %d lines\n", res.UnattributedLines)
		if res.UnattributedLines > 0 {
			impact := fmt.Sprintf("%d lines have unknown authorship", res.UnattributedLines)
			if res.UnattributedLines == 1 {
				impact = "1 line has unknown authorship"
			}
			_, _ = fmt.Fprintf(out, "Capture:      %s (%s)\n", res.Capture.Status, impact)
		}
	} else {
		_, _ = fmt.Fprintf(out, "Human:        %d lines\n", res.HumanLines)
	}
	_, _ = fmt.Fprintf(out, "Total:        %d lines\n", res.TotalLines)
	if incomplete {
		_, _ = fmt.Fprintf(out, "AI matched:   %.1f%%\n", res.AIPercentage)
	} else {
		_, _ = fmt.Fprintf(out, "AI %%:         %.1f%%\n", res.AIPercentage)
	}
}

// writeAttributionNotes explains capture gaps without changing JSON diagnostics.
func writeAttributionNotes(out io.Writer, res *service.AttributionResult) {
	const captureNote = "Capture is incomplete. Unmatched lines are unattributed, not confirmed human changes."
	notes := make([]string, 0, len(res.Diagnostics.Notes)+1)
	for _, note := range res.Diagnostics.Notes {
		if note != captureNote {
			notes = append(notes, note)
		}
	}
	if res.Capture != nil && res.Capture.Status != "complete" {
		note := "Some command capture evidence is unavailable."
		if res.Capture.Status == "pending" {
			note = "Some command capture evidence is still pending."
		}
		if res.TotalLines > 0 && res.AILines == res.TotalLines && res.UnattributedLines == 0 {
			note += fmt.Sprintf(" All %d changed lines matched AI evidence; no lines were left unattributed.", res.TotalLines)
		}
		notes = append(notes, note)
	}
	if len(notes) > 0 {
		_, _ = fmt.Fprintln(out, "Notes:")
		for _, note := range notes {
			_, _ = fmt.Fprintf(out, "  %s\n", note)
		}
	}
}

func attributionAgentLabels(details []service.ProviderAttribution) []string {
	labels := make([]string, 0, len(details))
	seen := make(map[string]struct{}, len(details))
	for _, detail := range details {
		provider := strings.TrimSpace(detail.Provider)
		if provider == "" {
			continue
		}

		label := attributionAgentName(provider)
		if model := strings.TrimSpace(detail.Model); model != "" {
			label += " (" + model + ")"
		}
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		labels = append(labels, label)
	}
	return labels
}

func attributionAgentName(provider string) string {
	switch provider {
	case "claude-code", "claude_code":
		return "Claude Code"
	case "codex":
		return "Codex"
	case "cursor":
		return "Cursor"
	case "copilot", "github-copilot":
		return "GitHub Copilot"
	case "gemini-cli", "gemini_cli":
		return "Gemini CLI"
	case "kiro-cli":
		return "Kiro CLI"
	case "kiro-ide":
		return "Kiro IDE"
	default:
		return provider
	}
}
