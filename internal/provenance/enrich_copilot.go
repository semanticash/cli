package provenance

import (
	"context"
	"encoding/json"

	"github.com/semanticash/cli/internal/redact"
)

func init() {
	RegisterEnricher(&copilotEnricher{})
}

// copilotEnricher synthesizes provenance blobs from Copilot CLI transcript payloads.
// Best-effort: Bash where command + output are present, Edit/Write where arguments
// are rich enough. Does not attempt enrichment for view or ask_user tools.
type copilotEnricher struct{}

// copilotToolNames are the Copilot transcript tool names this enricher handles.
// Copilot transcripts use lowercase names (bash, edit) and the synthetic
// copilot_file_edit for file operations detected from tool.execution_complete.
var copilotToolNames = map[string]bool{
	"bash": true, "Bash": true,
	"edit": true, "Edit": true,
	"Write":             true,
	"copilot_file_edit": true,
}

func (e *copilotEnricher) CanEnrich(provider, toolName string) bool {
	return provider == "copilot" && copilotToolNames[toolName]
}

func (e *copilotEnricher) Enrich(ctx context.Context, input EnrichInput) ([]byte, error) {
	// Load the step's payload blob (a full Copilot JSONL line).
	payload, err := input.BlobStore.Get(ctx, input.PayloadHash)
	if err != nil {
		return nil, err
	}

	// Extract tool arguments from the assistant.message payload.
	toolArgs := extractCopilotToolArgs(payload, input.ToolUseID, input.ToolName)

	// Load companion tool_result payload if available.
	var companionContent string
	if len(input.Companions) > 0 && input.Companions[0].PayloadHash != "" {
		companionPayload, err := input.BlobStore.Get(ctx, input.Companions[0].PayloadHash)
		if err == nil {
			companionContent = extractCopilotToolResultContent(companionPayload)
		}
	}

	switch input.ToolName {
	case "bash", "Bash":
		return synthesizeCopilotBashProvenance(toolArgs, companionContent)
	case "copilot_file_edit":
		// copilot_file_edit is synthetic from tool.execution_complete and only
		// carries file paths, not actual edit content. Best-effort: if the payload
		// has file_path, produce a coarse Write provenance.
		return synthesizeCopilotFileEditProvenance(payload)
	default:
		// edit, Edit, Write: try to extract structured input.
		if toolArgs == nil {
			return nil, nil
		}
		return synthesizeCopilotWriteEditProvenance(toolArgs)
	}
}

// extractCopilotToolArgs parses a Copilot assistant.message JSONL entry and
// returns the raw arguments for the tool request matching the given tool_use_id.
func extractCopilotToolArgs(payload []byte, toolUseID, toolName string) json.RawMessage {
	var entry struct {
		Data struct {
			ToolRequests []struct {
				ToolCallID string          `json:"toolCallId"`
				Name       string          `json:"name"`
				Arguments  json.RawMessage `json:"arguments"`
				Tool       struct {
					Name  string          `json:"name"`
					Input json.RawMessage `json:"input"`
				} `json:"tool"`
			} `json:"toolRequests"`
		} `json:"data"`
	}
	if json.Unmarshal(payload, &entry) != nil {
		return nil
	}

	for _, tr := range entry.Data.ToolRequests {
		name := tr.Name
		args := tr.Arguments
		if name == "" && tr.Tool.Name != "" {
			name = tr.Tool.Name
			args = tr.Tool.Input
		}
		if toolUseID != "" && tr.ToolCallID == toolUseID {
			return args
		}
		if toolUseID == "" && name == toolName {
			return args
		}
	}
	return nil
}

