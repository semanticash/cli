package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/semanticash/cli/internal/service"
	"github.com/semanticash/cli/internal/util"
	"github.com/spf13/cobra"
)

func NewTranscriptsCmd(rootOpts *RootOptions) *cobra.Command {
	var (
		asJSON          bool
		asJSONL         bool
		raw             bool
		verbose         bool
		cumulative      bool
		bySession       bool
		filterSessionID string
		commit          bool
		forceCheckpoint bool
		forceSession    bool
	)

	cmd := &cobra.Command{
		Use:   "transcripts [ref]",
		Short: "Show agent transcript(s) for a checkpoint or session",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if asJSON && asJSONL {
				return fmt.Errorf("flags --json and --jsonl are mutually exclusive")
			}

			ref, err := resolveRef(cmd.Context(), rootOpts.RepoPath, args)
			if err != nil {
				return err
			}

			svc := service.NewTranscriptService()
			res, err := svc.Transcripts(cmd.Context(), service.TranscriptsInput{
				RepoPath:        rootOpts.RepoPath,
				Ref:             ref,
				ForceCheckpoint: forceCheckpoint,
				ForceSession:    forceSession,
				Raw:             raw,
				Verbose:         verbose,
				Cumulative:      cumulative,
				BySession:       bySession,
				FilterSessionID: filterSessionID,
				Commit:          commit,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()

			if res.ResolvedAs == "session" {
				return renderSessionTranscript(out, res.Session, asJSON, asJSONL, raw, verbose)
			}

			return renderCheckpointTranscript(out, res.Checkpoint, asJSON, asJSONL, raw, verbose)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&asJSONL, "jsonl", false, "Output as JSONL (meta + one event per line)")
	cmd.Flags().BoolVar(&raw, "raw", false, "Include raw payload JSON (loads from blob store)")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "Show more fields (provider, tokens, etc.)")
	cmd.Flags().BoolVar(&cumulative, "cumulative", false, "Show all events up to checkpoint (default: delta since previous checkpoint)")
	cmd.Flags().BoolVar(&bySession, "by-session", false, "Group events by session")
	cmd.Flags().StringVar(&filterSessionID, "filter-session", "", "Filter to a specific session ID (checkpoint mode only)")
	cmd.Flags().BoolVar(&commit, "commit", false, "Show only sessions that touched files in the commit diff")
	cmd.Flags().BoolVar(&forceCheckpoint, "checkpoint", false, "Force resolution as checkpoint")
	cmd.Flags().BoolVar(&forceSession, "session", false, "Force resolution as session")

	return cmd
}

func renderCheckpointTranscript(out io.Writer, res *service.TranscriptsForCheckpointResult, asJSON, asJSONL, raw, verbose bool) error {
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}

	if asJSONL {
		enc := json.NewEncoder(out)
		if err := enc.Encode(res.Meta); err != nil {
			return err
		}
		for _, e := range res.Events {
			if err := enc.Encode(e); err != nil {
				return err
			}
		}
		return nil
	}

	_, _ = fmt.Fprintf(out, "Checkpoint: %s\n", util.ShortID(res.Meta.CheckpointID))
	if res.Meta.CommitHash != "" {
		_, _ = fmt.Fprintf(out, "Commit:     %s\n", res.Meta.CommitHash)
	}
	_, _ = fmt.Fprintf(out, "Sessions:   %d\n", res.Meta.SessionCount)
	_, _ = fmt.Fprintln(out)

	if len(res.Sessions) > 0 {
		for _, sess := range res.Sessions {
			_, _ = fmt.Fprintf(out, "Session: %s (%s)\n", sess.SessionID, sess.Provider)
			if sess.ProviderSessionID != "" {
				_, _ = fmt.Fprintf(out, "  Provider ID: %s\n", sess.ProviderSessionID)
			}
			_, _ = fmt.Fprintln(out)
			for _, e := range sess.Events {
				if e.Role == "system" {
					continue
				}
				printEvent(out, e, raw, verbose)
			}
		}
	} else {
		for _, e := range res.Events {
			if e.Role == "system" {
				continue
			}
			printEvent(out, e, raw, verbose)
		}
	}

	if len(res.Events) == 0 {
		_, _ = fmt.Fprintln(out, "No transcript events linked to this checkpoint.")
	}

	return nil
}

func renderSessionTranscript(out io.Writer, res *service.SessionTranscript, asJSON, asJSONL, raw, verbose bool) error {
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}

	if asJSONL {
		enc := json.NewEncoder(out)
		// header line with session metadata
		header := struct {
			SessionID         string `json:"session_id"`
			ProviderSessionID string `json:"provider_session_id,omitempty"`
			Provider          string `json:"provider"`
			EventCount        int    `json:"event_count"`
		}{res.SessionID, res.ProviderSessionID, res.Provider, len(res.Events)}
		if err := enc.Encode(header); err != nil {
			return err
		}
		for _, e := range res.Events {
			if err := enc.Encode(e); err != nil {
				return err
			}
		}
		return nil
	}

	_, _ = fmt.Fprintf(out, "Session:  %s (%s)\n", res.SessionID, res.Provider)
	if res.ProviderSessionID != "" {
		_, _ = fmt.Fprintf(out, "Provider: %s\n", res.ProviderSessionID)
	}
	_, _ = fmt.Fprintf(out, "Events:   %d\n", len(res.Events))
	_, _ = fmt.Fprintln(out)

	for _, e := range res.Events {
		if e.Role == "system" {
			continue
		}
		printEvent(out, e, raw, verbose)
	}

	if len(res.Events) == 0 {
		_, _ = fmt.Fprintln(out, "No transcript events for this session.")
	}

	return nil
}

func printEvent(out io.Writer, e service.TranscriptEvent, raw, verbose bool) {
	label := eventLabel(e)
	_, _ = fmt.Fprintf(out, "  [%s] %s", e.TsISO, label)
	if verbose && e.Provider != "" {
		_, _ = fmt.Fprintf(out, " (%s)", e.Provider)
	}
	_, _ = fmt.Fprintln(out)

	if e.Summary != "" {
		_, _ = fmt.Fprintf(out, "    %s\n", e.Summary)
	}

	if raw && e.Payload != "" {
		_, _ = fmt.Fprintf(out, "    %s\n", e.Payload)
	}

	if verbose && (e.TokensIn != 0 || e.TokensOut != 0 || e.TokensCacheRead != 0 || e.TokensCacheCreate != 0) {
		_, _ = fmt.Fprintf(out, "    tokens: in=%d out=%d cache_read=%d cache_create=%d\n",
			e.TokensIn, e.TokensOut, e.TokensCacheRead, e.TokensCacheCreate)
	}

	_, _ = fmt.Fprintln(out)
}

// eventLabel returns a human-readable label for a transcript event,
// distinguishing user prompts from tool results, and assistant
// thinking/text from tool calls.
func eventLabel(e service.TranscriptEvent) string {
	switch strings.ToLower(e.Role) {
	case "user":
		if e.Kind == "tool_result" {
			return "TOOL RESULT"
		}
		return "PROMPT"
	case "assistant":
		if e.HasThinking && e.ToolName == "" {
			return "THINKING"
		}
		if e.ToolName != "" {
			if e.FilePath != "" {
				return fmt.Sprintf("TOOL > %s(%s)", e.ToolName, e.FilePath)
			}
			return fmt.Sprintf("TOOL > %s", e.ToolName)
		}
		return "ASSISTANT"
	case "system":
		return "SYSTEM"
	default:
		if e.RoleUpper != "" {
			return e.RoleUpper
		}
		return strings.ToUpper(e.Kind)
	}
}
