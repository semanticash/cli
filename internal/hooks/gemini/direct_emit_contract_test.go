package gemini

import (
	"encoding/json"
	"testing"

	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/hooks/testutil"
)

// TestDirectEmit_Contract freezes Gemini's output. The Write and Edit
// cases exercise the explicit-rebuild hash-stability contract; the
// Bash case exercises the hardcoded-empty-description in the
// provenance blob; subagent_completed has its own contract for the
// string and parts-array llmContent shapes.
func TestDirectEmit_Contract(t *testing.T) {
	const transcriptRef = "/workspace/.gemini/sessions/sess-gemini-1/transcript.jsonl"

	cases := []testutil.Case{
		{
			Name:        "prompt",
			Description: "Gemini prompt using TruncateWithEllipsis; no whitespace normalization",
			Event: &hooks.Event{
				Type:          hooks.PromptSubmitted,
				SessionID:     "sess-gemini-1",
				TranscriptRef: transcriptRef,
				TurnID:        "turn-1",
				Prompt:        "Refactor the retry handler in payments.go",
				Timestamp:     1714000000000,
			},
		},
		{
			Name:        "write",
			Description: "Gemini write_file; input is rebuilt before synthesis to keep the blob hash stable across hook key-order drift",
			Event: &hooks.Event{
				Type:          hooks.ToolStepCompleted,
				SessionID:     "sess-gemini-1",
				TranscriptRef: transcriptRef,
				TurnID:        "turn-1",
				ToolUseID:     "gemini-step-1",
				ToolName:      "Write",
				ToolInput:     json.RawMessage(`{"file_path":"/repo/new.go","content":"package main\n"}`),
				Timestamp:     1714000010000,
			},
		},
		{
			Name:        "edit",
			Description: "Gemini replace with an instruction field that is deliberately dropped during input rebuild",
			Event: &hooks.Event{
				Type:          hooks.ToolStepCompleted,
				SessionID:     "sess-gemini-1",
				TranscriptRef: transcriptRef,
				TurnID:        "turn-1",
				ToolUseID:     "gemini-step-2",
				ToolName:      "Edit",
				ToolInput:     json.RawMessage(`{"file_path":"/repo/main.go","old_string":"foo","new_string":"bar","instruction":"Replace foo with bar"}`),
				Timestamp:     1714000020000,
			},
		},
		{
			Name:        "bash",
			Description: "Gemini run_shell_command; description is unredacted in payload, hardcoded empty in provenance",
			Event: &hooks.Event{
				Type:          hooks.ToolStepCompleted,
				SessionID:     "sess-gemini-1",
				TranscriptRef: transcriptRef,
				TurnID:        "turn-1",
				ToolUseID:     "gemini-step-3",
				ToolName:      "Bash",
				ToolInput:     json.RawMessage(`{"command":"go test ./...","description":"Run all tests"}`),
				ToolResponse:  json.RawMessage(`{"llmContent":"ok","returnDisplay":"tests passed"}`),
				Timestamp:     1714000030000,
			},
		},
		{
			Name:        "subagent_prompt",
			Description: "Gemini subagent prompt using the `request` field",
			Event: &hooks.Event{
				Type:          hooks.SubagentPromptSubmitted,
				SessionID:     "sess-gemini-1",
				TranscriptRef: transcriptRef,
				TurnID:        "turn-1",
				ToolUseID:     "gemini-agent-1",
				ToolName:      "Agent",
				ToolInput:     json.RawMessage(`{"request":"Review the auth package"}`),
				Timestamp:     1714000040000,
			},
		},
		{
			Name:        "subagent_completed_string",
			Description: "Gemini subagent completed with llmContent as a single string",
			Event: &hooks.Event{
				Type:          hooks.SubagentCompleted,
				SessionID:     "sess-gemini-1",
				TranscriptRef: transcriptRef,
				TurnID:        "turn-1",
				ToolUseID:     "gemini-agent-1",
				ToolName:      "Agent",
				ToolInput:     json.RawMessage(`{"request":"Review the auth package"}`),
				ToolResponse:  json.RawMessage(`{"llmContent":"Reviewed: found two issues in auth/session.go"}`),
				Timestamp:     1714000050000,
			},
		},
		{
			Name:        "subagent_completed_parts",
			Description: "Gemini subagent completed with llmContent as an array of {text} objects",
			Event: &hooks.Event{
				Type:          hooks.SubagentCompleted,
				SessionID:     "sess-gemini-1",
				TranscriptRef: transcriptRef,
				TurnID:        "turn-1",
				ToolUseID:     "gemini-agent-2",
				ToolName:      "Agent",
				ToolInput:     json.RawMessage(`{"request":"Analyze payments/"}`),
				ToolResponse:  json.RawMessage(`{"llmContent":[{"text":"Analyzed payments: one bug in retry logic"}]}`),
				Timestamp:     1714000060000,
			},
		},
	}

	testutil.RunGolden(t, &Provider{}, "testdata/direct_emit", cases)
}
