package intentgap

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubServer returns a test server that responds with the given status
// + body to all requests. Used to pin the four discovery outcomes
// without hitting the real API.
func stubServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sanity: callers must send auth + a branch query.
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("Authorization header missing or malformed: %q", got)
		}
		if r.URL.Query().Get("branch") == "" {
			t.Errorf("branch query param missing")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

const fakeRepoID = "11111111-2222-3333-4444-555555555555"
const fakeToken = "tok-x"

// Exactly-one match returns the PR. This is the happy path the
// pre-push handler proceeds on.
func TestLookupOpenPRByBranch_SingleMatch(t *testing.T) {
	body := `{"error":false,"message":"ok","payload":{"pull_requests":[{"pr_number":42,"state":"open","head_sha":"abc","head_branch":"feat/x"}]}}`
	srv := stubServer(t, http.StatusOK, body)
	defer srv.Close()

	pr, err := LookupOpenPRByBranch(context.Background(), srv.Client(), srv.URL, fakeToken, fakeRepoID, "feat/x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pr == nil || pr.PRNumber != 42 {
		t.Fatalf("expected PR #42, got %#v", pr)
	}
}

// Zero matches return ErrNoOpenPR (sentinel; caller writes doctor
// note "no open PR known for branch yet").
func TestLookupOpenPRByBranch_NoMatch(t *testing.T) {
	body := `{"error":false,"message":"ok","payload":{"pull_requests":[]}}`
	srv := stubServer(t, http.StatusOK, body)
	defer srv.Close()

	pr, err := LookupOpenPRByBranch(context.Background(), srv.Client(), srv.URL, fakeToken, fakeRepoID, "feat/x")
	if pr != nil {
		t.Fatalf("expected nil PR, got %#v", pr)
	}
	if !errors.Is(err, ErrNoOpenPR) {
		t.Fatalf("expected ErrNoOpenPR, got: %v", err)
	}
}

// Multiple matches return an AmbiguousPRError that carries the matches
// list and reports as ErrAmbiguousPR via errors.Is so callers can
// switch on the sentinel.
func TestLookupOpenPRByBranch_AmbiguousMatch(t *testing.T) {
	body := `{"error":false,"message":"ok","payload":{"pull_requests":[
		{"pr_number":42,"state":"open","head_branch":"feat/x"},
		{"pr_number":57,"state":"open","head_branch":"feat/x"}
	]}}`
	srv := stubServer(t, http.StatusOK, body)
	defer srv.Close()

	pr, err := LookupOpenPRByBranch(context.Background(), srv.Client(), srv.URL, fakeToken, fakeRepoID, "feat/x")
	if pr != nil {
		t.Fatalf("expected nil PR on ambiguity, got %#v", pr)
	}
	if !errors.Is(err, ErrAmbiguousPR) {
		t.Fatalf("expected ErrAmbiguousPR, got: %v", err)
	}
	var ambig *AmbiguousPRError
	if !errors.As(err, &ambig) {
		t.Fatalf("expected AmbiguousPRError type, got: %T", err)
	}
	if len(ambig.Matches) != 2 {
		t.Errorf("Matches length = %d, want 2", len(ambig.Matches))
	}
}

// Server failures are reported as unavailable discovery.
func TestLookupOpenPRByBranch_ServerError(t *testing.T) {
	srv := stubServer(t, http.StatusInternalServerError, `{"error":true,"message":"boom"}`)
	defer srv.Close()

	_, err := LookupOpenPRByBranch(context.Background(), srv.Client(), srv.URL, fakeToken, fakeRepoID, "feat/x")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got: %v", err)
	}
}

// Authentication failures are reported as unavailable discovery.
func TestLookupOpenPRByBranch_Unauthorized(t *testing.T) {
	srv := stubServer(t, http.StatusUnauthorized, `{"error":true,"message":"nope"}`)
	defer srv.Close()

	_, err := LookupOpenPRByBranch(context.Background(), srv.Client(), srv.URL, fakeToken, fakeRepoID, "feat/x")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got: %v", err)
	}
}
