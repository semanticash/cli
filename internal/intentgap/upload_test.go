package intentgap

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Same logical input must produce identical body bytes and the same
// canonical hash regardless of when it runs. This is what the
// server's recompute relies on to detect idempotent duplicates.
func TestBuildTransportOnlyBody_DeterministicHash(t *testing.T) {
	in := UploadInput{
		RepositoryID:     "11111111-2222-3333-4444-555555555555",
		PRNumber:         42,
		HeadSHA:          "deadbeefcafef00d1234567890abcdef12345678",
		BaseSHA:          "",
		Provider:         "claude_code",
		Model:            "",
		ProducerDeviceID: "host-a",
	}
	ts := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	_, hash1, err := BuildTransportOnlyBody(in, ts)
	if err != nil {
		t.Fatalf("build #1: %v", err)
	}
	_, hash2, err := BuildTransportOnlyBody(in, ts.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("build #2: %v", err)
	}
	if hash1 != hash2 {
		t.Fatalf("hash drift from produced_at change: %s vs %s", hash1, hash2)
	}
}

// Device metadata does not affect logical-payload deduplication.
func TestBuildTransportOnlyBody_DeviceIDExcludedFromHash(t *testing.T) {
	base := UploadInput{
		RepositoryID:     "11111111-2222-3333-4444-555555555555",
		PRNumber:         42,
		HeadSHA:          "deadbeefcafef00d1234567890abcdef12345678",
		Provider:         "claude_code",
		ProducerDeviceID: "host-a",
	}
	other := base
	other.ProducerDeviceID = "host-b"
	ts := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	_, hashA, err := BuildTransportOnlyBody(base, ts)
	if err != nil {
		t.Fatalf("build host-a: %v", err)
	}
	_, hashB, err := BuildTransportOnlyBody(other, ts)
	if err != nil {
		t.Fatalf("build host-b: %v", err)
	}
	if hashA != hashB {
		t.Fatalf("device_id leaked into hash: %s vs %s", hashA, hashB)
	}
}

// The body must carry all the fields the server's validator expects:
// payload_hash mirrored from the canonical recompute, producer_state
// pinned to transport_only, empty findings array, empty coverage
// object, and produced_at as RFC3339.
func TestBuildTransportOnlyBody_WireShape(t *testing.T) {
	in := UploadInput{
		RepositoryID:     "11111111-2222-3333-4444-555555555555",
		PRNumber:         42,
		HeadSHA:          "deadbeefcafef00d1234567890abcdef12345678",
		BaseSHA:          "00112233445566778899aabbccddeeff00112233",
		Provider:         "codex",
		Model:            "gpt-5",
		ProducerDeviceID: "host-a",
	}
	ts := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	body, hash, err := BuildTransportOnlyBody(in, ts)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("body is not valid JSON: %v\nbody=%s", err, body)
	}

	mustEqual(t, parsed, "producer_state", ProducerStateTransportOnly)
	mustEqual(t, parsed, "algorithm_version", AlgorithmVersionTransport)
	mustEqual(t, parsed, "finding_schema_version", FindingSchemaVersion)
	mustEqual(t, parsed, "redaction_version", RedactionVersion)
	mustEqual(t, parsed, "payload_hash", hash)
	mustEqual(t, parsed, "produced_at", "2026-06-09T12:00:00Z")
	mustEqual(t, parsed, "provider", "codex")

	findings, ok := parsed["findings"].([]any)
	if !ok || len(findings) != 0 {
		t.Errorf("findings should be empty array; got %v", parsed["findings"])
	}
	coverage, ok := parsed["coverage_summary"].(map[string]any)
	if !ok || len(coverage) != 0 {
		t.Errorf("coverage_summary should be empty object; got %v", parsed["coverage_summary"])
	}
}

// 201 (fresh insert) is success: caller records the new upload_id.
func TestPostUpload_FreshCreated(t *testing.T) {
	srv := stubUploadServer(t, http.StatusCreated, `{"error":false,"message":"ok","payload":{"upload_id":"u-1","received_at":"2026-06-09T12:00:00Z"}}`)
	defer srv.Close()

	in := uploadFixture()
	body, hash, _ := BuildTransportOnlyBody(in, time.Now())
	res, err := PostUpload(context.Background(), srv.Client(), srv.URL, "tok-x", in, body, hash)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if res.StatusCode != http.StatusCreated || res.UploadID != "u-1" {
		t.Errorf("unexpected result: %#v", res)
	}
}

