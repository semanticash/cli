package copilot

import (
	"encoding/json"
	"testing"

	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/hooks/testutil"
)

// TestDirectEmit_Contract freezes the full output for each event kind
// Copilot dispatches. Special attention to the TruncateClean
// semantics: Copilot's prompt summary normalizes whitespace and does
// not append an ellipsis. The prompt case includes a representative
// input so the golden captures the exact wire shape.
func TestDirectEmit_Contract(t *testing.T) {
	cases := []testutil.Case{
		{
			Name:        "prompt",
			Description: "User prompt; Copilot summary trims whitespace and collapses newlines without appending an ellipsis",
			Event: &hooks.Event{
				Type:      hooks.PromptSubmitted,
				SessionID: "sess-copilot-1",
				TurnID:    "turn-1",
				Prompt:    "  Refactor the\nretry handler  ",
				CWD:       "/repo",
				Timestamp: 1714000000000,
			},
		},
		{
			Name:        "write",
			Description: "Copilot Write with path/file_text shape, normalized to {file_path, content} in the payload blob",
			Event: &hooks.Event{
				Type:      hooks.ToolStepCompleted,
				SessionID: "sess-copilot-1",
				TurnID:    "turn-1",
				ToolUseID: "copilot-write-1",
				ToolName:  "Write",
				CWD:       "/repo",
				ToolInput: json.RawMessage(`{"path":"/repo/new.go","file_text":"package main\n"}`),
				Timestamp: 1714000010000,
			},
		},
		{
			Name:        "edit",
			Description: "Copilot Edit with path/old_str/new_str shape, normalized to the canonical scorer shape",
			Event: &hooks.Event{
				Type:      hooks.ToolStepCompleted,
				SessionID: "sess-copilot-1",
				TurnID:    "turn-1",
				ToolUseID: "copilot-edit-1",
				ToolName:  "Edit",
				CWD:       "/repo",
				ToolInput: json.RawMessage(`{"path":"/repo/main.go","old_str":"foo","new_str":"bar"}`),
				Timestamp: 1714000020000,
			},
		},
		{
			Name:        "bash",
			Description: "Copilot Bash with description; description is REDACTED in both payload and provenance (differs from Claude)",
			Event: &hooks.Event{
				Type:      hooks.ToolStepCompleted,
				SessionID: "sess-copilot-1",
				TurnID:    "turn-1",
				ToolUseID: "copilot-bash-1",
				ToolName:  "Bash",
				CWD:       "/repo",
				ToolInput: json.RawMessage(`{"command":"cat file.txt","description":"Print file"}`),
				ToolResponse: json.RawMessage(`{
					"resultType":"success",
					"textResultForLlm":"ok\n<exited with exit code 0>"
				}`),
				Timestamp: 1714000030000,
			},
		},
		{
			Name:        "subagent_prompt",
			Description: "Copilot Agent prompt; summary uses TruncateClean like the top-level prompt",
			Event: &hooks.Event{
				Type:      hooks.SubagentPromptSubmitted,
				SessionID: "sess-copilot-1",
				TurnID:    "turn-1",
				ToolUseID: "copilot-agent-1",
				ToolName:  "Agent",
				CWD:       "/repo",
				ToolInput: json.RawMessage(`{
					"description":"Create JSON",
					"prompt":"Create the json file",
					"agent_type":"general-purpose",
					"name":"json-creator"
				}`),
				Timestamp: 1714000040000,
			},
		},
		{
			Name:        "subagent_completed",
			Description: "Copilot SubagentStop; summary comes from textResultForLlm via TruncateClean",
			Event: &hooks.Event{
				Type:      hooks.SubagentCompleted,
				SessionID: "sess-copilot-1",
				TurnID:    "turn-1",
				ToolUseID: "copilot-agent-1",
				ToolName:  "Agent",
				CWD:       "/repo",
				ToolInput: json.RawMessage(`{
					"description":"Create JSON",
					"prompt":"Create the json file",
					"agent_type":"general-purpose",
					"name":"json-creator"
				}`),
				ToolResponse: json.RawMessage(`{
					"resultType":"success",
					"textResultForLlm":"Created /repo/out.json."
				}`),
				Timestamp: 1714000050000,
			},
		},
	}

	testutil.RunGolden(t, &Provider{}, "testdata/direct_emit", cases)
}
