package copilot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/semanticash/cli/internal/agents/api"
	agentcopilot "github.com/semanticash/cli/internal/agents/copilot"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/redact"
)

// BuildHookEvents constructs RawEvents directly from Copilot hook payloads.
func (p *Provider) BuildHookEvents(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	switch event.Type {
	case hooks.PromptSubmitted:
		return buildPromptEvent(event, bs)
	case hooks.ToolStepCompleted:
		switch event.ToolName {
		case "Write":
			return buildWriteEvent(event, bs)
		case "Edit":
			return buildEditEvent(event, bs)
		case "Bash":
			return buildBashEvent(event, bs)
		}
	case hooks.SubagentPromptSubmitted:
		return buildSubagentPromptEvent(event, bs)
	case hooks.SubagentCompleted:
		if event.ToolName == "Agent" {
			return buildAgentEvent(event, bs)
		}
	}
	return nil, nil
}

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

func buildPromptEvent(event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	if event.Prompt == "" {
		return nil, nil
	}

	var payloadHash string
	if bs != nil {
		h, _, err := bs.Put(context.Background(), []byte(event.Prompt))
		if err == nil {
			payloadHash = h
		}
	}

	summary := agentcopilot.Truncate(event.Prompt, 200)

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

func buildWriteEvent(event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
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
	payloadHash := synthesizeAssistantBlob(bs, "Write", inputJSON)
	provenanceHash := storeRawHookPayload(bs, event.ToolInput, event.ToolResponse)
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

func buildEditEvent(event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
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
	payloadHash := synthesizeAssistantBlob(bs, "Edit", inputJSON)
	provenanceHash := storeRawHookPayload(bs, event.ToolInput, event.ToolResponse)
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

func buildBashEvent(event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp copilotBashInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse Bash tool input: %w", err)
	}

	var result copilotToolResult
	if len(event.ToolResponse) > 0 {
		_ = json.Unmarshal(event.ToolResponse, &result)
	}

	redactedCmd := inp.Command
	if redactedCmd != "" {
		if r, err := redact.String(redactedCmd); err == nil {
			redactedCmd = r
		}
	}

	redactedDescription := inp.Description
	if redactedDescription != "" {
		if r, err := redact.String(redactedDescription); err == nil {
			redactedDescription = r
		}
	}

	redactedOutput := result.TextResultForLlm
	if redactedOutput != "" {
		if r, err := redact.String(redactedOutput); err == nil {
			redactedOutput = r
		}
	}

	inputJSON, _ := json.Marshal(map[string]any{
		"command":     redactedCmd,
		"description": redactedDescription,
	})
	payloadHash := synthesizeAssistantBlob(bs, "Bash", inputJSON)
	provenanceHash := storeRedactedBashPayload(bs, redactedCmd, redactedDescription, redactedOutput)
	toolUsesJSON := serializeStepToolUses("Bash", "", "exec")

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

func buildSubagentPromptEvent(event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp copilotTaskInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse Agent tool input: %w", err)
	}
	if inp.Prompt == "" {
		return nil, nil
	}

	var payloadHash string
	if bs != nil {
		h, _, err := bs.Put(context.Background(), []byte(inp.Prompt))
		if err == nil {
			payloadHash = h
		}
	}

	summary := agentcopilot.Truncate(inp.Prompt, 200)

	ev := makeBaseRawEvent(event)
	ev.Kind = "assistant"
	ev.Role = "assistant"
	ev.Summary = summary
	ev.PayloadHash = payloadHash
	ev.ProvenanceHash = storeRawHookPayload(bs, event.ToolInput, event.ToolResponse)
	ev.TurnID = event.TurnID
	ev.ToolUseID = event.ToolUseID
	ev.ToolName = "Agent"
	ev.EventSource = "hook"

	return []broker.RawEvent{ev}, nil
}

func buildAgentEvent(event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
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
	payloadHash := synthesizeAssistantBlob(bs, "Agent", inputJSON)
	provenanceHash := storeRawHookPayload(bs, event.ToolInput, event.ToolResponse)
	toolUsesJSON := serializeStepToolUses("Agent", "", "exec")

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
		summary = agentcopilot.Truncate(result.TextResultForLlm, 200)
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

func synthesizeAssistantBlob(bs api.BlobPutter, toolName string, toolInput json.RawMessage) string {
	if bs == nil {
		return ""
	}

	blob := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []map[string]any{
				{
					"type":  "tool_use",
					"name":  toolName,
					"input": json.RawMessage(toolInput),
				},
			},
		},
	}
	data, err := json.Marshal(blob)
	if err != nil {
		return ""
	}
	h, _, err := bs.Put(context.Background(), data)
	if err != nil {
		return ""
	}
	return h
}

func storeRawHookPayload(bs api.BlobPutter, toolInput, toolResponse json.RawMessage) string {
	if bs == nil {
		return ""
	}
	blob := map[string]json.RawMessage{
		"tool_input": toolInput,
	}
	if len(toolResponse) > 0 {
		blob["tool_response"] = toolResponse
	}
	data, err := json.Marshal(blob)
	if err != nil {
		return ""
	}
	h, _, err := bs.Put(context.Background(), data)
	if err != nil {
		return ""
	}
	return h
}

func storeRedactedBashPayload(bs api.BlobPutter, command, description, output string) string {
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
	h, _, err := bs.Put(context.Background(), data)
	if err != nil {
		return ""
	}
	return h
}

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

	hh := sha256.New()
	hh.Write([]byte(sourceKey))
	stableKey := event.ToolUseID
	if stableKey == "" {
		stableKey = event.TurnID
	}
	_, _ = fmt.Fprintf(hh, ":hook:%s:%s:%s", event.Type.HookPhase(), event.ToolName, stableKey)
	eventID := hex.EncodeToString(hh.Sum(nil))

	return broker.RawEvent{
		EventID:           eventID,
		SourceKey:         sourceKey,
		Provider:          agentcopilot.ProviderName,
		Timestamp:         event.Timestamp,
		ProviderSessionID: event.SessionID,
		SessionStartedAt:  event.Timestamp,
		SessionMetaJSON:   string(metaJSON),
		SourceProjectPath: event.CWD,
		Model:             event.Model,
	}
}

var _ hooks.DirectHookEmitter = (*Provider)(nil)
