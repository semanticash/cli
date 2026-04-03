package kirocli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/semanticash/cli/internal/agents/api"
	agentKiro "github.com/semanticash/cli/internal/agents/kiro"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/redact"
)

// BuildHookEvents constructs RawEvents directly from hook payloads.
// Implements hooks.DirectHookEmitter.
func (p *Provider) BuildHookEvents(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	switch event.Type {
	case hooks.PromptSubmitted:
		return buildPromptEvent(event, bs)
	case hooks.ToolStepCompleted:
		return buildStepEvent(event, bs)
	case hooks.SubagentPromptSubmitted:
		return buildSubagentPromptEvent(event, bs)
	case hooks.SubagentCompleted:
		return buildSubagentCompletedEvent(event, bs)
	default:
		return nil, nil
	}
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

	summary := event.Prompt
	if len(summary) > 200 {
		summary = summary[:200] + "..."
	}

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

func buildStepEvent(event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	switch event.ToolName {
	case "Write":
		return buildWriteEvent(event, bs)
	case "Edit":
		return buildEditEvent(event, bs)
	case "Bash":
		return buildBashEvent(event, bs)
	default:
		return nil, nil
	}
}

func buildWriteEvent(event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp fsWriteInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse fs_write tool_input: %w", err)
	}
	if inp.Path == "" {
		return nil, nil
	}

	// Synthesize standard assistant payload blob for attribution scoring.
	inputJSON, _ := json.Marshal(map[string]any{
		"file_path": inp.Path,
		"content":   inp.FileText,
	})
	payloadHash := synthesizeAssistantBlob(bs, "Write", inputJSON)
	provenanceHash := storeRawHookPayload(bs, event)
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

func buildEditEvent(event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
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
	payloadHash := synthesizeAssistantBlob(bs, "Edit", inputJSON)
	provenanceHash := storeRawHookPayload(bs, event)
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

func buildBashEvent(event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp bashInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse execute_bash tool_input: %w", err)
	}

	redactedCmd := inp.Command
	if redactedCmd != "" {
		if r, err := redact.String(redactedCmd); err == nil {
			redactedCmd = r
		}
	}

	summary := redactedCmd
	if len(summary) > 200 {
		summary = summary[:200] + "..."
	}

	var payloadHash string
	if bs != nil {
		blob := synthesizeBashBlob(redactedCmd)
		if blob != nil {
			h, _, err := bs.Put(context.Background(), blob)
			if err == nil {
				payloadHash = h
			}
		}
	}

	provenanceHash := storeRedactedBashProvenance(bs, event, redactedCmd)

	ev := makeBaseRawEvent(event)
	ev.Kind = "assistant"
	ev.Role = "assistant"
	ev.Summary = summary
	ev.PayloadHash = payloadHash
	ev.ProvenanceHash = provenanceHash
	ev.ToolUsesJSON = agentKiro.BuildToolUsesJSON("", "exec").String
	ev.TurnID = event.TurnID
	ev.ToolUseID = event.ToolUseID
	ev.ToolName = "Bash"
	ev.EventSource = "hook"

	return []broker.RawEvent{ev}, nil
}

func buildSubagentPromptEvent(event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
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

	var payloadHash string
	if bs != nil {
		h, _, err := bs.Put(context.Background(), []byte(prompt))
		if err == nil {
			payloadHash = h
		}
	}

	summary := prompt
	if len(summary) > 200 {
		summary = summary[:200] + "..."
	}

	ev := makeBaseRawEvent(event)
	ev.Kind = "assistant"
	ev.Role = "assistant"
	ev.Summary = summary
	ev.PayloadHash = payloadHash
	ev.ProvenanceHash = storeRawHookPayload(bs, event)
	ev.TurnID = event.TurnID
	ev.ToolUseID = event.ToolUseID
	ev.ToolName = "Agent"
	ev.EventSource = "hook"

	return []broker.RawEvent{ev}, nil
}

func buildSubagentCompletedEvent(event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
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
	if len(summary) > 200 {
		summary = summary[:200] + "..."
	}

	provenanceHash := storeRawHookPayload(bs, event)

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

// synthesizeAssistantBlob creates a standard assistant payload blob
// compatible with the attribution scorer.
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

func synthesizeBashBlob(redactedCmd string) []byte {
	inputJSON, _ := json.Marshal(map[string]string{"command": redactedCmd})
	blob := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []map[string]any{
				{
					"type":  "tool_use",
					"name":  "Bash",
					"input": json.RawMessage(inputJSON),
				},
			},
		},
	}
	data, err := json.Marshal(blob)
	if err != nil {
		return nil
	}
	return data
}

func storeRawHookPayload(bs api.BlobPutter, event *hooks.Event) string {
	if bs == nil {
		return ""
	}
	blob := map[string]json.RawMessage{
		"tool_input": event.ToolInput,
	}
	if len(event.ToolResponse) > 0 {
		blob["tool_response"] = event.ToolResponse
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

func storeRedactedBashProvenance(bs api.BlobPutter, event *hooks.Event, redactedCmd string) string {
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
		redactedStdout = resp.Result[0].Stdout
		if redactedStdout != "" {
			if r, err := redact.String(redactedStdout); err == nil {
				redactedStdout = r
			}
		}
		redactedStderr = resp.Result[0].Stderr
		if redactedStderr != "" {
			if r, err := redact.String(redactedStderr); err == nil {
				redactedStderr = r
			}
		}
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
	h, _, err := bs.Put(context.Background(), data)
	if err != nil {
		return ""
	}
	return h
}

func makeBaseRawEvent(event *hooks.Event) broker.RawEvent {
	hh := sha256.New()
	hh.Write([]byte(event.SessionID))
	stableKey := event.ToolUseID
	if stableKey == "" {
		stableKey = event.TurnID
	}
	_, _ = fmt.Fprintf(hh, ":hook:%s:%s:%s", event.Type.HookPhase(), event.ToolName, stableKey)
	eventID := hex.EncodeToString(hh.Sum(nil))

	meta := map[string]any{}
	if event.CWD != "" {
		meta["project_path"] = event.CWD
	}
	metaJSON, _ := json.Marshal(meta)

	return broker.RawEvent{
		EventID:           eventID,
		SourceKey:         event.SessionID,
		Provider:          agentKiro.ProviderNameCLI,
		Timestamp:         event.Timestamp,
		ProviderSessionID: event.SessionID,
		SessionStartedAt:  event.Timestamp,
		SessionMetaJSON:   string(metaJSON),
		SourceProjectPath: event.CWD,
		Model:             event.Model,
	}
}

// ensure Provider implements DirectHookEmitter.
var _ hooks.DirectHookEmitter = (*Provider)(nil)
