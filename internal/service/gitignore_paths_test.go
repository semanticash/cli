package service

import (
	"testing"

	"github.com/semanticash/cli/internal/providers"
)

func TestProviderGitignorePathsMatchProductionProviderNames(t *testing.T) {
	expected := map[string]string{
		"claude-code": ".claude/settings.local.json",
		"cursor":      ".cursor/hooks.json",
		"gemini-cli":  ".gemini/settings.json",
		"copilot":     ".github/hooks/semantica.json",
		"kiro-ide":    ".kiro/hooks/",
		"kiro-cli":    ".kiro/agents/semantica.json",
	}

	registry := providers.NewHookRegistry()

	for name, path := range expected {
		if registry.Get(name) == nil {
			t.Errorf("gitignore mapping references unknown provider %q", name)
		}
		if got := ProviderGitignorePaths[name]; got != path {
			t.Errorf("mapping[%q] = %q, want %q", name, got, path)
		}
	}

	for name := range ProviderGitignorePaths {
		if registry.Get(name) == nil {
			t.Errorf("mapping uses non-canonical provider name %q", name)
		}
	}
}