// 200 (idempotent duplicate) is also success: caller treats it the
// same as 201 but knows the row was pre-existing.
func TestPostUpload_IdempotentDuplicate(t *testing.T) {
	srv := stubUploadServer(t, http.StatusOK, `{"error":false,"message":"ok","payload":{"upload_id":"u-existing","received_at":"2026-06-08T12:00:00Z"}}`)
	defer srv.Close()

	in := uploadFixture()
	body, hash, _ := BuildTransportOnlyBody(in, time.Now())
	res, err := PostUpload(context.Background(), srv.Client(), srv.URL, "tok-x", in, body, hash)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if res.StatusCode != http.StatusOK || res.UploadID != "u-existing" {
		t.Errorf("unexpected result: %#v", res)
	}
}

// 4xx and 5xx are errors. The caller logs them and reports a failed
// manual upload.
func TestPostUpload_ServerError(t *testing.T) {
	srv := stubUploadServer(t, http.StatusInternalServerError, `{"error":true,"message":"boom"}`)
	defer srv.Close()

	in := uploadFixture()
	body, hash, _ := BuildTransportOnlyBody(in, time.Now())
	_, err := PostUpload(context.Background(), srv.Client(), srv.URL, "tok-x", in, body, hash)
	if err == nil {
		t.Fatalf("expected error on 500, got nil")
	}
	if !strings.Contains(err.Error(), "status 500") {
		t.Errorf("error should mention status; got %v", err)
	}
}

// Bearer token + Idempotency-Key must be on every request - the
// server rejects 400 if the header is missing.
func TestPostUpload_SendsAuthAndIdempotency(t *testing.T) {
	var gotAuth, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotKey = r.Header.Get("Idempotency-Key")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"error":false,"message":"ok","payload":{"upload_id":"u","received_at":"x"}}`)
	}))
	defer srv.Close()

	in := uploadFixture()
	body, hash, _ := BuildTransportOnlyBody(in, time.Now())
	if _, err := PostUpload(context.Background(), srv.Client(), srv.URL, "tok-x", in, body, hash); err != nil {
		t.Fatalf("post: %v", err)
	}
	if gotAuth != "Bearer tok-x" {
		t.Errorf("Authorization = %q, want Bearer tok-x", gotAuth)
	}
	if gotKey != hash {
		t.Errorf("Idempotency-Key = %q, want canonical hash %q", gotKey, hash)
	}
}

// An error response envelope overrides a successful HTTP status.
func TestPostUpload_RejectsEnvelopeError(t *testing.T) {
	srv := stubUploadServer(t, http.StatusOK, `{"error":true,"message":"upstream timeout","payload":{}}`)
	defer srv.Close()

	in := uploadFixture()
	body, hash, _ := BuildTransportOnlyBody(in, time.Now())
	_, err := PostUpload(context.Background(), srv.Client(), srv.URL, "tok-x", in, body, hash)
	if err == nil {
		t.Fatalf("expected error on envelope error=true, got nil")
	}
	if !strings.Contains(err.Error(), "upstream timeout") {
		t.Errorf("error should surface envelope message; got %v", err)
	}
}

// A successful status with a missing upload_id is also an error: the
// API contract requires that field on every success response. Without
// the check, the caller would treat an empty string as a valid upload
// id and the dashboard would render nonsense.
func TestPostUpload_RejectsMissingUploadID(t *testing.T) {
	srv := stubUploadServer(t, http.StatusCreated, `{"error":false,"message":"ok","payload":{}}`)
	defer srv.Close()

	in := uploadFixture()
	body, hash, _ := BuildTransportOnlyBody(in, time.Now())
	_, err := PostUpload(context.Background(), srv.Client(), srv.URL, "tok-x", in, body, hash)
	if err == nil {
		t.Fatalf("expected error on missing upload_id, got nil")
	}
	if !strings.Contains(err.Error(), "upload_id") {
		t.Errorf("error should mention upload_id; got %v", err)
	}
}

// ErrSkipped flows through errors.Is so callers can switch on it
// regardless of the underlying reason.
func TestSkipReason_MatchesErrSkipped(t *testing.T) {
	err := &SkipReason{Reason: "intent_gap.enabled is false"}
	if !errors.Is(err, ErrSkipped) {
		t.Errorf("SkipReason should satisfy errors.Is(err, ErrSkipped)")
	}
}

// --- helpers -------------------------------------------------------

func uploadFixture() UploadInput {
	return UploadInput{
		RepositoryID:     "11111111-2222-3333-4444-555555555555",
		PRNumber:         42,
		HeadSHA:          "deadbeefcafef00d1234567890abcdef12345678",
		Provider:         "claude_code",
		ProducerDeviceID: "host-a",
	}
}

func stubUploadServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}

func mustEqual(t *testing.T, m map[string]any, key string, want any) {
	t.Helper()
	got, ok := m[key]
	if !ok {
		t.Errorf("missing field %q", key)
		return
	}
	if got != want {
		t.Errorf("%s = %v, want %v", key, got, want)
	}
}
