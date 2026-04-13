package provenance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/version"
)

// UploadResult captures the outcome of uploading a single turn.
type UploadResult struct {
	TurnID     string
	ManifestID string
	Uploaded   bool
	Action     ManifestAction // What happened: ActionUploaded, ActionRetry, or ActionFail.
	Err        error
}

// prepareObject is the shape sent to POST /v1/provenance/prepare.
type prepareObject struct {
	Kind      string `json:"kind"`
	Hash      string `json:"hash"`
	SizeBytes int    `json:"size_bytes"`
}

// prepareRequestBody is the request body for POST /v1/provenance/prepare.
type prepareRequestBody struct {
	ConnectedRepoID string          `json:"connected_repo_id"`
	Objects         []prepareObject `json:"objects"`
}

// prepareUploadEntry is a single object that needs uploading.
type prepareUploadEntry struct {
	Kind         string `json:"kind"`
	Hash         string `json:"hash"`
	PresignedURL string `json:"presigned_url"`
}

// prepareSkipEntry is a single object already present on the backend.
type prepareSkipEntry struct {
	Kind string `json:"kind"`
	Hash string `json:"hash"`
}

// preparePayload is the response payload from POST /v1/provenance/prepare.
type preparePayload struct {
	Uploads []prepareUploadEntry `json:"uploads"`
	Skip    []prepareSkipEntry   `json:"skip"`
}

// apiEnvelope wraps all backend responses.
type apiEnvelope[T any] struct {
	Error   bool   `json:"error"`
	Message string `json:"message"`
	Payload T      `json:"payload"`
}

// UploadTurn performs the full upload cycle for a prepared sync result:
// prepare -> S3 PUT -> complete.
func UploadTurn(ctx context.Context, endpoint, token string, result SyncResult) UploadResult {
	out := UploadResult{
		TurnID:     result.TurnID,
		ManifestID: result.ManifestID,
	}

	// Parse the envelope to extract connected_repo_id and objects.
	var env syncEnvelope
	if err := json.Unmarshal(result.Envelope, &env); err != nil {
		out.Err = fmt.Errorf("parse envelope: %w", err)
		return out
	}

	// POST /v1/provenance/prepare.
	prepareBody := prepareRequestBody{
		ConnectedRepoID: env.ConnectedRepoID,
	}
	for _, obj := range env.Objects {
		prepareBody.Objects = append(prepareBody.Objects, prepareObject(obj))
	}

	prepared, err := callPrepare(ctx, endpoint, token, prepareBody)
	if err != nil {
		out.Err = fmt.Errorf("prepare: %w", err)
		return out
	}

	// PUT blobs to presigned S3 URLs in parallel.
	type putResult struct {
		kind, hash string
		err        error
	}
	putCh := make(chan putResult, len(prepared.Uploads))
	for _, upload := range prepared.Uploads {
		blob, ok := result.RedactedBlobs[upload.Hash]
		if !ok {
			out.Err = fmt.Errorf("blob %s/%s not found in redacted blobs", upload.Kind, upload.Hash)
			return out
		}
		go func(kind, hash, url string, data []byte) {
			putCh <- putResult{kind, hash, putS3Blob(ctx, url, data)}
		}(upload.Kind, upload.Hash, upload.PresignedURL, blob)
	}
	for range prepared.Uploads {
		pr := <-putCh
		if pr.err != nil {
			out.Err = fmt.Errorf("S3 PUT %s/%s: %w", pr.kind, pr.hash, pr.err)
			return out
		}
	}

	// POST /v1/provenance/complete with the full envelope.
	if err := callComplete(ctx, endpoint, token, result.Envelope); err != nil {
		out.Err = fmt.Errorf("complete: %w", err)
		return out
	}

	out.Uploaded = true
	return out
}

