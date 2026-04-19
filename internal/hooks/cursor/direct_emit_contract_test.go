package cursor

import (
	"encoding/json"
	"testing"

	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/hooks/testutil"
)

// TestDirectEmit_Contract freezes Cursor's full emission shape.
// Cursor has the most provider-specific wire details among the
// migrated providers: no-wrapper raw hook provenance, nested
// payload shapes, and the subagentStop summary built from
// subagent_type. Each case below covers one of those paths.
func TestDirectEmit_Contract(t *testing.T) {
	const transcriptRef = "/workspace/.cursor/projects/tmp-demo/agent-transcripts/conv-123/conv-123.jsonl"

	cases := []testutil.Case{
		{
			Name:        "prompt",
			Description: "Cursor prompt with raw-string TruncateWithEllipsis semantics",
			Event: &hooks.Event{
				Type:          hooks.PromptSubmitted,
				SessionID:     "conv-123",
				TranscriptRef: transcriptRef,
				TurnID:        "turn-1",
				Prompt:        "Refactor the retry handler in payments.go",
				Timestamp:     1714000000000,
			},
		},
		{
			Name:        "write",
			Description: "Cursor afterFileEdit for a Write; single edit normalized from edits[0].new_string",
			Event: &hooks.Event{
				Type:          hooks.ToolStepCompleted,
				SessionID:     "conv-123",
				TranscriptRef: transcriptRef,
				TurnID:        "turn-1",
				ToolUseID:     "cursor-step-1",
				ToolName:      "Write",
				ToolInput: json.RawMessage(`{
					"conversation_id":"conv-123",
					"file_path":"/repo/new.txt",
					"edits":[{"old_string":"","new_string":"hello\n"}]
				}`),
				Timestamp: 1714000010000,
			},
		},
		{
			Name:        "edit",
			Description: "Cursor afterFileEdit for an Edit; single edit normalized to {file_path, old_string, new_string}",
			Event: &hooks.Event{
				Type:          hooks.ToolStepCompleted,
				SessionID:     "conv-123",
				TranscriptRef: transcriptRef,
				TurnID:        "turn-1",
				ToolUseID:     "cursor-step-2",
				ToolName:      "Edit",
				ToolInput: json.RawMessage(`{
					"conversation_id":"conv-123",
					"file_path":"/repo/main.go",
					"edits":[{"old_string":"foo","new_string":"bar"}]
				}`),
				Timestamp: 1714000020000,
			},
		},
		{
			Name:        "bash",
			Description: "Cursor postToolUse with Shell payload; TrimSpace+ellipsis summary rule and structured tool_output",
			Event: &hooks.Event{
				Type:          hooks.ToolStepCompleted,
				SessionID:     "conv-123",
				TranscriptRef: transcriptRef,
				TurnID:        "turn-1",
				ToolUseID:     "shell-123",
				ToolName:      "Bash",
				ToolInput: json.RawMessage(`{
					"conversation_id":"conv-123",
					"tool_name":"Shell",
					"tool_use_id":"shell-123",
					"tool_input":{"command":"cat file.txt","cwd":"/repo","timeout":30000},
					"tool_output":"{\"output\":\"ok\\n\",\"exitCode\":0}"
				}`),
				Timestamp: 1714000030000,
			},
		},
		{
			Name:        "subagent_prompt",
			Description: "Cursor preToolUse with a nested tool_input.prompt shape",
			Event: &hooks.Event{
				Type:          hooks.SubagentPromptSubmitted,
				SessionID:     "conv-123",
				TranscriptRef: transcriptRef,
				TurnID:        "turn-1",
				ToolUseID:     "agent-123",
				ToolName:      "Agent",
				ToolInput:     json.RawMessage(`{"tool_input":{"prompt":"Review the auth package"}}`),
				Timestamp:     1714000040000,
			},
		},
		{
			Name:        "subagent_completed",
			Description: "Cursor subagentStop; summary built from subagent_type",
			Event: &hooks.Event{
				Type:          hooks.SubagentCompleted,
				SessionID:     "conv-123",
				TranscriptRef: transcriptRef,
				TurnID:        "turn-1",
				ToolUseID:     "agent-123",
				ToolName:      "Agent",
				ToolInput: json.RawMessage(`{
					"conversation_id":"conv-123",
					"subagent_id":"agent-123",
					"subagent_type":"general-purpose",
					"status":"completed",
					"duration_ms":9000,
					"message_count":3,
					"tool_call_count":1
				}`),
				Timestamp: 1714000050000,
			},
		},
	}

	testutil.RunGolden(t, &Provider{}, "testdata/direct_emit", cases)
}
