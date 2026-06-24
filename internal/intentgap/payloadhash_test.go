package intentgap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// goldenFixture matches testdata/payloadhash/golden.json. The CLI and API
// copies must stay byte-identical so hash drift is caught before upload.
type goldenFixture struct {
	Input          goldenInput `json:"input"`
	CanonicalBytes string      `json:"canonical_bytes"`
	PayloadHash    string      `json:"payload_hash"`
}

type goldenInput struct {
	RepositoryID          string          `json:"repository_id"`
	PRNumber              int32           `json:"pr_number"`
	HeadSHA               string          `json:"head_sha"`
	BaseSHA               string          `json:"base_sha"`
	AlgorithmVersion      string          `json:"algorithm_version"`
	PromptTemplateVersion string          `json:"prompt_template_version"`
	FindingSchemaVersion  string          `json:"finding_schema_version"`
	RedactionVersion      string          `json:"redaction_version"`
	Provider              string          `json:"provider"`
	Model                 string          `json:"model"`
	ProducerState         string          `json:"producer_state"`
	CoverageSummary       json.RawMessage `json:"coverage_summary"`
	Findings              json.RawMessage `json:"findings"`
}

func loadGolden(t *testing.T) goldenFixture {
	t.Helper()
	path := filepath.Join("testdata", "payloadhash", "golden.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden fixture: %v", err)
	}
	var f goldenFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse golden fixture: %v", err)
	}
	return f
}

// The CLI and API golden fixtures should stay byte-identical. Skip when the
// API repo is not checked out next to the CLI.
func TestGoldenFixtureMatchesAPI(t *testing.T) {
	cliPath := filepath.Join("testdata", "payloadhash", "golden.json")
	apiPath := filepath.Join("..", "..", "..", "api", "src", "internal", "intentgap", "testdata", "payloadhash", "golden.json")
	cliBytes, err := os.ReadFile(cliPath)
	if err != nil {
		t.Fatalf("read CLI fixture: %v", err)
	}
	apiBytes, err := os.ReadFile(apiPath)
	if err != nil {
		t.Skipf("API fixture not reachable (likely running outside the monorepo checkout): %v", err)
		return
	}
	if !bytes.Equal(cliBytes, apiBytes) {
		t.Fatalf("CLI golden fixture diverged from API canonical fixture.\nReplace cli/internal/intentgap/testdata/payloadhash/golden.json with the file at:\n  %s\n(then verify ComputePayloadHash still produces the pinned hash.)", apiPath)
	}
}

// The CLI encoder must produce the canonical bytes and hash pinned by the
// fixture; the server recomputes the same hash on upload.
func TestComputePayloadHash_MatchesGoldenBytes(t *testing.T) {
	g := loadGolden(t)
	hash, bytesOut, err := ComputePayloadHash(toInput(g.Input))
	if err != nil {
		t.Fatalf("ComputePayloadHash: %v", err)
	}
	if string(bytesOut) != g.CanonicalBytes {
		t.Fatalf("canonical bytes drift.\nwant: %s\ngot:  %s",
			g.CanonicalBytes, string(bytesOut))
	}
	expectedHash := sha256.Sum256([]byte(g.CanonicalBytes))
	expected := hex.EncodeToString(expectedHash[:])
	if hash != expected {
		t.Fatalf("payload_hash inconsistent with canonical bytes.\nwant: %s\ngot:  %s",
			expected, hash)
	}
	if hash != g.PayloadHash {
		t.Fatalf("hash drift vs pinned fixture.\nwant: %s\ngot:  %s",
			g.PayloadHash, hash)
	}
}

// Pins the contract that omitted coverage_summary / findings hash
// identically to explicit empties. Some upload paths send nil findings;
// the server uses {} / [] in its recompute, so the two must converge.
func TestComputePayloadHash_NilMeansEmpty(t *testing.T) {
	g := loadGolden(t)
	base := toInput(g.Input)

	withNil := base
	withNil.CoverageSummary = nil
	withNil.Findings = nil

	withEmpty := base
	withEmpty.CoverageSummary = json.RawMessage(`{}`)
	withEmpty.Findings = json.RawMessage(`[]`)

	nilHash, _, err := ComputePayloadHash(withNil)
	if err != nil {
		t.Fatalf("nil hash: %v", err)
	}
	emptyHash, _, err := ComputePayloadHash(withEmpty)
	if err != nil {
		t.Fatalf("empty hash: %v", err)
	}
	if nilHash != emptyHash {
		t.Fatalf("nil and empty diverged.\nnil:   %s\nempty: %s", nilHash, emptyHash)
	}
}

func toInput(g goldenInput) PayloadHashInput {
	return PayloadHashInput(g)
}
