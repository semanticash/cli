package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/semanticash/cli/internal/version"
)

type MeResponse struct {
	Email              string `json:"email"`
	WorkspaceName      string `json:"workspace_name,omitempty"`
	WorkspaceTierCode  string `json:"workspace_tier_code,omitempty"`
	WorkspaceTierTitle string `json:"workspace_tier_title,omitempty"`
}

// Me fetches the current authenticated user's workspace summary.
func Me(ctx context.Context) (*MeResponse, error) {
	token, err := AccessToken(ctx)
	if err != nil || token == "" {
		return nil, fmt.Errorf("not authenticated")
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "GET", EffectiveEndpoint()+"/v1/auth/cli", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", version.UserAgent())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("me request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("session expired or invalid. Run `semantica auth login` first")
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("server error: status %d", resp.StatusCode)
	}

	var envelope apiResponse[MeResponse]
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode me response: %w", err)
	}
	if envelope.Error {
		return nil, fmt.Errorf("me failed: %s", envelope.Message)
	}

	return &envelope.Payload, nil
}
