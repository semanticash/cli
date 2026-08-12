package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/semanticash/cli/internal/agents/api"
	agentclaude "github.com/semanticash/cli/internal/agents/claude"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/hooks/builder"
	"github.com/semanticash/cli/internal/platform"
)

// BuildHookEvents converts direct Codex hooks into stored events.
func (p *Provider) BuildHookEvents(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	switch event.Type {
	case hooks.PromptSubmitted:
		return buildPromptEvent(ctx, event, bs)
	case hooks.ToolStepCompleted:
		return buildStepEvent(ctx, event, bs)
	default:
		return nil, nil
	}
}

// buildPromptEvent stores a prompt event. Outbound redaction runs later.
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

// buildStepEvent converts a supported PostToolUse event.
func buildStepEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	switch event.ToolName {
	case "apply_patch":
		return buildApplyPatchEvents(ctx, event, bs)
	case "Bash":
		return buildBashEvent(ctx, event, bs)
	case "Write", "Edit":
		return buildClaudeShapedStepEvent(ctx, event, bs)
	default:
		return nil, nil
	}
}

// applyPatchToolInput contains an apply_patch command envelope.
type applyPatchToolInput struct {
	Command string `json:"command"`
}

// buildApplyPatchEvents emits one event per file in an apply_patch envelope.
// Content changes provide line evidence; metadata-only changes provide touch
// evidence.
func buildApplyPatchEvents(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp applyPatchToolInput
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse apply_patch tool_input: %w", err)
	}
	if inp.Command == "" {
		return nil, nil
	}

	files := parseApplyPatchEnvelope(inp.Command, event.CWD)
	if len(files) == 0 {
		return nil, nil
	}

	// All per-file events share the envelope provenance blob.
	provenanceHash := storeApplyPatchCanonicalProvenance(ctx, bs, event.CWD, files)
	out := make([]broker.RawEvent, 0, len(files))

	for i, f := range files {
		if f.path == "" && f.movedTo == "" {
			continue
		}
		switch f.op {
		case applyPatchOpAdd, applyPatchOpUpdate, applyPatchOpDelete:
			if f.path == "" {
				continue
			}
			ev, err := buildPatchFileEvent(ctx, event, bs, f.op, f.path, f.content, provenanceHash, fmt.Sprintf("%d", i))
			if err != nil {
				return nil, err
			}
			out = append(out, ev)
		case applyPatchOpMove:
			// Emit distinct source and destination events for a move.
			if f.path != "" {
				ev, err := buildPatchFileEvent(ctx, event, bs, applyPatchOpDelete, f.path, "", provenanceHash, fmt.Sprintf("%d:source", i))
				if err != nil {
					return nil, err
				}
				out = append(out, ev)
			}
			if f.movedTo != "" {
				ev, err := buildPatchFileEvent(ctx, event, bs, applyPatchOpAdd, f.movedTo, f.content, provenanceHash, fmt.Sprintf("%d:dest", i))
				if err != nil {
					return nil, err
				}
				out = append(out, ev)
			}
		}
	}
	return out, nil
}

