package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestConnectRepo_Success(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_API_KEY", "test-key")
	t.Setenv("XDG_CONFIG_HOME", dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/repos/connect" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(apiResponse[ConnectRepoResponse]{
			Payload: ConnectRepoResponse{
				Outcome:      "connected",
				RepositoryID: "repo-123",
				Message:      "Repository connected",
			},
		})
	}))
	defer srv.Close()
	t.Setenv("SEMANTICA_ENDPOINT", srv.URL)

	resp, err := ConnectRepo(context.Background(), "https://github.com/org/repo", "github")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Outcome != "connected" {
		t.Errorf("outcome = %q, want connected", resp.Outcome)
	}
	if resp.RepositoryID != "repo-123" {
		t.Errorf("repository_id = %q, want repo-123", resp.RepositoryID)
	}
}

func TestConnectRepo_MissingProviderIdentity_WithAuthURL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_API_KEY", "test-key")
	t.Setenv("XDG_CONFIG_HOME", dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(apiResponse[ConnectRepoResponse]{
			Payload: ConnectRepoResponse{
				Outcome:  "missing_provider_identity",
				Message:  "No linked GitLab identity",
				Provider: "gitlab",
				AuthURL:  "https://gitlab.com/oauth/authorize?...",
				State:    "state-abc",
			},
		})
	}))
	defer srv.Close()
	t.Setenv("SEMANTICA_ENDPOINT", srv.URL)

	resp, err := ConnectRepo(context.Background(), "https://gitlab.com/group/project", "gitlab")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Outcome != "missing_provider_identity" {
		t.Errorf("outcome = %q, want missing_provider_identity", resp.Outcome)
	}
	if resp.AuthURL == "" {
		t.Error("expected auth_url in response")
	}
	if resp.State == "" {
		t.Error("expected state in response")
	}
}

func TestConnectRepo_Unauthorized(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_API_KEY", "bad-key")
	t.Setenv("XDG_CONFIG_HOME", dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	t.Setenv("SEMANTICA_ENDPOINT", srv.URL)

	_, err := ConnectRepo(context.Background(), "https://github.com/org/repo", "github")
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if err.Error() != "session expired or invalid. Run `semantica auth login` first" {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestConnectRepo_ServerError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_API_KEY", "test-key")
	t.Setenv("XDG_CONFIG_HOME", dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("SEMANTICA_ENDPOINT", srv.URL)

	_, err := ConnectRepo(context.Background(), "https://github.com/org/repo", "github")
	if err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestConnectRepo_EnvelopeError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_API_KEY", "test-key")
	t.Setenv("XDG_CONFIG_HOME", dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(apiResponse[ConnectRepoResponse]{
			Error:   true,
			Message: "something went wrong",
		})
	}))
	defer srv.Close()
	t.Setenv("SEMANTICA_ENDPOINT", srv.URL)

	_, err := ConnectRepo(context.Background(), "https://github.com/org/repo", "github")
	if err == nil {
		t.Fatal("expected error when envelope.Error is true")
	}
}

func TestPollConnectRepo_Success(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_API_KEY", "test-key")
	t.Setenv("XDG_CONFIG_HOME", dir)

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount < 3 {
			_ = json.NewEncoder(w).Encode(apiResponse[ConnectRepoResponse]{
				Payload: ConnectRepoResponse{
					Error: "authorization_pending",
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(apiResponse[ConnectRepoResponse]{
			Payload: ConnectRepoResponse{
				Outcome:      "connected",
				RepositoryID: "repo-456",
				Message:      "Repository connected",
			},
		})
	}))
	defer srv.Close()
	t.Setenv("SEMANTICA_ENDPOINT", srv.URL)

	resp, err := PollConnectRepo(context.Background(), "state-xyz", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Outcome != "connected" {
		t.Errorf("outcome = %q, want connected", resp.Outcome)
	}
	if callCount < 3 {
		t.Errorf("expected at least 3 poll calls, got %d", callCount)
	}
}

func TestPollConnectRepo_Expired(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_API_KEY", "test-key")
	t.Setenv("XDG_CONFIG_HOME", dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(apiResponse[ConnectRepoResponse]{
			Payload: ConnectRepoResponse{
				Error: "expired_token",
			},
		})
	}))
	defer srv.Close()
	t.Setenv("SEMANTICA_ENDPOINT", srv.URL)

	_, err := PollConnectRepo(context.Background(), "state-xyz", 1)
	if err == nil {
		t.Fatal("expected error on expired_token")
	}
}

func TestPollConnectRepo_Unauthorized(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_API_KEY", "test-key")
	t.Setenv("XDG_CONFIG_HOME", dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	t.Setenv("SEMANTICA_ENDPOINT", srv.URL)

	_, err := PollConnectRepo(context.Background(), "state-xyz", 1)
	if err == nil {
		t.Fatal("expected error on 401")
	}
}

func TestPollConnectRepo_ProviderLinkedElsewhere(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_API_KEY", "test-key")
	t.Setenv("XDG_CONFIG_HOME", dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(apiResponse[ConnectRepoResponse]{
			Payload: ConnectRepoResponse{
				Outcome: "provider_identity_linked_elsewhere",
				Message: "This GitLab account is already linked to another Semantica user",
			},
		})
	}))
	defer srv.Close()
	t.Setenv("SEMANTICA_ENDPOINT", srv.URL)

	resp, err := PollConnectRepo(context.Background(), "state-xyz", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Outcome != "provider_identity_linked_elsewhere" {
		t.Errorf("outcome = %q, want provider_identity_linked_elsewhere", resp.Outcome)
	}
}

func TestConnectRepo_NotAuthenticated(t *testing.T) {
	disableSecureStore(t)
	t.Setenv("SEMANTICA_API_KEY", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	_, err := ConnectRepo(context.Background(), "https://github.com/org/repo", "github")
	if err == nil {
		t.Fatal("expected error when not authenticated")
	}
}

func TestPollConnectRepo_SessionExpiredMidPoll(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("SEMANTICA_API_KEY", "")
	disableSecureStore(t)

	// Save credentials that expire very soon.
	if err := SaveCredentials(&Credentials{
		AccessToken:  "tok",
		RefreshToken: "rt",
		ExpiresAt:    time.Now().Unix() - 60, // already expired
		Endpoint:     "https://unused.example.com",
	}); err != nil {
		t.Fatal(err)
	}

	// No refresh server is available, so refresh should fail with an auth error.
	_, err := PollConnectRepo(context.Background(), "state-xyz", 1)
	if err == nil {
		t.Fatal("expected error when session expired mid-poll")
	}
}
