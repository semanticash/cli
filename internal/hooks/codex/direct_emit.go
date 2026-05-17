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

// BuildHookEvents implements hooks.DirectHookEmitter for Codex.
//
// The dispatcher routes only PromptSubmitted and ToolStepCompleted
// through this path. Codex lifecycle events (SessionStart -> SessionOpened,
// Stop -> AgentCompleted) reach the dispatcher through ParseHookEvent
// and are handled by the dispatcher's own lifecycle cases.
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

// buildPromptEvent emits a user event for UserPromptSubmit. The
// prompt blob is stored unredacted at this layer; the upload pipeline
// applies the global redaction policy before any byte leaves the
// machine. The summary lives in the event for terminal display.
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

// buildStepEvent handles every PostToolUse Codex routes to us. Each
// tool gets its own builder so the per-tool shape work stays local
// to the case that owns it.
func buildStepEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter) ([]broker.RawEvent, error) {
	switch event.ToolName {
	case "apply_patch":
		return buildApplyPatchEvents(ctx, event, bs)
	case "Bash":
		return buildBashEvent(ctx, event, bs)
	case "Write", "Edit":
		// Codex's PostToolUse matcher accepts these names too; if the
		// tool_input matches Claude's Write/Edit shape we forward
		// directly without going through the patch grammar.
		return buildClaudeShapedStepEvent(ctx, event, bs)
	default:
		return nil, nil
	}
}

// applyPatchToolInput is what Codex sends inside tool_input for the
// apply_patch tool. The envelope itself is a single string, parsed by
// parseApplyPatchEnvelope.
type applyPatchToolInput struct {
	Command string `json:"command"`
}

// buildApplyPatchEvents parses an apply_patch envelope and emits one
// RawEvent per file the patch touches. Add/Update sections with new
// content synthesize a Claude-shaped Write assistant blob so the
// scorer's ExtractClaudeActions pipeline credits the resulting lines
// as line-level evidence. Sections without new content (Delete,
// deletion-only Update, empty-file Add, Move source) still emit a
// file-touched event so the path is not lost from attribution and
// handoff context - the scorer records those paths in
// ProviderTouchedFiles without inflating line counts.
//
// A single Codex turn can produce multiple apply_patch calls (e.g.
// Delete followed by Add for a content replacement). We emit per-event
// records and let the scorer union the line strings across all
// assistant events with payloads; downstream the matcher only credits
// lines that also appear in the final diff, so intermediate states
// drop out automatically without a turn-level accumulator.
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

	provenanceHash := builder.StoreWrappedHookProvenance(ctx, bs, event.ToolInput, event.ToolResponse)
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
			// A rename emits two events so both paths show up in
			// attribution. The "source"/"dest" tag suffixes give
			// each half a distinct deterministic ToolUseID, so
			// ComputeEventID produces a unique EventID per half and
			// the broker's insert-or-ignore path keeps both.
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

