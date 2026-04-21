package claude

import (
	"encoding/json"
	"testing"

	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/hooks/testutil"
)

// TestDirectEmit_Contract freezes the full []broker.RawEvent output
// and the content-addressed blob map for every event kind Claude
// dispatches. A change in any field emitted by the shared builder
// or by this provider's glue fails this test until a fresh golden
// is committed with an explicit rationale.
func TestDirectEmit_Contract(t *testing.T) {
	cases := []testutil.Case{
		{
			Name:        "prompt",
			Description: "UserPromptSubmit with a short prompt body",
			Event: &hooks.Event{
				Type:          hooks.PromptSubmitted,
				SessionID:     "sess-claude-1",
				TurnID:        "turn-1",
				Prompt:        "Refactor the retry handler in payments.go",
				TranscriptRef: "/workspace/.claude/projects/test/sess-claude-1.jsonl",
				Timestamp:     1714000000000,
			},
		},
		{
			Name:        "write",
			Description: "PostToolUse[Write] creating a new Go file",
			Event: &hooks.Event{
				Type:          hooks.ToolStepCompleted,
				SessionID:     "sess-claude-1",
				TurnID:        "turn-1",
				ToolUseID:     "toolu_write_1",
				ToolName:      "Write",
				ToolInput:     json.RawMessage(`{"file_path":"/repo/main.go","content":"package main\n"}`),
				TranscriptRef: "/workspace/.claude/projects/test/sess-claude-1.jsonl",
				Timestamp:     1714000010000,
			},
		},
		{
			Name:        "edit",
			Description: "PostToolUse[Edit] with a single old_string/new_string swap",
			Event: &hooks.Event{
				Type:          hooks.ToolStepCompleted,
				SessionID:     "sess-claude-1",
				TurnID:        "turn-1",
				ToolUseID:     "toolu_edit_1",
				ToolName:      "Edit",
				ToolInput:     json.RawMessage(`{"file_path":"/repo/main.go","old_string":"foo","new_string":"bar"}`),
				TranscriptRef: "/workspace/.claude/projects/test/sess-claude-1.jsonl",
				Timestamp:     1714000020000,
			},
		},
		{
			Name:        "bash",
			Description: "PostToolUse[Bash] with a description; verifies description flows unredacted to the payload blob",
			Event: &hooks.Event{
				Type:          hooks.ToolStepCompleted,
				SessionID:     "sess-claude-1",
				TurnID:        "turn-1",
				ToolUseID:     "toolu_bash_1",
				ToolName:      "Bash",
				ToolInput:     json.RawMessage(`{"command":"go test ./...","description":"Run all tests"}`),
				TranscriptRef: "/workspace/.claude/projects/test/sess-claude-1.jsonl",
				Timestamp:     1714000030000,
			},
		},
		{
			Name:        "subagent_prompt",
			Description: "PreToolUse[Agent] with a prompt",
			Event: &hooks.Event{
				Type:          hooks.SubagentPromptSubmitted,
				SessionID:     "sess-claude-1",
				TurnID:        "turn-1",
				ToolUseID:     "toolu_agent_1",
				ToolName:      "Agent",
				ToolInput:     json.RawMessage(`{"prompt":"Review the auth package","description":"Code review"}`),
				TranscriptRef: "/workspace/.claude/projects/test/sess-claude-1.jsonl",
				Timestamp:     1714000040000,
			},
		},
	}

	testutil.RunGolden(t, &Provider{}, "testdata/direct_emit", cases)
}
