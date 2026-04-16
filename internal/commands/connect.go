package commands

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	hspinner "charm.land/huh/v2/spinner"
	"github.com/semanticash/cli/internal/auth"
	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/provenance"
	"github.com/semanticash/cli/internal/service"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	"github.com/semanticash/cli/internal/util"
	"github.com/spf13/cobra"
)

func NewConnectCmd(rootOpts *RootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "connect",
		Short: "Connect this repo to Semantica",
		Long:  "Connects the current repository to your Semantica workspace. Requires authentication and a git remote.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConnect(cmd, rootOpts)
		},
	}
}

func NewDisconnectCmd(rootOpts *RootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "disconnect",
		Short: "Disconnect this repo from Semantica",
		Long:  "Stops syncing attribution from this repo to the dashboard. Local capture continues.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := git.OpenRepo(rootOpts.RepoPath)
			if err != nil {
				return fmt.Errorf("not inside a git repository")
			}
			semDir := filepath.Join(repo.Root(), ".semantica")

			if !util.IsEnabled(semDir) {
				return fmt.Errorf("semantica is not enabled. Run `semantica enable` first")
			}

			s, err := util.ReadSettings(semDir)
			if err != nil {
				return fmt.Errorf("read settings: %w", err)
			}

			if !s.Connected {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Not connected.")
				return nil
			}

			// Notify the API that this CLI is no longer syncing.
			// Best-effort: local disconnect proceeds regardless.
			if s.ConnectedRepoID != "" {
				if err := auth.DisconnectRepo(cmd.Context(), s.ConnectedRepoID); err != nil {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Note: could not notify the dashboard: %v\n", err)
				}
			}

			s.Connected = false
			if err := util.WriteSettings(semDir, s); err != nil {
				return fmt.Errorf("write settings: %w", err)
			}

			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Disconnected. Attribution will no longer sync to the dashboard.")
			return nil
		},
	}
}

func runConnect(cmd *cobra.Command, rootOpts *RootOptions) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	repo, err := git.OpenRepo(rootOpts.RepoPath)
	if err != nil {
		return fmt.Errorf("not inside a git repository")
	}
	semDir := filepath.Join(repo.Root(), ".semantica")

	if !util.IsEnabled(semDir) {
		return fmt.Errorf("semantica is not enabled. Run `semantica enable` first")
	}

	remoteURL, err := repo.RemoteURL(ctx)
	if err != nil || remoteURL == "" {
		return fmt.Errorf("no git remote found. Add a remote first: git remote add origin <url>")
	}

	token, err := auth.AccessToken(ctx)
	if err != nil || token == "" {
		return fmt.Errorf("not authenticated. Run `semantica auth login` first")
	}

	provider := git.ProviderFromRemoteURL(remoteURL)

	_, _ = fmt.Fprintf(out, "Connecting %s...\n", remoteURL)
	resp, err := auth.ConnectRepo(ctx, remoteURL, provider)
	if err != nil {
		return fmt.Errorf("connect failed: %w", err)
	}

	return handleConnectOutcome(cmd, rootOpts, semDir, resp)
}

