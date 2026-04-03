package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"runtime"
	"time"

	"github.com/semanticash/cli/internal/version"
)

// apiResponse wraps all backend responses: {"error": bool, "message": string, "payload": ...}
type apiResponse[T any] struct {
	Error   bool   `json:"error"`
	Message string `json:"message"`
	Payload T      `json:"payload"`
}

// LoginResponse is returned by the backend with the OAuth URL and state.
type LoginResponse struct {
	URL   string `json:"url"`   // full OAuth authorize URL
	State string `json:"state"` // correlation key for polling
}

// TokenResponse is returned by the backend on token exchange (OAuth callback or refresh).
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Email        string `json:"email,omitempty"`
	Error        string `json:"error,omitempty"` // "authorization_pending", "expired_token"
}

// RequestLogin starts the OAuth login flow for the given provider.
func RequestLogin(ctx context.Context, endpoint, provider string) (*LoginResponse, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "GET", endpoint+"/v1/auth/"+provider+"?origin=cli", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", version.UserAgent())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("login request returned status %d", resp.StatusCode)
	}

	var envelope apiResponse[LoginResponse]
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode login response: %w", err)
	}
	if envelope.Error {
		return nil, fmt.Errorf("login failed: %s", envelope.Message)
	}

	return &envelope.Payload, nil
}

type tokenPollRequest struct {
	State string `json:"state"`
}

// PollForToken polls the backend until the user completes OAuth authorization,
// the state expires, or the context is cancelled.
func PollForToken(ctx context.Context, endpoint, state string, interval int) (*TokenResponse, error) {
	if interval <= 0 {
		interval = 5
	}

	deadline := time.After(15 * time.Minute) // hard upper bound

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			return nil, fmt.Errorf("login expired (timeout)")
		case <-time.After(time.Duration(interval) * time.Second):
		}

		body, err := json.Marshal(tokenPollRequest{State: state})
		if err != nil {
			return nil, err
		}

		reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, "POST", endpoint+"/v1/auth/cli", bytes.NewReader(body))
		if err != nil {
			cancel()
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", version.UserAgent())

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("poll request failed: %w", err)
		}

		var envelope apiResponse[TokenResponse]
		decodeErr := json.NewDecoder(resp.Body).Decode(&envelope)
		_ = resp.Body.Close()
		cancel()

		if decodeErr != nil {
			return nil, fmt.Errorf("decode poll response: %w", decodeErr)
		}

		tok := envelope.Payload

		switch tok.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5
			continue
		case "expired_token":
			return nil, fmt.Errorf("login expired - please try again")
		case "":
			// Success.
			if tok.AccessToken == "" {
				return nil, fmt.Errorf("server returned empty access token")
			}
			return &tok, nil
		default:
			return nil, fmt.Errorf("unexpected error from server: %s", tok.Error)
		}
	}
}

// OpenBrowser opens the given URL in the user's default browser.
// Best-effort - returns an error if the browser cannot be opened.
func OpenBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return fmt.Errorf("unsupported platform %s - open %s manually", runtime.GOOS, url)
	}
	return cmd.Start()
}
