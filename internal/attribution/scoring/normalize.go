package scoring

import (
	"strings"
	"unicode"
)

// NormalizeWhitespace removes all whitespace characters from s.
// Used as a second-tier match when exact trimmed comparison fails,
// catching formatter/linter modifications like:
//
//	"func foo(){" vs "func foo() {"
//	"return   x+y" vs "return x + y"
func NormalizeWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if !unicode.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// BuildNormalizedSet derives a whitespace-stripped line set from the
// AI candidate set. Each trimmed line is stripped of all whitespace
// and stored per file path.
func BuildNormalizedSet(aiLines map[string]map[string]struct{}) map[string]map[string]struct{} {
	norm := make(map[string]map[string]struct{}, len(aiLines))
	for fp, lines := range aiLines {
		norm[fp] = make(map[string]struct{}, len(lines))
		for line := range lines {
			norm[fp][NormalizeWhitespace(line)] = struct{}{}
		}
	}
	return norm
}