// extractCopilotToolResultContent parses a Copilot tool.execution_complete
// or tool_result JSONL entry and returns the best available text output.
func extractCopilotToolResultContent(payload []byte) string {
	// Try the tool.execution_complete format with textResultForLlm.
	var execComplete struct {
		Data struct {
			TextResultForLlm string `json:"textResultForLlm"`
			ToolCallID       string `json:"toolCallId"`
		} `json:"data"`
	}
	if json.Unmarshal(payload, &execComplete) == nil && execComplete.Data.TextResultForLlm != "" {
		return execComplete.Data.TextResultForLlm
	}

	// Try the live transcript shape: data.result.detailedContent / data.result.content.
	var withResult struct {
		Data struct {
			Result struct {
				Content         string `json:"content"`
				DetailedContent string `json:"detailedContent"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(payload, &withResult) == nil {
		if withResult.Data.Result.DetailedContent != "" {
			return withResult.Data.Result.DetailedContent
		}
		if withResult.Data.Result.Content != "" {
			return withResult.Data.Result.Content
		}
	}

	// Fallback: try a flat content string.
	var flat struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(payload, &flat) == nil && flat.Message.Content != "" {
		return flat.Message.Content
	}

	return ""
}

// synthesizeCopilotBashProvenance produces the same shape as storeRedactedBashPayload
// in copilot/direct_emit.go: {"tool_input": {"command", "description"}, "tool_response": {"textResultForLlm"}}.
func synthesizeCopilotBashProvenance(toolArgs json.RawMessage, companionContent string) ([]byte, error) {
	var inp struct {
		Command     string `json:"command"`
		Description string `json:"description"`
	}
	if toolArgs != nil {
		_ = json.Unmarshal(toolArgs, &inp)
	}
	if inp.Command == "" {
		return nil, nil
	}

	redactedCmd := inp.Command
	if r, err := redact.String(redactedCmd); err == nil {
		redactedCmd = r
	}
	redactedDesc := inp.Description
	if redactedDesc != "" {
		if r, err := redact.String(redactedDesc); err == nil {
			redactedDesc = r
		}
	}
	redactedOutput := companionContent
	if redactedOutput != "" {
		if r, err := redact.String(redactedOutput); err == nil {
			redactedOutput = r
		}
	}

	blob := map[string]any{
		"tool_input": map[string]string{
			"command":     redactedCmd,
			"description": redactedDesc,
		},
		"tool_response": map[string]string{
			"textResultForLlm": redactedOutput,
		},
	}
	return json.Marshal(blob)
}

// synthesizeCopilotWriteEditProvenance produces the same shape as storeRawHookPayload
// in copilot/direct_emit.go: {"tool_input": {...}}.
// Copilot uses "path" instead of "file_path", and "old_str"/"new_str" instead of
// "old_string"/"new_string". We normalize to the canonical shape the backend expects.
func synthesizeCopilotWriteEditProvenance(toolArgs json.RawMessage) ([]byte, error) {
	var args struct {
		Path      string `json:"path"`
		FilePath  string `json:"file_path"`
		OldStr    string `json:"old_str"`
		NewStr    string `json:"new_str"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
		FileText  string `json:"file_text"`
		Content   string `json:"content"`
	}
	if json.Unmarshal(toolArgs, &args) != nil {
		return nil, nil
	}

	filePath := args.FilePath
	if filePath == "" {
		filePath = args.Path
	}
	if filePath == "" {
		return nil, nil
	}

	input := map[string]string{"file_path": filePath}

	// Normalize Edit fields.
	oldStr := args.OldString
	if oldStr == "" {
		oldStr = args.OldStr
	}
	newStr := args.NewString
	if newStr == "" {
		newStr = args.NewStr
	}
	if oldStr != "" || newStr != "" {
		input["old_string"] = oldStr
		input["new_string"] = newStr
	}

	// Normalize Write content.
	content := args.Content
	if content == "" {
		content = args.FileText
	}
	if content != "" {
		input["content"] = content
	}

	inputJSON, _ := json.Marshal(input)
	blob := map[string]json.RawMessage{
		"tool_input": inputJSON,
	}
	return json.Marshal(blob)
}

// synthesizeCopilotFileEditProvenance produces provenance from a
// tool.execution_complete event. These carry result.detailedContent (a diff)
// and result.content (a description) alongside file paths from telemetry.
func synthesizeCopilotFileEditProvenance(payload []byte) ([]byte, error) {
	var entry struct {
		Data struct {
			Result struct {
				Content         string `json:"content"`
				DetailedContent string `json:"detailedContent"`
			} `json:"result"`
			ToolTelemetry struct {
				Properties struct {
					FilePaths string `json:"filePaths"`
					Command   string `json:"command"` // "create", "edit", etc.
				} `json:"properties"`
			} `json:"toolTelemetry"`
		} `json:"data"`
	}
	if json.Unmarshal(payload, &entry) != nil {
		return nil, nil
	}

	// Extract file path from telemetry.
	var paths []string
	_ = json.Unmarshal([]byte(entry.Data.ToolTelemetry.Properties.FilePaths), &paths)
	if len(paths) == 0 {
		return nil, nil
	}

	input := map[string]any{
		"file_path": paths[0],
	}

	// If this was a create command with detailed content, include it as Write content.
	if entry.Data.ToolTelemetry.Properties.Command == "create" && entry.Data.Result.DetailedContent != "" {
		input["content"] = entry.Data.Result.Content
	}

	blob := map[string]any{
		"tool_input": input,
	}

	// Include the diff as tool_response if available.
	if entry.Data.Result.DetailedContent != "" {
		blob["tool_response"] = map[string]string{
			"diff": entry.Data.Result.DetailedContent,
		}
	}

	return json.Marshal(blob)
}
