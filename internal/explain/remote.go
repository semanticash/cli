package explain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/auth"
	"github.com/semanticash/cli/internal/service"
	"github.com/semanticash/cli/internal/util"
	"github.com/semanticash/cli/internal/version"
)

// remoteProvenanceTimeout caps how long the remote fetch waits.
// The skill is invoked interactively from an agent session, so a
// slow API should fall through to git-only fast rather than block
// the agent on a stalled connection.
const remoteProvenanceTimeout = 10 * time.Second

// remoteProvenance attempts to populate human_text from the
// workspace API. Returns the formatted text on a hit and an empty
// string plus a FallbackReason when no remote playbook is
// available. The reason values map directly to SKILL.md footers
// so the agent's response is honest about whether the workspace
// was queried, reachable, or had no playbook for this commit.
//
//   - remote_not_attempted: user is not connected, settings are
//     missing/incomplete, or no auth token is available.
//   - not_in_remote: API returned 404 or 200 with no playbook.
//     The API does not distinguish commit-absent from no-playbook,
//     so callers only claim that no remote playbook was available.
//   - remote_unavailable: API returned a transient error (5xx),
//     refused authorization mid-call (401/403), or the request
//     never completed (network error, timeout, malformed body).
func remoteProvenance(ctx context.Context, repoPath, commitHash string) (string, FallbackReason) {
	settings, err := util.ReadSettings(filepath.Join(repoPath, ".semantica"))
	if err != nil {
		return "", FallbackRemoteNotAttempted
	}
	if !settings.Connected || settings.ConnectedRepoID == "" {
		return "", FallbackRemoteNotAttempted
	}

	token, err := auth.AccessToken(ctx)
	if err != nil || token == "" {
		return "", FallbackRemoteNotAttempted
	}

	return fetchRemoteProvenance(ctx, auth.EffectiveEndpoint(),
		settings.ConnectedRepoID, commitHash, token)
}

// fetchRemoteProvenance is the pure HTTP layer: it makes the GET
// against /v1/repos/{id}/commits/{sha}/playbook, decodes the
// envelope, and maps responses to the FallbackReason enum. Split
// out from remoteProvenance so tests can drive the HTTP branches
// with httptest.NewServer without setting up settings.json or
// auth credentials.
func fetchRemoteProvenance(ctx context.Context, endpoint, repoID, commitHash, token string) (string, FallbackReason) {
	reqCtx, cancel := context.WithTimeout(ctx, remoteProvenanceTimeout)
	defer cancel()

	url := fmt.Sprintf("%s/v1/repos/%s/commits/%s/playbook",
		strings.TrimRight(endpoint, "/"), repoID, commitHash)
	req, err := http.NewRequestWithContext(reqCtx, "GET", url, nil)
	if err != nil {
		return "", FallbackRemoteUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", version.UserAgent())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", FallbackRemoteUnavailable
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		// Decoded below.
	case http.StatusNotFound:
		return "", FallbackNotInRemote
	default:
		// 401/403 (auth flap mid-call), 5xx, and anything else go
		// to remote_unavailable. Auth-token-missing is filtered out
		// before this function runs, so 401 here means the API
		// rejected an otherwise-valid-looking token; from the
		// SKILL.md body's perspective it's "tried, did not work."
		return "", FallbackRemoteUnavailable
	}

	var envelope struct {
		Error   bool                  `json:"error"`
		Message string                `json:"message"`
		Payload commitPlaybookPayload `json:"payload"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return "", FallbackRemoteUnavailable
	}
	playbookEmpty := len(envelope.Payload.Playbook) == 0 ||
		bytes.Equal(envelope.Payload.Playbook, []byte("null"))
	if envelope.Error || playbookEmpty {
			// Treat empty playbooks like 404: the safe user-facing
			// claim is that no remote playbook was available, not
			// that no remote capture exists.
		return "", FallbackNotInRemote
	}

	var narrative service.NarrativeResultJSON
	if err := json.Unmarshal(envelope.Payload.Playbook, &narrative); err != nil {
		return "", FallbackRemoteUnavailable
	}
	return formatRemoteProvenance(envelope.Payload.CommitSHA,
		envelope.Payload.CommitSubject, &narrative), ""
}

// commitPlaybookPayload mirrors the API's CommitPlaybookResponse
// shape. The Playbook field is RawMessage so we decode it lazily
// with the existing service.NarrativeResultJSON shape; if the
// upload pipeline ever changes the playbook schema, the json
// unmarshal in fetchRemoteProvenance fails closed and we surface
// remote_unavailable rather than rendering partial content.
type commitPlaybookPayload struct {
	CommitSHA     string          `json:"commit_sha"`
	CommitSubject string          `json:"commit_subject,omitempty"`
	Playbook      json.RawMessage `json:"playbook"`
	GeneratedAt   string          `json:"generated_at,omitempty"`
}

// formatRemoteProvenance renders the API playbook into the same
// shape the local-provenance formatter uses: short hash + subject
// header, followed by the [Playbook] block from writeSummary. The
// remote response carries less metadata than the local service
// (no AI involvement counts, no top files, no per-session breakdown),
// so the rendered output is deliberately shorter.
func formatRemoteProvenance(sha, subject string, narrative *service.NarrativeResultJSON) string {
	var b strings.Builder
	if subject == "" {
		subject = "(no subject)"
	}
	fmt.Fprintf(&b, "Commit %s - %s\n", shortHash(sha), subject)
	writeSummary(&b, narrative)
	return strings.TrimRight(b.String(), "\n")
}
