package intentgap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
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

// SchemaFilterResult reports the outcome of validating each finding in
// an array independently against the intent-gap finding schema.
//
// Kept always carries a valid JSON array (possibly "[]") suitable for
// downstream processing. DroppedReasons maps a normalized reason code
// (e.g. "schema_invalid_under_impl") to its count. DroppedSamples is a
// list of structural diagnostics intended for local logging only, not
// for upload. ArrayErr is non-nil only when validation cannot proceed
// for the whole array, such as an unparseable array or unavailable
// schema.
type SchemaFilterResult struct {
	Kept           json.RawMessage
	KeptCount      int
	DroppedCount   int
	DroppedReasons map[string]int
	DroppedSamples []string
	ArrayErr       error
}

// FilterFindingsBySchema validates each finding in the array
// independently. Findings that fail validation are dropped, counted by
// reason code, and recorded as structural diagnostics for local logging.
// The function returns a valid Kept array even when every finding is
// dropped. "All invalid" is not an error, only "the response isn't a
// JSON array of objects" is. Mirrors the cite-or-drop layer so one
// malformed finding does not fail the whole analysis.
func FilterFindingsBySchema(findings json.RawMessage) SchemaFilterResult {
	out := SchemaFilterResult{
		Kept:           json.RawMessage("[]"),
		DroppedReasons: map[string]int{},
	}
	trim := bytes.TrimSpace(findings)
	if len(trim) == 0 || string(trim) == "[]" {
		return out
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(trim, &arr); err != nil {
		out.ArrayErr = fmt.Errorf("findings: must be a JSON array (%w)", err)
		return out
	}
	if len(arr) == 0 {
		return out
	}
	schema, err := IntentGapFindingSchema()
	if err != nil {
		out.ArrayErr = fmt.Errorf("findings schema unavailable: %w", err)
		return out
	}

	var kept []json.RawMessage
	for i, raw := range arr {
		var v interface{}
		if err := json.Unmarshal(raw, &v); err != nil {
			out.DroppedCount++
			out.DroppedReasons["schema_invalid_unparseable"]++
			// Bytes are unparseable; we cannot extract structural-only
			// metadata, so log just the parse error. Avoid echoing the
			// raw bytes - they may include redacted-elsewhere content.
			out.DroppedSamples = append(out.DroppedSamples,
				fmt.Sprintf("finding[%d] unparseable: %v", i, err))
			continue
		}
		if err := schema.Validate(v); err != nil {
			kind := findingKindOrUnknown(v)
			reason := "schema_invalid_" + kind
			out.DroppedCount++
			out.DroppedReasons[reason]++
			// Structural-only diagnostic: kind, schema error path, and
			// the names of the top-level keys present on the finding.
			// Values are not echoed so prompt excerpts, summaries, file
			// paths, and code snippets do not leak through the log.
			out.DroppedSamples = append(out.DroppedSamples,
				fmt.Sprintf("finding[%d] kind=%s schema_error=%s keys=%s",
					i, kind, flattenSchemaErr(err), topLevelKeys(v)))
			continue
		}
		kept = append(kept, raw)
	}

	out.KeptCount = len(kept)
	if out.KeptCount > 0 {
		encoded, encErr := json.Marshal(kept)
		if encErr != nil {
			out.ArrayErr = fmt.Errorf("re-encode kept findings: %w", encErr)
			return out
		}
		out.Kept = encoded
	}
	return out
}

// findingKindOrUnknown reads the `kind` field from a decoded finding;
// when the field is missing or not one of the known kinds, it returns
// "unknown" so the drop-reason histogram stays bounded.
func findingKindOrUnknown(decoded interface{}) string {
	m, ok := decoded.(map[string]interface{})
	if !ok {
		return "unknown"
	}
	k, _ := m["kind"].(string)
	switch k {
	case "under_impl", "deferred", "unrequested":
		return k
	}
	return "unknown"
}

// topLevelKeys returns the sorted list of top-level property names of
// a decoded finding, formatted as a bracketed list. Keys are
// schema-defined identifiers; the values they point at can carry
// prompt text or code snippets and are intentionally not echoed.
func topLevelKeys(decoded interface{}) string {
	m, ok := decoded.(map[string]interface{})
	if !ok {
		return "[]"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return "[" + strings.Join(keys, ",") + "]"
}

func flattenSchemaErr(err error) string {
	s := err.Error()
	s = strings.ReplaceAll(s, "\n", "; ")
	s = strings.TrimSpace(s)
	return s
}