// buildPatchFileEvent emits one assistant RawEvent for a single file
// affected by an apply_patch envelope. When content is non-empty, the
// event carries a Claude-shaped Write assistant blob so the scorer's
// ExtractClaudeActions extractor produces line-level evidence; when
// content is empty (Delete, deletion-only Update, empty-file Add,
// Move source), the event is emitted without a payload hash so the
// file lands in ProviderTouchedFiles instead.
//
// The tag uniquely identifies this emission inside the envelope. The
// tag is folded into ToolUseID before ComputeEventID runs, so events
// from a single multi-file hook delivery get distinct, deterministic
// EventIDs without colliding on the broker's insert-or-ignore key.
func buildPatchFileEvent(ctx context.Context, event *hooks.Event, bs api.BlobPutter, op applyPatchOp, path, content, provenanceHash, tag string) (broker.RawEvent, error) {
	if path == "" {
		return broker.RawEvent{}, fmt.Errorf("buildPatchFileEvent: empty path for op=%v", op)
	}

	// Routed path is absolute (joined with the hook's cwd if the parser
	// returned relative). Every downstream consumer keys off this same
	// form:
	//   - broker.RouteEvents matches FilePaths against the registered
	//     repo's absolute canonical_path.
	//   - ExtractClaudeActions reads file_path from the synthesized
	//     Write blob and calls NormalizePath(file_path, repoRoot) to
	//     produce the repo-relative key that the final-diff matcher
	//     compares against. If we stored the parsed (possibly
	//     subdir-relative) path here instead, a Codex session launched
	//     from a subdirectory would produce blob keys like "main.go"
	//     while the final diff carries "pkg/main.go" and no match
	//     would land.
	//   - BuildCandidatesFromRows reads tool_uses_json's file_path for
	//     the provider-touch path; same key story applies.
	routedPath := absolutizeForRouting(path, event.CWD)

	// Tool naming differs between the line-level and provider-touch
	// paths so candidate building routes the event correctly:
	//
	//   - Content present -> "Write": the scorer loads the Claude-
	//     shaped blob and extracts per-line evidence via
	//     ExtractClaudeActions.
	//   - No content      -> "codex_file_edit": HasProviderFileEdit
	//     matches the name and the file lands in
	//     ProviderTouchedFiles without inflating line counts.
	//
	// Without the second case, deletion-only updates, empty-file adds,
	// and pure rename halves would be dropped by the
	// `PayloadHash == ""` gate in BuildCandidatesFromRows.
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

// absolutizeForRouting returns an absolute form of path suitable for
// broker.RouteEvents prefix-matching AND for downstream
// NormalizePath(file_path, repoRoot) relativization on the scorer
// side. Codex's apply_patch envelope can carry either an absolute or
// a workspace-relative path depending on which runtime emitted it,
// and parseApplyPatchEnvelope strips the repo prefix when it knows
// the root - but that prefix is only the cwd, which may be a
// subdirectory of the repo. Joining with cwd here preserves the
// subdir component so a session launched from /repo/pkg editing
// main.go still routes to /repo/pkg/main.go and the scorer
// relativizes against /repo to get pkg/main.go.
//
// platform.LooksAbsolutePath (not filepath.IsAbs) is used so an
// already-absolute path is recognized regardless of the host OS - in
// practice this matters for native paths on every platform and for
// MSYS-style /c/... paths that NormalizePath maps to C:/... on the
// scorer side. A bare POSIX /repo/... path on a Windows host would
// also be left untouched by this helper, but it would not actually
// route: the registered repo's canonical_path is native (C:\repo)
// and broker.RouteEvents would find no prefix match. We have no
// evidence Codex Desktop emits bare POSIX paths on Windows; if a
// future probe surfaces that shape, this helper (and the broker's
// path-compare) would need an explicit POSIX-to-native mapping
// alongside the MSYS one already in NormalizePath. When cwd is
// empty, path is returned unchanged.
func absolutizeForRouting(path, cwd string) string {
	if path == "" || platform.LooksAbsolutePath(path) || cwd == "" {
		return path
	}
	return filepath.ToSlash(filepath.Join(cwd, path))
}

// perFileEvent returns a shallow copy of event with a per-file
// ToolUseID. The clone is what feeds ComputeEventID inside
// BaseRawEvent, so each file emitted from one multi-file apply_patch
// hook delivery ends up with a distinct, deterministic EventID
// without mutating the caller's event struct.
func perFileEvent(event *hooks.Event, tag string) hooks.Event {
	clone := *event
	clone.ToolUseID = perFileToolUseID(event.ToolUseID, tag)
	return clone
}

// fileOpForPatch maps the parsed envelope op to the file_op token the
// scorer recognizes on a Write/Edit tool use.
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

// perFileToolUseID derives a stable, unique tool-use identifier for
// each emission from a multi-file apply_patch envelope. The tag is
// produced at the call site so source/destination halves of a Move
// (and any other op pair that shares a file ordinal) get distinct
// IDs. Without the per-file suffix, the dispatcher's ComputeEventID
// would collapse every emission into one event ID and downstream
// INSERT-OR-IGNORE suppression would drop all but the first.
func perFileToolUseID(base, tag string) string {
	if base == "" {
		return "apply_patch:" + tag
	}
	return base + ":" + tag
}

// buildBashEvent emits a Bash tool-use event. The command is stored
// in the payload blob using Claude's Write/Edit/Bash shape so the
// shared scorer recognizes it as a Bash assistant action and can
// extract deleted paths via ExtractDeletedPaths.
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

// buildClaudeShapedStepEvent forwards Write/Edit tool calls whose
// payload is already in Claude's tool_input shape (file_path +
// content / old_string / new_string). This path is rarely hit -
// Codex routes file mutations through apply_patch by default - but
// the PostToolUse matcher accepts both names, so we honor them.
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

// serializeStepToolUses returns the ToolUsesJSON string the scorer
// consumes on assistant events. We reuse Claude's serializer to keep
// the wire shape identical across providers.
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

// makeBaseRawEvent builds the envelope every Codex RawEvent shares.
// The source key is the transcript path so dispatch deduplication
// (via ComputeEventID) and downstream session linking key off a single
// stable identifier.
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
