package commands

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/semanticash/cli/internal/service"
	"github.com/semanticash/cli/internal/util"
	"github.com/spf13/cobra"
)

func NewSessionsCmd(rootOpts *RootOptions) *cobra.Command {
	var (
		asJSON     bool
		transcript bool
		limit      int64
		all        bool
	)

	cmd := &cobra.Command{
		Use:   "sessions [session_id]",
		Short: "List agent sessions or view a session transcript",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()

			if len(args) == 1 {
				sessionID := args[0]

				if transcript {
					svc := service.NewTranscriptService()
					res, err := svc.TranscriptsForSession(cmd.Context(), service.TranscriptsForSessionInput{
						RepoPath:  rootOpts.RepoPath,
						SessionID: sessionID,
					})
					if err != nil {
						return err
					}

					if asJSON {
						enc := json.NewEncoder(out)
						enc.SetIndent("", "  ")
						return enc.Encode(res)
					}

					_, _ = fmt.Fprintf(out, "Session: %s (%s)\n", res.SessionID, res.Provider)
					if res.ProviderSessionID != "" {
						_, _ = fmt.Fprintf(out, "Provider ID: %s\n", res.ProviderSessionID)
					}
					_, _ = fmt.Fprintf(out, "Events: %d\n\n", len(res.Events))

					for _, e := range res.Events {
						if e.Role == "system" {
							continue
						}
						printEvent(out, e, false, false)
					}

					return nil
				}

				svc := service.NewSessionService()
				s, err := svc.GetSession(cmd.Context(), service.SessionDetailInput{
					RepoPath:  rootOpts.RepoPath,
					SessionID: sessionID,
				})
				if err != nil {
					return err
				}

				if asJSON {
					enc := json.NewEncoder(out)
					enc.SetIndent("", "  ")
					return enc.Encode(s)
				}
				_, _ = fmt.Fprintf(out, "Session:     %s\n", s.SessionID)
				_, _ = fmt.Fprintf(out, "Provider:    %s\n", s.Provider)
				_, _ = fmt.Fprintf(out, "Provider ID: %s\n", s.ProviderSessionID)
				if s.ParentSessionID != "" {
					_, _ = fmt.Fprintf(out, "Parent:      %s\n", s.ParentSessionID)
				}
				_, _ = fmt.Fprintf(out, "Started:     %s\n", s.StartedAt)
				_, _ = fmt.Fprintf(out, "Last event:  %s\n", s.LastEventAt)
				_, _ = fmt.Fprintf(out, "Steps:       %d\n", s.StepCount)
				_, _ = fmt.Fprintf(out, "Tool calls:  %d\n", s.ToolCallCount)
				tok := fmt.Sprintf("%s in / %s out",
					service.CompactTokens(s.TokensIn),
					service.CompactTokens(s.TokensOut))
				if s.TokensCached > 0 {
					tok += fmt.Sprintf(" (+%s cached)", service.CompactTokens(s.TokensCached))
				}
				_, _ = fmt.Fprintf(out, "Tokens:      %s\n", tok)
				return nil
			}

			// List sessions as tree
			svc := service.NewSessionService()
			tree, err := svc.ListSessions(cmd.Context(), service.SessionListInput{
				RepoPath: rootOpts.RepoPath,
				Limit:    limit,
				All:      all,
			})
			if err != nil {
				return err
			}

			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(tree)
			}

			if len(tree.Roots) == 0 {
				_, _ = fmt.Fprintln(out, "No sessions found.")
				return nil
			}

			_, _ = fmt.Fprintf(out, "Sessions (%d)\n", tree.Total)
			for _, root := range tree.Roots {
				printSessionNode(out, root, "")
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&transcript, "transcript", false, "Show session transcript (requires session_id)")
	cmd.Flags().Int64Var(&limit, "limit", 50, "Max sessions to list")
	cmd.Flags().BoolVar(&all, "all", false, "Include sessions with no events")

	return cmd
}

func printSessionNode(out io.Writer, s *service.SessionInfo, indent string) {
	label := s.Provider
	isRoot := s.ParentSessionID == ""
	if !isRoot {
		label += " (subagent)"
	}
	if n := len(s.Children); n > 0 && isRoot {
		label += fmt.Sprintf(" +%d", n)
	}

	if isRoot {
		tok := fmt.Sprintf("tok %s/%s", service.CompactTokens(s.TokensIn), service.CompactTokens(s.TokensOut))
		if s.TokensCached > 0 {
			tok += fmt.Sprintf(" (+%s cached)", service.CompactTokens(s.TokensCached))
		}
		if len(s.Children) > 0 {
			tIn, tOut, tCached := treeTokens(s)
			treeTok := fmt.Sprintf("tree %s/%s", service.CompactTokens(tIn), service.CompactTokens(tOut))
			if tCached > 0 {
				treeTok += fmt.Sprintf(" (+%s cached)", service.CompactTokens(tCached))
			}
			tok += fmt.Sprintf(" (%s)", treeTok)
		}
		_, _ = fmt.Fprintf(out, "%s%s  %-24s  last_seen %s  steps %d  tools %d  %s\n",
			indent,
			util.ShortID(s.SessionID),
			label,
			service.RelativeTime(s.LastEventAtMs),
			s.StepCount,
			s.ToolCallCount,
			tok,
		)
	} else {
		tok := fmt.Sprintf("tok %s/%s", service.CompactTokens(s.TokensIn), service.CompactTokens(s.TokensOut))
		if s.TokensCached > 0 {
			tok += fmt.Sprintf(" (+%s cached)", service.CompactTokens(s.TokensCached))
		}
		_, _ = fmt.Fprintf(out, "%s%s  %-24s  steps %d  tools %d  %s\n",
			indent,
			util.ShortID(s.SessionID),
			label,
			s.StepCount,
			s.ToolCallCount,
			tok,
		)
	}

	for _, child := range s.Children {
		printSessionNode(out, child, indent+"  ")
	}
}

// treeTokens returns the sum of tokens_in, tokens_out, and tokens_cached
// across a node and all its descendants.
func treeTokens(s *service.SessionInfo) (int64, int64, int64) {
	in, out, cached := s.TokensIn, s.TokensOut, s.TokensCached
	for _, c := range s.Children {
		cIn, cOut, cCached := treeTokens(c)
		in += cIn
		out += cOut
		cached += cCached
	}
	return in, out, cached
}
