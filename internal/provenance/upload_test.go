package provenance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestUploadTurn_HappyPath(t *testing.T) {
	var prepareCalled, completeCalled atomic.Int32
	var s3Puts atomic.Int32

	// Mock backend and S3.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/provenance/prepare":
			prepareCalled.Add(1)
			var body prepareRequestBody
			_ = json.NewDecoder(r.Body).Decode(&body)

			// Return presigned URLs for all objects.
			var uploads []prepareUploadEntry
			for _, obj := range body.Objects {
				uploads = append(uploads, prepareUploadEntry{
					Kind:         obj.Kind,
					Hash:         obj.Hash,
					PresignedURL: r.Host, // placeholder - s3 mock handles below
				})
			}
			_ = json.NewEncoder(w).Encode(apiEnvelope[preparePayload]{
				Payload: preparePayload{Uploads: uploads},
			})

		case "/v1/provenance/complete":
			completeCalled.Add(1)
			_ = json.NewEncoder(w).Encode(apiEnvelope[json.RawMessage]{
				Message: "Turn registered",
			})

		default:
			w.WriteHeader(404)
		}
	}))
	defer backend.Close()

	// For this test, we need presigned URLs that point to an actual S3 mock.
	s3Mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			s3Puts.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(405)
	}))
	defer s3Mock.Close()

	// Rebuild backend with S3 mock URLs in prepare response.
	backend.Close()
	backend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/provenance/prepare":
			prepareCalled.Add(1)
			var body prepareRequestBody
			_ = json.NewDecoder(r.Body).Decode(&body)

			var uploads []prepareUploadEntry
			for _, obj := range body.Objects {
				uploads = append(uploads, prepareUploadEntry{
					Kind:         obj.Kind,
					Hash:         obj.Hash,
					PresignedURL: s3Mock.URL + "/" + obj.Kind + "/" + obj.Hash,
				})
			}
			_ = json.NewEncoder(w).Encode(apiEnvelope[preparePayload]{
				Payload: preparePayload{Uploads: uploads},
			})

		case "/v1/provenance/complete":
			completeCalled.Add(1)
			_ = json.NewEncoder(w).Encode(apiEnvelope[json.RawMessage]{
				Message: "Turn registered",
			})

		default:
			w.WriteHeader(404)
		}
	}))
	defer backend.Close()

	// Build a SyncResult with two objects.
	env := syncEnvelope{
		ConnectedRepoID:   "repo-123",
		Provider:          "claude_code",
		ProviderSessionID: "sess-1",
		TurnID:            "turn-abc",
		StartedAt:         1700000000,
		Objects: []syncObject{
			{Kind: "bundle", Hash: "bundlehash12345678", SizeBytes: 10},
			{Kind: "step_provenance", Hash: "stephash1234567890", SizeBytes: 5},
		},
	}
	envJSON, _ := json.Marshal(env)

	result := SyncResult{
		TurnID:      "turn-abc",
		ManifestID:  "m-1",
		ObjectCount: 2,
		Envelope:    envJSON,
		RedactedBlobs: map[string][]byte{
			"bundlehash12345678": []byte(`{"version":1}`),
			"stephash1234567890": []byte(`{"tool_input":{}}`),
		},
	}

	out := UploadTurn(context.Background(), backend.URL, "test-token", result)

	if out.Err != nil {
		t.Fatalf("unexpected error: %v", out.Err)
	}
	if !out.Uploaded {
		t.Error("expected Uploaded=true")
	}
	if prepareCalled.Load() != 1 {
		t.Errorf("prepare called %d times, want 1", prepareCalled.Load())
	}
	if completeCalled.Load() != 1 {
		t.Errorf("complete called %d times, want 1", completeCalled.Load())
	}
	if s3Puts.Load() != 2 {
		t.Errorf("S3 PUTs = %d, want 2", s3Puts.Load())
	}
}

