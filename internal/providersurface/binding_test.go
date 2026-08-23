// Package providersurface tests provider hook events against attribution
// candidate binding. Fixture tests do not verify live hook delivery.
package providersurface

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/attribution/events"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/hooks/claude"
	"github.com/semanticash/cli/internal/hooks/codex"
	"github.com/semanticash/cli/internal/hooks/copilot"
	"github.com/semanticash/cli/internal/hooks/cursor"
	"github.com/semanticash/cli/internal/hooks/gemini"
	"github.com/semanticash/cli/internal/hooks/kirocli"
)

// blobStore keeps emitted payloads in memory for candidate construction.
type blobStore struct{ m map[string][]byte }

func newBlobStore() *blobStore { return &blobStore{m: map[string][]byte{}} }

func (b *blobStore) Put(_ context.Context, p []byte) (string, int64, error) {
	h := fmt.Sprintf("blob-%d", len(b.m))
	b.m[h] = append([]byte(nil), p...)
	return h, int64(len(p)), nil
}

// chain converts direct-hook events and payloads into attribution candidates.
func chain(t *testing.T, emitter hooks.DirectHookEmitter, event *hooks.Event, repoRoot string) (events.Candidates, events.EventStats) {
	t.Helper()
	bs := newBlobStore()
	raws, err := emitter.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("BuildHookEvents: %v", err)
	}
	rows := make([]events.EventRow, 0, len(raws))
	for _, r := range raws {
		rows = append(rows, events.EventRow{
			Provider: r.Provider, Role: r.Role, ToolUses: r.ToolUsesJSON,
			PayloadHash: r.PayloadHash, Payload: bs.m[r.PayloadHash],
			EventID: r.EventID, Ts: r.Timestamp,
		})
	}
	return events.BuildCandidatesFromRows(rows, repoRoot, nil)
}

func toolStep(toolName, cwd, input string) *hooks.Event {
	return &hooks.Event{
		Type: hooks.ToolStepCompleted, SessionID: "sess-1", TurnID: "turn-1",
		ToolUseID: "step-1", ToolName: toolName, CWD: cwd, Timestamp: 1000,
		ToolInput: []byte(input),
	}
}

// TestProviderSurface_LineLevelBinding verifies provider ownership of emitted
// line evidence.
func TestProviderSurface_LineLevelBinding(t *testing.T) {
	const repoRoot = "/repo"
	cases := []struct {
		name         string
		emitter      hooks.DirectHookEmitter
		event        *hooks.Event
		wantProvider string
		wantFile     string
		wantLine     string
	}{
		{
			name:         "claude_write",
			emitter:      claude.New(),
			event:        toolStep("Write", repoRoot, `{"file_path":"/repo/main.go","content":"package main\n"}`),
			wantProvider: "claude_code", wantFile: "main.go", wantLine: "package main",
		},
		{
			name:         "cursor_write",
			emitter:      cursor.New(),
			event:        toolStep("Write", repoRoot, `{"conversation_id":"c1","file_path":"/repo/new.txt","edits":[{"old_string":"","new_string":"hello\n"}]}`),
			wantProvider: "cursor", wantFile: "new.txt", wantLine: "hello",
		},
		{
			name:         "copilot_write",
			emitter:      copilot.New(),
			event:        toolStep("Write", repoRoot, `{"path":"/repo/new.txt","file_text":"hello\n"}`),
			wantProvider: "copilot", wantFile: "new.txt", wantLine: "hello",
		},
		{
			name:         "gemini_write",
			emitter:      gemini.New(),
			event:        toolStep("Write", repoRoot, `{"file_path":"/repo/new.go","content":"package main\n"}`),
			wantProvider: "gemini_cli", wantFile: "new.go", wantLine: "package main",
		},
		{
			name:         "kirocli_write",
			emitter:      kirocli.New(),
			event:        toolStep("Write", repoRoot, `{"command":"create","path":"/repo/new.go","content":"package main\n"}`),
			wantProvider: "kiro-cli", wantFile: "new.go", wantLine: "package main",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cands, _ := chain(t, c.emitter, c.event, repoRoot)
			if got := cands.FileProvider[c.wantFile]; got != c.wantProvider {
				t.Errorf("FileProvider[%s] = %q, want %q", c.wantFile, got, c.wantProvider)
			}
			if _, ok := cands.AILines[c.wantFile][c.wantLine]; !ok {
				t.Errorf("AILines[%s] missing %q; got %v", c.wantFile, c.wantLine, cands.AILines[c.wantFile])
			}
			if _, ok := cands.LineProviders[c.wantFile][c.wantLine][c.wantProvider]; !ok {
				t.Errorf("LineProviders[%s][%s] owner = %v, want %q", c.wantFile, c.wantLine, cands.LineProviders[c.wantFile][c.wantLine], c.wantProvider)
			}
		})
	}
}

// TestProviderSurface_CodexApplyPatch verifies that additions produce line
// evidence and deletions produce provider-touch evidence.
func TestProviderSurface_CodexApplyPatch(t *testing.T) {
	repoRoot := t.TempDir()
	repoRoot, _ = filepath.EvalSymlinks(repoRoot)
	addFile := filepath.Join(repoRoot, "added.go")
	delFile := filepath.Join(repoRoot, "gone.go")

	addEnvelope := strings.Join([]string{
		"*** Begin Patch", "*** Add File: " + addFile,
		"+package added", "+func Added() {}", "*** End Patch", "",
	}, "\n")
	delEnvelope := strings.Join([]string{
		"*** Begin Patch", "*** Delete File: " + delFile, "*** End Patch", "",
	}, "\n")

	t.Run("add_is_line_level", func(t *testing.T) {
		ev := toolStep("apply_patch", repoRoot, fmt.Sprintf(`{"command":%q}`, addEnvelope))
		cands, _ := chain(t, codex.New(), ev, repoRoot)
		if got := cands.FileProvider["added.go"]; got != "codex" {
			t.Errorf("FileProvider[added.go] = %q, want codex", got)
		}
		if _, ok := cands.AILines["added.go"]["package added"]; !ok {
			t.Errorf("AILines[added.go] missing added content; got %v", cands.AILines["added.go"])
		}
	})

	t.Run("delete_is_provider_touch", func(t *testing.T) {
		ev := toolStep("apply_patch", repoRoot, fmt.Sprintf(`{"command":%q}`, delEnvelope))
		cands, _ := chain(t, codex.New(), ev, repoRoot)
		if got := cands.ProviderTouchedFiles["gone.go"]; got != "codex" {
			t.Errorf("ProviderTouchedFiles[gone.go] = %q, want codex", got)
		}
		if _, ok := cands.AILines["gone.go"]; ok {
			t.Errorf("delete should not produce line evidence; got %v", cands.AILines["gone.go"])
		}
	})
}
