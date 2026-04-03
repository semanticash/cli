package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// disableSecureStore forces file-only credential storage for tests
// that need predictable file-based behavior without OS keychain interference.
func disableSecureStore(t *testing.T) {
	t.Helper()
	orig := newSecureStoreFn
	newSecureStoreFn = func() credentialStore {
		return &unavailableStore{}
	}
	t.Cleanup(func() { newSecureStoreFn = orig })
}

// unavailableStore simulates a genuinely unavailable keyring (service not running).
// Error messages must match isKeyringUnavailable() patterns for file fallback to trigger.
type unavailableStore struct{}

func (u *unavailableStore) load() (*Credentials, error) {
	return nil, fmt.Errorf("failed to connect to dbus session bus")
}
func (u *unavailableStore) save(_ *Credentials) error {
	return fmt.Errorf("failed to connect to dbus session bus")
}
func (u *unavailableStore) delete() error { return nil }

func TestAccessToken_EnvVarOverride(t *testing.T) {
	t.Setenv("SEMANTICA_API_KEY", "sk-test-key")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	tok, err := AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "sk-test-key" {
		t.Errorf("token = %q, want %q", tok, "sk-test-key")
	}
}

func TestAccessToken_NoCredentials(t *testing.T) {
	disableSecureStore(t)
	t.Setenv("SEMANTICA_API_KEY", "")
	t.Setenv("SEMANTICA_ENDPOINT", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	tok, err := AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "" {
		t.Errorf("expected empty token, got %q", tok)
	}
}

func TestAccessToken_ValidToken(t *testing.T) {
	disableSecureStore(t)
	dir := t.TempDir()
	t.Setenv("SEMANTICA_API_KEY", "")
	t.Setenv("SEMANTICA_ENDPOINT", "")
	t.Setenv("XDG_CONFIG_HOME", dir)

	if err := SaveCredentials(&Credentials{
		AccessToken:  "at-valid",
		RefreshToken: "rt-xxx",
		ExpiresAt:    time.Now().Unix() + 3600,
		Email:        "test@example.com",
		Endpoint:     "https://unused.example.com",
	}); err != nil {
		t.Fatal(err)
	}

	tok, err := AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "at-valid" {
		t.Errorf("token = %q, want %q", tok, "at-valid")
	}
}

func TestAccessToken_ExpiredTriesRefresh(t *testing.T) {
	disableSecureStore(t)
	dir := t.TempDir()
	t.Setenv("SEMANTICA_API_KEY", "")
	t.Setenv("SEMANTICA_ENDPOINT", "")
	t.Setenv("XDG_CONFIG_HOME", dir)

	// Mock server that handles refresh.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/cli" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}

		var req struct {
			Email        string `json:"email"`
			GrantType    string `json:"grant_type"`
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(400)
			return
		}

		if req.Email != "test@example.com" {
			t.Errorf("email = %q, want %q", req.Email, "test@example.com")
		}
		if req.GrantType != "refresh_token" {
			t.Errorf("grant_type = %q, want %q", req.GrantType, "refresh_token")
		}
		if req.RefreshToken != "rt-original" {
			t.Errorf("refresh_token = %q, want %q", req.RefreshToken, "rt-original")
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(apiResponse[TokenResponse]{
			Error:   false,
			Message: "Success",
			Payload: TokenResponse{
				AccessToken:  "at-refreshed",
				RefreshToken: "rt-new",
				ExpiresIn:    3600,
				Email:        "test@example.com",
			},
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	// Save expired credentials with endpoint pointing to test server.
	if err := SaveCredentials(&Credentials{
		AccessToken:  "at-expired",
		RefreshToken: "rt-original",
		ExpiresAt:    time.Now().Unix() - 60, // expired
		Email:        "test@example.com",
		Endpoint:     srv.URL,
	}); err != nil {
		t.Fatal(err)
	}

	tok, err := AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "at-refreshed" {
		t.Errorf("token = %q, want %q", tok, "at-refreshed")
	}

	// Verify credentials were saved.
	saved, err := LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if saved.AccessToken != "at-refreshed" {
		t.Errorf("saved access_token = %q, want %q", saved.AccessToken, "at-refreshed")
	}
	if saved.RefreshToken != "rt-new" {
		t.Errorf("saved refresh_token = %q, want %q", saved.RefreshToken, "rt-new")
	}
}

func TestAccessToken_RefreshFails(t *testing.T) {
	disableSecureStore(t)
	dir := t.TempDir()
	t.Setenv("SEMANTICA_API_KEY", "")
	t.Setenv("SEMANTICA_ENDPOINT", "")
	t.Setenv("XDG_CONFIG_HOME", dir)

	// Mock server that rejects refresh.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	// Save expired credentials with endpoint pointing to test server.
	if err := SaveCredentials(&Credentials{
		AccessToken:  "at-expired",
		RefreshToken: "rt-bad",
		ExpiresAt:    time.Now().Unix() - 60,
		Endpoint:     srv.URL,
	}); err != nil {
		t.Fatal(err)
	}

	_, err := AccessToken(context.Background())
	if err == nil {
		t.Fatal("expected error when refresh fails")
	}
}

func TestIsAPIKeyAuth_WithEnvVar(t *testing.T) {
	t.Setenv("SEMANTICA_API_KEY", "sk-test")
	if !IsAPIKeyAuth() {
		t.Error("expected IsAPIKeyAuth()=true when SEMANTICA_API_KEY is set")
	}
}

func TestIsAPIKeyAuth_WithoutEnvVar(t *testing.T) {
	t.Setenv("SEMANTICA_API_KEY", "")
	if IsAPIKeyAuth() {
		t.Error("expected IsAPIKeyAuth()=false when SEMANTICA_API_KEY is empty")
	}
}

func TestForceRefresh_Success(t *testing.T) {
	disableSecureStore(t)
	dir := t.TempDir()
	t.Setenv("SEMANTICA_API_KEY", "")
	t.Setenv("SEMANTICA_ENDPOINT", "")
	t.Setenv("XDG_CONFIG_HOME", dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(apiResponse[TokenResponse]{
			Payload: TokenResponse{
				AccessToken:  "at-force-refreshed",
				RefreshToken: "rt-new",
				ExpiresIn:    3600,
				Email:        "test@example.com",
			},
		})
	}))
	defer srv.Close()

	// Save credentials with endpoint pointing to test server.
	if err := SaveCredentials(&Credentials{
		AccessToken:  "at-old",
		RefreshToken: "rt-valid",
		ExpiresAt:    time.Now().Unix() + 3600,
		Email:        "test@example.com",
		Endpoint:     srv.URL,
	}); err != nil {
		t.Fatal(err)
	}

	tok, err := ForceRefresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "at-force-refreshed" {
		t.Errorf("token = %q, want %q", tok, "at-force-refreshed")
	}

	saved, err := LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if saved.AccessToken != "at-force-refreshed" {
		t.Errorf("saved access_token = %q", saved.AccessToken)
	}
	if saved.RefreshToken != "rt-new" {
		t.Errorf("saved refresh_token = %q", saved.RefreshToken)
	}
}

func TestRevokeSession_SendsRefreshTokenHeaderWhenAvailable(t *testing.T) {
	disableSecureStore(t)
	dir := t.TempDir()
	t.Setenv("SEMANTICA_API_KEY", "")
	t.Setenv("SEMANTICA_ENDPOINT", "")
	t.Setenv("XDG_CONFIG_HOME", dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/cli" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer at-valid" {
			t.Fatalf("Authorization = %q, want %q", got, "Bearer at-valid")
		}
		if got := r.Header.Get("X-Refresh-Token"); got != "rt-valid" {
			t.Fatalf("X-Refresh-Token = %q, want %q", got, "rt-valid")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := SaveCredentials(&Credentials{
		AccessToken:  "at-valid",
		RefreshToken: "rt-valid",
		ExpiresAt:    time.Now().Unix() + 3600,
		Endpoint:     srv.URL, // must match test server so resolveEndpoint sends request here
	}); err != nil {
		t.Fatal(err)
	}

	if err := RevokeSession(context.Background(), "at-valid"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestForceRefresh_NoCredentials(t *testing.T) {
	disableSecureStore(t)
	t.Setenv("SEMANTICA_API_KEY", "")
	t.Setenv("SEMANTICA_ENDPOINT", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	_, err := ForceRefresh(context.Background())
	if err == nil {
		t.Fatal("expected error when no credentials exist")
	}
}

func TestForceRefresh_ServerRejects(t *testing.T) {
	disableSecureStore(t)
	dir := t.TempDir()
	t.Setenv("SEMANTICA_API_KEY", "")
	t.Setenv("SEMANTICA_ENDPOINT", "")
	t.Setenv("XDG_CONFIG_HOME", dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	if err := SaveCredentials(&Credentials{
		AccessToken:  "at-old",
		RefreshToken: "rt-bad",
		ExpiresAt:    time.Now().Unix() + 3600,
		Email:        "test@example.com",
		Endpoint:     srv.URL,
	}); err != nil {
		t.Fatal(err)
	}

	_, err := ForceRefresh(context.Background())
	if err == nil {
		t.Fatal("expected error when server rejects refresh")
	}
}

func TestCredentials_EndpointPersisted(t *testing.T) {
	disableSecureStore(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	creds := &Credentials{
		AccessToken:  "at",
		RefreshToken: "rt",
		ExpiresAt:    time.Now().Unix() + 3600,
		Email:        "test@example.com",
		Endpoint:     "https://dev.example.com",
	}
	if err := SaveCredentials(creds); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Endpoint != "https://dev.example.com" {
		t.Errorf("endpoint = %q, want %q", loaded.Endpoint, "https://dev.example.com")
	}
}
