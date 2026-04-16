package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/service"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	"github.com/semanticash/cli/internal/util"
	"github.com/spf13/cobra"
)

func NewRewindCmd(rootOpts *RootOptions) *cobra.Command {
	var (
		noSafety bool
		exact    bool
		asJSON   bool
		yes      bool
	)

	cmd := &cobra.Command{
		Use:   "rewind [checkpoint_id]",
		Short: "Restore the working tree to a checkpoint",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			checkpointID, err := resolveRef(cmd.Context(), rootOpts.RepoPath, args)
			if aborted, rerr := handleAbort(cmd.OutOrStdout(), err); aborted || rerr != nil {
				return rerr
			}

			repo, err := git.OpenRepo(rootOpts.RepoPath)
			if err != nil {
				return err
			}
			repoRoot := repo.Root()
			out := cmd.OutOrStdout()

			// Look up checkpoint details for confirmation.
			semDir := filepath.Join(repoRoot, ".semantica")
			dbPath := filepath.Join(semDir, "lineage.db")
			h, err := sqlstore.Open(cmd.Context(), dbPath, sqlstore.DefaultOpenOptions())
			if err != nil {
				return err
			}

			repoRow, err := h.Queries.GetRepositoryByRootPath(cmd.Context(), repoRoot)
			if err != nil {
				_ = sqlstore.Close(h)
				return fmt.Errorf("repository not found")
			}
			resolvedID, err := sqlstore.ResolveCheckpointID(cmd.Context(), h.Queries, repoRow.RepositoryID, checkpointID)
			if err != nil {
				_ = sqlstore.Close(h)
				return err
			}
			cp, err := h.Queries.GetCheckpointByID(cmd.Context(), resolvedID)
			_ = sqlstore.Close(h)
			if err != nil {
				return fmt.Errorf("checkpoint %s not found", checkpointID)
			}

			// Skip confirmation for JSON output or --yes flag.
			if !asJSON && !yes {
				ts := time.UnixMilli(cp.CreatedAt).Format("2006-01-02 15:04")
				msg := cp.Kind
				if cp.Message.Valid && cp.Message.String != "" {
					msg = cp.Message.String
				}

				_, _ = fmt.Fprintf(out, "Checkpoint:  %s\n", util.ShortID(resolvedID))
				_, _ = fmt.Fprintf(out, "Created:     %s\n", ts)
				_, _ = fmt.Fprintf(out, "Description: %s\n", msg)

				// Show linked commit if one exists.
				h2, err2 := sqlstore.Open(cmd.Context(), dbPath, sqlstore.DefaultOpenOptions())
				if err2 == nil {
					links, _ := h2.Queries.GetCommitLinksByCheckpoint(cmd.Context(), resolvedID)
					if len(links) > 0 {
						hash := links[0].CommitHash
						if len(hash) > 7 {
							hash = hash[:7]
						}
						subject, _ := repo.CommitSubject(cmd.Context(), links[0].CommitHash)
						if subject != "" {
							_, _ = fmt.Fprintf(out, "Commit:      %s %s\n", hash, subject)
						} else {
							_, _ = fmt.Fprintf(out, "Commit:      %s\n", hash)
						}
					}
					_ = sqlstore.Close(h2)
				}

				_, _ = fmt.Fprintln(out)

				// Build warning based on flags.
				warning := "This will restore your working tree to this checkpoint."
				if exact {
					warning += "\nFiles not present in the checkpoint will be deleted."
				}
				if noSafety {
					warning += "\nNo safety checkpoint will be created (--no-safety)."
					dirty, _ := repo.IsDirty(cmd.Context())
					if dirty {
						warning += "\nYou have uncommitted changes that may be lost."
					}
				} else {
					warning += "\nA safety checkpoint will be created first."
				}

				_, _ = fmt.Fprintln(out, warning)
				_, _ = fmt.Fprintln(out)

				green := lipgloss.Color("#02BA84")
				confirmTheme := huh.ThemeFunc(func(isDark bool) *huh.Styles {
					s := huh.ThemeCharm(isDark)
					s.Focused.FocusedButton = s.Focused.FocusedButton.Background(green)
					s.Focused.BlurredButton = s.Focused.BlurredButton.Foreground(green)
					return s
				})

				var confirmed bool
				form := huh.NewForm(
					huh.NewGroup(
						huh.NewConfirm().
							Title("Proceed with rewind?").
							Affirmative("Yes").
							Negative("No").
							Value(&confirmed),
					),
				).WithTheme(confirmTheme)
				if err := form.Run(); err != nil {
					if errors.Is(err, huh.ErrUserAborted) {
						_, _ = fmt.Fprintln(out, "Rewind cancelled.")
						return nil
					}
					return err
				}
				if !confirmed {
					_, _ = fmt.Fprintln(out, "Rewind cancelled.")
					return nil
				}
			}

			svc := service.NewRewindService()
			var res *service.RewindResult
			action := func() {
				res, err = svc.Rewind(cmd.Context(), service.RewindInput{
					RepoPath:     rootOpts.RepoPath,
					CheckpointID: resolvedID,
					NoSafety:     noSafety,
					Exact:        exact,
				})
			}
			if spinErr := runWithOptionalSpinner(out, asJSON, "Restoring files...", action); spinErr != nil {
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

			_, _ = fmt.Fprintf(out, "Restored to checkpoint %s\n", util.ShortID(res.CheckpointID))
			if res.SafetyCheckpointID != "" {
				_, _ = fmt.Fprintf(out, "Safety checkpoint: %s\n", util.ShortID(res.SafetyCheckpointID))
			}
			_, _ = fmt.Fprintf(out, "Files restored: %d\n", res.FilesRestored)
			if exact {
				_, _ = fmt.Fprintf(out, "Files deleted: %d\n", res.FilesDeleted)
			} else {
				_, _ = fmt.Fprintln(out, "\nNote: extra untracked files were not removed. Use --exact to fully match the checkpoint.")
			}
			_, _ = fmt.Fprintln(out, "Review changes with: git status / git diff")
			return nil
		},
	}

	cmd.Flags().BoolVar(&noSafety, "no-safety", false, "Do not create a safety checkpoint before rewinding (dangerous)")
	cmd.Flags().BoolVar(&exact, "exact", false, "Also delete files not present in the checkpoint file set")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation prompt")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")

	return cmd
}
