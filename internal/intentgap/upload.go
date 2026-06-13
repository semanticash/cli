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

// Producer states accepted by the intent-gap upload contract.
const (
	ProducerStateTransportOnly = "transport_only"
	ProducerStateAnalyzed      = "analyzed"
	ProducerStateErrored       = "errored"
)

// Versions encoded in uploads and canonical payload hashes.
const (
	AlgorithmVersionTransport = "0.1.0-transport"
	FindingSchemaVersion      = "1"
	RedactionVersion          = "1"
)

// UploadInput contains repository and producer metadata for an upload.
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

// UploadResult contains the accepted upload identity and HTTP status.
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

// AnalyzedBodyInput combines upload metadata with analyzer output.
type AnalyzedBodyInput struct {
	UploadInput
	PromptTemplateVersion string
	Findings              json.RawMessage
	CoverageSummary       json.RawMessage
}

// BuildAnalyzedBody builds an analyzed request and its canonical hash.
func BuildAnalyzedBody(in AnalyzedBodyInput, producedAt time.Time) ([]byte, string, error) {
	return buildUploadBody(buildBodyOpts{
		Upload:                in.UploadInput,
		ProducerState:         ProducerStateAnalyzed,
		AlgorithmVersion:      AlgorithmVersionAnalyzed,
		PromptTemplateVersion: in.PromptTemplateVersion,
		Findings:              in.Findings,
		CoverageSummary:       in.CoverageSummary,
		ProducedAt:            producedAt,
	})
}

// BuildErroredBody builds an errored request with no findings.
//
// Errored rows let the server's materializer keep showing the prior
// analyzed verdict on the check (it filters errored upstream) while
// dashboard surfaces still see the failure entry for diagnostics.
func BuildErroredBody(in UploadInput, reason, promptTemplateVersion string, producedAt time.Time) ([]byte, string, error) {
	coverage := map[string]any{"error_reason": reason}
	coverageBytes, _ := json.Marshal(coverage)
	return buildUploadBody(buildBodyOpts{
		Upload:                in,
		ProducerState:         ProducerStateErrored,
		AlgorithmVersion:      AlgorithmVersionAnalyzed,
		PromptTemplateVersion: promptTemplateVersion,
		Findings:              nil,
		CoverageSummary:       coverageBytes,
		ProducedAt:            producedAt,
	})
}

// buildBodyOpts contains shared body-builder inputs.
type buildBodyOpts struct {
	Upload                UploadInput
	ProducerState         string
	AlgorithmVersion      string
	PromptTemplateVersion string
	Findings              json.RawMessage
	CoverageSummary       json.RawMessage
	ProducedAt            time.Time
}

// buildUploadBody computes the canonical hash and encodes the wire body.
func buildUploadBody(o buildBodyOpts) ([]byte, string, error) {
	hashInput := PayloadHashInput{
		RepositoryID:          o.Upload.RepositoryID,
		PRNumber:              o.Upload.PRNumber,
		HeadSHA:               o.Upload.HeadSHA,
		BaseSHA:               o.Upload.BaseSHA,
		AlgorithmVersion:      o.AlgorithmVersion,
		PromptTemplateVersion: o.PromptTemplateVersion,
		FindingSchemaVersion:  FindingSchemaVersion,
		RedactionVersion:      RedactionVersion,
		Provider:              o.Upload.Provider,
		Model:                 o.Upload.Model,
		ProducerState:         o.ProducerState,
		CoverageSummary:       o.CoverageSummary,
		Findings:              o.Findings,
	}
	hash, _, err := ComputePayloadHash(hashInput)
	if err != nil {
		return nil, "", fmt.Errorf("compute payload hash: %w", err)
	}

	// Empty coverage / findings serialize as {} / [] so the wire shape
	// matches what the canonical hash collapsed nil to.
	cov := emptyJSONOr(o.CoverageSummary, "{}")
	findings := emptyJSONOr(o.Findings, "[]")

	body := map[string]any{
		"repository_id":           o.Upload.RepositoryID,
		"pr_number":               o.Upload.PRNumber,
		"head_sha":                o.Upload.HeadSHA,
		"base_sha":                o.Upload.BaseSHA,
		"algorithm_version":       o.AlgorithmVersion,
		"prompt_template_version": o.PromptTemplateVersion,
		"finding_schema_version":  FindingSchemaVersion,
		"redaction_version":       RedactionVersion,
		"provider":                o.Upload.Provider,
		"model":                   o.Upload.Model,
		"producer_state":          o.ProducerState,
		"producer_device_id":      o.Upload.ProducerDeviceID,
		"payload_hash":            hash,
		"coverage_summary":        json.RawMessage(cov),
		"findings":                json.RawMessage(findings),
		"produced_at":             o.ProducedAt.UTC().Format(time.RFC3339),
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, "", fmt.Errorf("encode body: %w", err)
	}
	return encoded, hash, nil
}

// emptyJSONOr normalizes empty JSON fields to their wire defaults.
func emptyJSONOr(raw json.RawMessage, fallback string) []byte {
	if len(raw) == 0 {
		return []byte(fallback)
	}
	return []byte(raw)
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

// PostUpload sends a prepared body. Both fresh and duplicate responses
// are successful outcomes.
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
