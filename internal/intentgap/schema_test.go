package intentgap

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Keep local validation aligned with the API's canonical schema.
func TestIntentGapSchemaMatchesAPI(t *testing.T) {
	apiPath := filepath.Join("..", "..", "..", "api", "src", "internal", "intentgap", "schemas", "intent_gap.schema.json")
	apiBytes, err := os.ReadFile(apiPath)
	if err != nil {
		t.Skipf("API schema not reachable (outside monorepo checkout): %v", err)
		return
	}
	if !bytes.Equal(apiBytes, intentGapSchemaBytes) {
		t.Fatalf("CLI embedded schema diverged from API canonical copy.\nReplace cli/internal/intentgap/schemas/intent_gap.schema.json with:\n  %s", apiPath)
	}
}

func TestValidateFindings_EmptyIsValid(t *testing.T) {
	if err := ValidateFindings(nil); err != nil {
		t.Errorf("nil findings: %v", err)
	}
	if err := ValidateFindings(json.RawMessage(`[]`)); err != nil {
		t.Errorf("empty array findings: %v", err)
	}
}

func TestValidateFindings_RejectsArbitraryObject(t *testing.T) {
	findings := json.RawMessage(`[{"whatever": true}]`)
	err := ValidateFindings(findings)
	if err == nil {
		t.Fatalf("expected schema rejection for arbitrary object")
	}
	if !strings.HasPrefix(err.Error(), "findings[0]:") {
		t.Errorf("expected index-prefixed error, got: %v", err)
	}
}

func TestValidateFindings_AcceptsCanonicalDeferred(t *testing.T) {
	findings := json.RawMessage(`[
		{
			"schema_version": "1",
			"finding_id": "f_0123456789abcdef",
			"kind": "deferred",
			"title": "Deferred validation step",
			"confidence": "medium",
			"originally_requested_in": {
				"turn_id": "t-1",
				"prompt_excerpt": "add input validation",
				"prompt_excerpt_hash": "h-1"
			},
			"trajectory_note": "added validate() then removed after revert prompt",
			"current_state": {
				"file": "handler.go",
				"line_range": [12, 24],
				"summary": "validation removed; no replacement"
			}
		}
	]`)
	if err := ValidateFindings(findings); err != nil {
		t.Fatalf("canonical deferred finding rejected: %v", err)
	}
}

func TestValidateFindings_RejectsBadKind(t *testing.T) {
	findings := json.RawMessage(`[
		{
			"schema_version": "1",
			"finding_id": "f_0123456789abcdef",
			"kind": "totally-made-up",
			"title": "x",
			"confidence": "low"
		}
	]`)
	err := ValidateFindings(findings)
	if err == nil {
		t.Fatalf("expected schema rejection for bad kind")
	}
}

func TestValidateFindings_RejectsBadFindingID(t *testing.T) {
	findings := json.RawMessage(`[
		{
			"schema_version": "1",
			"finding_id": "not-a-valid-id",
			"kind": "under_impl",
			"title": "x",
			"confidence": "low",
			"expected_intent": {"summary": "s", "turn_id": "t", "prompt_excerpt": "p", "prompt_excerpt_hash": "h"},
			"observed_diff_evidence": {"summary": "s", "ai_authored_regions_checked": []},
			"missing_or_partial_area": {"note": "n"}
		}
	]`)
	err := ValidateFindings(findings)
	if err == nil {
		t.Fatalf("expected schema rejection for malformed finding_id")
	}
}
