package kirocli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/semanticash/cli/internal/agents/api"
	agentKiro "github.com/semanticash/cli/internal/agents/kiro"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/hooks/builder"
)

// BuildHookEvents constructs RawEvents directly from hook payloads.
// Implements hooks.DirectHookEmitter.
func (p *Provider) BuildHookEvents(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	switch event.Type {
	case hooks.PromptSubmitted:
		return buildPromptEvent(ctx, event, bs)
	case hooks.ToolStepCompleted:
		return buildStepEvent(ctx, event, bs)
	case hooks.SubagentPromptSubmitted:
		return buildSubagentPromptEvent(ctx, event, bs)
	case hooks.SubagentCompleted:
		return buildSubagentCompletedEvent(ctx, event, bs)
	default:
		return nil, nil
	}
}

func buildPromptEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	if event.Prompt == "" {
		return nil, nil
	}

	payloadHash := builder.StorePromptPayload(ctx, bs, event.Prompt)
	summary := builder.TruncateWithEllipsis(event.Prompt, 200)

	ev := makeBaseRawEvent(event)
	ev.Kind = "user"
	ev.Role = "user"
	ev.Summary = summary
	ev.PayloadHash = payloadHash
	ev.ProvenanceHash = payloadHash
	ev.TurnID = event.TurnID
	ev.EventSource = "hook"

	return []broker.RawEvent{ev}, nil
}

func buildStepEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	switch event.ToolName {
	case "Write":
		return buildWriteEvent(ctx, event, bs)
	case "Edit":
		return buildEditEvent(ctx, event, bs)
	case "Bash":
		return buildBashEvent(ctx, event, bs)
	default:
		return nil, nil
	}
}

// buildWriteEvent emits a Write tool-use event. Kiro CLI's fs_write
// payload uses `path` and `file_text` instead of the canonical
// `file_path` and `content`, so the input is rebuilt before landing
// in the synthesized assistant blob.
//
// The ToolUsesJSON emitted here uses Kiro's synthetic tool name
// (`kiro_file_edit`) via agentKiro.BuildToolUsesJSON rather than the
// real tool name the other providers use. This is a deliberate
// divergence from the other providers, documented in matrix row 9,
// and is exercised by the kirocli direct-emit tests.
func buildWriteEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp fsWriteInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse fs_write tool_input: %w", err)
	}
	if inp.Path == "" {
		return nil, nil
	}

	inputJSON, _ := json.Marshal(map[string]any{
		"file_path": inp.Path,
		"content":   inp.FileText,
	})
	payloadHash := builder.SynthesizeAssistantBlob(ctx, bs, "Write", inputJSON)
	provenanceHash := builder.StoreWrappedHookProvenance(ctx, bs, event.ToolInput, event.ToolResponse)
	toolUsesJSON := agentKiro.BuildToolUsesJSON(inp.Path, "create").String

	ev := makeBaseRawEvent(event)
	ev.Kind = "assistant"
	ev.Role = "assistant"
	ev.PayloadHash = payloadHash
	ev.ProvenanceHash = provenanceHash
	ev.ToolUsesJSON = toolUsesJSON
	ev.TurnID = event.TurnID
	ev.ToolUseID = event.ToolUseID
	ev.ToolName = "Write"
	ev.EventSource = "hook"
	ev.FilePaths = []string{inp.Path}

	return []broker.RawEvent{ev}, nil
}

// buildEditEvent shares the same input type as buildWriteEvent
// because Kiro CLI sends both operations under fs_write with a
// `command` discriminator. The Semantica-facing tool name (Edit)
// comes from the hook dispatch upstream.
func buildEditEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp fsWriteInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse fs_write tool_input: %w", err)
	}
	if inp.Path == "" {
		return nil, nil
	}

	inputJSON, _ := json.Marshal(map[string]any{
		"file_path":  inp.Path,
		"old_string": inp.OldStr,
		"new_string": inp.NewStr,
	})
	payloadHash := builder.SynthesizeAssistantBlob(ctx, bs, "Edit", inputJSON)
	provenanceHash := builder.StoreWrappedHookProvenance(ctx, bs, event.ToolInput, event.ToolResponse)
	toolUsesJSON := agentKiro.BuildToolUsesJSON(inp.Path, "edit").String

	ev := makeBaseRawEvent(event)
	ev.Kind = "assistant"
	ev.Role = "assistant"
	ev.PayloadHash = payloadHash
	ev.ProvenanceHash = provenanceHash
	ev.ToolUsesJSON = toolUsesJSON
	ev.TurnID = event.TurnID
	ev.ToolUseID = event.ToolUseID
	ev.ToolName = "Edit"
	ev.EventSource = "hook"
	ev.FilePaths = []string{inp.Path}

	return []broker.RawEvent{ev}, nil
}

// buildBashEvent emits a Bash tool-use event.
//
// Kiro-specific rules preserved here:
//
//   - Kiro CLI's execute_bash payload does not carry a description
//     field. The redacted command is used directly as the summary,
//     with TruncateWithEllipsis applied on overflow.
//   - The ToolUsesJSON ships as an empty string because
//     agentKiro.BuildToolUsesJSON returns an empty NullString when
//     the file path is empty. This is the flip side of the
//     synthetic-name contract documented in matrix row 9, and is
//     asserted by the kirocli direct-emit tests.
func buildBashEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp bashInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse execute_bash tool_input: %w", err)
	}

	redactedCmd := builder.Redact(inp.Command)
	summary := builder.TruncateWithEllipsis(redactedCmd, 200)

	inputJSON, _ := json.Marshal(map[string]string{"command": redactedCmd})
	payloadHash := builder.SynthesizeAssistantBlob(ctx, bs, "Bash", inputJSON)
	provenanceHash := storeRedactedBashProvenance(ctx, bs, event, redactedCmd)
	toolUsesJSON := agentKiro.BuildToolUsesJSON("", "exec").String

	ev := makeBaseRawEvent(event)
	ev.Kind = "assistant"
	ev.Role = "assistant"
	ev.Summary = summary
	ev.PayloadHash = payloadHash
	ev.ProvenanceHash = provenanceHash
	ev.ToolUsesJSON = toolUsesJSON
	ev.TurnID = event.TurnID
	ev.ToolUseID = event.ToolUseID
	ev.ToolName = "Bash"
	ev.EventSource = "hook"

	return []broker.RawEvent{ev}, nil
}

