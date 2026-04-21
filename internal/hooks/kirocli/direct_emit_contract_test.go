package kirocli

import (
	"encoding/json"
	"testing"

	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/hooks/testutil"
)

// TestDirectEmit_Contract freezes Kiro CLI's output. The most
// important invariants here are the synthetic kiro_file_edit tool
// name in ToolUsesJSON for Write and Edit, the empty ToolUsesJSON
// for Bash, and the Prompt-then-Task fallback for subagent inputs.
// These capture the full wire shape including hashes and
// blob contents.
func TestDirectEmit_Contract(t *testing.T) {
	cases := []testutil.Case{
		{
			Name:        "prompt",
			Description: "Kiro CLI prompt; SourceProjectPath derived from event.CWD (no transcript reference)",
			Event: &hooks.Event{
				Type:      hooks.PromptSubmitted,
				SessionID: "sess-kiro-1",
				TurnID:    "turn-1",
				CWD:       "/repo",
				Prompt:    "Add a retry handler to payments.go",
				Timestamp: 1714000000000,
			},
		},
		{
			Name:        "write",
			Description: "Kiro CLI fs_write create; ToolUsesJSON uses the synthetic kiro_file_edit name",
			Event: &hooks.Event{
				Type:      hooks.ToolStepCompleted,
				SessionID: "sess-kiro-1",
				TurnID:    "turn-1",
				ToolUseID: "kiro-step-1",
				ToolName:  "Write",
				CWD:       "/repo",
				ToolInput: json.RawMessage(`{"command":"create","path":"/repo/new.go","file_text":"package main\n"}`),
				Timestamp: 1714000010000,
			},
		},
		{
			Name:        "edit",
			Description: "Kiro CLI fs_write str_replace; old_str/new_str renamed to old_string/new_string in the payload blob",
			Event: &hooks.Event{
				Type:      hooks.ToolStepCompleted,
				SessionID: "sess-kiro-1",
				TurnID:    "turn-1",
				ToolUseID: "kiro-step-2",
				ToolName:  "Edit",
				ToolInput: json.RawMessage(`{"command":"str_replace","path":"/repo/main.go","old_str":"foo","new_str":"bar"}`),
				Timestamp: 1714000020000,
			},
		},
		{
			Name:        "bash",
			Description: "Kiro CLI execute_bash with a result array; ToolUsesJSON ships empty, stdout and stderr redacted",
			Event: &hooks.Event{
				Type:      hooks.ToolStepCompleted,
				SessionID: "sess-kiro-1",
				TurnID:    "turn-1",
				ToolUseID: "kiro-step-3",
				ToolName:  "Bash",
				ToolInput: json.RawMessage(`{"command":"ls","working_dir":"/repo"}`),
				ToolResponse: json.RawMessage(`{
					"success":true,
					"result":[{"exit_status":"0","stdout":"file1\nfile2","stderr":""}]
				}`),
				Timestamp: 1714000030000,
			},
		},
		{
			Name:        "subagent_prompt_from_prompt_field",
			Description: "Kiro CLI subagent input with prompt field set; prompt takes precedence over task",
			Event: &hooks.Event{
				Type:      hooks.SubagentPromptSubmitted,
				SessionID: "sess-kiro-1",
				TurnID:    "turn-1",
				ToolUseID: "kiro-agent-1",
				ToolName:  "Agent",
				ToolInput: json.RawMessage(`{"prompt":"Review this PR","task":"fallback task"}`),
				Timestamp: 1714000040000,
			},
		},
		{
			Name:        "subagent_prompt_from_task_fallback",
			Description: "Kiro CLI subagent input missing prompt; task becomes the subagent intent",
			Event: &hooks.Event{
				Type:      hooks.SubagentPromptSubmitted,
				SessionID: "sess-kiro-1",
				ToolName:  "Agent",
				ToolInput: json.RawMessage(`{"task":"Generate the JSON schema"}`),
				Timestamp: 1714000050000,
			},
		},
		{
			Name:        "subagent_completed",
			Description: "Kiro CLI subagent response {success, result: []string}; summary is result[0]",
			Event: &hooks.Event{
				Type:      hooks.SubagentCompleted,
				SessionID: "sess-kiro-1",
				TurnID:    "turn-1",
				ToolUseID: "kiro-agent-1",
				ToolName:  "Agent",
				ToolInput: json.RawMessage(`{"prompt":"Review the auth package"}`),
				ToolResponse: json.RawMessage(`{
					"success":true,
					"result":["Reviewed: found two issues in auth/session.go"]
				}`),
				Timestamp: 1714000060000,
			},
		},
	}

	testutil.RunGolden(t, &Provider{}, "testdata/direct_emit", cases)
}
