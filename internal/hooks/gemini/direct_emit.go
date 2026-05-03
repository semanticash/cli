package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/agents/api"
	agentgemini "github.com/semanticash/cli/internal/agents/gemini"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/hooks/builder"
)

// resolveGeminiFilePath joins a relative Gemini file_path against CWD.
// Empty paths, paths without a CWD, OS-native absolute paths, and
// POSIX-style "/foo" paths are returned unchanged. The POSIX check is
// important on Windows: Gemini's JSON payloads can serialize forward-
// slash absolute paths regardless of host OS, and filepath.IsAbs would
// otherwise treat them as relative and stitch on the CWD.
func resolveGeminiFilePath(cwd, filePath string) string {
	if filePath == "" || cwd == "" {
		return filePath
	}
	if filepath.IsAbs(filePath) || strings.HasPrefix(filePath, "/") {
		return filePath
	}
	return filepath.Clean(filepath.Join(cwd, filePath))
}

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

// writeInput is the tool_input shape for write_file.
type writeInput struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

// editInput is the tool_input shape for replace/edit_file. The
// instruction field appears on some Gemini hook payloads and is
// deliberately dropped when rebuilding the canonical scorer input.
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

// buildWriteEvent rebuilds the tool_input into the canonical
// {file_path, content} shape before synthesizing the payload blob.
// The rebuild is not strictly necessary for the current Gemini hook
// payload (which already uses those field names), but it normalizes
// the key ordering that json.Marshal produces. Passing event.ToolInput
// directly would tie the stored blob hash to whatever key order the
// incoming payload happened to use, which can vary between Gemini
// CLI versions. The explicit rebuild keeps the hash stable.
func buildWriteEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp writeInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse write_file tool_input: %w", err)
	}
	if inp.FilePath == "" {
		return nil, nil
	}

	resolvedPath := resolveGeminiFilePath(event.CWD, inp.FilePath)

	inputJSON, _ := json.Marshal(map[string]any{
		"file_path": resolvedPath,
		"content":   inp.Content,
	})
	payloadHash := builder.SynthesizeAssistantBlob(ctx, bs, "Write", inputJSON)
	provenanceHash := builder.StoreWrappedHookProvenance(ctx, bs, event.ToolInput, event.ToolResponse)
	toolUsesJSON := serializeStepToolUses("Write", resolvedPath, "write")

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
	ev.FilePaths = []string{resolvedPath}

	return []broker.RawEvent{ev}, nil
}

// buildEditEvent drops the optional instruction field when rebuilding
// the canonical input shape. Only the fields the attribution scorer
// consumes land in the payload blob.
func buildEditEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp editInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse replace tool_input: %w", err)
	}
	if inp.FilePath == "" {
		return nil, nil
	}

	resolvedPath := resolveGeminiFilePath(event.CWD, inp.FilePath)

	inputJSON, _ := json.Marshal(map[string]any{
		"file_path":  resolvedPath,
		"old_string": inp.OldString,
		"new_string": inp.NewString,
	})
	payloadHash := builder.SynthesizeAssistantBlob(ctx, bs, "Edit", inputJSON)
	provenanceHash := builder.StoreWrappedHookProvenance(ctx, bs, event.ToolInput, event.ToolResponse)
	toolUsesJSON := serializeStepToolUses("Edit", resolvedPath, "edit")

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
	ev.FilePaths = []string{resolvedPath}

	return []broker.RawEvent{ev}, nil
}

// buildBashEvent emits a Bash tool-use event.
//
// Gemini-specific rules preserved here:
//
//   - The description is unredacted in the payload blob. Only the
//     command is redacted in the payload. The provenance blob stores
//     an empty description field regardless (see storeRedactedBashProvenance).
//   - The summary uses the description verbatim when present, and
//     truncates only in the command-fallback branch when description
//     is empty.
func buildBashEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp bashInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse run_shell_command tool_input: %w", err)
	}

	redactedCmd := builder.Redact(inp.Command)

	summary := inp.Description
	if summary == "" && redactedCmd != "" {
		summary = builder.TruncateWithEllipsis(redactedCmd, 200)
	}

	inputJSON, _ := json.Marshal(map[string]string{
		"command":     redactedCmd,
		"description": inp.Description,
	})
	payloadHash := builder.SynthesizeAssistantBlob(ctx, bs, "Bash", inputJSON)
	provenanceHash := storeRedactedBashProvenance(ctx, bs, event, redactedCmd)
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

// subagentInput captures Gemini 0.40+ invoke_agent input.
type subagentInput struct {
	Prompt    string `json:"prompt"`
	AgentName string `json:"agent_name"`
}

func buildSubagentPromptEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp subagentInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil || inp.Prompt == "" {
		return nil, nil
	}

	payloadHash := builder.StorePromptPayload(ctx, bs, inp.Prompt)
	summary := builder.TruncateWithEllipsis(inp.Prompt, 200)
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

// buildSubagentCompletedEvent captures Gemini 0.40+ invoke_agent
// completion. returnDisplay carries structured state; llmContent is
// retained as a fallback summary source.
func buildSubagentCompletedEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp subagentInput
	if len(event.ToolInput) > 0 {
		_ = json.Unmarshal(event.ToolInput, &inp)
	}

	var resp struct {
		LLMContent    json.RawMessage `json:"llmContent"`
		ReturnDisplay struct {
			State     string `json:"state"`
			AgentName string `json:"agentName"`
		} `json:"returnDisplay"`
	}
	if len(event.ToolResponse) > 0 {
		_ = json.Unmarshal(event.ToolResponse, &resp)
	}

	// Prefer the dispatched agent name, but fall back to response
	// metadata if the input was not available.
	agentName := inp.AgentName
	if agentName == "" {
		agentName = resp.ReturnDisplay.AgentName
	}

	summary := buildSubagentSummary(agentName, resp.ReturnDisplay.State, resp.LLMContent)

	inputJSON, _ := json.Marshal(map[string]any{
		"prompt":     inp.Prompt,
		"agent_name": agentName,
		"state":      resp.ReturnDisplay.State,
	})
	payloadHash := builder.SynthesizeAssistantBlob(ctx, bs, "Agent", inputJSON)
	provenanceHash := builder.StoreWrappedHookProvenance(ctx, bs, event.ToolInput, event.ToolResponse)
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

// buildSubagentSummary returns a stable summary for UI and logs.
func buildSubagentSummary(agentName, state string, llmContent json.RawMessage) string {
	if agentName != "" && state != "" {
		return builder.TruncateWithEllipsis(fmt.Sprintf("%s subagent %s", agentName, state), 200)
	}
	if len(llmContent) > 0 {
		var text string
		if json.Unmarshal(llmContent, &text) == nil && text != "" {
			return builder.TruncateWithEllipsis(text, 200)
		}
		var parts []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(llmContent, &parts) == nil && len(parts) > 0 && parts[0].Text != "" {
			return builder.TruncateWithEllipsis(parts[0].Text, 200)
		}
	}
	return "Gemini subagent completed"
}

// storeRedactedBashProvenance captures the Gemini-specific Bash
// provenance shape. The description is stored as an empty string
// regardless of the incoming value; the tool response is reduced to
// a single output field built from returnDisplay (with redaction
// applied). Both are current behavior and are preserved here.
func storeRedactedBashProvenance(ctx context.Context, bs api.BlobPutter, event *hooks.Event, redactedCmd string) string {
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

	redactedOutput := builder.Redact(resp.ReturnDisplay)

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
	return builder.PutAndHash(ctx, bs, data)
}

// serializeStepToolUses produces the ToolUsesJSON string via
// agentgemini's helper. The shape matches Claude, Copilot, and
// Cursor (name plus file_path plus file_op); only the implementing
// package differs.
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

// makeBaseRawEvent derives Gemini session metadata before delegating
// the common envelope fields to the shared builder.
func makeBaseRawEvent(event *hooks.Event) broker.RawEvent {
	providerSessionID := deriveProviderSessionIDForEmit(event.TranscriptRef)

	meta := map[string]any{"source_key": event.TranscriptRef}
	metaJSON, _ := json.Marshal(meta)

	return builder.BaseRawEvent(builder.BaseInput{
		Event:             event,
		SourceKey:         event.TranscriptRef,
		Provider:          agentgemini.ProviderName,
		ProviderSessionID: providerSessionID,
		SessionMetaJSON:   string(metaJSON),
		SourceProjectPath: event.CWD,
	})
}

// deriveProviderSessionIDForEmit reads the JSONL header sessionId when
// available, otherwise it falls back to the filename-derived ID.
func deriveProviderSessionIDForEmit(transcriptRef string) string {
	f, err := os.Open(transcriptRef)
	if err != nil {
		return agentgemini.ExtractSessionID(transcriptRef)
	}
	defer func() { _ = f.Close() }()
	if id := agentgemini.ReadSessionIDFromHeader(f); id != "" {
		return id
	}
	return agentgemini.ExtractSessionID(transcriptRef)
}

// ensure Provider implements DirectHookEmitter.
var _ hooks.DirectHookEmitter = (*Provider)(nil)
