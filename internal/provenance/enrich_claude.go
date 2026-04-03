package provenance

import (
	"context"
	"encoding/json"

	"github.com/semanticash/cli/internal/redact"
)

func init() {
	RegisterEnricher(&claudeEnricher{})
}

// claudeEnricher synthesizes provenance blobs from Claude Code transcript payloads.
// Reuses the same payload structure that Claude's JSONL transcript produces:
// assistant messages with tool_use content blocks, and companion tool_result messages.
type claudeEnricher struct{}

// claudeToolNames are the Claude Code tool names this enricher handles.
var claudeToolNames = map[string]bool{
	"Write": true, "Edit": true, "Bash": true,
}

func (e *claudeEnricher) CanEnrich(provider, toolName string) bool {
	return provider == "claude_code" && claudeToolNames[toolName]
}

func (e *claudeEnricher) Enrich(ctx context.Context, input EnrichInput) ([]byte, error) {
	// Load the step's payload blob (a full Claude JSONL assistant message).
	payload, err := input.BlobStore.Get(ctx, input.PayloadHash)
	if err != nil {
		return nil, err
	}

	// Extract the tool_use block matching this step's tool_use_id.
	toolInput := extractClaudeToolInput(payload, input.ToolUseID, input.ToolName)
	if toolInput == nil {
		return nil, nil
	}

	// Load companion tool_result payload if available.
	var companion *claudeToolResult
	if len(input.Companions) > 0 && input.Companions[0].PayloadHash != "" {
		companionPayload, err := input.BlobStore.Get(ctx, input.Companions[0].PayloadHash)
		if err == nil {
			companion = extractClaudeToolResult(companionPayload, input.ToolUseID)
		}
	}

	switch input.ToolName {
	case "Bash":
		return synthesizeBashProvenance(input.ToolName, input.ToolUseID, toolInput, companion)
	default:
		return synthesizeWriteEditProvenance(toolInput, companion)
	}
}

