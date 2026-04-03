package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMe_Success(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_API_KEY", "test-key")
	t.Setenv("XDG_CONFIG_HOME", dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/cli" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(apiResponse[MeResponse]{
			Payload: MeResponse{
				Email:              "dev@example.com",
				WorkspaceName:      "Acme",
				WorkspaceTierCode:  "free",
				WorkspaceTierTitle: "Free",
			},
		})
	}))
	defer srv.Close()
	t.Setenv("SEMANTICA_ENDPOINT", srv.URL)

	resp, err := Me(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.WorkspaceTierTitle != "Free" {
		t.Fatalf("workspace_tier_title = %q, want Free", resp.WorkspaceTierTitle)
	}
}

func TestMe_Unauthorized(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEMANTICA_API_KEY", "test-key")
	t.Setenv("XDG_CONFIG_HOME", dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	t.Setenv("SEMANTICA_ENDPOINT", srv.URL)

	_, err := Me(context.Background())
	if err == nil {
		t.Fatal("expected error on 401")
	}
}
