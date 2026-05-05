package kirocli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/agents/api"
	agentKiro "github.com/semanticash/cli/internal/agents/kiro"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/hooks/builder"
)

// resolveKiroFilePath joins relative Kiro CLI paths against CWD.
// POSIX-style absolute paths are accepted on every host because
// Kiro may emit forward-slash paths on Windows.
func resolveKiroFilePath(cwd, path string) string {
	if path == "" || cwd == "" {
		return path
	}
	if filepath.IsAbs(path) || strings.HasPrefix(path, "/") {
		return path
	}
	return filepath.Clean(filepath.Join(cwd, path))
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

// buildWriteEvent emits a Write event for Kiro's create command.
// Paths are canonicalized before routing and storage.
func buildWriteEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp fsWriteInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse write tool_input: %w", err)
	}
	if inp.Path == "" {
		return nil, nil
	}

	resolvedPath := resolveKiroFilePath(event.CWD, inp.Path)

	inputJSON, _ := json.Marshal(map[string]any{
		"file_path": resolvedPath,
		"content":   inp.Content,
	})
	payloadHash := builder.SynthesizeAssistantBlob(ctx, bs, "Write", inputJSON)
	provenanceHash := builder.StoreWrappedHookProvenance(ctx, bs, event.ToolInput, event.ToolResponse)
	toolUsesJSON := agentKiro.BuildToolUsesJSON(resolvedPath, "create").String

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

// buildEditEvent emits an Edit event for strReplace and insert.
// insert has no old content, so it is stored as old_string="" and
// new_string=<content>.
func buildEditEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp fsWriteInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse write tool_input: %w", err)
	}
	if inp.Path == "" {
		return nil, nil
	}

	resolvedPath := resolveKiroFilePath(event.CWD, inp.Path)

	oldString := inp.OldStr
	newString := inp.NewStr
	if inp.Command == "insert" {
		oldString = ""
		newString = inp.Content
	}

	inputJSON, _ := json.Marshal(map[string]any{
		"file_path":  resolvedPath,
		"old_string": oldString,
		"new_string": newString,
	})
	payloadHash := builder.SynthesizeAssistantBlob(ctx, bs, "Edit", inputJSON)
	provenanceHash := builder.StoreWrappedHookProvenance(ctx, bs, event.ToolInput, event.ToolResponse)
	toolUsesJSON := agentKiro.BuildToolUsesJSON(resolvedPath, "edit").String

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
// When present, __tool_use_purpose is used as the summary instead
// of the raw shell command.
func buildBashEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp bashInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse shell tool_input: %w", err)
	}

	redactedCmd := builder.Redact(inp.Command)
	summary := inp.Purpose
	if summary == "" {
		summary = redactedCmd
	}
	summary = builder.TruncateWithEllipsis(summary, 200)

	inputJSON, _ := json.Marshal(map[string]string{"command": redactedCmd})
	payloadHash := builder.SynthesizeAssistantBlob(ctx, bs, "Bash", inputJSON)
	provenanceHash := storeRedactedBashProvenance(ctx, bs, event, redactedCmd)
	toolUsesJSON := agentKiro.BuildToolUsesJSON("", "exec").String

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

// buildSubagentPromptEvent emits the parent boundary for a Kiro
// AgentCrew call. Summaries prefer purpose, then task, then a stage
// count placeholder.
func buildSubagentPromptEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp subagentInput
	if len(event.ToolInput) > 0 {
		_ = json.Unmarshal(event.ToolInput, &inp)
	}

	summary := subagentDispatchSummary(inp)
	if summary == "" {
		return nil, nil
	}

	body := inp.Purpose
	if body == "" {
		body = inp.Task
	}
	if body == "" {
		body = summary
	}
	payloadHash := builder.StorePromptPayload(ctx, bs, body)
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

// subagentDispatchSummary returns the display summary for a subagent
// dispatch.
func subagentDispatchSummary(inp subagentInput) string {
	if inp.Purpose != "" {
		return builder.TruncateWithEllipsis(inp.Purpose, 200)
	}
	if inp.Task != "" {
		return builder.TruncateWithEllipsis(inp.Task, 200)
	}
	if len(inp.Stages) > 0 {
		return fmt.Sprintf("Kiro subagent: %d stages", len(inp.Stages))
	}
	return ""
}

// buildSubagentCompletedEvent emits the completion boundary for an
// AgentCrew call. The first text response becomes the summary.
func buildSubagentCompletedEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	summary := "Kiro subagent completed"
	if len(event.ToolResponse) > 0 {
		var resp bashResponse
		if json.Unmarshal(event.ToolResponse, &resp) == nil {
			for _, item := range resp.Items {
				if item.Text != "" {
					summary = item.Text
					break
				}
			}
		}
	}
	summary = builder.TruncateWithEllipsis(summary, 200)

	provenanceHash := builder.StoreWrappedHookProvenance(ctx, bs, event.ToolInput, event.ToolResponse)

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

// storeRedactedBashProvenance stores the redacted stdout/stderr from
// the first structured shell response item.
func storeRedactedBashProvenance(ctx context.Context, bs api.BlobPutter, event *hooks.Event, redactedCmd string) string {
	if bs == nil {
		return ""
	}

	var resp bashResponse
	if len(event.ToolResponse) > 0 {
		_ = json.Unmarshal(event.ToolResponse, &resp)
	}

	redactedStdout := ""
	redactedStderr := ""
	for _, item := range resp.Items {
		if item.Json != nil {
			redactedStdout = builder.Redact(item.Json.Stdout)
			redactedStderr = builder.Redact(item.Json.Stderr)
			break
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
	return builder.PutAndHash(ctx, bs, data)
}

// makeBaseRawEvent uses the workspace-scoped Kiro session ID as both
// the source key and provider session ID.
func makeBaseRawEvent(event *hooks.Event) broker.RawEvent {
	meta := map[string]any{}
	if event.CWD != "" {
		meta["project_path"] = event.CWD
	}
	metaJSON, _ := json.Marshal(meta)

	return builder.BaseRawEvent(builder.BaseInput{
		Event:             event,
		SourceKey:         event.SessionID,
		Provider:          agentKiro.ProviderNameCLI,
		ProviderSessionID: event.SessionID,
		SessionMetaJSON:   string(metaJSON),
		SourceProjectPath: event.CWD,
	})
}

// ensure Provider implements DirectHookEmitter.
var _ hooks.DirectHookEmitter = (*Provider)(nil)
