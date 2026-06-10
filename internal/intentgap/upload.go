package intentgap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/semanticash/cli/internal/version"
)

// Producer states accepted by the intent-gap upload contract. The
// background push path currently emits transport_only rows.
const (
	ProducerStateTransportOnly = "transport_only"
	ProducerStateAnalyzed      = "analyzed"
	ProducerStateErrored       = "errored"
)

// Versions encoded in every upload. Bumping any of these invalidates
// the server's recompute, so they're pinned here as named constants
// rather than scattered string literals.
const (
	AlgorithmVersionTransport = "0.1.0-transport"
	FindingSchemaVersion      = "1"
	RedactionVersion          = "1"
)

// UploadInput is the local repo data needed for a transport-only
// upload.
type UploadInput struct {
	RepositoryID     string
	PRNumber         int32
	HeadSHA          string
	BaseSHA          string
	Provider         string
	Model            string
	ProducerDeviceID string
}

// UploadResponse is the API's success body for both 201 (fresh) and
// 200 (idempotent duplicate).
type UploadResponse struct {
	UploadID   string `json:"upload_id"`
	ReceivedAt string `json:"received_at"`
}

// UploadResult records what the upload did, so the caller can write
// activity logs and surface the outcome in doctor without re-parsing
// the HTTP response.
type UploadResult struct {
	StatusCode int
	UploadID   string
	ReceivedAt string
}

// ErrSkipped marks a clean skip, such as a disabled setting or a
// branch with no open PR.
var ErrSkipped = errors.New("intentgap: upload skipped")

// SkipReason wraps a skip cause with the ErrSkipped sentinel so
// errors.Is(err, ErrSkipped) holds for any skip outcome.
type SkipReason struct {
	Reason string
}

func (e *SkipReason) Error() string { return "intentgap: skipped: " + e.Reason }
func (e *SkipReason) Is(target error) bool {
	return target == ErrSkipped
}

// BuildTransportOnlyBody builds the request body bytes plus the
// canonical payload hash for a transport-only upload. Pure: same
// input bytes always produce the same body bytes and the same hash,
// which is what the server's recompute relies on for idempotency.
func BuildTransportOnlyBody(in UploadInput, producedAt time.Time) ([]byte, string, error) {
	hashInput := PayloadHashInput{
		RepositoryID:          in.RepositoryID,
		PRNumber:              in.PRNumber,
		HeadSHA:               in.HeadSHA,
		BaseSHA:               in.BaseSHA,
		AlgorithmVersion:      AlgorithmVersionTransport,
		PromptTemplateVersion: "",
		FindingSchemaVersion:  FindingSchemaVersion,
		RedactionVersion:      RedactionVersion,
		Provider:              in.Provider,
		Model:                 in.Model,
		ProducerState:         ProducerStateTransportOnly,
		CoverageSummary:       nil,
		Findings:              nil,
	}
	hash, _, err := ComputePayloadHash(hashInput)
	if err != nil {
		return nil, "", fmt.Errorf("compute payload hash: %w", err)
	}

	body := map[string]any{
		"repository_id":           in.RepositoryID,
		"pr_number":               in.PRNumber,
		"head_sha":                in.HeadSHA,
		"base_sha":                in.BaseSHA,
		"algorithm_version":       AlgorithmVersionTransport,
		"prompt_template_version": "",
		"finding_schema_version":  FindingSchemaVersion,
		"redaction_version":       RedactionVersion,
		"provider":                in.Provider,
		"model":                   in.Model,
		"producer_state":          ProducerStateTransportOnly,
		"producer_device_id":      in.ProducerDeviceID,
		"payload_hash":            hash,
		"coverage_summary":        map[string]any{},
		"findings":                []any{},
		"produced_at":             producedAt.UTC().Format(time.RFC3339),
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, "", fmt.Errorf("encode body: %w", err)
	}
	return encoded, hash, nil
}

// PostUpload sends the prepared body to the server's intent-gap
// findings endpoint. 200 (idempotent duplicate) and 201 (fresh row)
// are both success; anything else is an error the caller logs.
func PostUpload(
	ctx context.Context,
	httpClient *http.Client,
	endpoint, token string,
	in UploadInput,
	body []byte,
	idempotencyKey string,
) (*UploadResult, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if endpoint == "" || token == "" {
		return nil, errors.New("intentgap: missing endpoint or token")
	}

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	u := fmt.Sprintf("%s/v1/repos/%s/prs/%d/intent_gap/findings",
		endpoint, in.RepositoryID, in.PRNumber)

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", version.UserAgent())
	req.Header.Set("Idempotency-Key", idempotencyKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(snippet))
	}

	var wire struct {
		Error   bool           `json:"error"`
		Message string         `json:"message"`
		Payload UploadResponse `json:"payload"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	// The API response envelope is part of the success contract; do not
	// treat the HTTP status alone as authoritative.
	if wire.Error {
		msg := wire.Message
		if msg == "" {
			msg = "envelope error flag set without message"
		}
		return nil, fmt.Errorf("status %d envelope error: %s", resp.StatusCode, msg)
	}
	if wire.Payload.UploadID == "" {
		return nil, fmt.Errorf("status %d missing upload_id in payload", resp.StatusCode)
	}

	return &UploadResult{
		StatusCode: resp.StatusCode,
		UploadID:   wire.Payload.UploadID,
		ReceivedAt: wire.Payload.ReceivedAt,
	}, nil
}