func TestUploadTurn_SkipsAlreadyUploaded(t *testing.T) {
	var s3Puts atomic.Int32

	s3Mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s3Puts.Add(1)
		w.WriteHeader(200)
	}))
	defer s3Mock.Close()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/provenance/prepare":
			// Bundle already exists, only step_provenance needs upload.
			_ = json.NewEncoder(w).Encode(apiEnvelope[preparePayload]{
				Payload: preparePayload{
					Uploads: []prepareUploadEntry{
						{Kind: "step_provenance", Hash: "stephash1234567890", PresignedURL: s3Mock.URL + "/sp"},
					},
					Skip: []prepareSkipEntry{
						{Kind: "bundle", Hash: "bundlehash12345678"},
					},
				},
			})
		case "/v1/provenance/complete":
			_ = json.NewEncoder(w).Encode(apiEnvelope[json.RawMessage]{})
		default:
			w.WriteHeader(404)
		}
	}))
	defer backend.Close()

	env := syncEnvelope{
		ConnectedRepoID: "repo-123",
		Provider:        "claude_code",
		TurnID:          "turn-1",
		StartedAt:       1,
		Objects: []syncObject{
			{Kind: "bundle", Hash: "bundlehash12345678", SizeBytes: 10},
			{Kind: "step_provenance", Hash: "stephash1234567890", SizeBytes: 5},
		},
	}
	envJSON, _ := json.Marshal(env)

	out := UploadTurn(context.Background(), backend.URL, "tok", SyncResult{
		TurnID:      "turn-1",
		ManifestID:  "m-1",
		ObjectCount: 2,
		Envelope:    envJSON,
		RedactedBlobs: map[string][]byte{
			"bundlehash12345678": []byte(`{}`),
			"stephash1234567890": []byte(`{}`),
		},
	})

	if out.Err != nil {
		t.Fatalf("unexpected error: %v", out.Err)
	}
	// Only the step_provenance should be PUT (bundle was skipped).
	if s3Puts.Load() != 1 {
		t.Errorf("S3 PUTs = %d, want 1 (bundle should be skipped)", s3Puts.Load())
	}
}

func TestUploadTurn_PrepareReturns401(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer backend.Close()

	env := syncEnvelope{ConnectedRepoID: "r", Provider: "p", TurnID: "t", StartedAt: 1,
		Objects: []syncObject{{Kind: "bundle", Hash: "abcdef1234567890", SizeBytes: 1}}}
	envJSON, _ := json.Marshal(env)

	out := UploadTurn(context.Background(), backend.URL, "bad-token", SyncResult{
		TurnID: "t", ManifestID: "m", Envelope: envJSON,
		RedactedBlobs: map[string][]byte{"abcdef1234567890": {}},
	})

	if !IsUnauthorized(out.Err) {
		t.Errorf("expected unauthorized error, got: %v", out.Err)
	}
}

func TestUploadTurn_CompleteReturns409(t *testing.T) {
	s3Mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer s3Mock.Close()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/provenance/prepare":
			_ = json.NewEncoder(w).Encode(apiEnvelope[preparePayload]{
				Payload: preparePayload{
					Uploads: []prepareUploadEntry{
						{Kind: "bundle", Hash: "abcdef1234567890", PresignedURL: s3Mock.URL + "/b"},
					},
				},
			})
		case "/v1/provenance/complete":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":true,"message":"Turn ID already registered with different identity"}`))
		}
	}))
	defer backend.Close()

	env := syncEnvelope{ConnectedRepoID: "r", Provider: "p", TurnID: "t", StartedAt: 1,
		Objects: []syncObject{{Kind: "bundle", Hash: "abcdef1234567890", SizeBytes: 1}}}
	envJSON, _ := json.Marshal(env)

	out := UploadTurn(context.Background(), backend.URL, "tok", SyncResult{
		TurnID: "t", ManifestID: "m", Envelope: envJSON,
		RedactedBlobs: map[string][]byte{"abcdef1234567890": {0x01}},
	})

	if out.Err == nil {
		t.Fatal("expected error on 409 conflict")
	}
	if out.Uploaded {
		t.Error("should not be marked as uploaded on conflict")
	}
}

