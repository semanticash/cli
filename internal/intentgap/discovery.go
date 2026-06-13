package intentgap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/semanticash/cli/internal/version"
)

// OpenPR contains the PR fields needed for local analysis.
type OpenPR struct {
	PRNumber   int32  `json:"pr_number"`
	State      string `json:"state"`
	Title      string `json:"title,omitempty"`
	HeadSHA    string `json:"head_sha,omitempty"`
	HeadBranch string `json:"head_branch,omitempty"`
	BaseBranch string `json:"base_branch,omitempty"`
	IsDraft    bool   `json:"is_draft"`
}

// Discovery errors distinguish missing, ambiguous, and unavailable PR context.
var (
	ErrNoOpenPR    = errors.New("intentgap: no open PR for branch")
	ErrAmbiguousPR = errors.New("intentgap: multiple open PRs for branch")
	ErrUnavailable = errors.New("intentgap: PR-context discovery server unavailable")
)

// AmbiguousPRError contains all open PRs matching a branch.
type AmbiguousPRError struct {
	Matches []OpenPR
}

func (e *AmbiguousPRError) Error() string {
	return fmt.Sprintf("intentgap: %d open PRs match branch", len(e.Matches))
}

func (e *AmbiguousPRError) Is(target error) bool {
	return target == ErrAmbiguousPR
}

// findOpenPRsResponse mirrors the API response envelope.
type findOpenPRsResponse struct {
	Error   bool   `json:"error"`
	Message string `json:"message"`
	Payload struct {
		PullRequests []OpenPR `json:"pull_requests"`
	} `json:"payload"`
}

// LookupOpenPRByBranch returns the single open PR for a short branch name.
// It uses http.DefaultClient when httpClient is nil and applies a 10-second timeout.
func LookupOpenPRByBranch(
	ctx context.Context,
	httpClient *http.Client,
	endpoint, token, repoID, branch string,
) (*OpenPR, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if endpoint == "" || token == "" || repoID == "" || branch == "" {
		return nil, fmt.Errorf("%w: missing required parameter", ErrUnavailable)
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	u := fmt.Sprintf("%s/v1/repos/%s/prs/by-head-branch?branch=%s",
		endpoint, url.PathEscape(repoID), url.QueryEscape(branch))

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrUnavailable, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", version.UserAgent())
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: auth %d", ErrUnavailable, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%w: status %d body=%s", ErrUnavailable, resp.StatusCode, string(body))
	}

	var parsed findOpenPRsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrUnavailable, err)
	}

	switch len(parsed.Payload.PullRequests) {
	case 0:
		return nil, ErrNoOpenPR
	case 1:
		pr := parsed.Payload.PullRequests[0]
		return &pr, nil
	default:
		return nil, &AmbiguousPRError{Matches: parsed.Payload.PullRequests}
	}
}
