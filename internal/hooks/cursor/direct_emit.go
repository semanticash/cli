package cursor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/semanticash/cli/internal/agents/api"
	agentcursor "github.com/semanticash/cli/internal/agents/cursor"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/hooks/builder"
)

// BuildHookEvents constructs RawEvents directly from Cursor IDE hook payloads.
func (p *Provider) BuildHookEvents(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	switch event.Type {
	case hooks.PromptSubmitted:
		return buildPromptEvent(ctx, event, bs)
	case hooks.ToolStepCompleted:
		switch event.ToolName {
		case "Write", "Edit":
			return buildFileEditEvent(ctx, event, bs)
		case "Bash":
			return buildBashEvent(ctx, event, bs)
		}
	case hooks.SubagentCompleted:
		if event.ToolName == "Agent" {
			return buildAgentEvent(ctx, event, bs)
		}
	case hooks.SubagentPromptSubmitted:
		return buildSubagentPromptEvent(ctx, event, bs)
	}
	return nil, nil
}

// Cursor hook payloads are structured differently from the other
// providers. File edits arrive as an afterFileEdit payload with a
// nested edits array; shell runs arrive as postToolUse payloads with
// the tool_input one level deeper; subagent events carry a distinct
// subagentStop envelope. The types below capture those shapes.
type cursorFileEditPayload struct {
	ConversationID string       `json:"conversation_id"`
	GenerationID   string       `json:"generation_id,omitempty"`
	TranscriptPath string       `json:"transcript_path,omitempty"`
	FilePath       string       `json:"file_path"`
	Edits          []cursorEdit `json:"edits"`
}

type cursorPostToolUsePayload struct {
	ConversationID string `json:"conversation_id"`
	TranscriptPath string `json:"transcript_path,omitempty"`
	ToolName       string `json:"tool_name"`
	ToolUseID      string `json:"tool_use_id,omitempty"`
	ToolInput      struct {
		Command string `json:"command"`
		CWD     string `json:"cwd,omitempty"`
		Timeout int64  `json:"timeout,omitempty"`
	} `json:"tool_input"`
	ToolOutput string `json:"tool_output,omitempty"`
	CWD        string `json:"cwd,omitempty"`
}

type cursorPreToolUsePayload struct {
	ToolInput struct {
		Prompt string `json:"prompt"`
	} `json:"tool_input"`
}

type cursorSubagentStopPayload struct {
	ConversationID       string `json:"conversation_id"`
	TranscriptPath       string `json:"transcript_path,omitempty"`
	SubagentID           string `json:"subagent_id,omitempty"`
	SubagentType         string `json:"subagent_type,omitempty"`
	AgentTranscriptPath  string `json:"agent_transcript_path,omitempty"`
	Status               string `json:"status,omitempty"`
	DurationMS           int64  `json:"duration_ms,omitempty"`
	MessageCount         int64  `json:"message_count,omitempty"`
	ToolCallCount        int64  `json:"tool_call_count,omitempty"`
	ParentConversationID string `json:"parent_conversation_id,omitempty"`
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

// buildFileEditEvent handles both Write and Edit tool events. Cursor
// uses a single internal afterFileEdit hook for both; the Write or
// Edit semantics are derived from the payload shape in
// normalizeCursorEditInput rather than from the event type.
func buildFileEditEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var payload cursorFileEditPayload
	if err := json.Unmarshal(event.ToolInput, &payload); err != nil {
		return nil, fmt.Errorf("parse afterFileEdit payload: %w", err)
	}
	if payload.FilePath == "" {
		return nil, nil
	}

	inputJSON, fileOp := normalizeCursorEditInput(event.ToolName, payload)
	payloadHash := builder.SynthesizeAssistantBlob(ctx, bs, event.ToolName, inputJSON)
	provenanceHash := storeRawHookPayload(ctx, bs, event.ToolInput)
	toolUsesJSON := serializeStepToolUses(event.ToolName, payload.FilePath, fileOp)

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
	ev.FilePaths = []string{payload.FilePath}

	return []broker.RawEvent{ev}, nil
}

func buildBashEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var payload cursorPostToolUsePayload
	if err := json.Unmarshal(event.ToolInput, &payload); err != nil {
		return nil, fmt.Errorf("parse postToolUse payload: %w", err)
	}

	redactedCommand := builder.Redact(payload.ToolInput.Command)
	redactedOutput := builder.Redact(extractCursorToolOutput(payload.ToolOutput))

	inputJSON, _ := json.Marshal(map[string]any{
		"command": redactedCommand,
		"cwd":     payload.ToolInput.CWD,
		"timeout": payload.ToolInput.Timeout,
	})
	payloadHash := builder.SynthesizeAssistantBlob(ctx, bs, "Bash", inputJSON)
	provenanceHash := storeRedactedBashPayload(ctx, bs, payload, redactedCommand, redactedOutput)
	toolUsesJSON := serializeStepToolUses("Bash", "", "exec")

	// Cursor's Bash summary is TrimSpace plus truncate-with-ellipsis,
	// which matches neither TruncateWithEllipsis (no trim) nor
	// TruncateClean (different whitespace handling, no ellipsis), so
	// the rule stays inline.
	summary := strings.TrimSpace(redactedCommand)
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
	var payload cursorPreToolUsePayload
	if err := json.Unmarshal(event.ToolInput, &payload); err != nil {
		return nil, fmt.Errorf("parse preToolUse payload: %w", err)
	}
	if payload.ToolInput.Prompt == "" {
		return nil, nil
	}

	payloadHash := builder.StorePromptPayload(ctx, bs, payload.ToolInput.Prompt)
	summary := builder.TruncateWithEllipsis(payload.ToolInput.Prompt, 200)
	provenanceHash := storeRawHookPayload(ctx, bs, event.ToolInput)

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

func buildAgentEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var payload cursorSubagentStopPayload
	if err := json.Unmarshal(event.ToolInput, &payload); err != nil {
		return nil, fmt.Errorf("parse subagentStop payload: %w", err)
	}

	inputJSON, _ := json.Marshal(map[string]any{
		"subagent_id":            payload.SubagentID,
		"subagent_type":          payload.SubagentType,
		"status":                 payload.Status,
		"duration_ms":            payload.DurationMS,
		"message_count":          payload.MessageCount,
		"tool_call_count":        payload.ToolCallCount,
		"agent_transcript_path":  payload.AgentTranscriptPath,
		"parent_conversation_id": payload.ParentConversationID,
	})
	payloadHash := builder.SynthesizeAssistantBlob(ctx, bs, "Agent", inputJSON)
	provenanceHash := storeRawHookPayload(ctx, bs, event.ToolInput)
	toolUsesJSON := serializeStepToolUses("Agent", "", "exec")

	// Summary prefers the subagent type when present, falling back
	// to a neutral placeholder. Cursor-specific shape; stays inline.
	summary := "Cursor subagent completed"
	if payload.SubagentType != "" {
		summary = payload.SubagentType + " subagent completed"
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

// normalizeCursorEditInput converts the nested Cursor afterFileEdit
// payload into the canonical tool_input shape the attribution scorer
// consumes. A Write maps to {file_path, content}. A single edit maps
// to {file_path, old_string, new_string}. Multi-edit payloads (only
// emitted by Cursor today) preserve the edits array under the same
// file_path.
func normalizeCursorEditInput(toolName string, payload cursorFileEditPayload) (json.RawMessage, string) {
	if toolName == "Write" {
		content := ""
		if len(payload.Edits) > 0 {
			content = payload.Edits[0].NewString
		}
		input, _ := json.Marshal(map[string]any{
			"file_path": payload.FilePath,
			"content":   content,
		})
		return input, "write"
	}

	if len(payload.Edits) == 1 {
		input, _ := json.Marshal(map[string]any{
			"file_path":  payload.FilePath,
			"old_string": payload.Edits[0].OldString,
			"new_string": payload.Edits[0].NewString,
		})
		return input, "edit"
	}

	input, _ := json.Marshal(map[string]any{
		"file_path": payload.FilePath,
		"edits":     payload.Edits,
	})
	return input, "edit"
}

// extractCursorToolOutput unwraps Cursor's tool_output string. The
// field is a JSON-encoded string (not structured), usually shaped as
// {"output":"...","exitCode":N}. Pull the inner text when available;
// otherwise return the original string.
func extractCursorToolOutput(toolOutput string) string {
	if toolOutput == "" {
		return ""
	}
	var payload struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal([]byte(toolOutput), &payload); err == nil && payload.Output != "" {
		return payload.Output
	}
	return toolOutput
}

// storeRawHookPayload stores the hook payload bytes without the
// shared {tool_input, tool_response} wrapper that Claude, Copilot,
// Gemini, and Kiro CLI use. Cursor's incoming hook payload is the
// tool_input for downstream consumers, so wrapping it again would
// double-encode. The helper stays in Cursor's glue to preserve the
// current wire shape.
//
// An empty payload skips the blob put (no zero-byte blobs in the
// store), which also preserves current behavior.
func storeRawHookPayload(ctx context.Context, bs api.BlobPutter, payload json.RawMessage) string {
	if bs == nil || len(payload) == 0 {
		return ""
	}
	return builder.PutAndHash(ctx, bs, payload)
}

// storeRedactedBashPayload captures the Cursor-specific Bash
// provenance shape. Cursor includes conversation_id, transcript_path,
// and cwd at the top level, and stores tool_output as a structured
// object after redacting the inner output string. The shape diverges
// enough from the other providers that it stays in Cursor's glue.
func storeRedactedBashPayload(ctx context.Context, bs api.BlobPutter, payload cursorPostToolUsePayload, redactedCommand, redactedOutput string) string {
	if bs == nil {
		return ""
	}
	toolOutput := map[string]any{}
	if payload.ToolOutput != "" {
		_ = json.Unmarshal([]byte(payload.ToolOutput), &toolOutput)
	}
	if redactedOutput != "" {
		toolOutput["output"] = redactedOutput
	}
	blob := map[string]any{
		"conversation_id": payload.ConversationID,
		"transcript_path": payload.TranscriptPath,
		"tool_name":       payload.ToolName,
		"tool_use_id":     payload.ToolUseID,
		"tool_input": map[string]any{
			"command": redactedCommand,
			"cwd":     payload.ToolInput.CWD,
			"timeout": payload.ToolInput.Timeout,
		},
		"tool_output": toolOutput,
		"cwd":         payload.CWD,
	}
	data, err := json.Marshal(blob)
	if err != nil {
		return ""
	}
	return builder.PutAndHash(ctx, bs, data)
}

// serializeStepToolUses produces the ToolUsesJSON string via
// agentcursor's helper. The shape matches Claude, Copilot, and
// Gemini (name plus file_path plus file_op); only the implementing
// package differs. A future cleanup could consolidate the four
// agent-side helpers into one shared package, but that is out of
// scope here.
func serializeStepToolUses(toolName, filePath, fileOp string) string {
	tu := agentcursor.ToolUse{
		Name:     toolName,
		FilePath: filePath,
		FileOp:   fileOp,
	}
	if s := agentcursor.SerializeToolUses([]agentcursor.ToolUse{tu}, []string{"tool_use"}); s.Valid {
		return s.String
	}
	return ""
}

// makeBaseRawEvent assembles the Cursor-specific source key, project
// path, and session metadata, then delegates to the shared builder.
// Cursor uses the transcript reference when available and falls back
// to a "cursor:" prefix plus the session ID when no transcript is
// attached. The session metadata omits source_key entirely when the
// transcript reference is empty, which the other providers do not.
func makeBaseRawEvent(event *hooks.Event) broker.RawEvent {
	sourceKey := event.TranscriptRef
	if sourceKey == "" {
		sourceKey = "cursor:" + event.SessionID
	}
	projectPath := agentcursor.DecodeProjectPathFromSourceKey(event.TranscriptRef)

	meta := map[string]any{}
	if event.TranscriptRef != "" {
		meta["source_key"] = event.TranscriptRef
	}
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
		SourceKey:         sourceKey,
		Provider:          agentcursor.ProviderName,
		ProviderSessionID: event.SessionID,
		SessionMetaJSON:   string(metaJSON),
		SourceProjectPath: sourceProjectPath,
	})
}

var _ hooks.DirectHookEmitter = (*Provider)(nil)