// storeApplyPatchCanonicalProvenance stores the canonical multi-file shape:
//
//	{ "version": 1, "files": [ { "path": "...", "operation": "...", ... }, ... ] }
//
// It returns an empty hash when storage is unavailable.
func storeApplyPatchCanonicalProvenance(ctx context.Context, bs api.BlobPutter, cwd string, files []applyPatchFile) string {
	if bs == nil || len(files) == 0 {
		return ""
	}

	type entry struct {
		Path          string  `json:"path"`
		Operation     string  `json:"operation"`
		Content       *string `json:"content,omitempty"`
		OldText       *string `json:"old_text,omitempty"`
		NewText       *string `json:"new_text,omitempty"`
		DiffAvailable bool    `json:"diff_available"`
		Reason        string  `json:"reason,omitempty"`
	}

	strPtr := func(s string) *string { return &s }

	entries := make([]entry, 0, len(files))
	for _, f := range files {
		switch f.op {
		case applyPatchOpAdd:
			if f.path == "" {
				continue
			}
			c := f.content
			entries = append(entries, entry{
				Path:          absolutizeForRouting(f.path, cwd),
				Operation:     "create",
				Content:       strPtr(c),
				DiffAvailable: true,
			})

		case applyPatchOpUpdate:
			if f.path == "" {
				continue
			}
			entries = append(entries, entry{
				Path:          absolutizeForRouting(f.path, cwd),
				Operation:     "edit",
				OldText:       strPtr(f.removed),
				NewText:       strPtr(f.content),
				DiffAvailable: true,
			})

		case applyPatchOpDelete:
			if f.path == "" {
				continue
			}
			// Delete sections do not include the prior content.
			entries = append(entries, entry{
				Path:          absolutizeForRouting(f.path, cwd),
				Operation:     "delete",
				DiffAvailable: false,
				Reason:        "delete event has no preimage",
			})

		case applyPatchOpMove:
			// Moves may include source or destination content.
			if f.path != "" {
				sourceEntry := entry{
					Path:      absolutizeForRouting(f.path, cwd),
					Operation: "move",
				}
				if f.removed != "" {
					sourceEntry.OldText = strPtr(f.removed)
					sourceEntry.DiffAvailable = true
				} else {
					sourceEntry.DiffAvailable = false
					sourceEntry.Reason = "move source has no preimage"
				}
				entries = append(entries, sourceEntry)
			}
			if f.movedTo != "" {
				destEntry := entry{
					Path:      absolutizeForRouting(f.movedTo, cwd),
					Operation: "create",
				}
				if f.content != "" {
					destEntry.Content = strPtr(f.content)
					destEntry.DiffAvailable = true
				} else {
					destEntry.DiffAvailable = false
					destEntry.Reason = "move destination has no content"
				}
				entries = append(entries, destEntry)
			}
		}
	}
	if len(entries) == 0 {
		return ""
	}

	blob, err := json.Marshal(map[string]any{
		"version": 1,
		"files":   entries,
	})
	if err != nil {
		return ""
	}
	return builder.PutAndHash(ctx, bs, blob)
}

// buildPatchFileEvent emits line evidence when content is available and touch
// evidence otherwise. tag gives each file a stable event identity.
func buildPatchFileEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter, op applyPatchOp, path, content, provenanceHash, tag string) (broker.RawEvent, error) {
	if path == "" {
		return broker.RawEvent{}, fmt.Errorf("buildPatchFileEvent: empty path for op=%v", op)
	}

	// Routing and scoring share the same absolute path.
	routedPath := absolutizeForRouting(path, event.CWD)

	// Content uses Write evidence; metadata-only changes use file-touch evidence.
	toolName := "codex_file_edit"
	var payloadHash string
	if content != "" {
		writeInput, err := json.Marshal(map[string]string{
			"file_path": routedPath,
			"content":   content,
		})
		if err != nil {
			return broker.RawEvent{}, fmt.Errorf("marshal patch input for %s: %w", routedPath, err)
		}
		payloadHash = builder.SynthesizeAssistantBlob(ctx, bs, "Write", writeInput)
		toolName = "Write"
	}

	perFile := perFileEvent(event, tag)
	ev := makeBaseRawEvent(&perFile)
	ev.Kind = "assistant"
	ev.Role = "assistant"
	ev.PayloadHash = payloadHash
	ev.ProvenanceHash = provenanceHash
	ev.ToolUsesJSON = serializeStepToolUses(toolName, routedPath, fileOpForPatch(op))
	ev.TurnID = event.TurnID
	ev.ToolUseID = perFile.ToolUseID
	ev.ToolName = toolName
	ev.EventSource = "hook"
	ev.FilePaths = []string{routedPath}
	return ev, nil
}

// absolutizeForRouting resolves workspace-relative paths against the hook cwd.
// Absolute paths are preserved across host path styles.
func absolutizeForRouting(path, cwd string) string {
	if path == "" || platform.LooksAbsolutePath(path) || cwd == "" {
		return path
	}
	return filepath.ToSlash(filepath.Join(cwd, path))
}

