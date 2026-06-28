// Package providers owns the production-wiring entry points for
// every provider registry the CLI uses. Splitting this out from
// cmd/semantica/main.go keeps the production provider list
// discoverable, testable, and reusable across binaries (current
// CLI plus any future embedded build or sub-binary). The package
// has no business logic - it only enumerates the production set
// of writers and future hook providers in the agreed
// fallback order.
package providers

import (
	"time"

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

// NewIntentGapWriterRegistry returns the WriterRegistry the intent-gap
// analyzer uses. Order matches the default writer registry (Claude
// first, then Codex, then the rest) so user preference for Claude is
// preserved. Where this diverges is per-writer timeouts:
//
//   - claude_code=25s caps Claude's worst case. Dogfood telemetry
//     showed Claude sitting at the global 120s timeout on intent-gap
//     prompts (~150KB) without responding; capping at 25s means a
//     hanging Claude falls through to the next writer in 25s instead
//     of blocking the chain for two minutes.
//   - codex=45s and copilot=60s reflect observed success times with
//     healthy headroom.
//   - Writers not in the map use the shared 120s default.
//
// Writers whose CLI is not installed (Find() returns "") are skipped
// without any subprocess invocation, so an uninstalled provider in
// the list adds zero wall-clock to a run.
func NewIntentGapWriterRegistry() *llm.WriterRegistry {
	return llm.NewWriterRegistryWithTimeouts(
		[]llm.Writer{
			llm.Claude(),
			llm.Codex(),
			llm.Cursor(),
			llm.Gemini(),
			llm.Copilot(),
			llm.KiroCLI(),
		},
		map[string]time.Duration{
			"claude_code": 25 * time.Second,
			"codex":       45 * time.Second,
			"copilot":     60 * time.Second,
		},
	)
}

// NewIntentGapWriterRegistryRestrictedTo returns an intent-gap
// registry containing only the named writer. It exists for the
// SEMANTICA_INTENTGAP_FORCE_WRITER debug knob: a developer
// dogfooding a specific provider can force the chain to that one
// writer (with its default 120s timeout, no per-writer cap) to
// observe its real behavior under the intent-gap workload. Returns
// nil when the name is not a known production writer; the caller
// then falls back to the default registry.
func NewIntentGapWriterRegistryRestrictedTo(name string) *llm.WriterRegistry {
	w := resolveProductionWriter(name)
	if w == nil {
		return nil
	}
	return llm.NewWriterRegistry(w)
}

func resolveProductionWriter(name string) llm.Writer {
	switch name {
	case "claude_code":
		return llm.Claude()
	case "codex":
		return llm.Codex()
	case "cursor":
		return llm.Cursor()
	case "gemini_cli":
		return llm.Gemini()
	case "copilot":
		return llm.Copilot()
	case "kiro_cli":
		return llm.KiroCLI()
	}
	return nil
}

// NewHookRegistry returns the production hooks.Registry used by
// every consumer that reads the capture-side provider set: the
// worker, the commit-msg hook, enable/disable, capture, agents,
// and the health checks. Argument order does not matter because the
// registry's List() always returns providers in the canonical
// order defined in internal/hooks.providerOrder, so all consumers
// see the same iteration regardless of insertion sequence.
//
// Tests that want a custom set construct hooks.NewRegistry
// directly with their own providers; this constructor exists for
// the production-wiring path only.
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
