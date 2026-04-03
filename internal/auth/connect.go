package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/semanticash/cli/internal/version"
)

// ConnectRepoResponse is the structured response from POST /v1/repos/connect
// and POST /v1/repos/connect/poll.
type ConnectRepoResponse struct {
	Outcome               string `json:"outcome"`
	RepositoryID          string `json:"repository_id,omitempty"`
	Message               string `json:"message"`
	GithubAppRecommended  bool   `json:"github_app_recommended,omitempty"`
	GitlabWebhookRequired bool   `json:"gitlab_webhook_required,omitempty"`
	GitlabWebhookURL      string `json:"gitlab_webhook_url,omitempty"`
	GitlabWebhookSecret   string `json:"gitlab_webhook_secret,omitempty"`
	GitlabWebhookReason   string `json:"gitlab_webhook_reason,omitempty"`
	Provider              string `json:"provider,omitempty"`
	AuthURL               string `json:"auth_url,omitempty"`
	State                 string `json:"state,omitempty"`
	Error                 string `json:"error,omitempty"`

	// Conflict metadata (repo_belongs_to_other_workspace only).
	WorkspaceName          string `json:"workspace_name,omitempty"`
	RequestAccessSupported bool   `json:"request_access_supported,omitempty"`
	ExistingRequestStatus  string `json:"existing_request_status,omitempty"`
}

type connectRepoRequest struct {
	RemoteURL string `json:"remote_url"`
	Provider  string `json:"provider,omitempty"`
}

type connectPollRequest struct {
	State string `json:"state"`
}

// ConnectRepo calls POST /v1/repos/connect and decodes the structured outcome.
func ConnectRepo(ctx context.Context, remoteURL, provider string) (*ConnectRepoResponse, error) {
	token, err := AccessToken(ctx)
	if err != nil || token == "" {
		return nil, fmt.Errorf("not authenticated")
	}

	endpoint := EffectiveEndpoint()
	body, err := json.Marshal(connectRepoRequest{RemoteURL: remoteURL, Provider: provider})
	if err != nil {
		return nil, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "POST", endpoint+"/v1/repos/connect", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", version.UserAgent())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("session expired or invalid. Run `semantica auth login` first")
	}

	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("server error: status %d", resp.StatusCode)
	}

	var envelope apiResponse[ConnectRepoResponse]
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode connect response: %w", err)
	}
	if envelope.Error {
		return nil, fmt.Errorf("connect failed: %s", envelope.Message)
	}

	return &envelope.Payload, nil
}

// PollConnectRepo calls POST /v1/repos/connect/poll until the flow completes.
func PollConnectRepo(ctx context.Context, state string, interval int) (*ConnectRepoResponse, error) {
	if interval <= 0 {
		interval = 3
	}

	endpoint := EffectiveEndpoint()
	deadline := time.After(10 * time.Minute)

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			return nil, fmt.Errorf("connect timed out")
		case <-time.After(time.Duration(interval) * time.Second):
		}

		// Re-check the token each iteration in case the session changed.
		token, err := AccessToken(ctx)
		if err != nil || token == "" {
			return nil, fmt.Errorf("session expired. Run `semantica auth login` first")
		}

		body, err := json.Marshal(connectPollRequest{State: state})
		if err != nil {
			return nil, err
		}

		reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, "POST", endpoint+"/v1/repos/connect/poll", bytes.NewReader(body))
		if err != nil {
			cancel()
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("User-Agent", version.UserAgent())

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("poll request failed: %w", err)
		}

		statusCode := resp.StatusCode
		var envelope apiResponse[ConnectRepoResponse]
		decodeErr := json.NewDecoder(resp.Body).Decode(&envelope)
		_ = resp.Body.Close()
		cancel()

		if statusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("session expired. Run `semantica auth login` first")
		}

		if decodeErr != nil {
			return nil, fmt.Errorf("decode poll response: %w", decodeErr)
		}

		if envelope.Error {
			return nil, fmt.Errorf("connect failed: %s", envelope.Message)
		}

		result := envelope.Payload

		switch result.Error {
		case "authorization_pending":
			continue
		case "expired_token":
			return nil, fmt.Errorf("connect authorization expired - run `semantica connect` again")
		case "":
			return &result, nil
		default:
			return nil, fmt.Errorf("unexpected error from server: %s", result.Error)
		}
	}
}

// AccessRequestResponse is the response from POST /v1/workspaces/access-requests.
type AccessRequestResponse struct {
	WorkspaceName string `json:"workspace_name"`
}

// RequestWorkspaceAccess creates an access request for the repo's workspace.
func RequestWorkspaceAccess(ctx context.Context, remoteURL, repositoryID string) (*AccessRequestResponse, error) {
	token, err := AccessToken(ctx)
	if err != nil || token == "" {
		return nil, fmt.Errorf("not authenticated")
	}

	endpoint := EffectiveEndpoint()

	type requestBody struct {
		RemoteURL    string `json:"remote_url,omitempty"`
		RepositoryID string `json:"repository_id,omitempty"`
	}
	body, err := json.Marshal(requestBody{RemoteURL: remoteURL, RepositoryID: repositoryID})
	if err != nil {
		return nil, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "POST", endpoint+"/v1/workspaces/access-requests", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", version.UserAgent())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("access request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("session expired. Run `semantica auth login` first")
	}

	var envelope apiResponse[AccessRequestResponse]
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if envelope.Error {
		return nil, fmt.Errorf("access request failed: %s", envelope.Message)
	}
	if resp.StatusCode >= 300 {
		if envelope.Message != "" {
			return nil, fmt.Errorf("access request failed: %s", envelope.Message)
		}
		return nil, fmt.Errorf("access request failed: status %d", resp.StatusCode)
	}

	return &envelope.Payload, nil
}
