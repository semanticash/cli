package events

import (
	"fmt"
	"testing"
)

// These tests cover candidate construction from EventRows. Provider hook
// integration is tested in internal/providersurface.

// canonicalProviders lists provider IDs stored on attribution events.
var canonicalProviders = []string{
	"claude_code", "codex", "cursor", "copilot", "gemini_cli", "kiro-cli", "kiro-ide",
}

// TestCandidateBuilder_PreservesProviderIdentity verifies that provider IDs
// survive candidate construction.
func TestCandidateBuilder_PreservesProviderIdentity(t *testing.T) {
	const repoRoot = "/test/repo"
	toolUses := `{"content_types":["tool_use"],"tools":[{"name":"Write","file_path":"main.go","file_op":"write"}]}`
	for _, provider := range canonicalProviders {
		t.Run(provider, func(t *testing.T) {
			rows := []EventRow{{
				Provider: provider, Role: "assistant", ToolUses: toolUses,
				PayloadHash: "h", Payload: makePayload(repoRoot, "main.go", "package main\nfunc main() {}\n"),
			}}
			cands, stats := BuildCandidatesFromRows(rows, repoRoot, nil)

			if stats.AIToolEvents != 1 || stats.PayloadsLoaded != 1 {
				t.Fatalf("stats = %+v, want one loaded AI tool event", stats)
			}
			if cands.FileProvider["main.go"] != provider {
				t.Errorf("FileProvider[main.go] = %q, want %q", cands.FileProvider["main.go"], provider)
			}
			if _, ok := cands.AILines["main.go"]["package main"]; !ok {
				t.Errorf("missing line evidence for %q; got %v", provider, cands.AILines["main.go"])
			}
			if _, ok := cands.LineProviders["main.go"]["package main"][provider]; !ok {
				t.Errorf("LineProviders owner = %v, want %q", cands.LineProviders["main.go"]["package main"], provider)
			}
		})
	}
}

// TestDetector_ProviderTouchShapes verifies that recognized edit tokens produce
// provider-touch evidence without line evidence.
func TestDetector_ProviderTouchShapes(t *testing.T) {
	const repoRoot = "/test/repo"
	cases := []struct {
		name     string
		provider string
		pattern  string // recognized content-type or tool-name token
		toolName string
	}{
		{"cursor_file_edit", "cursor", "cursor_file_edit", "cursor_edit"},
		{"cursor_edit", "cursor", "cursor_edit", "cursor_edit"},
		{"copilot_file_edit", "copilot", "copilot_file_edit", "copilot_file_edit"},
		{"kiro_file_edit", "kiro-cli", "kiro_file_edit", "kiro_file_edit"},
		{"codex_file_edit", "codex", "codex_file_edit", "codex_file_edit"},
		{"gemini_write_file", "gemini_cli", "write_file", "write_file"},
		{"gemini_edit_file", "gemini_cli", "edit_file", "edit_file"},
		{"gemini_save_file", "gemini_cli", "save_file", "save_file"},
		{"gemini_replace", "gemini_cli", "replace", "replace"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			toolUses := fmt.Sprintf(`{"content_types":["%s"],"tools":[{"name":"%s","file_path":"handler.go","file_op":"edit"}]}`, c.pattern, c.toolName)
			rows := []EventRow{{Provider: c.provider, Role: "assistant", ToolUses: toolUses}}
			cands, stats := BuildCandidatesFromRows(rows, repoRoot, nil)

			if stats.AIToolEvents != 1 {
				t.Fatalf("%s not recognized as an AI tool event: stats=%+v", c.pattern, stats)
			}
			if got := cands.ProviderTouchedFiles["handler.go"]; got != c.provider {
				t.Errorf("ProviderTouchedFiles[handler.go] = %q, want %q", got, c.provider)
			}
			if len(cands.AILines) != 0 {
				t.Errorf("provider-touch produced line evidence: %v", cands.AILines)
			}
		})
	}
}

// TestDetector_DefensiveTokensRecognized verifies compatibility tokens that
// current extractors do not emit.
func TestDetector_DefensiveTokensRecognized(t *testing.T) {
	for _, tok := range []string{"editFile", "createFile"} {
		toolUses := fmt.Sprintf(`{"content_types":["tool_use"],"tools":[{"name":"%s","file_path":"f.go"}]}`, tok)
		if !HasProviderFileEdit(toolUses) {
			t.Errorf("HasProviderFileEdit(%q) = false, want true", tok)
		}
	}
}
