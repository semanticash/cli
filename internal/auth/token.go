package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/semanticash/cli/internal/version"
)

// DefaultEndpoint is used when no override or stored session endpoint exists.
const DefaultEndpoint = "https://api.semantica.sh"

// EffectiveEndpoint returns the backend endpoint for auth and remote calls.
func EffectiveEndpoint() string {
	if ep := os.Getenv("SEMANTICA_ENDPOINT"); ep != "" {
		return ep
	}
	creds, err := LoadCredentials()
	if err == nil && creds != nil && creds.Endpoint != "" {
		return creds.Endpoint
	}
	return DefaultEndpoint
}

// sessionState describes the local session without network calls.
type sessionState struct {
	creds         *Credentials
	endpointMatch bool
	valid         bool // non-expired and endpoint-matched
}

// inspectSession loads the current session and checks endpoint match and expiry.
// Returns a loadErr if credential storage itself failed (locked keychain, etc.)
// as distinct from "no credentials found" (creds == nil, loadErr == nil).
func inspectSession() (sessionState, error) {
	creds, err := LoadCredentials()
	if err != nil {
		return sessionState{}, err
	}
	if creds == nil {
		return sessionState{}, nil
	}

	endpointMatch := true
	if envEP := os.Getenv("SEMANTICA_ENDPOINT"); envEP != "" && creds.Endpoint != "" && envEP != creds.Endpoint {
		endpointMatch = false
	}

	return sessionState{
		creds:         creds,
		endpointMatch: endpointMatch,
		valid:         !creds.IsExpired() && endpointMatch,
	}, nil
}

// AccessToken returns a usable access token.
// It prefers SEMANTICA_API_KEY, then the stored session, refreshing if needed.
// It returns ("", nil) when no usable credentials are available.
func AccessToken(ctx context.Context) (string, error) {
	if key := os.Getenv("SEMANTICA_API_KEY"); key != "" {
		return key, nil
	}

	ss, loadErr := inspectSession()
	if loadErr != nil {
		return "", fmt.Errorf("credential storage error: %w", loadErr)
	}
	if ss.creds == nil {
		return "", nil
	}
	if !ss.endpointMatch {
		return "", nil
	}
	if ss.valid {
		return ss.creds.AccessToken, nil
	}

	// Expired session. Try to refresh it.
	endpoint := resolveEndpoint(ss.creds)
	refreshed, err := refreshToken(ctx, endpoint, ss.creds.Email, ss.creds.RefreshToken)
	if err != nil {
		return "", fmt.Errorf("session expired, run `semantica auth login`: %w", err)
	}

	ss.creds.AccessToken = refreshed.AccessToken
	ss.creds.RefreshToken = refreshed.RefreshToken
	ss.creds.ExpiresAt = time.Now().Unix() + int64(refreshed.ExpiresIn)
	if refreshed.Email != "" {
		ss.creds.Email = refreshed.Email
	}
	if err := SaveCredentials(ss.creds); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not save refreshed credentials: %v\n", err)
	}

	return ss.creds.AccessToken, nil
}

// AuthState describes the effective local authentication state.
type AuthState struct {
	Authenticated bool   // true if auth is configured for the current endpoint
	Source        string // "api_key", "session", or ""
	Email         string // from credentials, empty for API key auth
	Endpoint      string // the endpoint this session is pinned to
	EndpointMatch bool   // true if stored endpoint matches effective endpoint
	StorageError  string // non-empty if credential storage itself failed
}

// GetAuthState reports auth state without refreshing or making network calls.
func GetAuthState() AuthState {
	if os.Getenv("SEMANTICA_API_KEY") != "" {
		return AuthState{
			Authenticated: true,
			Source:        "api_key",
			EndpointMatch: true,
		}
	}

	ss, loadErr := inspectSession()
	if loadErr != nil {
		return AuthState{
			StorageError: loadErr.Error(),
		}
	}
	if ss.creds == nil {
		return AuthState{}
	}

	return AuthState{
		Authenticated: ss.endpointMatch,
		Source:        "session",
		Email:         ss.creds.Email,
		Endpoint:      ss.creds.Endpoint,
		EndpointMatch: ss.endpointMatch,
	}
}

// IsAPIKeyAuth returns true if the token came from SEMANTICA_API_KEY.
func IsAPIKeyAuth() bool {
	return os.Getenv("SEMANTICA_API_KEY") != ""
}

// ForceRefresh refreshes the stored session and returns the new access token.
func ForceRefresh(ctx context.Context) (string, error) {
	creds, err := LoadCredentials()
	if err != nil || creds == nil {
		return "", fmt.Errorf("no credentials to refresh")
	}

	endpoint := resolveEndpoint(creds)
	refreshed, err := refreshToken(ctx, endpoint, creds.Email, creds.RefreshToken)
	if err != nil {
		return "", err
	}

	creds.AccessToken = refreshed.AccessToken
	creds.RefreshToken = refreshed.RefreshToken
	creds.ExpiresAt = time.Now().Unix() + int64(refreshed.ExpiresIn)
	if refreshed.Email != "" {
		creds.Email = refreshed.Email
	}
	if err := SaveCredentials(creds); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not save refreshed credentials: %v\n", err)
	}

	return creds.AccessToken, nil
}

// RevokeSession asks the backend to invalidate the current session.
func RevokeSession(ctx context.Context, accessToken string) error {
	creds, _ := LoadCredentials()
	endpoint := resolveEndpoint(creds)

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "DELETE", endpoint+"/v1/auth/cli", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if creds != nil && creds.RefreshToken != "" {
		req.Header.Set("X-Refresh-Token", creds.RefreshToken)
	}
	req.Header.Set("User-Agent", version.UserAgent())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("revoke request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("revoke returned status %d", resp.StatusCode)
	}
	return nil
}

// resolveEndpoint returns the endpoint for a loaded credentials set.
func resolveEndpoint(creds *Credentials) string {
	if ep := os.Getenv("SEMANTICA_ENDPOINT"); ep != "" {
		return ep
	}
	if creds != nil && creds.Endpoint != "" {
		return creds.Endpoint
	}
	return DefaultEndpoint
}

type refreshRequest struct {
	Email        string `json:"email"`
	GrantType    string `json:"grant_type"`
	RefreshToken string `json:"refresh_token"`
}

func refreshToken(ctx context.Context, endpoint, email, refreshTok string) (*TokenResponse, error) {
	body, err := json.Marshal(refreshRequest{
		Email:        email,
		GrantType:    "refresh_token",
		RefreshToken: refreshTok,
	})
	if err != nil {
		return nil, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "PUT", endpoint+"/v1/auth/cli", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", version.UserAgent())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh returned status %d", resp.StatusCode)
	}

	var envelope apiResponse[TokenResponse]
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode refresh response: %w", err)
	}
	if envelope.Error {
		return nil, fmt.Errorf("refresh error: %s", envelope.Message)
	}

	tok := envelope.Payload
	if tok.Error != "" {
		return nil, fmt.Errorf("refresh error: %s", tok.Error)
	}

	return &tok, nil
}