func handleConnectOutcome(cmd *cobra.Command, rootOpts *RootOptions, semDir string, resp *auth.ConnectRepoResponse) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	switch resp.Outcome {
	case "connected", "already_connected":
		s, err := util.ReadSettings(semDir)
		if err != nil {
			return fmt.Errorf("read settings: %w", err)
		}
		wasConnected := s.Connected
		s.Connected = true
		s.ConnectedRepoID = resp.RepositoryID
		if err := util.WriteSettings(semDir, s); err != nil {
			return fmt.Errorf("write settings: %w", err)
		}

		if resp.Outcome == "connected" {
			_, _ = fmt.Fprintln(out, "Connected! Attribution will sync to the dashboard on each commit.")
		} else if wasConnected {
			_, _ = fmt.Fprintln(out, "Already connected.")
		} else {
			_, _ = fmt.Fprintln(out, "Connected. This repo was already registered with Semantica, and local sync is now enabled.")
		}

		// Sync a small initial batch of packaged provenance. The rest drains on later checkpoints.
		const connectBackfillBatchSize = 20
		repoRoot := filepath.Dir(semDir)
		endpoint := auth.EffectiveEndpoint()
		tok, tokErr := auth.AccessToken(ctx)
		if tokErr != nil || tok == "" {
			_, _ = fmt.Fprintln(out, "Note: could not sync provenance history (auth unavailable). It will sync on future checkpoints.")
		} else {
			var results []provenance.UploadResult
			var syncErr error

			sp := hspinner.New().
				WithTheme(commandSpinnerTheme()).
				Title("Syncing provenance...")

			syncOpts := &provenance.UploadOptions{
				OnProgress: func(current, total int, r provenance.UploadResult) {
					sp.Title(fmt.Sprintf("Syncing provenance: %d/%d", current, total))
				},
			}

			sp.Action(func() {
				results, syncErr = provenance.SyncAndUpload(ctx, repoRoot, endpoint, tok, 0, connectBackfillBatchSize, syncOpts)
				if syncErr != nil {
					return
				}
				for _, r := range results {
					if r.Err != nil && provenance.IsUnauthorized(r.Err) {
						if refreshed, refreshErr := auth.ForceRefresh(ctx); refreshErr == nil {
							retryResults, retryErr := provenance.SyncAndUpload(ctx, repoRoot, endpoint, refreshed, 0, connectBackfillBatchSize, syncOpts)
							if retryErr == nil {
								results = retryResults
							}
						}
						break
					}
				}
			})

			if spinErr := sp.Run(); spinErr != nil {
				results, syncErr = provenance.SyncAndUpload(ctx, repoRoot, endpoint, tok, 0, connectBackfillBatchSize, nil)
			}

			if syncErr != nil {
				_, _ = fmt.Fprintf(out, "Note: initial provenance sync failed: %v. It will retry on future checkpoints.\n", syncErr)
			} else {
				uploaded, retryable, permanent := 0, 0, 0
				for _, r := range results {
					switch r.Action {
					case provenance.ActionUploaded:
						uploaded++
					case provenance.ActionRetry:
						retryable++
					case provenance.ActionFail:
						permanent++
					}
				}
				if uploaded > 0 {
					_, _ = fmt.Fprintf(out, "Synced %d provenance turn(s) to dashboard. Remaining history will continue syncing over time.\n", uploaded)
				}
				if retryable > 0 {
					_, _ = fmt.Fprintf(out, "Note: %d turn(s) will retry on future checkpoints.\n", retryable)
				}
				if permanent > 0 {
					_, _ = fmt.Fprintf(out, "Note: %d turn(s) could not be synced and will not retry automatically.\n", permanent)
				}
			}
		}

		// Backfill historical attribution for commits made before connect.
		backfillAttribution(ctx, out, semDir, s.ConnectedRepoID)

		if resp.GithubAppRecommended {
			_, _ = fmt.Fprintln(out)
			_, _ = fmt.Fprintln(out, "Tip: Install the Semantica GitHub App to enable PR comments and check runs.")
		}
		if resp.GitlabWebhookRequired {
			_, _ = fmt.Fprintln(out)
			_, _ = fmt.Fprintln(out, "GitLab MR comments and checks require a project webhook, and Semantica could not create it automatically.")
			_, _ = fmt.Fprintln(out, "Ask a GitLab maintainer to add this project webhook:")
			if resp.GitlabWebhookURL != "" {
				_, _ = fmt.Fprintf(out, "  URL: %s\n", resp.GitlabWebhookURL)
			}
			if resp.GitlabWebhookSecret != "" {
				_, _ = fmt.Fprintf(out, "  Secret token: %s\n", resp.GitlabWebhookSecret)
			}
			_, _ = fmt.Fprintln(out, "  Event: Merge request events")
			if resp.GitlabWebhookReason == "insufficient_permission" {
				_, _ = fmt.Fprintln(out, "Semantica needs maintainer-level access to create GitLab project webhooks automatically.")
			}
		}
		return nil

	case "missing_provider_identity", "provider_identity_expired":
		if resp.AuthURL == "" || resp.State == "" {
			return fmt.Errorf("connect failed: server did not provide an authorization URL (unexpected response)")
		}

		providerName := providerDisplayName(resp.Provider)
		if resp.Outcome == "missing_provider_identity" {
			_, _ = fmt.Fprintf(out, "This repo requires a %s identity. Opening browser to authorize...\n", providerName)
		} else {
			_, _ = fmt.Fprintf(out, "Your %s session has expired. Opening browser to re-authorize...\n", providerName)
		}

		if err := auth.OpenBrowser(strings.TrimSpace(resp.AuthURL)); err != nil {
			_, _ = fmt.Fprintf(out, "Open this URL in your browser:\n  %s\n", resp.AuthURL)
		}

		_, _ = fmt.Fprintln(out, "Waiting for authorization...")

		pollResult, err := auth.PollConnectRepo(ctx, resp.State, 3)
		if err != nil {
			return fmt.Errorf("authorization failed: %w", err)
		}

		return handleConnectOutcome(cmd, rootOpts, semDir, pollResult)

	case "insufficient_repo_access":
		return fmt.Errorf("push or admin access required on this repository")

	case "repo_belongs_to_other_workspace":
		return handleWorkspaceConflict(cmd, resp)

	case "provider_unavailable":
		return fmt.Errorf("provider temporarily unavailable. Try again later")

	case "unsupported_provider":
		return fmt.Errorf("unsupported host. Only GitHub and GitLab are supported")

	case "provider_identity_linked_elsewhere":
		return fmt.Errorf("this provider account is already linked to another Semantica user")

	case "no_remote":
		return fmt.Errorf("no git remote found. Add a remote first")

	default:
		if resp.Message != "" {
			return fmt.Errorf("connect failed: %s", resp.Message)
		}
		return fmt.Errorf("connect failed: unexpected outcome %q", resp.Outcome)
	}
}

