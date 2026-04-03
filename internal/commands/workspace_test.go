package commands

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/auth"
	"github.com/spf13/cobra"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type errorReadCloser struct {
	err error
}

func (r errorReadCloser) Read(_ []byte) (int, error) {
	return 0, r.err
}

func (r errorReadCloser) Close() error {
	return nil
}

func withDefaultTransport(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = rt
	t.Cleanup(func() {
		http.DefaultClient.Transport = old
	})
}

func TestWorkspaceAPIGet_ReadResponseError(t *testing.T) {
	withDefaultTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       errorReadCloser{err: errors.New("boom")},
		}, nil
	}))

	_, err := workspaceAPIGet(context.Background(), "https://example.com", "tok")
	if err == nil || !strings.Contains(err.Error(), "read response") {
		t.Fatalf("workspaceAPIGet error = %v, want read response error", err)
	}
}

func TestWorkspaceAPIPost_ReadResponseError(t *testing.T) {
	withDefaultTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       errorReadCloser{err: errors.New("boom")},
		}, nil
	}))

	err := workspaceAPIPost(context.Background(), "https://example.com", "tok")
	if err == nil || !strings.Contains(err.Error(), "read response") {
		t.Fatalf("workspaceAPIPost error = %v, want read response error", err)
	}
}

func TestHandleWorkspaceConflict_KnownStatuses(t *testing.T) {
	tests := []struct {
		name    string
		resp    *auth.ConnectRepoResponse
		wantErr string
		wantOut string
	}{
		{
			name: "pending",
			resp: &auth.ConnectRepoResponse{
				WorkspaceName:         "Acme",
				ExistingRequestStatus: "pending",
			},
			wantErr: "access request pending",
			wantOut: "Your access request is still pending approval.",
		},
		{
			name: "rejected",
			resp: &auth.ConnectRepoResponse{
				WorkspaceName:         "Acme",
				ExistingRequestStatus: "rejected",
			},
			wantErr: "access request rejected",
			wantOut: "Your access request was declined.",
		},
		{
			name: "approved",
			resp: &auth.ConnectRepoResponse{
				WorkspaceName:         "Acme",
				ExistingRequestStatus: "approved",
			},
			wantErr: "access approved but not yet connected locally",
			wantOut: "Access to Acme was approved.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)

			err := handleWorkspaceConflict(cmd, tt.resp)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
			if !strings.Contains(out.String(), tt.wantOut) {
				t.Fatalf("output = %q, want substring %q", out.String(), tt.wantOut)
			}
		})
	}
}

func TestHandleWorkspaceConflict_NonInteractive(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetIn(io.NopCloser(strings.NewReader("")))

	err := handleWorkspaceConflict(cmd, &auth.ConnectRepoResponse{
		WorkspaceName:          "Acme",
		RequestAccessSupported: true,
	})
	if err == nil || !strings.Contains(err.Error(), "repository belongs to Acme") {
		t.Fatalf("error = %v, want non-interactive failure", err)
	}
	if !strings.Contains(out.String(), "Rerun `semantica connect` in an interactive terminal to request access.") {
		t.Fatalf("output = %q, want rerun guidance", out.String())
	}
}
