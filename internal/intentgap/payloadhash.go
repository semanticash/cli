// Package intentgap mirrors the API's canonical payload_hash computation.
// The upload endpoint recomputes the hash server-side and rejects mismatches,
// so the CLI and API encoders must stay byte-compatible.
//
// The golden fixture under testdata/payloadhash/ is copied from the API
// fixture, and the tests compare them when both repositories are present.
package intentgap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// PayloadHashInput captures the 13 fields that contribute to a CLI
// upload's payload_hash. Excluded fields (per the canonical spec):
// tenant_id, producer_user_id (server-owned), producer_device_id
// (intentional cross-device dedup), analysis_source, analysis_scope
// (server-forced for this endpoint), produced_at, received_at,
// upload_id.
type PayloadHashInput struct {
	RepositoryID          string
	PRNumber              int32
	HeadSHA               string
	BaseSHA               string
	AlgorithmVersion      string
	PromptTemplateVersion string
	FindingSchemaVersion  string
	RedactionVersion      string
	Provider              string
	Model                 string
	ProducerState         string
	// CoverageSummary and Findings carry raw JSON. nil or empty input
	// is treated as {} / [] so callers that omit the fields hash the
	// same as ones that send empty literals.
	CoverageSummary json.RawMessage
	Findings        json.RawMessage
}

// payloadHashFieldOrder pins the top-level emit order. Lexicographic
// sorting was rejected because explicit numeric order keeps the
// fixture bytes human-auditable.
var payloadHashFieldOrder = []string{
	"repository_id",
	"pr_number",
	"head_sha",
	"base_sha",
	"algorithm_version",
	"prompt_template_version",
	"finding_schema_version",
	"redaction_version",
	"provider",
	"model",
	"producer_state",
	"coverage_summary",
	"findings",
}

// ComputePayloadHash returns the lowercase-hex sha256 of the canonical
// payload bytes and the bytes themselves.
func ComputePayloadHash(in PayloadHashInput) (string, []byte, error) {
	canonical, err := canonicalPayloadBytes(in)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), canonical, nil
}

func canonicalPayloadBytes(in PayloadHashInput) ([]byte, error) {
	cov, err := canonicalizeObjectOrEmpty(in.CoverageSummary)
	if err != nil {
		return nil, fmt.Errorf("canonicalize coverage_summary: %w", err)
	}
	findings, err := canonicalizeArrayOrEmpty(in.Findings)
	if err != nil {
		return nil, fmt.Errorf("canonicalize findings: %w", err)
	}

	values := map[string][]byte{
		"repository_id":           jsonStrBytes(in.RepositoryID),
		"pr_number":               []byte(strconv.FormatInt(int64(in.PRNumber), 10)),
		"head_sha":                jsonStrBytes(in.HeadSHA),
		"base_sha":                jsonStrBytes(in.BaseSHA),
		"algorithm_version":       jsonStrBytes(in.AlgorithmVersion),
		"prompt_template_version": jsonStrBytes(in.PromptTemplateVersion),
		"finding_schema_version":  jsonStrBytes(in.FindingSchemaVersion),
		"redaction_version":       jsonStrBytes(in.RedactionVersion),
		"provider":                jsonStrBytes(in.Provider),
		"model":                   jsonStrBytes(in.Model),
		"producer_state":          jsonStrBytes(in.ProducerState),
		"coverage_summary":        cov,
		"findings":                findings,
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, key := range payloadHashFieldOrder {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(jsonStrBytes(key))
		buf.WriteByte(':')
		buf.Write(values[key])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// jsonStrBytes encodes a Go string as a JSON string literal with HTML
// escaping disabled. The API mirror does the same; flipping it would
// produce <  / >  / & for <, >, &, breaking parity with
// the CLI's serializer.
func jsonStrBytes(s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	out := buf.Bytes()
	if len(out) > 0 && out[len(out)-1] == '\n' {
		out = out[:len(out)-1]
	}
	return out
}

func canonicalizeObjectOrEmpty(raw json.RawMessage) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte("{}"), nil
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("expected JSON object: %w", err)
	}
	return canonicalObject(parsed)
}

func canonicalizeArrayOrEmpty(raw json.RawMessage) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte("[]"), nil
	}
	var parsed []json.RawMessage
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("expected JSON array: %w", err)
	}
	return canonicalArray(parsed)
}

func canonicalize(raw json.RawMessage) ([]byte, error) {
	trim := bytes.TrimSpace(raw)
	if len(trim) == 0 {
		return nil, fmt.Errorf("empty JSON value")
	}
	switch trim[0] {
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trim, &obj); err != nil {
			return nil, err
		}
		return canonicalObject(obj)
	case '[':
		var arr []json.RawMessage
		if err := json.Unmarshal(trim, &arr); err != nil {
			return nil, err
		}
		return canonicalArray(arr)
	default:
		dec := json.NewDecoder(bytes.NewReader(trim))
		dec.UseNumber()
		var v interface{}
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		var out bytes.Buffer
		enc := json.NewEncoder(&out)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			return nil, err
		}
		b := out.Bytes()
		if len(b) > 0 && b[len(b)-1] == '\n' {
			b = b[:len(b)-1]
		}
		return b, nil
	}
}

func canonicalObject(obj map[string]json.RawMessage) ([]byte, error) {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(jsonStrBytes(k))
		buf.WriteByte(':')
		val, err := canonicalize(obj[k])
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", k, err)
		}
		buf.Write(val)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func canonicalArray(arr []json.RawMessage) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, v := range arr {
		if i > 0 {
			buf.WriteByte(',')
		}
		val, err := canonicalize(v)
		if err != nil {
			return nil, fmt.Errorf("index %d: %w", i, err)
		}
		buf.Write(val)
	}
	buf.WriteByte(']')
	return buf.Bytes(), nil
}
