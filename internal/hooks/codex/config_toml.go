package codex

import (
	"fmt"

	"github.com/pelletier/go-toml/v2"
)

// readConfigTOML parses config data without replacing malformed content.
func readConfigTOML(data []byte) (map[string]any, error) {
	if len(data) == 0 {
		return make(map[string]any), nil
	}
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse codex config.toml: %w", err)
	}
	if doc == nil {
		doc = make(map[string]any)
	}
	return doc, nil
}

// writeConfigTOML returns deterministic TOML. Parsing and rewriting does not
// preserve comments or key order.
func writeConfigTOML(doc map[string]any) ([]byte, error) {
	out, err := toml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshal codex config.toml: %w", err)
	}
	return out, nil
}