// perFileEvent returns a copy with a stable per-file tool-use ID.
func perFileEvent(event *hooks.Event, tag string) hooks.Event {
	clone := *event
	clone.ToolUseID = perFileToolUseID(event.ToolUseID, tag)
	return clone
}

// fileOpForPatch maps patch operations to scorer operations.
func fileOpForPatch(op applyPatchOp) string {
	switch op {
	case applyPatchOpAdd:
		return "write"
	case applyPatchOpUpdate:
		return "edit"
	case applyPatchOpDelete:
		return "delete"
	default:
		return ""
	}
}

// perFileToolUseID derives a stable ID for one file in a patch envelope.
func perFileToolUseID(base, tag string) string {
	if base == "" {
		return "apply_patch:" + tag
	}
	return base + ":" + tag
}

// buildBashEvent stores a Bash event in the shared assistant payload shape.
func buildBashEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var inp struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(event.ToolInput, &inp); err != nil {
		return nil, fmt.Errorf("parse Bash tool_input: %w", err)
	}
	if inp.Command == "" {
		return nil, nil
	}
	redactedCmd := builder.Redact(inp.Command)

	inputJSON, err := json.Marshal(map[string]string{"command": redactedCmd})
	if err != nil {
		return nil, fmt.Errorf("marshal Bash payload: %w", err)
	}
	payloadHash := builder.SynthesizeAssistantBlob(ctx, bs, "Bash", inputJSON)
	provenanceHash := builder.StoreWrappedHookProvenance(ctx, bs, event.ToolInput, event.ToolResponse)

	ev := makeBaseRawEvent(event)
	ev.Kind = "assistant"
	ev.Role = "assistant"
	ev.Summary = builder.TruncateWithEllipsis(redactedCmd, 200)
	ev.PayloadHash = payloadHash
	ev.ProvenanceHash = provenanceHash
	ev.ToolUsesJSON = serializeStepToolUses("Bash", "", "exec")
	ev.TurnID = event.TurnID
	ev.ToolUseID = event.ToolUseID
	ev.ToolName = "Bash"
	ev.EventSource = "hook"
	return []broker.RawEvent{ev}, nil
}

// buildClaudeShapedStepEvent stores compatible Write and Edit payloads.
func buildClaudeShapedStepEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	var generic struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(event.ToolInput, &generic); err != nil || generic.FilePath == "" {
		return nil, nil
	}
	payloadHash := builder.SynthesizeAssistantBlob(ctx, bs, event.ToolName, event.ToolInput)
	provenanceHash := builder.StoreWrappedHookProvenance(ctx, bs, event.ToolInput, event.ToolResponse)
	fileOp := "edit"
	if event.ToolName == "Write" {
		fileOp = "write"
	}

	ev := makeBaseRawEvent(event)
	ev.Kind = "assistant"
	ev.Role = "assistant"
	ev.PayloadHash = payloadHash
	ev.ProvenanceHash = provenanceHash
	ev.ToolUsesJSON = serializeStepToolUses(event.ToolName, generic.FilePath, fileOp)
	ev.TurnID = event.TurnID
	ev.ToolUseID = event.ToolUseID
	ev.ToolName = event.ToolName
	ev.EventSource = "hook"
	ev.FilePaths = []string{generic.FilePath}
	return []broker.RawEvent{ev}, nil
}

// serializeStepToolUses returns the shared assistant tool-use shape.
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

// makeBaseRawEvent builds the common Codex event envelope.
func makeBaseRawEvent(event *hooks.Event) broker.RawEvent {
	meta := map[string]any{"source_key": event.TranscriptRef}
	if event.CWD != "" {
		meta["project_path"] = event.CWD
	}
	metaJSON, _ := json.Marshal(meta)

	return builder.BaseRawEvent(builder.BaseInput{
		Event:             event,
		SourceKey:         event.TranscriptRef,
		Provider:          providerName,
		ProviderSessionID: event.SessionID,
		SessionMetaJSON:   string(metaJSON),
		SourceProjectPath: event.CWD,
	})
}
