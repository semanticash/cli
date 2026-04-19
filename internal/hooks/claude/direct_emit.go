package claude

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/semanticash/cli/internal/agents/api"
	agentclaude "github.com/semanticash/cli/internal/agents/claude"
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
	default:
		return nil, nil
	}
}

// writeInput is the tool_input shape for Write from Claude hook payloads.
type writeInput struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

// editInput is the tool_input shape for Edit from Claude hook payloads.
type editInput struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// bashInput is the tool_input shape for Bash from Claude hook payloads.
type bashInput struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// buildPromptEvent creates a user prompt event from UserPromptSubmit.
// The stored blob and the provenance hash both reference the raw
// prompt text; the scorer and the dashboard can key off the same
// content hash on either field.
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

// buildStepEvent creates an assistant tool-use event from
// PostToolUse[Write/Edit/Bash].
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

// buildWriteEvent emits a Write tool-use event. Claude's hook payload
// already uses the canonical tool_input shape consumed by the
// attribution scorer, so the raw ToolInput flows straight into the
// synthesized assistant blob without renormalization.
func buildWriteEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp writeInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse Write tool_input: %w", err)
	}
	if inp.FilePath == "" {
		return nil, nil
	}

	payloadHash := builder.SynthesizeAssistantBlob(ctx, bs, event.ToolName, event.ToolInput)
	provenanceHash := builder.StoreWrappedHookProvenance(ctx, bs, event.ToolInput, event.ToolResponse)
	toolUsesJSON := serializeStepToolUses(event.ToolName, inp.FilePath, "write")

	ev := makeBaseRawEvent(event)
	ev.Kind = "assistant"
	ev.Role = "assistant"
	ev.PayloadHash = payloadHash
	ev.ProvenanceHash = provenanceHash
	ev.ToolUsesJSON = toolUsesJSON
	ev.TurnID = event.TurnID
	ev.ToolUseID = event.ToolUseID
	ev.ToolName = event.ToolName
	ev.EventSource = "hook"
	ev.FilePaths = []string{inp.FilePath}

	return []broker.RawEvent{ev}, nil
}

// buildEditEvent emits an Edit tool-use event. As with Write, the
// Claude tool_input is already in the canonical shape and is passed
// through to the synthesized blob unchanged. The replace_all flag,
// when present, lands in the blob alongside the string fields.
func buildEditEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp editInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse Edit tool_input: %w", err)
	}
	if inp.FilePath == "" {
		return nil, nil
	}

	payloadHash := builder.SynthesizeAssistantBlob(ctx, bs, event.ToolName, event.ToolInput)
	provenanceHash := builder.StoreWrappedHookProvenance(ctx, bs, event.ToolInput, event.ToolResponse)
	toolUsesJSON := serializeStepToolUses(event.ToolName, inp.FilePath, "edit")

	ev := makeBaseRawEvent(event)
	ev.Kind = "assistant"
	ev.Role = "assistant"
	ev.PayloadHash = payloadHash
	ev.ProvenanceHash = provenanceHash
	ev.ToolUsesJSON = toolUsesJSON
	ev.TurnID = event.TurnID
	ev.ToolUseID = event.ToolUseID
	ev.ToolName = event.ToolName
	ev.EventSource = "hook"
	ev.FilePaths = []string{inp.FilePath}

	return []broker.RawEvent{ev}, nil
}

// buildBashEvent emits a Bash tool-use event.
//
// Claude-specific rules preserved here:
//
//   - The description is UNREDACTED in both the payload blob and the
//     summary. Redaction of the description only happens in the
//     provenance blob (see storeRedactedBashProvenance). The emitted
//     event shape depends on that split.
//   - The summary truncates only in the command-fallback branch. When
//     a description is present it is used verbatim, even when longer
//     than 200 characters.
func buildBashEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp bashInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse Bash tool_input: %w", err)
	}

	redactedCmd := builder.Redact(inp.Command)

	summary := inp.Description
	if summary == "" && redactedCmd != "" {
		summary = builder.TruncateWithEllipsis(redactedCmd, 200)
	}

	// The payload blob carries the redacted command alongside the
	// unredacted description so the attribution scorer sees the
	// exact bytes Claude would have written.
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
	ev.ToolName = event.ToolName
	ev.EventSource = "hook"

	return []broker.RawEvent{ev}, nil
}