// callPrepare sends POST /v1/provenance/prepare and parses the response.
func callPrepare(ctx context.Context, endpoint, token string, body prepareRequestBody) (*preparePayload, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint+"/v1/provenance/prepare", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	setHeaders(req, token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		msg := drainBody(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}

	var envelope apiEnvelope[preparePayload]
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode prepare response: %w", err)
	}
	if envelope.Error {
		return nil, fmt.Errorf("prepare rejected: %s", envelope.Message)
	}

	return &envelope.Payload, nil
}

// putS3Blob uploads a blob to a presigned S3 URL via PUT.
func putS3Blob(ctx context.Context, presignedURL string, blob []byte) error {
	req, err := http.NewRequestWithContext(ctx, "PUT", presignedURL, bytes.NewReader(blob))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(blob))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		body := make([]byte, 1024)
		n, _ := resp.Body.Read(body)
		return fmt.Errorf("S3 PUT returned %d: %s", resp.StatusCode, string(body[:n]))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// callComplete sends POST /v1/provenance/complete with the envelope body.
func callComplete(ctx context.Context, endpoint, token string, envelope []byte) error {
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint+"/v1/provenance/complete", bytes.NewReader(envelope))
	if err != nil {
		return err
	}
	setHeaders(req, token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return errUnauthorized
	}
	if resp.StatusCode == http.StatusConflict {
		return fmt.Errorf("turn already registered with different identity: %w", errTerminal)
	}
	if resp.StatusCode != http.StatusOK {
		msg := drainBody(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}

	var envelope_ apiEnvelope[json.RawMessage]
	if err := json.NewDecoder(resp.Body).Decode(&envelope_); err != nil {
		return fmt.Errorf("decode complete response: %w", err)
	}
	if envelope_.Error {
		return fmt.Errorf("complete rejected: %s", envelope_.Message)
	}

	return nil
}