func TestUploadTurn_S3PutFails(t *testing.T) {
	s3Mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer s3Mock.Close()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(apiEnvelope[preparePayload]{
			Payload: preparePayload{
				Uploads: []prepareUploadEntry{
					{Kind: "bundle", Hash: "abcdef1234567890", PresignedURL: s3Mock.URL + "/b"},
				},
			},
		})
	}))
	defer backend.Close()

	env := syncEnvelope{ConnectedRepoID: "r", Provider: "p", TurnID: "t", StartedAt: 1,
		Objects: []syncObject{{Kind: "bundle", Hash: "abcdef1234567890", SizeBytes: 1}}}
	envJSON, _ := json.Marshal(env)

	out := UploadTurn(context.Background(), backend.URL, "tok", SyncResult{
		TurnID: "t", ManifestID: "m", Envelope: envJSON,
		RedactedBlobs: map[string][]byte{"abcdef1234567890": {0x01}},
	})

	if out.Err == nil {
		t.Fatal("expected error when S3 PUT fails")
	}
}

func TestUploadTurn_PrepareHeadersSet(t *testing.T) {
	var gotAuth, gotUA, gotCT string

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewEncoder(w).Encode(apiEnvelope[preparePayload]{
			Payload: preparePayload{},
		})
	}))
	defer backend.Close()

	env := syncEnvelope{ConnectedRepoID: "r", Provider: "p", TurnID: "t", StartedAt: 1}
	envJSON, _ := json.Marshal(env)

	_ = UploadTurn(context.Background(), backend.URL, "my-secret-token", SyncResult{
		TurnID: "t", ManifestID: "m", Envelope: envJSON,
		RedactedBlobs: map[string][]byte{},
	})

	if gotAuth != "Bearer my-secret-token" {
		t.Errorf("Authorization = %q, want Bearer my-secret-token", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if gotUA == "" {
		t.Error("expected User-Agent header to be set")
	}
}

func TestIsUnauthorized(t *testing.T) {
	if !IsUnauthorized(errUnauthorized) {
		t.Error("expected true for errUnauthorized")
	}
	if IsUnauthorized(nil) {
		t.Error("expected false for nil")
	}
	if IsUnauthorized(io.EOF) {
		t.Error("expected false for io.EOF")
	}
}

func TestIsTerminal(t *testing.T) {
	if !IsTerminal(fmt.Errorf("wrapped: %w", errTerminal)) {
		t.Error("expected true for wrapped errTerminal")
	}
	if IsTerminal(nil) {
		t.Error("expected false for nil")
	}
	if IsTerminal(io.EOF) {
		t.Error("expected false for io.EOF")
	}
	if IsTerminal(errUnauthorized) {
		t.Error("expected false for errUnauthorized")
	}
}

// ---------- ClassifyOutcome tests ----------

func TestClassifyOutcome_SuccessReturnsUploaded(t *testing.T) {
	if got := ClassifyOutcome(nil, 0); got != ActionUploaded {
		t.Errorf("ClassifyOutcome(nil, 0) = %d, want ActionUploaded", got)
	}
}

func TestClassifyOutcome_TransientBelowCapReturnsRetry(t *testing.T) {
	err := fmt.Errorf("network timeout")
	for _, attempts := range []int64{0, 1, 2, 3} {
		if got := ClassifyOutcome(err, attempts); got != ActionRetry {
			t.Errorf("ClassifyOutcome(transient, %d) = %d, want ActionRetry", attempts, got)
		}
	}
}

func TestClassifyOutcome_TransientAtCapReturnsFail(t *testing.T) {
	err := fmt.Errorf("network timeout")
	// MaxUploadAttempts is 5, so attempts=4 means next would be 5th → fail.
	if got := ClassifyOutcome(err, 4); got != ActionFail {
		t.Errorf("ClassifyOutcome(transient, 4) = %d, want ActionFail", got)
	}
}

func TestClassifyOutcome_TransientAboveCapReturnsFail(t *testing.T) {
	err := fmt.Errorf("S3 forbidden")
	if got := ClassifyOutcome(err, 10); got != ActionFail {
		t.Errorf("ClassifyOutcome(transient, 10) = %d, want ActionFail", got)
	}
}

func TestClassifyOutcome_TerminalAtZeroAttemptsReturnsFail(t *testing.T) {
	err := fmt.Errorf("conflict: %w", errTerminal)
	if got := ClassifyOutcome(err, 0); got != ActionFail {
		t.Errorf("ClassifyOutcome(terminal, 0) = %d, want ActionFail", got)
	}
}

func TestClassifyOutcome_UnauthorizedIsTransient(t *testing.T) {
	// 401 is not terminal (caller handles refresh), but if passed here it's transient.
	err := fmt.Errorf("prepare: %w", errUnauthorized)
	if got := ClassifyOutcome(err, 0); got != ActionRetry {
		t.Errorf("ClassifyOutcome(unauthorized, 0) = %d, want ActionRetry", got)
	}
}

// ---------- End-to-end: UploadTurn error → ClassifyOutcome chain ----------

func TestUploadTurn_TransientError_ClassifiesAsRetry(t *testing.T) {
	// Backend returns 500 (transient).
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/provenance/prepare":
			_ = json.NewEncoder(w).Encode(apiEnvelope[preparePayload]{
				Payload: preparePayload{},
			})
		case "/v1/provenance/complete":
			w.WriteHeader(500)
		}
	}))
	defer backend.Close()

	env := syncEnvelope{ConnectedRepoID: "r", Provider: "p", TurnID: "t", StartedAt: 1}
	envJSON, _ := json.Marshal(env)

	inputAttempts := int64(2)
	out := UploadTurn(context.Background(), backend.URL, "tok", SyncResult{
		TurnID: "t", ManifestID: "m", Envelope: envJSON,
		UploadAttempts: inputAttempts, RedactedBlobs: map[string][]byte{},
	})

	if out.Err == nil {
		t.Fatal("expected error")
	}
	// Should classify as retry (transient, attempts=2 < 5).
	if got := ClassifyOutcome(out.Err, inputAttempts); got != ActionRetry {
		t.Errorf("ClassifyOutcome = %d, want ActionRetry", got)
	}
}

