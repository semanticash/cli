// Package providers owns the production-wiring entry points for
// every provider registry the CLI uses. Splitting this out from
// cmd/semantica/main.go keeps the production provider list
// discoverable, testable, and reusable across binaries (current
// CLI plus any future embedded build or sub-binary). The package
// has no business logic - it only enumerates the production set
// of writers and future hook providers in the agreed
// fallback order.
package providers

import "github.com/semanticash/cli/internal/llm"

// NewWriterRegistry returns the production WriterRegistry used by
// `semantica explain --generate` and the post-commit auto-playbook
// flow. The fallback order matters: when a user has multiple AI
// CLIs installed, the first writer that successfully generates a
// response wins. Order rationale:
//
//   - Claude Code first: established daily-driver assumption used
//     throughout the codebase; produces JSON-shaped responses that
//     parse cleanly into the narrative shape.
//   - Codex second: first-class capture provider with
//     `codex exec --output-last-message` for clean final-output
//     capture.
//   - Cursor / Gemini / Copilot / Kiro CLI follow: stable but
//     less battle-tested for playbook generation.
//
// Tests that want a custom set construct llm.NewWriterRegistry
// directly with their own writers; this constructor exists for the
// production-wiring path only.
func NewWriterRegistry() *llm.WriterRegistry {
	return llm.NewWriterRegistry(
		llm.Claude(),
		llm.Codex(),
		llm.Cursor(),
		llm.Gemini(),
		llm.Copilot(),
		llm.KiroCLI(),
	)
}
