package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRequestLogin_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if !strings.HasPrefix(r.URL.Path, "/v1/auth/github") {
			t.Errorf("path = %s, want /v1/auth/github", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(apiResponse[LoginResponse]{
			Error:   false,
			Message: "OK",
			Payload: LoginResponse{
				URL:   "https://github.com/login/oauth/authorize?state=abc123",
				State: "abc123",
			},
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	resp, err := RequestLogin(context.Background(), srv.URL, "github")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.URL != "https://github.com/login/oauth/authorize?state=abc123" {
		t.Errorf("URL = %q, want %q", resp.URL, "https://github.com/login/oauth/authorize?state=abc123")
	}
	if resp.State != "abc123" {
		t.Errorf("State = %q, want %q", resp.State, "abc123")
	}
}

func TestRequestLogin_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := RequestLogin(context.Background(), srv.URL, "github")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %q, expected it to contain '500'", err.Error())
	}
}

func TestPollForToken_ImmediateSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/auth/cli" {
			t.Errorf("path = %s, want /v1/auth/cli", r.URL.Path)
		}

		var req tokenPollRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		if req.State != "test-state" {
			t.Errorf("state = %q, want %q", req.State, "test-state")
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(apiResponse[TokenResponse]{
			Error:   false,
			Message: "OK",
			Payload: TokenResponse{
				AccessToken:  "at-fresh",
				RefreshToken: "rt-fresh",
				ExpiresIn:    3600,
				Email:        "user@example.com",
			},
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	tok, err := PollForToken(context.Background(), srv.URL, "test-state", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok.AccessToken != "at-fresh" {
		t.Errorf("AccessToken = %q, want %q", tok.AccessToken, "at-fresh")
	}
	if tok.RefreshToken != "rt-fresh" {
		t.Errorf("RefreshToken = %q, want %q", tok.RefreshToken, "rt-fresh")
	}
	if tok.ExpiresIn != 3600 {
		t.Errorf("ExpiresIn = %d, want %d", tok.ExpiresIn, 3600)
	}
	if tok.Email != "user@example.com" {
		t.Errorf("Email = %q, want %q", tok.Email, "user@example.com")
	}
}

func TestPollForToken_PendingThenSuccess(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")

		if n == 1 {
			// First call: authorization pending.
			if err := json.NewEncoder(w).Encode(apiResponse[TokenResponse]{
				Payload: TokenResponse{
					Error: "authorization_pending",
				},
			}); err != nil {
				t.Errorf("encode response: %v", err)
			}
			return
		}

		// Second call: success.
		if err := json.NewEncoder(w).Encode(apiResponse[TokenResponse]{
			Payload: TokenResponse{
				AccessToken:  "at-after-pending",
				RefreshToken: "rt-after-pending",
				ExpiresIn:    7200,
				Email:        "pending@example.com",
			},
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	tok, err := PollForToken(context.Background(), srv.URL, "test-state", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok.AccessToken != "at-after-pending" {
		t.Errorf("AccessToken = %q, want %q", tok.AccessToken, "at-after-pending")
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("server called %d times, want 2", got)
	}
}

func TestPollForToken_Expired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(apiResponse[TokenResponse]{
			Payload: TokenResponse{
				Error: "expired_token",
			},
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	_, err := PollForToken(context.Background(), srv.URL, "test-state", 1)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("error = %q, expected it to contain 'expired'", err.Error())
	}
}
