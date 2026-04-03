package gemini

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	agentgemini "github.com/semanticash/cli/internal/agents/gemini"
	"github.com/semanticash/cli/internal/agents/api"
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

// writeInput is the tool_input shape for write_file.
type writeInput struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

// editInput is the tool_input shape for replace/edit_file.
type editInput struct {
	FilePath    string `json:"file_path"`
	OldString   string `json:"old_string"`
	NewString   string `json:"new_string"`
	Instruction string `json:"instruction"`
}

// bashInput is the tool_input shape for run_shell_command.
type bashInput struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

func buildWriteEvent(event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp writeInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse write_file tool_input: %w", err)
	}
	if inp.FilePath == "" {
		return nil, nil
	}

	// Synthesize standard assistant payload blob for attribution scoring.
	inputJSON, _ := json.Marshal(map[string]any{
		"file_path": inp.FilePath,
		"content":   inp.Content,
	})
	payloadHash := synthesizeAssistantBlob(bs, "Write", inputJSON)
	provenanceHash := storeRawHookPayload(bs, event)
	toolUsesJSON := serializeStepToolUses("Write", inp.FilePath, "write")

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
	ev.FilePaths = []string{inp.FilePath}

	return []broker.RawEvent{ev}, nil
}

func buildEditEvent(event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp editInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse replace tool_input: %w", err)
	}
	if inp.FilePath == "" {
		return nil, nil
	}

	inputJSON, _ := json.Marshal(map[string]any{
		"file_path":  inp.FilePath,
		"old_string": inp.OldString,
		"new_string": inp.NewString,
	})
	payloadHash := synthesizeAssistantBlob(bs, "Edit", inputJSON)
	provenanceHash := storeRawHookPayload(bs, event)
	toolUsesJSON := serializeStepToolUses("Edit", inp.FilePath, "edit")

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
	ev.FilePaths = []string{inp.FilePath}

	return []broker.RawEvent{ev}, nil
}

func buildBashEvent(event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp bashInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse run_shell_command tool_input: %w", err)
	}

	redactedCmd := inp.Command
	if redactedCmd != "" {
		if r, err := redact.String(redactedCmd); err == nil {
			redactedCmd = r
		}
	}

	summary := inp.Description
	if summary == "" && redactedCmd != "" {
		summary = redactedCmd
		if len(summary) > 200 {
			summary = summary[:200] + "..."
		}
	}

	var payloadHash string
	if bs != nil {
		blob := synthesizeBashBlob(redactedCmd, inp.Description)
		if blob != nil {
			h, _, err := bs.Put(context.Background(), blob)
			if err == nil {
				payloadHash = h
			}
		}
	}

	provenanceHash := storeRedactedBashProvenance(bs, event, redactedCmd)
	toolUsesJSON := serializeStepToolUses("Bash", "", "exec")

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
	var inp struct {
		Request string `json:"request"`
	}
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil || inp.Request == "" {
		return nil, nil
	}

	var payloadHash string
	if bs != nil {
		h, _, err := bs.Put(context.Background(), []byte(inp.Request))
		if err == nil {
			payloadHash = h
		}
	}

	summary := inp.Request
	if len(summary) > 200 {
		summary = summary[:200] + "..."
	}

	provenanceHash := storeRawHookPayload(bs, event)

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

func buildSubagentCompletedEvent(event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp struct {
		Request string `json:"request"`
	}
	if len(event.ToolInput) > 0 {
		_ = json.Unmarshal(event.ToolInput, &inp)
	}

	// Extract result from tool_response.llmContent.
	summary := "Gemini subagent completed"
	if len(event.ToolResponse) > 0 {
		var resp struct {
			LLMContent json.RawMessage `json:"llmContent"`
		}
		if json.Unmarshal(event.ToolResponse, &resp) == nil && len(resp.LLMContent) > 0 {
			// llmContent can be a string or array of objects with text.
			var text string
			if json.Unmarshal(resp.LLMContent, &text) == nil && text != "" {
				summary = text
			} else {
				var parts []struct {
					Text string `json:"text"`
				}
				if json.Unmarshal(resp.LLMContent, &parts) == nil && len(parts) > 0 {
					summary = parts[0].Text
				}
			}
		}
	}
	if len(summary) > 200 {
		summary = summary[:200] + "..."
	}

	inputJSON, _ := json.Marshal(map[string]any{"request": inp.Request})
	payloadHash := synthesizeAssistantBlob(bs, "Agent", inputJSON)
	provenanceHash := storeRawHookPayload(bs, event)
	toolUsesJSON := serializeStepToolUses("Agent", "", "exec")

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

func synthesizeBashBlob(redactedCmd, description string) []byte {
	redactedInput := map[string]string{
		"command":     redactedCmd,
		"description": description,
	}
	inputJSON, _ := json.Marshal(redactedInput)
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

	var resp struct {
		LLMContent    string `json:"llmContent"`
		ReturnDisplay string `json:"returnDisplay"`
	}
	if len(event.ToolResponse) > 0 {
		_ = json.Unmarshal(event.ToolResponse, &resp)
	}

	redactedOutput := resp.ReturnDisplay
	if redactedOutput != "" {
		if r, err := redact.String(redactedOutput); err == nil {
			redactedOutput = r
		}
	}

	blob := map[string]any{
		"tool_name":   event.ToolName,
		"tool_use_id": event.ToolUseID,
		"tool_input": map[string]string{
			"command":     redactedCmd,
			"description": "",
		},
		"tool_response": map[string]any{
			"output": redactedOutput,
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
	tu := agentgemini.ToolUse{
		Name:     toolName,
		FilePath: filePath,
		FileOp:   fileOp,
	}
	if s := agentgemini.SerializeToolUses([]agentgemini.ToolUse{tu}, []string{"tool_use"}); s.Valid {
		return s.String
	}
	return ""
}

func makeBaseRawEvent(event *hooks.Event) broker.RawEvent {
	providerSessionID := agentgemini.ExtractSessionID(event.TranscriptRef)

	meta := map[string]any{"source_key": event.TranscriptRef}
	metaJSON, _ := json.Marshal(meta)

	hh := sha256.New()
	hh.Write([]byte(event.TranscriptRef))
	stableKey := event.ToolUseID
	if stableKey == "" {
		stableKey = event.TurnID
	}
	_, _ = fmt.Fprintf(hh, ":hook:%s:%s:%s", event.Type.HookPhase(), event.ToolName, stableKey)
	eventID := hex.EncodeToString(hh.Sum(nil))

	sourceProjectPath := event.CWD

	return broker.RawEvent{
		EventID:           eventID,
		SourceKey:         event.TranscriptRef,
		Provider:          agentgemini.ProviderName,
		Timestamp:         event.Timestamp,
		ProviderSessionID: providerSessionID,
		SessionStartedAt:  event.Timestamp,
		SessionMetaJSON:   string(metaJSON),
		SourceProjectPath: sourceProjectPath,
		Model:             event.Model,
	}
}

// ensure Provider implements DirectHookEmitter.
var _ hooks.DirectHookEmitter = (*Provider)(nil)
