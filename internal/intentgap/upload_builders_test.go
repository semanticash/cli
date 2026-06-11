package intentgap

import (
	"encoding/json"
	"testing"
	"time"
)

func sampleUploadInput() UploadInput {
	return UploadInput{
		RepositoryID:     "11111111-2222-3333-4444-555555555555",
		PRNumber:         42,
		HeadSHA:          "deadbeefcafef00d1234567890abcdef12345678",
		BaseSHA:          "00112233445566778899aabbccddeeff00112233",
		Provider:         "claude_code",
		Model:            "claude-opus-4-7",
		ProducerDeviceID: "dev-1",
	}
}

func sampleTime() time.Time {
	return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
}

// Happy path: analyzed body carries findings, algorithm_version flips
// to the analyzer marker, prompt_template_version reaches the wire.
func TestBuildAnalyzedBody_WireShape(t *testing.T) {
	findings := json.RawMessage(`[{"schema_version":"1"}]`)
	coverage := json.RawMessage(`{"commits":3}`)

	body, hash, err := BuildAnalyzedBody(AnalyzedBodyInput{
		UploadInput:           sampleUploadInput(),
		PromptTemplateVersion: PromptTemplateVersion,
		Findings:              findings,
		CoverageSummary:       coverage,
	}, sampleTime())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if parsed["producer_state"] != ProducerStateAnalyzed {
		t.Errorf("producer_state = %v, want analyzed", parsed["producer_state"])
	}
	if parsed["algorithm_version"] != AlgorithmVersionAnalyzed {
		t.Errorf("algorithm_version = %v, want %s", parsed["algorithm_version"], AlgorithmVersionAnalyzed)
	}
	if parsed["prompt_template_version"] != PromptTemplateVersion {
		t.Errorf("prompt_template_version = %v, want %s", parsed["prompt_template_version"], PromptTemplateVersion)
	}
	if parsed["payload_hash"] != hash {
		t.Errorf("payload_hash field = %v, want returned hash %s", parsed["payload_hash"], hash)
	}
}

// Errored body has empty findings and surfaces the reason in
// coverage_summary so dashboard / doctor can render the failure.
func TestBuildErroredBody_WireShape(t *testing.T) {
	body, hash, err := BuildErroredBody(sampleUploadInput(), "LLM unavailable", PromptTemplateVersion, sampleTime())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if parsed["producer_state"] != ProducerStateErrored {
		t.Errorf("producer_state = %v, want errored", parsed["producer_state"])
	}
	if parsed["payload_hash"] != hash {
		t.Errorf("payload_hash field = %v, want returned hash %s", parsed["payload_hash"], hash)
	}
	cov, _ := parsed["coverage_summary"].(map[string]any)
	if cov["error_reason"] != "LLM unavailable" {
		t.Errorf("error_reason = %v, want \"LLM unavailable\"", cov["error_reason"])
	}
	findings, _ := parsed["findings"].([]any)
	if len(findings) != 0 {
		t.Errorf("findings should be empty on errored; got %v", findings)
	}
}

// Identical analyzed input produces an identical hash regardless of
// produced_at. This is the same idempotency contract transport-only
// has - the server's recompute won't see produced_at, so a retry
// after a timestamp tick must still dedup.
func TestBuildAnalyzedBody_DeterministicHash(t *testing.T) {
	in := AnalyzedBodyInput{
		UploadInput:           sampleUploadInput(),
		PromptTemplateVersion: PromptTemplateVersion,
		Findings:              json.RawMessage(`[]`),
		CoverageSummary:       json.RawMessage(`{"commits":0}`),
	}
	_, h1, _ := BuildAnalyzedBody(in, sampleTime())
	_, h2, _ := BuildAnalyzedBody(in, sampleTime().Add(time.Hour))
	if h1 != h2 {
		t.Errorf("hash drifted across produced_at change: %s vs %s", h1, h2)
	}
}

// Analyzed and errored uploads produce DIFFERENT hashes for the same
// PR / head: producer_state is part of the canonical hash input, so
// the server treats them as separate rows.
func TestBuildAnalyzedBody_DifferentFromErroredHash(t *testing.T) {
	in := AnalyzedBodyInput{
		UploadInput:           sampleUploadInput(),
		PromptTemplateVersion: PromptTemplateVersion,
	}
	_, analyzedHash, _ := BuildAnalyzedBody(in, sampleTime())
	_, erroredHash, _ := BuildErroredBody(sampleUploadInput(), "x", PromptTemplateVersion, sampleTime())
	if analyzedHash == erroredHash {
		t.Errorf("analyzed and errored produced same hash; producer_state should differentiate them")
	}
}