// extractClaudeToolInput parses a Claude assistant JSONL entry and returns
// the raw input for the tool_use block matching the given tool_use_id.
func extractClaudeToolInput(payload []byte, toolUseID, toolName string) json.RawMessage {
	var entry struct {
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(payload, &entry) != nil {
		return nil
	}

	for _, block := range entry.Message.Content {
		if block.Type != "tool_use" {
			continue
		}
		// Match by tool_use_id if available, fall back to tool name.
		if toolUseID != "" && block.ID == toolUseID {
			return block.Input
		}
		if toolUseID == "" && block.Name == toolName {
			return block.Input
		}
	}
	return nil
}

// claudeToolResult holds the parsed tool_result from a Claude transcript entry.
type claudeToolResult struct {
	// Structured fields from toolUseResult (Bash).
	Stdout      string `json:"stdout"`
	Stderr      string `json:"stderr"`
	Interrupted bool   `json:"interrupted"`
	// RawResponse preserves the original tool_result content as raw JSON,
	// used for Write/Edit tool_response to match the hook-produced shape.
	RawResponse json.RawMessage `json:"-"`
}

// extractClaudeToolResult parses a Claude user JSONL entry containing
// tool_result data. It prefers the top-level toolUseResult field (which
// has structured stdout/stderr/interrupted for Bash) and falls back to
// message.content[].content (a flat string).
func extractClaudeToolResult(payload []byte, toolUseID string) *claudeToolResult {
	if toolUseID == "" {
		return nil
	}

	var entry struct {
		ToolUseResult json.RawMessage `json:"toolUseResult"`
		Message       struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(payload, &entry) != nil {
		return nil
	}

	// Prefer the structured toolUseResult (available for Bash, Read, etc.).
	// Parse structured fields for Bash and keep the raw JSON for tool_response.
	// toolUseResult can be either a structured object {stdout, stderr, interrupted}
	// (successful Bash) or a plain JSON string "Error: ..." (failed Bash).
	if len(entry.ToolUseResult) > 0 {
		var structured struct {
			Stdout      string `json:"stdout"`
			Stderr      string `json:"stderr"`
			Interrupted bool   `json:"interrupted"`
		}
		if json.Unmarshal(entry.ToolUseResult, &structured) == nil && (structured.Stdout != "" || structured.Stderr != "") {
			return &claudeToolResult{
				Stdout:      structured.Stdout,
				Stderr:      structured.Stderr,
				Interrupted: structured.Interrupted,
				RawResponse: entry.ToolUseResult,
			}
		}
		// toolUseResult is a plain string (e.g., failed Bash: "Error: Exit code 1\n...").
		var plainStr string
		if json.Unmarshal(entry.ToolUseResult, &plainStr) == nil && plainStr != "" {
			return &claudeToolResult{
				Stdout:      plainStr,
				RawResponse: entry.ToolUseResult,
			}
		}
	}

	// Fallback: extract flat content string from message.content[].
	var blocks []struct {
		Type      string          `json:"type"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
	}
	if json.Unmarshal(entry.Message.Content, &blocks) != nil {
		return nil
	}
	for _, block := range blocks {
		if block.Type == "tool_result" && block.ToolUseID == toolUseID {
			// Content can be a string or structured object -- preserve as raw JSON.
			var contentStr string
			if json.Unmarshal(block.Content, &contentStr) == nil {
				return &claudeToolResult{
					Stdout:      contentStr,
					RawResponse: block.Content,
				}
			}
			return &claudeToolResult{
				RawResponse: block.Content,
			}
		}
	}
	return nil
}

// synthesizeWriteEditProvenance produces the same shape as storeRawHookPayload
// in claude/direct_emit.go: {"tool_input": {...}} with optional tool_response.
// The tool_response is passed through as raw JSON, matching how the hook path
// stores event.ToolResponse directly.
func synthesizeWriteEditProvenance(toolInput json.RawMessage, companion *claudeToolResult) ([]byte, error) {
	blob := map[string]json.RawMessage{
		"tool_input": toolInput,
	}
	if companion != nil && len(companion.RawResponse) > 0 {
		blob["tool_response"] = companion.RawResponse
	}
	return json.Marshal(blob)
}

// synthesizeBashProvenance produces the same shape as storeRedactedBashProvenance
// in claude/direct_emit.go: {"tool_name", "tool_use_id", "tool_input": {"command", "description"}, "tool_response": {"stdout", "stderr", "interrupted"}}.
func synthesizeBashProvenance(toolName, toolUseID string, toolInput json.RawMessage, companion *claudeToolResult) ([]byte, error) {
	// Parse command and description from tool_input.
	var inp struct {
		Command     string `json:"command"`
		Description string `json:"description"`
	}
	if toolInput != nil {
		_ = json.Unmarshal(toolInput, &inp)
	}

	// Redact command and description.
	redactedCmd := inp.Command
	if redactedCmd != "" {
		if r, err := redact.String(redactedCmd); err == nil {
			redactedCmd = r
		}
	}
	redactedDesc := inp.Description
	if redactedDesc != "" {
		if r, err := redact.String(redactedDesc); err == nil {
			redactedDesc = r
		}
	}

	// Extract and redact stdout/stderr/interrupted from companion.
	var stdout, stderr string
	var interrupted bool
	if companion != nil {
		stdout = companion.Stdout
		stderr = companion.Stderr
		interrupted = companion.Interrupted
	}
	if stdout != "" {
		if r, err := redact.String(stdout); err == nil {
			stdout = r
		}
	}
	if stderr != "" {
		if r, err := redact.String(stderr); err == nil {
			stderr = r
		}
	}

	blob := map[string]any{
		"tool_name":   toolName,
		"tool_use_id": toolUseID,
		"tool_input": map[string]string{
			"command":     redactedCmd,
			"description": redactedDesc,
		},
		"tool_response": map[string]any{
			"stdout":      stdout,
			"stderr":      stderr,
			"interrupted": interrupted,
		},
	}
	return json.Marshal(blob)
}
