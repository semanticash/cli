package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/semanticash/cli/internal/agents/api"
	agentcopilot "github.com/semanticash/cli/internal/agents/copilot"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/hooks/builder"
)

// BuildHookEvents constructs RawEvents directly from Copilot hook payloads.
func (p *Provider) BuildHookEvents(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	switch event.Type {
	case hooks.PromptSubmitted:
		return buildPromptEvent(ctx, event, bs)
	case hooks.ToolStepCompleted:
		switch event.ToolName {
		case "Write":
			return buildWriteEvent(ctx, event, bs)
		case "Edit":
			return buildEditEvent(ctx, event, bs)
		case "Bash":
			return buildBashEvent(ctx, event, bs)
		}
	case hooks.SubagentPromptSubmitted:
		return buildSubagentPromptEvent(ctx, event, bs)
	case hooks.SubagentCompleted:
		if event.ToolName == "Agent" {
			return buildAgentEvent(ctx, event, bs)
		}
	}
	return nil, nil
}

// Provider-specific tool_input shapes. Copilot uses snake_case keys
// that do not match the canonical shape the attribution scorer
// consumes, so each builder normalizes the input before handing it
// to the shared assistant-blob helper.
type copilotWriteInput struct {
	Path     string `json:"path"`
	FileText string `json:"file_text"`
}

type copilotEditInput struct {
	Path   string `json:"path"`
	OldStr string `json:"old_str"`
	NewStr string `json:"new_str"`
}

type copilotBashInput struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

type copilotToolResult struct {
	ResultType       string `json:"resultType,omitempty"`
	TextResultForLlm string `json:"textResultForLlm,omitempty"`
}

type copilotTaskInput struct {
	Description string `json:"description,omitempty"`
	Prompt      string `json:"prompt,omitempty"`
	AgentType   string `json:"agent_type,omitempty"`
	Name        string `json:"name,omitempty"`
}

func buildPromptEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	if event.Prompt == "" {
		return nil, nil
	}

	payloadHash := builder.StorePromptPayload(ctx, bs, event.Prompt)
	summary := builder.TruncateClean(event.Prompt, 200)

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

func buildWriteEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp copilotWriteInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse Write tool input: %w", err)
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
	toolUsesJSON := serializeStepToolUses("Write", inp.Path, "write")

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

func buildEditEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp copilotEditInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse Edit tool input: %w", err)
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
	toolUsesJSON := serializeStepToolUses("Edit", inp.Path, "edit")

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

func buildBashEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp copilotBashInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse Bash tool input: %w", err)
	}

	var result copilotToolResult
	if len(event.ToolResponse) > 0 {
		_ = json.Unmarshal(event.ToolResponse, &result)
	}

	redactedCmd := builder.Redact(inp.Command)
	redactedDescription := builder.Redact(inp.Description)
	redactedOutput := builder.Redact(result.TextResultForLlm)

	inputJSON, _ := json.Marshal(map[string]any{
		"command":     redactedCmd,
		"description": redactedDescription,
	})
	payloadHash := builder.SynthesizeAssistantBlob(ctx, bs, "Bash", inputJSON)
	provenanceHash := storeRedactedBashPayload(ctx, bs, redactedCmd, redactedDescription, redactedOutput)
	toolUsesJSON := serializeStepToolUses("Bash", "", "exec")

	// Summary formation for Bash is Copilot-specific: trim the
	// description (or the command when the description is empty),
	// then truncate with an ellipsis on overflow. This does not
	// match either of the shared truncate helpers, so the rule
	// stays inline.
	summary := strings.TrimSpace(redactedDescription)
	if summary == "" {
		summary = strings.TrimSpace(redactedCmd)
	}
	if len(summary) > 200 {
		summary = summary[:200] + "..."
	}

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

func buildSubagentPromptEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp copilotTaskInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse Agent tool input: %w", err)
	}
	if inp.Prompt == "" {
		return nil, nil
	}

	payloadHash := builder.StorePromptPayload(ctx, bs, inp.Prompt)
	summary := builder.TruncateClean(inp.Prompt, 200)

	ev := makeBaseRawEvent(event)
	ev.Kind = "assistant"
	ev.Role = "assistant"
	ev.Summary = summary
	ev.PayloadHash = payloadHash
	ev.ProvenanceHash = builder.StoreWrappedHookProvenance(ctx, bs, event.ToolInput, event.ToolResponse)
	ev.TurnID = event.TurnID
	ev.ToolUseID = event.ToolUseID
	ev.ToolName = "Agent"
	ev.EventSource = "hook"

	return []broker.RawEvent{ev}, nil
}

func buildAgentEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp copilotTaskInput
	if len(event.ToolInput) > 0 {
		_ = json.Unmarshal(event.ToolInput, &inp)
	}

	var result copilotToolResult
	if len(event.ToolResponse) > 0 {
		_ = json.Unmarshal(event.ToolResponse, &result)
	}

	inputJSON, _ := json.Marshal(map[string]any{
		"description": inp.Description,
		"prompt":      inp.Prompt,
		"agent_type":  inp.AgentType,
		"name":        inp.Name,
	})
	payloadHash := builder.SynthesizeAssistantBlob(ctx, bs, "Agent", inputJSON)
	provenanceHash := builder.StoreWrappedHookProvenance(ctx, bs, event.ToolInput, event.ToolResponse)
	toolUsesJSON := serializeStepToolUses("Agent", "", "exec")

	// Summary resolution order: description, name, then a fallback
	// placeholder. If the tool response carried an LLM-facing text
	// result, it overrides the above, using the shared clean-truncate
	// rule so whitespace is normalized.
	summary := strings.TrimSpace(inp.Description)
	if summary == "" {
		summary = strings.TrimSpace(inp.Name)
	}
	if summary == "" {
		summary = "Copilot task completed"
	}
	if len(summary) > 200 {
		summary = summary[:200] + "..."
	}
	if result.TextResultForLlm != "" {
		summary = builder.TruncateClean(result.TextResultForLlm, 200)
	}

	ev := makeBaseRawEvent(event)
	ev.Kind = "assistant"
	ev.Role = "assistant"
	ev.Summary = summary
	ev.PayloadHash = payloadHash
	ev.ProvenanceHash = provenanceHash
	ev.ToolUsesJSON = toolUsesJSON
	ev.TurnID = event.TurnID
	ev.ToolUseID = event.ToolUseID
	ev.ToolName = "Agent"
	ev.EventSource = "hook"

	return []broker.RawEvent{ev}, nil
}

// storeRedactedBashPayload captures the Copilot-specific Bash
// provenance shape. The envelope around tool_input and tool_response
// matches the shared wrapper, but the tool_response subshape is
// provider-specific (Copilot surfaces textResultForLlm, not
// stdout/stderr), so this helper stays in the provider glue.
func storeRedactedBashPayload(ctx context.Context, bs api.BlobPutter, command, description, output string) string {
	if bs == nil {
		return ""
	}
	blob := map[string]any{
		"tool_input": map[string]string{
			"command":     command,
			"description": description,
		},
		"tool_response": map[string]string{
			"textResultForLlm": output,
		},
	}
	data, err := json.Marshal(blob)
	if err != nil {
		return ""
	}
	return builder.PutAndHash(ctx, bs, data)
}

// serializeStepToolUses produces the provider-specific ToolUsesJSON
// shape. Copilot uses agentcopilot.SerializeToolUses, which matches
// Claude, Cursor, and Gemini in field layout but not in package,
// since each provider carries its own ToolUse type. A future cleanup
// could consolidate the four packages, but that change is out of
// scope here.
func serializeStepToolUses(toolName, filePath, fileOp string) string {
	tu := agentcopilot.ToolUse{
		Name:     toolName,
		FilePath: filePath,
		FileOp:   fileOp,
	}
	if s := agentcopilot.SerializeToolUses([]agentcopilot.ToolUse{tu}, []string{"tool_use"}); s.Valid {
		return s.String
	}
	return ""
}

// makeBaseRawEvent assembles the provider-specific source key and
// session metadata, then delegates the envelope construction to the
// shared builder. Copilot uses the transcript reference when
// available and falls back to a "copilot:" prefix plus the session
// ID when no transcript is attached.
func makeBaseRawEvent(event *hooks.Event) broker.RawEvent {
	sourceKey := event.TranscriptRef
	if sourceKey == "" {
		sourceKey = "copilot:" + event.SessionID
	}

	meta := map[string]any{"source_key": sourceKey}
	if event.CWD != "" {
		meta["project_path"] = event.CWD
	}
	metaJSON, _ := json.Marshal(meta)

	return builder.BaseRawEvent(builder.BaseInput{
		Event:             event,
		SourceKey:         sourceKey,
		Provider:          agentcopilot.ProviderName,
		ProviderSessionID: event.SessionID,
		SessionMetaJSON:   string(metaJSON),
		SourceProjectPath: event.CWD,
	})
}

var _ hooks.DirectHookEmitter = (*Provider)(nil)