// setHeaders sets standard request headers.
func setHeaders(req *http.Request, token string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", version.UserAgent())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// drainBody reads up to 512 bytes from a response body for error messages.
func drainBody(body io.Reader) string {
	b := make([]byte, 512)
	n, _ := body.Read(b)
	return string(b[:n])
}

// errUnauthorized signals a 401 so the caller can attempt token refresh.
var errUnauthorized = fmt.Errorf("unauthorized")

// IsUnauthorized returns true if the error chain contains a 401 from the backend.
func IsUnauthorized(err error) bool {
	return errors.Is(err, errUnauthorized)
}

// MaxUploadAttempts is the retry cap for transient upload failures.
const MaxUploadAttempts = 5

// UploadOptions configures optional SyncAndUpload behavior.
type UploadOptions struct {
	// OnProgress is called after each manifest is processed. The arguments
	// are (current index starting at 1, total count, result for this item).
	// If nil, no progress is reported.
	OnProgress func(current, total int, result UploadResult)
}

// SyncAndUpload prepares packaged manifests and uploads them to the backend.
// It handles the full cycle: prepare blobs, claim the manifest, upload, and
// persist the resulting manifest state. It returns one UploadResult per
// manifest it processed, including local preparation failures.
func SyncAndUpload(ctx context.Context, repoRoot, endpoint, token string, watermarkTs int64, limit int, opts *UploadOptions) ([]UploadResult, error) {
	results, err := SyncPendingTurns(ctx, repoRoot, watermarkTs, limit)
	if err != nil {
		return nil, err
	}

	semDir := filepath.Join(repoRoot, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.OpenOptions{
		BusyTimeout: 100 * time.Millisecond,
		Synchronous: "NORMAL",
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = sqlstore.Close(h) }()

	total := len(results)
	processed := 0
	progress := func(ur UploadResult) {
		processed++
		if opts != nil && opts.OnProgress != nil {
			opts.OnProgress(processed, total, ur)
		}
	}

	var out []UploadResult
	for _, r := range results {
		// Surface locally-failed manifests (missing blobs) so callers can report them.
		if r.Skipped {
			ur := UploadResult{
				TurnID:     r.TurnID,
				ManifestID: r.ManifestID,
				Action:     ActionFail,
				Err:        fmt.Errorf("skipped: missing local blobs"),
			}
			out = append(out, ur)
			progress(ur)
			continue
		}

		rows, claimErr := h.Queries.MarkManifestUploading(ctx, sqldb.MarkManifestUploadingParams{
			UpdatedAt:  time.Now().UnixMilli(),
			ManifestID: r.ManifestID,
		})
		if claimErr != nil || rows == 0 {
			continue
		}

		uploadCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		ur := UploadTurn(uploadCtx, endpoint, token, r)
		cancel()

		// Auth failures reset to packaged without burning a retry attempt.
		// The caller (worker) refreshes the token and retries the batch.
		if ur.Err != nil && IsUnauthorized(ur.Err) {
			ur.Action = ActionRetry
			if err := h.Queries.ResetManifestToPackaged(ctx, sqldb.ResetManifestToPackagedParams{
				UpdatedAt:  time.Now().UnixMilli(),
				ManifestID: r.ManifestID,
			}); err != nil {
				slog.Warn("upload: reset manifest after auth failure", "turn", r.TurnID, "manifest_id", r.ManifestID, "err", err)
				ur.Action = ActionFail
				ur.Uploaded = false
				ur.Err = fmt.Errorf("reset manifest after auth failure: %w", err)
			}
			out = append(out, ur)
			progress(ur)
			continue
		}

		action := ClassifyOutcome(ur.Err, r.UploadAttempts)
		ur.Action = action

		if err := persistManifestAction(ctx, h, r.ManifestID, action, ur.Err); err != nil {
			slog.Warn("upload: persist manifest action failed", "turn", r.TurnID, "manifest_id", r.ManifestID, "action", action, "err", err)
			ur.Uploaded = false
			ur.Action = ActionFail
			if ur.Err != nil {
				ur.Err = fmt.Errorf("%v; persist manifest state: %w", ur.Err, err)
			} else {
				ur.Err = fmt.Errorf("persist manifest state: %w", err)
			}
		}

		out = append(out, ur)
		progress(ur)
	}
	return out, nil
}

// ManifestAction describes what the caller should do with a manifest after upload.
type ManifestAction int

const (
	// ActionUploaded means the upload succeeded.
	ActionUploaded ManifestAction = iota
	// ActionRetry means a transient error occurred below the retry cap.
	ActionRetry
	// ActionFail means a terminal error or retry cap exceeded.
	ActionFail
)

// ClassifyOutcome decides the manifest state transition based on the upload
// result and the manifest's current attempt count.
func ClassifyOutcome(err error, uploadAttempts int64) ManifestAction {
	if err == nil {
		return ActionUploaded
	}
	if IsTerminal(err) || uploadAttempts+1 >= MaxUploadAttempts {
		return ActionFail
	}
	return ActionRetry
}

func persistManifestAction(ctx context.Context, h *sqlstore.Handle, manifestID string, action ManifestAction, lastErr error) error {
	now := time.Now().UnixMilli()
	switch action {
	case ActionUploaded:
		return h.Queries.MarkManifestUploaded(ctx, sqldb.MarkManifestUploadedParams{
			UpdatedAt:  now,
			ManifestID: manifestID,
		})
	case ActionRetry:
		return h.Queries.ResetManifestForRetry(ctx, sqldb.ResetManifestForRetryParams{
			LastError:  sqlstore.NullStr(errString(lastErr)),
			UpdatedAt:  now,
			ManifestID: manifestID,
		})
	case ActionFail:
		return h.Queries.MarkManifestFailed(ctx, sqldb.MarkManifestFailedParams{
			LastError:  sqlstore.NullStr(errString(lastErr)),
			UpdatedAt:  now,
			ManifestID: manifestID,
		})
	default:
		return fmt.Errorf("unknown manifest action: %d", action)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// errTerminal marks errors that should not be retried.
var errTerminal = fmt.Errorf("terminal")

// IsTerminal returns true if the error should not be retried (e.g. 409
// conflict means the turn is registered under a different identity).
func IsTerminal(err error) bool {
	return errors.Is(err, errTerminal)
}
