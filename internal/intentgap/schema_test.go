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

// A finding that includes the optional agent_action_citation field
// with a well-formed shape passes schema validation. The field is
// additive; existing producers that omit it continue to validate.
func TestValidateFindings_AcceptsAgentActionCitation(t *testing.T) {
	body := `[{
		"schema_version":"1",
		"finding_id":"f_0000000000000000",
		"kind":"under_impl",
		"title":"t",
		"confidence":"medium",
		"expected_intent":{"summary":"s","turn_id":"t1","prompt_excerpt":"p","prompt_excerpt_hash":"h"},
		"observed_diff_evidence":{"summary":"s","ai_authored_regions_checked":[{"file":"a.go","lines":[[1,2]]}]},
		"missing_or_partial_area":{"note":"n"},
		"agent_action_citation":{"action_id":"a_0123456789abcdef","scope":{"file":"a.go","line_range":[1,2]}}
	}]`
	if err := ValidateFindings(json.RawMessage(body)); err != nil {
		t.Errorf("expected agent_action_citation to validate; got %v", err)
	}
}

// A no_action_citation that carries a concrete scope passes
// validation. File-only negative scopes are also
// allowed; only a missing scope is rejected at the schema level.
func TestValidateFindings_AcceptsNoActionCitation(t *testing.T) {
	body := `[{
		"schema_version":"1",
		"finding_id":"f_0000000000000000",
		"kind":"under_impl",
		"title":"t",
		"confidence":"medium",
		"expected_intent":{"summary":"s","turn_id":"t1","prompt_excerpt":"p","prompt_excerpt_hash":"h"},
		"observed_diff_evidence":{"summary":"s","ai_authored_regions_checked":[{"file":"a.go","lines":[[1,2]]}]},
		"missing_or_partial_area":{"note":"n"},
		"no_action_citation":{"scope":{"file":"a.go"}}
	}]`
	if err := ValidateFindings(json.RawMessage(body)); err != nil {
		t.Errorf("expected no_action_citation to validate; got %v", err)
	}
}

// An agent_action_citation whose action_id does not match the
// canonical pattern is rejected at the schema level. The cite-or-
// drop layer also rejects unknown IDs, but schema validation catches
// malformed citations before the validator step.
func TestValidateFindings_RejectsMalformedActionID(t *testing.T) {
	body := `[{
		"schema_version":"1",
		"finding_id":"f_0000000000000000",
		"kind":"under_impl",
		"title":"t",
		"confidence":"medium",
		"expected_intent":{"summary":"s","turn_id":"t1","prompt_excerpt":"p","prompt_excerpt_hash":"h"},
		"observed_diff_evidence":{"summary":"s","ai_authored_regions_checked":[{"file":"a.go","lines":[[1,2]]}]},
		"missing_or_partial_area":{"note":"n"},
		"agent_action_citation":{"action_id":"not_a_real_action_id"}
	}]`
	if err := ValidateFindings(json.RawMessage(body)); err == nil {
		t.Errorf("expected malformed action_id to fail schema; got nil")
	}
}

// A no_action_citation without a scope is rejected at the schema
// level. The cite-or-drop layer rejects this too, but schema validation
// makes the producer contract explicit.
func TestValidateFindings_RejectsNoActionCitationWithoutScope(t *testing.T) {
	body := `[{
		"schema_version":"1",
		"finding_id":"f_0000000000000000",
		"kind":"under_impl",
		"title":"t",
		"confidence":"medium",
		"expected_intent":{"summary":"s","turn_id":"t1","prompt_excerpt":"p","prompt_excerpt_hash":"h"},
		"observed_diff_evidence":{"summary":"s","ai_authored_regions_checked":[{"file":"a.go","lines":[[1,2]]}]},
		"missing_or_partial_area":{"note":"n"},
		"no_action_citation":{}
	}]`
	if err := ValidateFindings(json.RawMessage(body)); err == nil {
		t.Errorf("expected scopeless no_action_citation to fail schema; got nil")
	}
}

// Citation scopes require a non-empty file path so schema validation
// matches the cite-or-drop validator.
func TestValidateFindings_RejectsNoActionCitationWithEmptyFile(t *testing.T) {
	body := `[{
		"schema_version":"1",
		"finding_id":"f_0000000000000000",
		"kind":"under_impl",
		"title":"t",
		"confidence":"medium",
		"expected_intent":{"summary":"s","turn_id":"t1","prompt_excerpt":"p","prompt_excerpt_hash":"h"},
		"observed_diff_evidence":{"summary":"s","ai_authored_regions_checked":[{"file":"a.go","lines":[[1,2]]}]},
		"missing_or_partial_area":{"note":"n"},
		"no_action_citation":{"scope":{"file":""}}
	}]`
	if err := ValidateFindings(json.RawMessage(body)); err == nil {
		t.Errorf("expected empty citation scope file to fail schema; got nil")
	}
}
