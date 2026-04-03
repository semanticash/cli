package cursor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/semanticash/cli/internal/agents/api"
	agentcursor "github.com/semanticash/cli/internal/agents/cursor"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/redact"
)

// BuildHookEvents constructs RawEvents directly from Cursor IDE hook payloads.
func (p *Provider) BuildHookEvents(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	switch event.Type {
	case hooks.PromptSubmitted:
		return buildPromptEvent(event, bs)
	case hooks.ToolStepCompleted:
		switch event.ToolName {
		case "Write", "Edit":
			return buildFileEditEvent(event, bs)
		case "Bash":
			return buildBashEvent(event, bs)
		}
	case hooks.SubagentCompleted:
		if event.ToolName == "Agent" {
			return buildAgentEvent(event, bs)
		}
	case hooks.SubagentPromptSubmitted:
		return buildSubagentPromptEvent(event, bs)
	}
	return nil, nil
}

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

func buildFileEditEvent(event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var payload cursorFileEditPayload
	if err := json.Unmarshal(event.ToolInput, &payload); err != nil {
		return nil, fmt.Errorf("parse afterFileEdit payload: %w", err)
	}
	if payload.FilePath == "" {
		return nil, nil
	}

	inputJSON, fileOp := normalizeCursorEditInput(event.ToolName, payload)
	payloadHash := synthesizeAssistantBlob(bs, event.ToolName, inputJSON)
	provenanceHash := storeRawHookPayload(bs, event.ToolInput)
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

func buildBashEvent(event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var payload cursorPostToolUsePayload
	if err := json.Unmarshal(event.ToolInput, &payload); err != nil {
		return nil, fmt.Errorf("parse postToolUse payload: %w", err)
	}

	redactedCommand := payload.ToolInput.Command
	if redactedCommand != "" {
		if redacted, err := redact.String(redactedCommand); err == nil {
			redactedCommand = redacted
		}
	}

	redactedOutput := extractCursorToolOutput(payload.ToolOutput)
	if redactedOutput != "" {
		if redacted, err := redact.String(redactedOutput); err == nil {
			redactedOutput = redacted
		}
	}

	inputJSON, _ := json.Marshal(map[string]any{
		"command": redactedCommand,
		"cwd":     payload.ToolInput.CWD,
		"timeout": payload.ToolInput.Timeout,
	})
	payloadHash := synthesizeAssistantBlob(bs, "Bash", inputJSON)
	provenanceHash := storeRedactedBashPayload(bs, payload, redactedCommand, redactedOutput)
	toolUsesJSON := serializeStepToolUses("Bash", "", "exec")

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

func buildSubagentPromptEvent(event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var payload cursorPreToolUsePayload
	if err := json.Unmarshal(event.ToolInput, &payload); err != nil {
		return nil, fmt.Errorf("parse preToolUse payload: %w", err)
	}
	if payload.ToolInput.Prompt == "" {
		return nil, nil
	}

	var payloadHash string
	if bs != nil {
		h, _, err := bs.Put(context.Background(), []byte(payload.ToolInput.Prompt))
		if err == nil {
			payloadHash = h
		}
	}

	summary := payload.ToolInput.Prompt
	if len(summary) > 200 {
		summary = summary[:200] + "..."
	}

	ev := makeBaseRawEvent(event)
	ev.Kind = "assistant"
	ev.Role = "assistant"
	ev.Summary = summary
	ev.PayloadHash = payloadHash
	ev.ProvenanceHash = storeRawHookPayload(bs, event.ToolInput)
	ev.TurnID = event.TurnID
	ev.ToolUseID = event.ToolUseID
	ev.ToolName = "Agent"
	ev.EventSource = "hook"

	return []broker.RawEvent{ev}, nil
}

func buildAgentEvent(event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
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
	payloadHash := synthesizeAssistantBlob(bs, "Agent", inputJSON)
	provenanceHash := storeRawHookPayload(bs, event.ToolInput)
	toolUsesJSON := serializeStepToolUses("Agent", "", "exec")

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

func storeRawHookPayload(bs api.BlobPutter, payload json.RawMessage) string {
	if bs == nil || len(payload) == 0 {
		return ""
	}
	h, _, err := bs.Put(context.Background(), payload)
	if err != nil {
		return ""
	}
	return h
}

func storeRedactedBashPayload(bs api.BlobPutter, payload cursorPostToolUsePayload, redactedCommand, redactedOutput string) string {
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
	h, _, err := bs.Put(context.Background(), data)
	if err != nil {
		return ""
	}
	return h
}

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

	hh := sha256.New()
	hh.Write([]byte(sourceKey))
	stableKey := event.ToolUseID
	if stableKey == "" {
		stableKey = event.TurnID
	}
	_, _ = fmt.Fprintf(hh, ":hook:%s:%s:%s", event.Type.HookPhase(), event.ToolName, stableKey)
	eventID := hex.EncodeToString(hh.Sum(nil))

	sourceProjectPath := projectPath
	if sourceProjectPath == "" && event.CWD != "" {
		sourceProjectPath = event.CWD
	}

	return broker.RawEvent{
		EventID:           eventID,
		SourceKey:         sourceKey,
		Provider:          agentcursor.ProviderName,
		Timestamp:         event.Timestamp,
		ProviderSessionID: event.SessionID,
		SessionStartedAt:  event.Timestamp,
		SessionMetaJSON:   string(metaJSON),
		SourceProjectPath: sourceProjectPath,
		Model:             event.Model,
	}
}

var _ hooks.DirectHookEmitter = (*Provider)(nil)