func TestUploadTurn_TransientAtCap_ClassifiesAsFail(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/provenance/prepare":
			_ = json.NewEncoder(w).Encode(apiEnvelope[preparePayload]{Payload: preparePayload{}})
		case "/v1/provenance/complete":
			w.WriteHeader(500)
		}
	}))
	defer backend.Close()

	env := syncEnvelope{ConnectedRepoID: "r", Provider: "p", TurnID: "t", StartedAt: 1}
	envJSON, _ := json.Marshal(env)

	inputAttempts := int64(4)
	out := UploadTurn(context.Background(), backend.URL, "tok", SyncResult{
		TurnID: "t", ManifestID: "m", Envelope: envJSON,
		UploadAttempts: inputAttempts, RedactedBlobs: map[string][]byte{},
	})

	if out.Err == nil {
		t.Fatal("expected error")
	}
	// attempts=4, so 4+1 >= 5 → fail.
	if got := ClassifyOutcome(out.Err, inputAttempts); got != ActionFail {
		t.Errorf("ClassifyOutcome = %d, want ActionFail", got)
	}
}

func TestUploadTurn_ConflictIsTerminal(t *testing.T) {
	s3Mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer s3Mock.Close()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/provenance/prepare":
			_ = json.NewEncoder(w).Encode(apiEnvelope[preparePayload]{
				Payload: preparePayload{
					Uploads: []prepareUploadEntry{
						{Kind: "bundle", Hash: "abcdef1234567890", PresignedURL: s3Mock.URL + "/b"},
					},
				},
			})
		case "/v1/provenance/complete":
			w.WriteHeader(http.StatusConflict)
		}
	}))
	defer backend.Close()

	env := syncEnvelope{ConnectedRepoID: "r", Provider: "p", TurnID: "t", StartedAt: 1,
		Objects: []syncObject{{Kind: "bundle", Hash: "abcdef1234567890", SizeBytes: 1}}}
	envJSON, _ := json.Marshal(env)

	out := UploadTurn(context.Background(), backend.URL, "tok", SyncResult{
		TurnID: "t", ManifestID: "m", Envelope: envJSON,
		RedactedBlobs: map[string][]byte{"abcdef1234567890": {0x01}},
	})

	if out.Err == nil {
		t.Fatal("expected error on conflict")
	}
	if !IsTerminal(out.Err) {
		t.Error("409 conflict should be a terminal error")
	}
}
