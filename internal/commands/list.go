package commands

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/semanticash/cli/internal/service"
	"github.com/semanticash/cli/internal/util"
	"github.com/spf13/cobra"
)

func NewListCmd(rootOpts *RootOptions) *cobra.Command {
	var (
		limit   int64
		asJSON  bool
		asJSONL bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List checkpoints for this repository",
		RunE: func(cmd *cobra.Command, args []string) error {
			if asJSON && asJSONL {
				return fmt.Errorf("flags --json and --jsonl are mutually exclusive")
			}

			svc := service.NewListService()
			res, err := svc.ListCheckpoints(cmd.Context(), service.ListCheckpointsInput{
				RepoPath: rootOpts.RepoPath,
				Limit:    limit,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()

			// Helper: normalize trigger/message for display.
			normalizeTrigger := func(t string) string {
				t = strings.TrimSpace(t)
				if t == "" {
					return "-"
				}
				return t
			}

			normalizeMessage := func(kind, trigger, msg string) string {
				msg = strings.TrimSpace(msg)
				// Auto commit message is redundant (it repeats created_at).
				if kind == "auto" && (strings.TrimSpace(trigger) == "commit" || strings.TrimSpace(trigger) == "pre-commit") &&
					strings.HasPrefix(msg, "Auto checkpoint") {
					return "-"
				}
				if msg == "" {
					return "-"
				}
				return msg
			}

			shortCommit := func(full string) string {
				full = strings.TrimSpace(full)
				if full == "" {
					return "-"
				}
				return util.ShortID(full)
			}

			normalizeSubject := func(s string) string {
				s = strings.TrimSpace(s)
				if s == "" {
					return "-"
				}
				return s
			}

			// Shared JSON shape for a checkpoint
			type jsonCheckpoint struct {
				ID            string `json:"id"`
				CreatedAt     string `json:"created_at"`      // RFC3339
				CreatedAtUnix int64  `json:"created_at_unix"` // unix seconds
				Kind          string `json:"kind"`
				Trigger       string `json:"trigger,omitempty"`
				Bytes         *int64 `json:"bytes,omitempty"`
				CommitHash    string `json:"commit_hash,omitempty"`
				CommitSubject string `json:"commit_subject,omitempty"`
				Message       string `json:"message,omitempty"`
				ManifestHash  string `json:"manifest_hash,omitempty"`
			}

			// JSONL mode: one object per line
			if asJSONL {
				enc := json.NewEncoder(out) // Encode adds a trailing \n each call
				for _, it := range res.Items {
					ts := time.UnixMilli(it.CreatedAt).Format(time.RFC3339)
					obj := jsonCheckpoint{
						ID:            it.ID,
						CreatedAt:     ts,
						CreatedAtUnix: it.CreatedAt,
						Kind:          it.Kind,
						Trigger:       strings.TrimSpace(it.Trigger),
						Bytes:         it.SizeBytes,
						CommitHash:    strings.TrimSpace(it.CommitHash),
						CommitSubject: strings.TrimSpace(it.CommitSubject),
						Message:       strings.TrimSpace(it.Message),
						ManifestHash:  it.ManifestHash,
					}
					if err := enc.Encode(obj); err != nil {
						return err
					}
				}
				return nil
			}

			// JSON mode: one pretty-printed object with {count, items}
			if asJSON {
				type jsonOut struct {
					Count int              `json:"count"`
					Items []jsonCheckpoint `json:"items"`
				}

				items := make([]jsonCheckpoint, 0, len(res.Items))
				for _, it := range res.Items {
					ts := time.UnixMilli(it.CreatedAt).Format(time.RFC3339)
					items = append(items, jsonCheckpoint{
						ID:            it.ID,
						CreatedAt:     ts,
						CreatedAtUnix: it.CreatedAt,
						Kind:          it.Kind,
						Trigger:       strings.TrimSpace(it.Trigger),
						Bytes:         it.SizeBytes,
						CommitHash:    strings.TrimSpace(it.CommitHash),
						CommitSubject: strings.TrimSpace(it.CommitSubject),
						Message:       strings.TrimSpace(it.Message),
						ManifestHash:  it.ManifestHash,
					})
				}

				payload := jsonOut{
					Count: len(items),
					Items: items,
				}

				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(payload)
			}

			// Human/table mode
			if len(res.Items) == 0 {
				_, _ = fmt.Fprintln(out, "No checkpoints found")
				return nil
			}

			_, _ = fmt.Fprintf(out, "Checkpoints (%d)\n\n", len(res.Items))

			rows := make([][]string, 0, len(res.Items))
			for _, it := range res.Items {
				age := relativeAge(it.CreatedAt)

				bytesStr := "-"
				if it.SizeBytes != nil {
					bytesStr = compactBytes(*it.SizeBytes)
				}

				trigger := normalizeTrigger(it.Trigger)
				message := normalizeMessage(it.Kind, it.Trigger, it.Message)
				if len(message) > 40 && message != "-" {
					message = message[:37] + "..."
				}

				commit := shortCommit(it.CommitHash)
				subject := normalizeSubject(it.CommitSubject)
				if len(subject) > 50 && subject != "-" {
					subject = subject[:47] + "..."
				}

				rows = append(rows, []string{
					util.ShortID(it.ID), age, it.Kind, trigger, commit, bytesStr, message, subject,
				})
			}

			headerStyle := lipgloss.NewStyle().Bold(true).Padding(0, 1)
			cellStyle := lipgloss.NewStyle().Padding(0, 1)
			dimStyle := cellStyle.Faint(true)

			t := table.New().
				Headers("ID", "AGE", "KIND", "TRIGGER", "COMMIT", "BYTES", "MESSAGE", "COMMIT MSG").
				Rows(rows...).
				Border(lipgloss.NormalBorder()).
				BorderRow(false).
				BorderColumn(false).
				BorderLeft(false).
				BorderRight(false).
				BorderTop(false).
				BorderBottom(false).
				BorderHeader(true).
				StyleFunc(func(row, col int) lipgloss.Style {
					if row == table.HeaderRow {
						return headerStyle
					}
					// Dim the bytes and message columns.
					if col == 5 || col == 6 {
						return dimStyle
					}
					return cellStyle
				})

			_, _ = fmt.Fprintln(out, t.Render())

			return nil
		},
	}

	cmd.Flags().Int64VarP(&limit, "limit", "n", 20, "Maximum number of checkpoints to list")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&asJSONL, "jsonl", false, "Output as JSONL (one JSON object per line)")

	return cmd
}

// compactBytes formats a byte count as a human-readable string.
func compactBytes(b int64) string {
	switch {
	case b < 1024:
		return fmt.Sprintf("%dB", b)
	case b < 1024*1024:
		return fmt.Sprintf("%.1fK", float64(b)/1024)
	case b < 1024*1024*1024:
		return fmt.Sprintf("%.1fM", float64(b)/(1024*1024))
	default:
		return fmt.Sprintf("%.1fG", float64(b)/(1024*1024*1024))
	}
}