// buildSubagentPromptEvent creates a subagent prompt event from
// PreToolUse[Agent]. A missing or empty prompt is a no-op, matching
// the rule for top-level prompts.
func buildSubagentPromptEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp struct {
		Prompt string `json:"prompt"`
	}
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

// storeRedactedBashProvenance captures the Claude-specific Bash
// provenance shape. The envelope wraps a tool_input with the redacted
// command and description, plus a tool_response with redacted stdout,
// stderr, and an interrupted flag. Each provider's Bash tool emits a
// different response shape, so this helper stays in the provider
// glue rather than moving to the shared builder.
func storeRedactedBashProvenance(ctx context.Context, bs api.BlobPutter, event *hooks.Event, redactedCmd string) string {
	if bs == nil {
		return ""
	}

	var inp bashInput
	if len(event.ToolInput) > 0 {
		_ = json.Unmarshal(event.ToolInput, &inp)
	}
	redactedDescription := builder.Redact(inp.Description)

	var resp struct {
		Stdout      string `json:"stdout"`
		Stderr      string `json:"stderr"`
		Interrupted bool   `json:"interrupted"`
	}
	if len(event.ToolResponse) > 0 {
		_ = json.Unmarshal(event.ToolResponse, &resp)
	}

	redactedStdout := builder.Redact(resp.Stdout)
	redactedStderr := builder.Redact(resp.Stderr)

	blob := map[string]any{
		"tool_name":   event.ToolName,
		"tool_use_id": event.ToolUseID,
		"tool_input": map[string]string{
			"command":     redactedCmd,
			"description": redactedDescription,
		},
		"tool_response": map[string]any{
			"stdout":      redactedStdout,
			"stderr":      redactedStderr,
			"interrupted": resp.Interrupted,
		},
	}
	data, err := json.Marshal(blob)
	if err != nil {
		return ""
	}
	return builder.PutAndHash(ctx, bs, data)
}

// serializeStepToolUses produces the ToolUsesJSON string in the same
// shape as transcript events, so the attribution scorer and file path
// extraction work without changes.
func serializeStepToolUses(toolName, filePath, fileOp string) string {
	tu := agentclaude.ToolUse{
		Name:     toolName,
		FilePath: filePath,
		FileOp:   fileOp,
	}
	if s := agentclaude.SerializeToolUses([]agentclaude.ToolUse{tu}, []string{"tool_use"}); s.Valid {
		return s.String
	}
	return ""
}

// makeBaseRawEvent assembles the provider-specific source key and
// session metadata, then delegates envelope construction to the
// shared builder. Claude derives the provider session ID, parent
// session ID, and project path from the transcript path, with a CWD
// fallback for project path when the transcript does not encode it.
func makeBaseRawEvent(event *hooks.Event) broker.RawEvent {
	providerSessionID := agentclaude.ExtractSessionIDFromPath(event.TranscriptRef)
	if providerSessionID == "" {
		providerSessionID = agentclaude.ExtractBasename(event.TranscriptRef)
	}
	parentSessionID := agentclaude.ExtractParentSessionID(event.TranscriptRef)
	projectPath := agentclaude.DecodeProjectPathFromSourceKey(event.TranscriptRef)

	meta := map[string]any{"source_key": event.TranscriptRef}
	if projectPath != "" {
		meta["project_path"] = projectPath
	}
	metaJSON, _ := json.Marshal(meta)

	sourceProjectPath := projectPath
	if sourceProjectPath == "" && event.CWD != "" {
		sourceProjectPath = event.CWD
	}

	return builder.BaseRawEvent(builder.BaseInput{
		Event:             event,
		SourceKey:         event.TranscriptRef,
		Provider:          agentclaude.ProviderName,
		ProviderSessionID: providerSessionID,
		ParentSessionID:   parentSessionID,
		SessionMetaJSON:   string(metaJSON),
		SourceProjectPath: sourceProjectPath,
	})
}

// ensure Provider implements DirectHookEmitter.
var _ hooks.DirectHookEmitter = (*Provider)(nil)
