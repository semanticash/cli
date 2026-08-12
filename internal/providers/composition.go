// Package providers builds the provider registries used by the CLI.
package providers

import (
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/hooks/claude"
	"github.com/semanticash/cli/internal/hooks/codex"
	"github.com/semanticash/cli/internal/hooks/copilot"
	"github.com/semanticash/cli/internal/hooks/cursor"
	"github.com/semanticash/cli/internal/hooks/gemini"
	"github.com/semanticash/cli/internal/hooks/kirocli"
	"github.com/semanticash/cli/internal/hooks/kiroide"
	"github.com/semanticash/cli/internal/llm"
)

// NewWriterRegistry returns the CLI's LLM writers in fallback order.
// The first available writer to complete successfully wins.
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

// NewHookRegistry returns all hook providers supported by the CLI.
// Registry.List returns them in canonical order.
func NewHookRegistry() *hooks.Registry {
	return hooks.NewRegistry(
		claude.New(),
		codex.New(),
		copilot.New(),
		cursor.New(),
		gemini.New(),
		kirocli.New(),
		kiroide.New(),
	)
}
