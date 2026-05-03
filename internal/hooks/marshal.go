package hooks

import (
	"bytes"
	"encoding/json"
)

// MarshalSettingsJSON serializes hook settings as indented JSON
// without HTML escaping, keeping shell commands readable.
func MarshalSettingsJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// MarshalCompactJSON is the compact counterpart for json.RawMessage
// fragments embedded inside settings documents.
func MarshalCompactJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
