package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/semanticash/cli/internal/service"
	"github.com/semanticash/cli/internal/service/implementations"
	"github.com/semanticash/cli/internal/util"
	"github.com/spf13/cobra"
)

func NewImplementationsCmd(rootOpts *RootOptions) *cobra.Command {
	var (
		asJSON        bool
		all           bool
		includeSingle bool
		limit         int64
	)

	cmd := &cobra.Command{
		Use:     "implementations [implementation_id]",
		Aliases: []string{"impl"},
		Short:   "List or inspect cross-repo implementations",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()

			if len(args) == 1 {
				return showImplementation(cmd, out, args[0], asJSON)
			}
			return listImplementations(cmd, out, implementations.ListInput{
				Limit:         limit,
				All:           all,
				IncludeSingle: includeSingle,
			}, asJSON)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&all, "all", false, "Show all implementations including old dormant and single-repo")
	cmd.Flags().BoolVar(&includeSingle, "include-single", false, "Include single-repo implementations")
	cmd.Flags().Int64Var(&limit, "limit", 20, "Max implementations to list")

	return cmd
}

func listImplementations(cmd *cobra.Command, out io.Writer, in implementations.ListInput, asJSON bool) error {
	result, err := implementations.List(cmd.Context(), in)
	if err != nil {
		return err
	}

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	if len(result.Items) == 0 {
		_, _ = fmt.Fprintln(out, "No implementations found.")
		return nil
	}

	_, _ = fmt.Fprintf(out, "IMPLEMENTATIONS\n\n")
	_, _ = fmt.Fprintf(out, "%-10s %-30s %-18s %-10s %s\n",
		"ID", "Title", "Repos", "State", "Commits")

	for _, item := range result.Items {
		id := util.ShortID(item.ImplementationID)
		title := item.Title
		if title == "" {
			title = "\u2014" // em dash
		}
		if len(title) > 28 {
			title = title[:27] + "\u2026"
		}

		repoNames := make([]string, 0, len(item.Repos))
		for _, r := range item.Repos {
			repoNames = append(repoNames, r.DisplayName)
		}
		repos := strings.Join(repoNames, ", ")
		if len(repos) > 16 {
			repos = repos[:15] + "\u2026"
		}

		_, _ = fmt.Fprintf(out, "%-10s %-30s %-18s %-10s %d\n",
			id, title, repos, item.State, item.CommitCount)
	}

	return nil
}

func showImplementation(cmd *cobra.Command, out io.Writer, implID string, asJSON bool) error {
	detail, err := implementations.GetDetail(cmd.Context(), implID)
	if err != nil {
		return err
	}

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(detail)
	}

	_, _ = fmt.Fprintf(out, "Implementation %s\n", util.ShortID(detail.ImplementationID))
	if detail.Title != "" {
		_, _ = fmt.Fprintf(out, "Title: %s\n", detail.Title)
	}
	_, _ = fmt.Fprintf(out, "State: %s\n", detail.State)

	// Find origin repo.
	for _, r := range detail.Repos {
		if r.Role == "origin" {
			_, _ = fmt.Fprintf(out, "Origin: %s\n", r.DisplayName)
			break
		}
	}

	_, _ = fmt.Fprintf(out, "\nRepos\n")
	for _, r := range detail.Repos {
		_, _ = fmt.Fprintf(out, "  %-14s %-12s first seen %s, %d sessions\n",
			r.DisplayName, r.Role,
			service.RelativeTime(r.FirstSeenAt),
			r.SessionCount)
	}

	if len(detail.Timeline) > 0 {
		_, _ = fmt.Fprintf(out, "\nTimeline\n")
		for _, e := range detail.Timeline {
			prefix := "  "
			if e.CrossRepo {
				prefix = "\u2192 " // → arrow
			}
			ts := service.RelativeTime(e.Timestamp)
			_, _ = fmt.Fprintf(out, "  %s %s%-14s %s\n", ts, prefix, e.RepoName, e.Summary)
		}
	}

	// Stats line.
	_, _ = fmt.Fprintf(out, "\nSessions: %d", len(detail.Sessions))
	if detail.TotalTokensIn > 0 || detail.TotalTokensOut > 0 {
		_, _ = fmt.Fprintf(out, "   Tokens: %s in / %s out",
			service.CompactTokens(detail.TotalTokensIn),
			service.CompactTokens(detail.TotalTokensOut))
	}
	_, _ = fmt.Fprintln(out)

	return nil
}
