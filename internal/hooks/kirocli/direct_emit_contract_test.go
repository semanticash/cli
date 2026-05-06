package kirocli

import (
	"encoding/json"
	"testing"

	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/hooks/testutil"
)

// TestDirectEmit_Contract freezes Kiro CLI's direct-emission shape,
// including tool-use JSON, summaries, hashes, and stored blobs.
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
			Description: "Kiro CLI write create; ToolUsesJSON carries the canonical Write tool name for line-level attribution",
			Event: &hooks.Event{
				Type:      hooks.ToolStepCompleted,
				SessionID: "sess-kiro-1",
				TurnID:    "turn-1",
				ToolUseID: "kiro-step-1",
				ToolName:  "Write",
				CWD:       "/repo",
				ToolInput: json.RawMessage(`{"command":"create","path":"/repo/new.go","content":"package main\n"}`),
				Timestamp: 1714000010000,
			},
		},
		{
			Name:        "edit",
			Description: "Kiro CLI write strReplace; ToolUsesJSON carries the canonical Edit tool name; oldStr/newStr renamed to old_string/new_string in the payload blob",
			Event: &hooks.Event{
				Type:      hooks.ToolStepCompleted,
				SessionID: "sess-kiro-1",
				TurnID:    "turn-1",
				ToolUseID: "kiro-step-2",
				ToolName:  "Edit",
				ToolInput: json.RawMessage(`{"command":"strReplace","path":"/repo/main.go","oldStr":"foo","newStr":"bar"}`),
				Timestamp: 1714000020000,
			},
		},
		{
			Name:        "edit_insert",
			Description: "Kiro CLI write insert; new content lands as new_string with old_string=\"\"; ToolUsesJSON still carries the canonical Edit tool name (line-level scoring)",
			Event: &hooks.Event{
				Type:      hooks.ToolStepCompleted,
				SessionID: "sess-kiro-1",
				TurnID:    "turn-1",
				ToolUseID: "kiro-step-3",
				ToolName:  "Edit",
				ToolInput: json.RawMessage(`{"command":"insert","path":"/repo/main.go","content":"// new line\n"}`),
				Timestamp: 1714000030000,
			},
		},
		{
			Name:        "bash",
			Description: "Kiro CLI shell tool_response with an items[].Json variant; ToolUsesJSON ships empty, stdout and stderr redacted",
			Event: &hooks.Event{
				Type:      hooks.ToolStepCompleted,
				SessionID: "sess-kiro-1",
				TurnID:    "turn-1",
				ToolUseID: "kiro-step-3",
				ToolName:  "Bash",
				ToolInput: json.RawMessage(`{"command":"ls","working_dir":"/repo"}`),
				ToolResponse: json.RawMessage(`{
					"items":[{"Json":{"exit_status":"exit status: 0","stdout":"file1\nfile2","stderr":""}}]
				}`),
				Timestamp: 1714000030000,
			},
		},
		{
			Name:        "subagent_prompt_with_purpose",
			Description: "Kiro CLI AgentCrew dispatch; __tool_use_purpose wins over task",
			Event: &hooks.Event{
				Type:      hooks.SubagentPromptSubmitted,
				SessionID: "sess-kiro-1",
				TurnID:    "turn-1",
				ToolUseID: "kiro-subagent-1",
				ToolName:  "Agent",
				ToolInput: json.RawMessage(`{"task":"top level fallback","__tool_use_purpose":"Dispatch three repo investigations","mode":"blocking","stages":[{"name":"a","role":"kiro_default","prompt_template":"do A"},{"name":"b","role":"kiro_default","prompt_template":"do B"},{"name":"c","role":"kiro_default","prompt_template":"do C"}]}`),
				Timestamp: 1714000040000,
			},
		},
		{
			Name:        "subagent_prompt_with_task",
			Description: "Kiro CLI AgentCrew dispatch missing __tool_use_purpose; top-level task becomes the summary",
			Event: &hooks.Event{
				Type:      hooks.SubagentPromptSubmitted,
				SessionID: "sess-kiro-1",
				ToolName:  "Agent",
				ToolInput: json.RawMessage(`{"task":"Generate the JSON schema","mode":"blocking","stages":[{"name":"only","role":"kiro_default","prompt_template":"do it"}]}`),
				Timestamp: 1714000050000,
			},
		},
		{
			Name:        "subagent_completed",
			Description: "Kiro CLI AgentCrew response items[].Text; first text becomes the summary",
			Event: &hooks.Event{
				Type:      hooks.SubagentCompleted,
				SessionID: "sess-kiro-1",
				TurnID:    "turn-1",
				ToolUseID: "kiro-subagent-1",
				ToolName:  "Agent",
				ToolInput: json.RawMessage(`{"task":"Review the auth package","stages":[{"name":"a"}]}`),
				ToolResponse: json.RawMessage(`{
					"items":[{"Text":"Pipeline completed: 1 stages finished.\n\n## a\n\nReviewed: found two issues in auth/session.go"}]
				}`),
				Timestamp: 1714000060000,
			},
		},
	}

	testutil.RunGolden(t, &Provider{}, "testdata/direct_emit", cases)
}