func handleWorkspaceConflict(cmd *cobra.Command, resp *auth.ConnectRepoResponse) error {
	out := cmd.OutOrStdout()
	ctx := cmd.Context()

	workspaceName := resp.WorkspaceName
	if workspaceName == "" {
		workspaceName = "another workspace"
	}

	switch resp.ExistingRequestStatus {
	case "pending":
		_, _ = fmt.Fprintf(out, "This repository is already connected to %s.\n", workspaceName)
		_, _ = fmt.Fprintln(out, "Your access request is still pending approval.")
		return nil
	case "rejected":
		_, _ = fmt.Fprintf(out, "This repository is already connected to %s.\n", workspaceName)
		_, _ = fmt.Fprintln(out, "Your access request was declined. Contact the workspace owner/admin for access.")
		return nil
	case "approved":
		_, _ = fmt.Fprintf(out, "Access to %s was approved. Rerun `semantica connect`.\n", workspaceName)
		return nil
	}

	_, _ = fmt.Fprintf(out, "This repository is already connected to %s.\n", workspaceName)

	if !resp.RequestAccessSupported {
		return fmt.Errorf("this workspace does not support access requests")
	}

	if !isTerminalWriter(out) {
		_, _ = fmt.Fprintln(out, "Rerun `semantica connect` in an interactive terminal to request access.")
		return fmt.Errorf("repository belongs to %s", workspaceName)
	}

	_, _ = fmt.Fprint(out, "Request access so this machine can sync to that workspace? [Y/n] ")
	var answer string
	if _, err := fmt.Fscanln(cmd.InOrStdin(), &answer); err != nil {
		_, _ = fmt.Fprintln(out, "\nCould not read input. Rerun in an interactive terminal to request access.")
		return fmt.Errorf("repository belongs to %s", workspaceName)
	}
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer != "" && answer != "y" && answer != "yes" {
		_, _ = fmt.Fprintln(out, "Access request cancelled.")
		return fmt.Errorf("repository belongs to %s", workspaceName)
	}

	reqResp, reqErr := auth.RequestWorkspaceAccess(ctx, "", resp.RepositoryID)
	if reqErr != nil {
		return fmt.Errorf("failed to request access: %w", reqErr)
	}
	if workspaceName == "another workspace" && reqResp != nil && reqResp.WorkspaceName != "" {
		workspaceName = reqResp.WorkspaceName
	}

	_, _ = fmt.Fprintf(out, "Access requested from %s.\n", workspaceName)
	_, _ = fmt.Fprintln(out, "Semantica will keep capturing locally until access is approved.")
	_, _ = fmt.Fprintln(out, "Rerun `semantica connect` after approval.")
	return nil
}

func backfillAttribution(ctx context.Context, out io.Writer, semDir, connectedRepoID string) {
	repoRoot := filepath.Dir(semDir)
	dbPath := filepath.Join(semDir, "lineage.db")

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.OpenOptions{
		BusyTimeout: 200 * time.Millisecond,
		Synchronous: "NORMAL",
	})
	if err != nil {
		_, _ = fmt.Fprintf(out, "Note: could not inspect local attribution history: %v. It will retry on future checkpoints.\n", err)
		return
	}
	defer func() { _ = sqlstore.Close(h) }()

	// Look up the local repository_id from the repo root path. commit_links
	// uses this local ID, not the hosted connected repo ID.
	localRepo, err := h.Queries.GetRepositoryByRootPath(ctx, repoRoot)
	if err != nil {
		if err != sql.ErrNoRows {
			_, _ = fmt.Fprintf(out, "Note: could not inspect local attribution history: %v. It will retry on future checkpoints.\n", err)
		}
		return
	}

	hasBacklog, err := service.InitBackfillState(ctx, h, connectedRepoID, localRepo.RepositoryID)
	if err != nil || !hasBacklog {
		if err != nil {
			_, _ = fmt.Fprintf(out, "Note: could not initialize historical attribution sync: %v. It will retry on future checkpoints.\n", err)
		}
		return
	}

	const connectAttrBatchSize = 10
	result := service.DrainBackfillBatch(ctx, repoRoot, connectedRepoID, connectAttrBatchSize)

	if result.Uploaded > 0 {
		if result.Done {
			_, _ = fmt.Fprintf(out, "Backfilled attribution for %d historical commit(s).\n", result.Uploaded)
		} else {
			_, _ = fmt.Fprintf(out, "Backfilled attribution for %d historical commit(s). Remaining history will continue syncing over time.\n", result.Uploaded)
		}
	}
	if result.Failed {
		_, _ = fmt.Fprintf(out, "Note: historical attribution replay paused after %d commit(s): %s. It will retry on future checkpoints.\n", result.Uploaded, result.Reason)
	}
}

func providerDisplayName(provider string) string {
	switch provider {
	case "github":
		return "GitHub"
	case "gitlab":
		return "GitLab"
	default:
		return provider
	}
}
