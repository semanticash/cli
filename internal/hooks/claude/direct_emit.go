package claude

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/semanticash/cli/internal/agents/api"
	agentclaude "github.com/semanticash/cli/internal/agents/claude"
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
	default:
		return nil, nil
	}
}

// buildPromptEvent creates a user prompt event from UserPromptSubmit.
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

	// Truncate prompt for summary.
	summary := event.Prompt
	if len(summary) > 200 {
		summary = summary[:200] + "..."
	}

	ev := makeBaseRawEvent(event)
	ev.Kind = "user"
	ev.Role = "user"
	ev.Summary = summary
	ev.PayloadHash = payloadHash
	ev.ProvenanceHash = payloadHash // Prompt text is stored in both blob references.
	ev.TurnID = event.TurnID
	ev.EventSource = "hook"

	return []broker.RawEvent{ev}, nil
}

// buildStepEvent creates an assistant tool-use event from PostToolUse[Write/Edit/Bash].
// The synthesized payload blob is compatible with the attribution scorer.
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

func buildWriteEvent(event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp writeInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse Write tool_input: %w", err)
	}
	if inp.FilePath == "" {
		return nil, nil
	}

	// payload_hash stores the normalized blob used by the attribution scorer.
	payloadHash := synthesizeAssistantBlob(bs, event.ToolName, inp.FilePath, event.ToolInput)

	// provenance_hash stores the original hook payload for this step.
	provenanceHash := storeRawHookPayload(bs, event)

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

func buildEditEvent(event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp editInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse Edit tool_input: %w", err)
	}
	if inp.FilePath == "" {
		return nil, nil
	}

	payloadHash := synthesizeAssistantBlob(bs, event.ToolName, inp.FilePath, event.ToolInput)
	provenanceHash := storeRawHookPayload(bs, event)
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

func buildBashEvent(event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp bashInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse Bash tool_input: %w", err)
	}

	// Redact secrets from the command text before persistence.
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

	// payload_hash stores the redacted blob used by the attribution scorer.
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

	// provenance_hash keeps the original Bash payload shape with sensitive
	// fields redacted.
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
	ev.ToolName = event.ToolName
	ev.EventSource = "hook"

	return []broker.RawEvent{ev}, nil
}

// buildSubagentPromptEvent creates a subagent prompt event from PreToolUse[Agent].
func buildSubagentPromptEvent(event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil || inp.Prompt == "" {
		return nil, nil
	}

	var payloadHash string
	if bs != nil {
		h, _, err := bs.Put(context.Background(), []byte(inp.Prompt))
		if err == nil {
			payloadHash = h
		}
	}

	summary := inp.Prompt
	if len(summary) > 200 {
		summary = summary[:200] + "..."
	}

	// provenance_hash stores the original hook payload for this step.
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

// synthesizeAssistantBlob creates the standard assistant payload blob used
// by the attribution scorer.
//
// Shape: {"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{...}}]}}
func synthesizeAssistantBlob(bs api.BlobPutter, toolName, filePath string, toolInput json.RawMessage) string {
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

// synthesizeBashBlob creates a minimal assistant payload blob for Bash events
// using the redacted command text. It never stores the raw command string.
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

// storeRawHookPayload stores the hook payload in its original shape.
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

// storeRedactedBashProvenance stores a redacted Bash payload while preserving
// the original field structure.
func storeRedactedBashProvenance(bs api.BlobPutter, event *hooks.Event, redactedCmd string) string {
	if bs == nil {
		return ""
	}

	var inp bashInput
	if len(event.ToolInput) > 0 {
		_ = json.Unmarshal(event.ToolInput, &inp)
	}
	redactedDescription := inp.Description
	if redactedDescription != "" {
		if r, err := redact.String(redactedDescription); err == nil {
			redactedDescription = r
		}
	}

	// Parse tool_response so stdout and stderr can be redacted.
	var resp struct {
		Stdout      string `json:"stdout"`
		Stderr      string `json:"stderr"`
		Interrupted bool   `json:"interrupted"`
	}
	if len(event.ToolResponse) > 0 {
		_ = json.Unmarshal(event.ToolResponse, &resp)
	}

	redactedStdout := resp.Stdout
	if redactedStdout != "" {
		if r, err := redact.String(redactedStdout); err == nil {
			redactedStdout = r
		}
	}
	redactedStderr := resp.Stderr
	if redactedStderr != "" {
		if r, err := redact.String(redactedStderr); err == nil {
			redactedStderr = r
		}
	}

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
	h, _, err := bs.Put(context.Background(), data)
	if err != nil {
		return ""
	}
	return h
}

// serializeStepToolUses produces the ToolUsesJSON string in the same shape
// in the same shape as transcript events, so the attribution scorer and
// file path extraction work without changes.
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

// makeBaseRawEvent creates a RawEvent with session context from a hook Event.
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

	// Content-addressed event ID from stable hook context.
	// Uses ToolUseID (provider-assigned, stable across retries) or TurnID
	// (for prompt events) instead of timestamp, so duplicate hook deliveries
	// produce the same event_id and INSERT OR IGNORE suppresses them.
	hh := sha256.New()
	hh.Write([]byte(event.TranscriptRef))
	stableKey := event.ToolUseID
	if stableKey == "" {
		stableKey = event.TurnID
	}
	_, _ = fmt.Fprintf(hh, ":hook:%s:%s:%s", event.Type.HookPhase(), event.ToolName, stableKey)
	eventID := hex.EncodeToString(hh.Sum(nil))

	// Derive SourceProjectPath from CWD for routing.
	sourceProjectPath := projectPath
	if sourceProjectPath == "" && event.CWD != "" {
		sourceProjectPath = event.CWD
	}

	return broker.RawEvent{
		EventID:           eventID,
		SourceKey:         event.TranscriptRef,
		Provider:          agentclaude.ProviderName,
		Timestamp:         event.Timestamp,
		ProviderSessionID: providerSessionID,
		ParentSessionID:   parentSessionID,
		SessionStartedAt:  event.Timestamp,
		SessionMetaJSON:   string(metaJSON),
		SourceProjectPath: sourceProjectPath,
		Model:             event.Model,
	}
}

// ensure Provider implements DirectHookEmitter.
var _ hooks.DirectHookEmitter = (*Provider)(nil)
