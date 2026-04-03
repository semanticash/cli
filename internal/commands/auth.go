package commands

import (
	"fmt"
	"strings"
	"time"

	"github.com/manifoldco/promptui"
	"github.com/semanticash/cli/internal/auth"
	"github.com/spf13/cobra"
)

func NewAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage authentication with Semantica",
	}

	cmd.AddCommand(newAuthLoginCmd())
	cmd.AddCommand(newAuthLogoutCmd())
	cmd.AddCommand(newAuthStatusCmd())

	return cmd
}

func newAuthLoginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Authenticate with Semantica",
		RunE: func(cmd *cobra.Command, args []string) error {
			provider, err := promptProvider(cmd)
			if err != nil {
				return err
			}

			return runLogin(cmd, provider)
		},
	}
}

// promptProvider asks the user to select an OAuth provider.
func promptProvider(cmd *cobra.Command) (string, error) {
	type providerOption struct {
		Label string
		Value string
	}

	options := []providerOption{
		{Label: "GitHub", Value: "github"},
		{Label: "GitLab", Value: "gitlab"},
	}

	prompt := promptui.Select{
		Label: "Select provider",
		Items: options,
		Templates: &promptui.SelectTemplates{
			Label:    "{{ . }}:",
			Active:   "\U000025B8 {{ .Label | cyan }}",
			Inactive: "  {{ .Label }}",
			Selected: "\U00002713 {{ .Label | green }}",
		},
	}

	idx, _, err := prompt.Run()
	if err != nil {
		return "", fmt.Errorf("provider selection: %w", err)
	}

	return options[idx].Value, nil
}

// runLogin performs the OAuth login flow for the given provider.
func runLogin(cmd *cobra.Command, provider string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	endpoint := auth.EffectiveEndpoint()

	lr, err := auth.RequestLogin(ctx, endpoint, provider)
	if err != nil {
		return fmt.Errorf("start login: %w", err)
	}

	if err := auth.OpenBrowser(strings.TrimSpace(lr.URL)); err != nil {
		_, _ = fmt.Fprintf(out, "Open this URL in your browser:\n  %s\n", lr.URL)
	} else {
		_, _ = fmt.Fprintln(out, "Opening browser...")
	}

	_, _ = fmt.Fprintln(out, "Waiting for authorization...")

	tok, err := auth.PollForToken(ctx, endpoint, lr.State, 5)
	if err != nil {
		return fmt.Errorf("authorization failed: %w", err)
	}

	creds := &auth.Credentials{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    time.Now().Unix() + int64(tok.ExpiresIn),
		Email:        tok.Email,
		Endpoint:     endpoint,
	}
	if err := auth.SaveCredentials(creds); err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}

	if tok.Email != "" {
		_, _ = fmt.Fprintf(out, "Authenticated as %s\n", tok.Email)
		_, _ = fmt.Fprintln(out, "Run `semantica connect` inside a repo to start syncing.")
	} else {
		_, _ = fmt.Fprintln(out, "Authenticated successfully")
		_, _ = fmt.Fprintln(out, "Run `semantica connect` inside a repo to start syncing.")
	}

	return nil
}

func newAuthLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Log out of Semantica",
		RunE: func(cmd *cobra.Command, args []string) error {
			creds, err := auth.LoadCredentials()
			if err != nil {
				return fmt.Errorf("logout: %w", err)
			}

			// Invalidate the session on the backend if we have a token.
			if creds != nil && creds.AccessToken != "" {
				if err := auth.RevokeSession(cmd.Context(), creds.AccessToken); err != nil {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not revoke remote session: %v\n", err)
				}
			}

			if err := auth.DeleteCredentials(); err != nil {
				return fmt.Errorf("logout: %w", err)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Logged out. Run `semantica auth login` to re-authenticate.")
			return nil
		},
	}
}

func newAuthStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show authentication status",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintln(out, formatAuthState(auth.GetAuthState()))
			return nil
		},
	}
}

// formatAuthState returns a human-readable string for the effective auth state.
// Used by both `auth status` and `semantica status`.
func formatAuthState(s auth.AuthState) string {
	if s.StorageError != "" {
		return fmt.Sprintf("Authenticated: no (credential storage error: %s)", s.StorageError)
	}
	if !s.Authenticated {
		return "Authenticated: no"
	}

	if s.Source == "api_key" {
		return "Authenticated: yes (API key)"
	}

	if s.Email != "" {
		return fmt.Sprintf("Authenticated: yes (%s)", s.Email)
	}
	return "Authenticated: yes"
}