// buildSubagentPromptEvent parses Kiro CLI's subagent input with a
// prompt-then-task fallback. Older Kiro CLI versions used `task`
// while newer versions use `prompt`; supporting both keeps the
// emitter compatible with the in-flight transition.
func buildSubagentPromptEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp subagentInput
	if len(event.ToolInput) > 0 {
		_ = json.Unmarshal(event.ToolInput, &inp)
	}
	prompt := inp.Prompt
	if prompt == "" {
		prompt = inp.Task
	}
	if prompt == "" {
		return nil, nil
	}

	payloadHash := builder.StorePromptPayload(ctx, bs, prompt)
	summary := builder.TruncateWithEllipsis(prompt, 200)
	provenanceHash := builder.StoreWrappedHookProvenance(ctx, bs, event.ToolInput, event.ToolResponse)

	ev := makeBaseRawEvent(event)
	ev.Kind = "assistant"
	ev.Role = "assistant"
	ev.Summary = summary
	ev.PayloadHash = payloadHash
	ev.ProvenanceHash = provenanceHash
	ev.TurnID = event.TurnID
	ev.ToolUseID = event.ToolUseID
	ev.ToolName = "Agent"
	ev.EventSource = "hook"

	return []broker.RawEvent{ev}, nil
}

// buildSubagentCompletedEvent surfaces the first string from the
// Kiro-specific subagent response shape ({success, result: []string})
// as the event summary. If the response is absent or yields nothing,
// a neutral placeholder is used.
func buildSubagentCompletedEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	summary := "Kiro subagent completed"
	if len(event.ToolResponse) > 0 {
		var resp struct {
			Success bool     `json:"success"`
			Result  []string `json:"result"`
		}
		if json.Unmarshal(event.ToolResponse, &resp) == nil && len(resp.Result) > 0 {
			summary = resp.Result[0]
		}
	}
	summary = builder.TruncateWithEllipsis(summary, 200)

	provenanceHash := builder.StoreWrappedHookProvenance(ctx, bs, event.ToolInput, event.ToolResponse)

	ev := makeBaseRawEvent(event)
	ev.Kind = "assistant"
	ev.Role = "assistant"
	ev.Summary = summary
	ev.ProvenanceHash = provenanceHash
	ev.TurnID = event.TurnID
	ev.ToolUseID = event.ToolUseID
	ev.ToolName = "Agent"
	ev.EventSource = "hook"

	return []broker.RawEvent{ev}, nil
}

// storeRedactedBashProvenance captures the Kiro-specific Bash
// provenance shape. Kiro's execute_bash response carries a result
// array whose first entry holds the exit status, stdout, and stderr;
// the stored blob redacts stdout and stderr and reduces the response
// to {stdout, stderr}. The envelope {tool_name, tool_use_id,
// tool_input, tool_response} is shared with Claude and Gemini in
// shape, but each provider's tool_response subshape differs.
func storeRedactedBashProvenance(ctx context.Context, bs api.BlobPutter, event *hooks.Event, redactedCmd string) string {
	if bs == nil {
		return ""
	}

	var resp bashResponse
	if len(event.ToolResponse) > 0 {
		_ = json.Unmarshal(event.ToolResponse, &resp)
	}

	redactedStdout := ""
	redactedStderr := ""
	if len(resp.Result) > 0 {
		redactedStdout = builder.Redact(resp.Result[0].Stdout)
		redactedStderr = builder.Redact(resp.Result[0].Stderr)
	}

	blob := map[string]any{
		"tool_name":   event.ToolName,
		"tool_use_id": event.ToolUseID,
		"tool_input":  map[string]string{"command": redactedCmd},
		"tool_response": map[string]any{
			"stdout": redactedStdout,
			"stderr": redactedStderr,
		},
	}
	data, err := json.Marshal(blob)
	if err != nil {
		return ""
	}
	return builder.PutAndHash(ctx, bs, data)
}

// makeBaseRawEvent uses Kiro CLI's session ID as both the source key
// and the provider session ID. Unlike the other providers, Kiro CLI
// does not carry a transcript reference through the hook pipeline,
// so the session ID is the only stable identifier available. The
// session metadata carries only the project path (CWD), when set.
func makeBaseRawEvent(event *hooks.Event) broker.RawEvent {
	meta := map[string]any{}
	if event.CWD != "" {
		meta["project_path"] = event.CWD
	}
	metaJSON, _ := json.Marshal(meta)

	return builder.BaseRawEvent(builder.BaseInput{
		Event:             event,
		SourceKey:         event.SessionID,
		Provider:          agentKiro.ProviderNameCLI,
		ProviderSessionID: event.SessionID,
		SessionMetaJSON:   string(metaJSON),
		SourceProjectPath: event.CWD,
	})
}

// ensure Provider implements DirectHookEmitter.
var _ hooks.DirectHookEmitter = (*Provider)(nil)
