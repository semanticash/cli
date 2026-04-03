package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/semanticash/cli/internal/auth"
	"github.com/semanticash/cli/internal/version"
	"github.com/spf13/cobra"
)

const cliUpgradeCommand = "curl -fsSL https://semantica.sh/install.sh | sh"

// lookupWorkspaceTierTitle returns the hosted workspace tier when a session is active.
func lookupWorkspaceTierTitle(ctx context.Context) string {
	state := auth.GetAuthState()
	if !state.Authenticated || state.Source != "session" {
		return ""
	}

	me, err := auth.Me(ctx)
	if err != nil {
		return ""
	}

	return me.WorkspaceTierTitle
}

// lookupCLIUpdate reports a newer public CLI release when one is available.
func lookupCLIUpdate(ctx context.Context) *version.UpdateInfo {
	info, err := version.CheckForUpdate(ctx)
	if err != nil || info == nil || !info.Available {
		return nil
	}
	return info
}

// NewWorkspaceCmd creates the `semantica workspace` command group.
func NewWorkspaceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workspace",
		Short: "Manage workspace access and settings",
	}

	cmd.AddCommand(newWorkspaceRequestsCmd())
	return cmd
}

func newWorkspaceRequestsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "requests",
		Short: "List, approve, or reject workspace access requests",
		RunE:  runWorkspaceRequestsList,
	}

	cmd.AddCommand(newWorkspaceRequestsApproveCmd())
	cmd.AddCommand(newWorkspaceRequestsRejectCmd())
	return cmd
}

func runWorkspaceRequestsList(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	token, err := workspaceAccessToken(ctx)
	if err != nil {
		return err
	}

	endpoint := auth.EffectiveEndpoint()
	result, err := workspaceAPIGet(ctx, endpoint+"/v1/workspaces/access-requests", token)
	if err != nil {
		return fmt.Errorf("failed to list requests: %w", err)
	}

	var payload struct {
		Requests []struct {
			RequestID    string `json:"request_id"`
			UserEmail    string `json:"user_email"`
			UserName     string `json:"user_name"`
			RepositoryID string `json:"repository_id"`
			RepoURL      string `json:"repo_url"`
			CreatedAt    string `json:"created_at"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	if len(payload.Requests) == 0 {
		_, _ = fmt.Fprintln(out, "No pending access requests.")
		return nil
	}

	for _, r := range payload.Requests {
		name := r.UserEmail
		if r.UserName != "" {
			name = r.UserName + " (" + r.UserEmail + ")"
		}
		repo := r.RepoURL
		if repo == "" {
			repo = r.RepositoryID
		}
		_, _ = fmt.Fprintf(out, "  %s  %s  %s  %s\n", r.RequestID, name, repo, r.CreatedAt)
	}
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Approve: semantica workspace requests approve <request-id>")
	_, _ = fmt.Fprintln(out, "Reject:  semantica workspace requests reject <request-id>")

	return nil
}

func newWorkspaceRequestsApproveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "approve <request-id>",
		Short: "Approve a workspace access request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			requestID := args[0]

			token, err := workspaceAccessToken(ctx)
			if err != nil {
				return err
			}

			endpoint := auth.EffectiveEndpoint()
			if err := workspaceAPIPost(ctx, endpoint+"/v1/workspaces/access-requests/"+requestID+"/approve", token); err != nil {
				return fmt.Errorf("approve failed: %w", err)
			}

			_, _ = fmt.Fprintln(out, "Access request approved. The user can now run `semantica connect`.")
			return nil
		},
	}
}

func newWorkspaceRequestsRejectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reject <request-id>",
		Short: "Reject a workspace access request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			requestID := args[0]

			token, err := workspaceAccessToken(ctx)
			if err != nil {
				return err
			}

			endpoint := auth.EffectiveEndpoint()
			if err := workspaceAPIPost(ctx, endpoint+"/v1/workspaces/access-requests/"+requestID+"/reject", token); err != nil {
				return fmt.Errorf("reject failed: %w", err)
			}

			_, _ = fmt.Fprintln(out, "Access request rejected.")
			return nil
		},
	}
}

func workspaceAccessToken(ctx context.Context) (string, error) {
	token, err := auth.AccessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("authentication error: %w", err)
	}
	if token == "" {
		state := auth.GetAuthState()
		if state.Authenticated && !state.EndpointMatch {
			return "", fmt.Errorf("endpoint mismatch: credentials target a different server. Run `semantica auth login` to re-authenticate")
		}
		return "", fmt.Errorf("not authenticated. Run `semantica auth login` first")
	}
	return token, nil
}

// workspaceAPIGet makes an authenticated GET request and returns the payload JSON.
func workspaceAPIGet(ctx context.Context, url, token string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", version.UserAgent())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var envelope struct {
		Error   bool            `json:"error"`
		Message string          `json:"message"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	if envelope.Error {
		return nil, fmt.Errorf("%s", envelope.Message)
	}
	return envelope.Payload, nil
}

// workspaceAPIPost makes an authenticated POST request with no body.
func workspaceAPIPost(ctx context.Context, url, token string) error {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(nil))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", version.UserAgent())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var envelope struct {
		Error   bool   `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(respBody, &envelope) == nil && envelope.Error {
		return fmt.Errorf("%s", envelope.Message)
	}
	return nil
}
