package kiroide

import (
	"context"
	"testing"

	agentKiro "github.com/semanticash/cli/internal/agents/kiro"
	"github.com/semanticash/cli/internal/attribution/events"
)

// TestKiroIDE_ReplayBindsEvidence verifies candidate binding from replayed Kiro
// IDE operations. Creates produce line evidence; relocations produce
// provider-touch evidence.
func TestKiroIDE_ReplayBindsEvidence(t *testing.T) {
	const repoRoot = "/repo"

	chain := func(op agentKiro.FileOperation) events.Candidates {
		bs := newKiroFakeBlobPutter()
		ev, ok := buildEventForOp(context.Background(), op, "exec-1", 0, "sess-1", 0, "transcript", repoRoot, bs)
		if !ok {
			t.Fatalf("buildEventForOp dropped op %+v", op)
		}
		row := events.EventRow{
			Provider: ev.Provider, Role: ev.Role, ToolUses: ev.ToolUsesJSON,
			PayloadHash: ev.PayloadHash, Payload: bs.stored[ev.PayloadHash],
			EventID: ev.EventID, Ts: ev.Timestamp,
		}
		cands, _ := events.BuildCandidatesFromRows([]events.EventRow{row}, repoRoot, nil)
		return cands
	}

	t.Run("create_is_line_level", func(t *testing.T) {
		cands := chain(agentKiro.FileOperation{
			ActionType: "create", ActionID: "a1",
			FilePath: "main.go", Content: "package main\nfunc main() {}\n",
		})
		if _, ok := cands.AILines["main.go"]["package main"]; !ok {
			t.Errorf("AILines[main.go] missing content; got %v", cands.AILines["main.go"])
		}
		if got := cands.FileProvider["main.go"]; got != agentKiro.ProviderNameIDE {
			t.Errorf("FileProvider[main.go] = %q, want %q", got, agentKiro.ProviderNameIDE)
		}
		if _, ok := cands.LineProviders["main.go"]["package main"][agentKiro.ProviderNameIDE]; !ok {
			t.Errorf("LineProviders[main.go][package main] owner = %v, want %q", cands.LineProviders["main.go"]["package main"], agentKiro.ProviderNameIDE)
		}
	})

	t.Run("relocate_is_provider_touch", func(t *testing.T) {
		cands := chain(agentKiro.FileOperation{
			ActionType: "smartRelocate", ActionID: "a2",
			SourcePath: "old/name.go", DestPath: "new/name.go",
		})
		if got := cands.ProviderTouchedFiles["new/name.go"]; got != agentKiro.ProviderNameIDE {
			t.Errorf("ProviderTouchedFiles[new/name.go] = %q, want %q", got, agentKiro.ProviderNameIDE)
		}
		if len(cands.AILines) != 0 {
			t.Errorf("relocate should not produce line evidence; got %v", cands.AILines)
		}
	})
}
