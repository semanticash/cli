package intentgap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	_ "embed"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// intentGapSchemaBytes is the embedded copy of the canonical intent-gap
// finding schema. The canonical source lives in the API repo at
// api/docs/schemas/intent_gap.schema.json. CI pins the two copies
// byte-for-byte (see TestIntentGapSchemaMatchesAPI).
//
//go:embed schemas/intent_gap.schema.json
var intentGapSchemaBytes []byte

var (
	intentGapSchema     *jsonschema.Schema
	intentGapSchemaErr  error
	intentGapSchemaOnce sync.Once
)

// IntentGapFindingSchema returns the compiled Draft 2020-12 schema for
// a single intent-gap finding. Compiled lazily on first use and cached
// for the process lifetime.
func IntentGapFindingSchema() (*jsonschema.Schema, error) {
	intentGapSchemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.Draft = jsonschema.Draft2020
		if err := c.AddResource("intent_gap.schema.json", bytes.NewReader(intentGapSchemaBytes)); err != nil {
			intentGapSchemaErr = fmt.Errorf("add resource: %w", err)
			return
		}
		s, err := c.Compile("intent_gap.schema.json")
		if err != nil {
			intentGapSchemaErr = fmt.Errorf("compile: %w", err)
			return
		}
		intentGapSchema = s
	})
	if intentGapSchemaErr != nil {
		return nil, intentGapSchemaErr
	}
	return intentGapSchema, nil
}

// ValidateFindings checks each element of a findings array against the
// intent-gap finding schema. Empty / nil input is valid: an analyzed
// upload with no findings is a legitimate "no gaps found" result.
//
// The CLI calls this before upload so schema violations surface
// locally with a clear field path; the server's identical validator
// is the second line of defense.
func ValidateFindings(findings json.RawMessage) error {
	trim := bytes.TrimSpace(findings)
	if len(trim) == 0 || string(trim) == "[]" {
		return nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(trim, &arr); err != nil {
		return fmt.Errorf("findings: must be a JSON array (%w)", err)
	}
	if len(arr) == 0 {
		return nil
	}
	schema, err := IntentGapFindingSchema()
	if err != nil {
		return fmt.Errorf("findings schema unavailable: %w", err)
	}
	for i, raw := range arr {
		var v interface{}
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("findings[%d]: invalid JSON: %w", i, err)
		}
		if err := schema.Validate(v); err != nil {
			return fmt.Errorf("findings[%d]: %s", i, flattenSchemaErr(err))
		}
	}
	return nil
}

func flattenSchemaErr(err error) string {
	s := err.Error()
	s = strings.ReplaceAll(s, "\n", "; ")
	s = strings.TrimSpace(s)
	return s
}
