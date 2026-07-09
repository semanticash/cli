package llm

import "strings"

// sanitizeUTF8 replaces invalid UTF-8 sequences with U+FFFD. Some
// LLM CLIs reject or hang on invalid UTF-8, so the registry normalizes
// prompts after redaction and before dispatch.
func sanitizeUTF8(s string) string {
	return strings.ToValidUTF8(s, "�")
}
