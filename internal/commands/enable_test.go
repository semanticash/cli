package commands

import (
	"regexp"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/service"
)

func TestRenderEnablePlain(t *testing.T) {
	res := &service.EnableResult{
		RepoRoot:           "/tmp/repo",
		SemanticaDir:       "/tmp/repo/.semantica",
		CheckpointID:       "6215ac30-1234-5678-9abc-def012345678",
		WorkspaceTierTitle: "Free",
		UpdateAvailable:    true,
		LatestVersion:      "v0.2.0",
		HooksInstalled:     true,
		Providers:          []string{"claude-code", "cursor"},
	}

	got := renderEnablePlain(res)

	for _, want := range []string{
		"Semantica enabled",
		"Repo: /tmp/repo",
		"Store: /tmp/repo/.semantica",
		"Workspace tier: Free",
		"Hooks: pre-commit, post-commit, commit-msg installed",
		"Agents: claude-code, cursor",
		"Update available",
		"Version: v0.2.0",
		"Install: " + cliUpgradeCommand,
		"Note: If Cursor is open, reopen this project for hooks to take effect.",
		"Tip: Run `semantica connect` to sync attribution to your dashboard.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("plain output missing %q:\n%s", want, got)
		}
	}
}

func TestRenderEnableCard(t *testing.T) {
	res := &service.EnableResult{
		RepoRoot:           "/tmp/repo",
		SemanticaDir:       "/tmp/repo/.semantica",
		CheckpointID:       "6215ac30-1234-5678-9abc-def012345678",
		WorkspaceTierTitle: "Free",
		UpdateAvailable:    true,
		LatestVersion:      "v0.2.0",
		HooksInstalled:     true,
		Providers:          []string{"claude-code"},
	}

	got := stripANSI(renderEnableCard(res))

	for _, want := range []string{
		"Semantica",
		"Code, with provenance.",
		"Enabled in /tmp/repo",
		"Hooks: pre-commit, post-commit, commit-msg installed",
		"Workspace tier: Free",
		"Agents: claude-code",
		"Update available",
		"Version: v0.2.0",
		"Install:",
		cliUpgradeCommand,
		"Store: /tmp/repo/.semantica",
		"Tip:",
		"semantica connect",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("card output missing %q:\n%s", want, got)
		}
	}
}

func stripANSI(s string) string {
	re := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	return re.ReplaceAllString(s, "")
}
