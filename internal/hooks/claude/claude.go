package claude

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/agents/api"
	agentclaude "github.com/semanticash/cli/internal/agents/claude"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/util"
)

const providerName = "claude-code"

// Provider implements hooks.HookProvider for Claude Code.
type Provider struct{}

func init() {
	hooks.RegisterProvider(&Provider{})
}

func (p *Provider) Name() string        { return providerName }
func (p *Provider) DisplayName() string { return "Claude Code" }

func (p *Provider) IsAvailable() bool {
	if util.ResolveExecutable([]string{"claude"}) != "" {
		return true
	}
	// Claude Code installed via VS Code extension or desktop app creates
	// ~/.claude without adding the CLI binary to PATH.
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(home, ".claude"))
	return err == nil
}

type hookMatcher struct {
	Matcher string      `json:"matcher"`
	Hooks   []hookEntry `json:"hooks"`
}

type hookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

const semanticaMarker = "semantica capture claude-code"

func (p *Provider) InstallHooks(ctx context.Context, repoRoot string, binaryPath string) (int, error) {
	settingsPath := filepath.Join(repoRoot, ".claude", "settings.local.json")

	// Read existing settings.
	var raw map[string]json.RawMessage
	data, err := os.ReadFile(settingsPath)
	if err == nil {
		if err := json.Unmarshal(data, &raw); err != nil {
			return 0, fmt.Errorf("parse existing %s: %w", settingsPath, err)
		}
	}
	if raw == nil {
		raw = make(map[string]json.RawMessage)
	}

	// Parse existing hooks.
	existingHooks := make(map[string][]hookMatcher)
	if h, ok := raw["hooks"]; ok {
		if err := json.Unmarshal(h, &existingHooks); err != nil {
			return 0, fmt.Errorf("parse hooks in %s: %w", settingsPath, err)
		}
	}

	bin := binaryPath
	if bin == "" {
		bin = "semantica"
	}

	hookDefs := []struct {
		hookPoint string
		matcher   string
		command   string
	}{
		{"UserPromptSubmit", "", bin + " capture claude-code user-prompt-submit"},
		{"Stop", "", bin + " capture claude-code stop"},
		{"PostToolUse", "Task", bin + " capture claude-code post-task"},
		{"PreToolUse", "Agent", bin + " capture claude-code pre-agent"},
		{"PostToolUse", "Agent", bin + " capture claude-code post-agent"},
		{"PostToolUse", "Write", bin + " capture claude-code post-write"},
		{"PostToolUse", "Edit", bin + " capture claude-code post-edit"},
		{"PostToolUse", "Bash", bin + " capture claude-code post-bash"},
		{"SessionStart", "", bin + " capture claude-code session-start"},
		{"SessionEnd", "", bin + " capture claude-code session-end"},
	}

	count := 0
	for _, def := range hookDefs {
		// Check if this specific hook already exists (match by command, not just marker).
		matchers := existingHooks[def.hookPoint]
		found := false
		for _, m := range matchers {
			if m.Matcher != def.matcher {
				continue
			}
			for _, h := range m.Hooks {
				if strings.Contains(h.Command, semanticaMarker) {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if found {
			count++
			continue
		}

		// Append our hook.
		existingHooks[def.hookPoint] = append(matchers, hookMatcher{
			Matcher: def.matcher,
			Hooks:   []hookEntry{{Type: "command", Command: def.command}},
		})
		count++
	}

	hooksJSON, _ := json.Marshal(existingHooks)
	raw["hooks"] = hooksJSON

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("marshal settings: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return 0, fmt.Errorf("mkdir .claude: %w", err)
	}
	if err := os.WriteFile(settingsPath, out, 0o644); err != nil {
		return 0, fmt.Errorf("write settings: %w", err)
	}

	return count, nil
}

func (p *Provider) UninstallHooks(ctx context.Context, repoRoot string) error {
	settingsPath := filepath.Join(repoRoot, ".claude", "settings.local.json")

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil // No settings file - nothing to uninstall.
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}

	hooksRaw, ok := raw["hooks"]
	if !ok {
		return nil
	}

	var hooksMap map[string][]hookMatcher
	if err := json.Unmarshal(hooksRaw, &hooksMap); err != nil {
		return nil
	}

	for hookPoint, matchers := range hooksMap {
		var kept []hookMatcher
		for _, m := range matchers {
			var keptHooks []hookEntry
			for _, h := range m.Hooks {
				if !strings.Contains(h.Command, semanticaMarker) {
					keptHooks = append(keptHooks, h)
				}
			}
			if len(keptHooks) > 0 {
				m.Hooks = keptHooks
				kept = append(kept, m)
			}
		}
		if len(kept) > 0 {
			hooksMap[hookPoint] = kept
		} else {
			delete(hooksMap, hookPoint)
		}
	}

	hooksJSON, _ := json.Marshal(hooksMap)
	raw["hooks"] = hooksJSON
	out, _ := json.MarshalIndent(raw, "", "  ")
	return os.WriteFile(settingsPath, out, 0o644)
}

func (p *Provider) AreHooksInstalled(ctx context.Context, repoRoot string) bool {
	data, err := os.ReadFile(filepath.Join(repoRoot, ".claude", "settings.local.json"))
	if err != nil {
		return false
	}
	return strings.Contains(string(data), semanticaMarker)
}

func (p *Provider) HookBinary(ctx context.Context, repoRoot string) (string, error) {
	data, err := os.ReadFile(filepath.Join(repoRoot, ".claude", "settings.local.json"))
	if err != nil {
		return "", err
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", err
	}
	hooksRaw, ok := raw["hooks"]
	if !ok {
		return "", fmt.Errorf("no hooks in settings")
	}
	var hooksMap map[string][]hookMatcher
	if err := json.Unmarshal(hooksRaw, &hooksMap); err != nil {
		return "", err
	}

	// Find any semantica command and extract the binary token.
	for _, matchers := range hooksMap {
		for _, m := range matchers {
			for _, h := range m.Hooks {
				if strings.Contains(h.Command, semanticaMarker) {
					parts := strings.Fields(h.Command)
					if len(parts) > 0 {
						return parts[0], nil
					}
				}
			}
		}
	}
	return "", fmt.Errorf("no semantica hook found")
}

// DiscoverSubagentTranscripts scans the subagents/ directory under the parent
// transcript's session directory and returns paths to all child JSONL files.
// Claude Code stores subagent transcripts at:
//
//	<project>/<parent-uuid>/subagents/<agent-id>.jsonl
//
// Compact files (agent-acompact-*.jsonl) are excluded - they are compaction
// artifacts that coexist with the original transcript. Since event IDs include
// the transcript path in their content hash, reading both would produce
// duplicate events with different IDs, inflating attribution.
func (p *Provider) DiscoverSubagentTranscripts(ctx context.Context, parentTranscriptRef string) ([]string, error) {
	// Parent transcript: <project>/<parent-uuid>.jsonl
	// Subagents dir:     <project>/<parent-uuid>/subagents/
	parentDir := strings.TrimSuffix(parentTranscriptRef, ".jsonl")
	subagentsDir := filepath.Join(parentDir, "subagents")

	entries, err := os.ReadDir(subagentsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read subagents dir: %w", err)
	}

	var paths []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		// Skip compaction artifacts to prevent duplicate event ingestion.
		if strings.Contains(e.Name(), "acompact") {
			continue
		}
		paths = append(paths, filepath.Join(subagentsDir, e.Name()))
	}
	return paths, nil
}

// SubagentStateKey returns a stable key for a subagent's capture state,
// derived from the transcript filename (e.g., "agent-a4d3f93317e599c55").
func (p *Provider) SubagentStateKey(subagentTranscriptRef string) string {
	base := filepath.Base(subagentTranscriptRef)
	return strings.TrimSuffix(base, ".jsonl")
}

// stdinPayload is the JSON structure sent by Claude Code hooks on stdin.
type stdinPayload struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	CWD            string          `json:"cwd,omitempty"`
	Prompt         string          `json:"prompt,omitempty"`
	Model          string          `json:"model,omitempty"`
	ToolUseID      string          `json:"tool_use_id,omitempty"`
	ToolName       string          `json:"tool_name,omitempty"`
	ToolInput      json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse   json.RawMessage `json:"tool_response,omitempty"`
	// Claude docs also use tool_result - accept it as an alias.
	ToolResult json.RawMessage `json:"tool_result,omitempty"`
}

// stateAlteringTools are the tool names captured as direct step events.
var stateAlteringTools = map[string]bool{
	"Write": true,
	"Edit":  true,
	"Bash":  true,
}

func (p *Provider) ParseHookEvent(ctx context.Context, hookName string, stdin io.Reader) (*hooks.Event, error) {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return nil, fmt.Errorf("read stdin: %w", err)
	}

	var payload stdinPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("parse stdin JSON: %w", err)
	}

	// Prefer tool_response, fall back to tool_result.
	toolResponse := payload.ToolResponse
	if len(toolResponse) == 0 {
		toolResponse = payload.ToolResult
	}

	event := &hooks.Event{
		SessionID:     payload.SessionID,
		TranscriptRef: payload.TranscriptPath,
		Prompt:        payload.Prompt,
		Model:         payload.Model,
		Timestamp:     time.Now().UnixMilli(),
		CWD:           payload.CWD,
		ToolName:      payload.ToolName,
		ToolInput:     payload.ToolInput,
		ToolResponse:  toolResponse,
		ToolUseID:     payload.ToolUseID,
	}

	switch hookName {
	case "user-prompt-submit":
		event.Type = hooks.PromptSubmitted
	case "stop":
		event.Type = hooks.AgentCompleted
	case "post-task", "post-agent":
		event.Type = hooks.SubagentCompleted
		event.ToolUseID = payload.ToolUseID
	case "pre-agent":
		event.Type = hooks.SubagentPromptSubmitted
	case "post-write", "post-edit", "post-bash":
		if stateAlteringTools[payload.ToolName] {
			event.Type = hooks.ToolStepCompleted
		} else {
			return nil, nil
		}
	case "session-start":
		event.Type = hooks.SessionOpened
	case "session-end":
		event.Type = hooks.SessionClosed
	default:
		return nil, nil // Unknown hook - skip.
	}

	return event, nil
}

func (p *Provider) TranscriptOffset(ctx context.Context, transcriptRef string) (int, error) {
	f, err := os.Open(transcriptRef)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer func() { _ = f.Close() }()

	count := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB line buffer
	for scanner.Scan() {
		count++
	}
	return count, scanner.Err()
}

// ReadFromOffset reads transcript events starting from a line-based offset.
//
// Stale offset policy: Claude Code has no compaction hook, so context
// compaction can rewrite/truncate the transcript, invalidating saved offsets.
// When the saved offset exceeds the current line count, we treat it as a
// compaction rewrite - reset to EOF (accept a gap) rather than read from an
// invalid position. This matches the ContextCompacted "accept a gap" policy.
func (p *Provider) ReadFromOffset(ctx context.Context, transcriptRef string, offset int, bs api.BlobPutter) ([]broker.RawEvent, int, error) {
	f, err := os.Open(transcriptRef)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, offset, nil
		}
		return nil, offset, err
	}
	defer func() { _ = f.Close() }()

	// Count total lines to detect stale offsets after transcript compaction.
	totalLines := 0
	prescan := bufio.NewScanner(f)
	prescan.Buffer(make([]byte, 1024*1024), 1024*1024)
	for prescan.Scan() {
		totalLines++
	}
	if err := prescan.Err(); err != nil {
		return nil, offset, fmt.Errorf("prescan transcript: %w", err)
	}

	if offset > totalLines {
		// Transcript was rewritten (compaction) - offset is invalid.
		// Reset to EOF baseline; next turn will capture from here.
		slog.Warn("claude transcript offset invalid after rewrite; resetting baseline",
			"transcript", transcriptRef,
			"saved_offset", offset,
			"total_lines", totalLines,
		)
		return nil, totalLines, nil
	}

	// Rewind to start for the actual read pass.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, offset, fmt.Errorf("seek to start: %w", err)
	}

	providerSessionID := agentclaude.ExtractSessionIDFromPath(transcriptRef)
	if providerSessionID == "" {
		providerSessionID = agentclaude.ExtractBasename(transcriptRef)
	}
	parentSessionID := agentclaude.ExtractParentSessionID(transcriptRef)
	projectPath := agentclaude.DecodeProjectPathFromSourceKey(transcriptRef)

	model := hooks.ModelFromContext(ctx)

	meta := map[string]any{"source_key": transcriptRef}
	// ExtractFields is called below per line, but we also track the first
	// model name seen in the transcript so sessions that were created before
	// the hook sent a model value still get one.
	if projectPath != "" {
		meta["project_path"] = projectPath
	}
	metaJSON, _ := json.Marshal(meta)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	lineNum := 0
	var events []broker.RawEvent

	for scanner.Scan() {
		if lineNum < offset {
			lineNum++
			continue
		}

		raw := scanner.Bytes()
		line := strings.TrimSpace(string(raw))
		if line == "" {
			lineNum++
			continue
		}

		fields := agentclaude.ExtractFields(line)
		if fields.Skip {
			lineNum++
			continue
		}

		// Pick up model from the first assistant message that has one.
		if model == "" && fields.Model != "" {
			model = fields.Model
		}

		// Content-addressed event ID.
		hh := sha256.New()
		hh.Write([]byte(transcriptRef))
		_, _ = fmt.Fprintf(hh, ":%d:", lineNum)
		hh.Write(raw)
		eventID := hex.EncodeToString(hh.Sum(nil))

		// Store payload blobs selectively. Read events remain visible in the
		// timeline, but their raw file-content payloads are intentionally not
		// persisted to keep observational events lightweight.
		var payloadHash string
		if bs != nil && shouldStoreTranscriptPayload(fields) {
			payloadHash, _, _ = bs.Put(ctx, bytes.TrimRight(raw, "\r\n"))
		}

		toolUsesStr := ""
		if s := agentclaude.SerializeToolUses(fields.ToolUses, fields.ContentTypes); s.Valid {
			toolUsesStr = s.String
		}

		filePaths := broker.ExtractFilePaths(toolUsesStr)

		// Extract first tool_use_id and tool_name for dedup support.
		var firstToolUseID, firstToolName string
		for _, tu := range fields.ToolUses {
			if tu.ToolUseID != "" {
				firstToolUseID = tu.ToolUseID
				firstToolName = tu.Name
				break
			}
		}

		events = append(events, broker.RawEvent{
			EventID:           eventID,
			SourceKey:         transcriptRef,
			Provider:          agentclaude.ProviderName,
			SourcePosition:    int64(lineNum),
			Timestamp:         fields.Ts,
			Kind:              fields.Kind,
			Role:              fields.Role,
			ToolUsesJSON:      toolUsesStr,
			Summary:           fields.Summary,
			PayloadHash:       payloadHash,
			TokensIn:          fields.TokensIn,
			TokensOut:         fields.TokensOut,
			TokensCacheRead:   fields.TokensCacheRead,
			TokensCacheCreate: fields.TokensCacheCreate,
			ProviderEventID:   fields.ProviderEventID,
			FilePaths:         filePaths,
			ProviderSessionID: providerSessionID,
			ParentSessionID:   parentSessionID,
			SessionStartedAt:  fields.Ts,
			SessionMetaJSON:   string(metaJSON),
			SourceProjectPath: projectPath,
			Model:             model,
			ToolUseID:         firstToolUseID,
			ToolName:          firstToolName,
			EventSource:       "transcript",
		})

		lineNum++
	}

	return events, lineNum, scanner.Err()
}

func shouldStoreTranscriptPayload(fields agentclaude.ExtractedFields) bool {
	if fields.IsFileReadResult {
		return false
	}
	return !agentclaude.HasOnlyReadToolUses(fields.ToolUses)
}

const stopHookSentinel = "semantica capture claude-code stop"

// PrepareTranscript implements hooks.TranscriptPreparer.
// Claude Code writes transcripts asynchronously. On Stop, wait for the
// stop-hook sentinel corresponding to this invocation; on other events, fall
// back to the generic hook_progress sentinel.
func (p *Provider) PrepareTranscript(ctx context.Context, transcriptRef string) error {
	const (
		pollInterval = 50 * time.Millisecond
		maxWait      = 3 * time.Second
		staleThresh  = 2 * time.Minute
		tailBytes    = 4096
		maxSkew      = 2 * time.Second
	)

	// Check if file is stale - if unmodified for > 2min, skip waiting.
	info, err := os.Stat(transcriptRef)
	if err != nil {
		return nil // File doesn't exist - nothing to prepare.
	}
	if time.Since(info.ModTime()) > staleThresh {
		return nil // Stale file - skip.
	}

	deadline := time.Now().Add(maxWait)
	eventType, hasEventType := hooks.HookEventTypeFromContext(ctx)
	hookTS := hooks.HookTimestampFromContext(ctx)
	var hookStart time.Time
	if hookTS > 0 {
		hookStart = time.UnixMilli(hookTS)
	}

	for time.Now().Before(deadline) {
		if hasEventType && eventType == hooks.AgentCompleted && !hookStart.IsZero() {
			if checkStopSentinel(transcriptRef, tailBytes, hookStart, maxSkew) {
				return nil
			}
		} else if hasHookProgress(transcriptRef) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil // Fail open.
		case <-time.After(pollInterval):
		}
	}

	return nil // Timeout - fail open.
}

// hasHookProgress checks if the transcript tail contains a hook_progress sentinel.
func hasHookProgress(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	// Read last 4KB - the sentinel should be near the end.
	info, err := f.Stat()
	if err != nil {
		return false
	}
	size := info.Size()
	readFrom := size - 4096
	if readFrom < 0 {
		readFrom = 0
	}
	if _, err := f.Seek(readFrom, io.SeekStart); err != nil {
		return false
	}
	tail, err := io.ReadAll(f)
	if err != nil {
		return false
	}
	return strings.Contains(string(tail), "hook_progress")
}

func checkStopSentinel(path string, tailBytes int64, hookStart time.Time, maxSkew time.Duration) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return false
	}
	offset := info.Size() - tailBytes
	if offset < 0 {
		offset = 0
	}
	buf := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(buf, offset); err != nil {
		return false
	}

	lowerBound := hookStart.Add(-maxSkew)
	upperBound := hookStart.Add(maxSkew)

	for _, line := range strings.Split(string(buf), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, stopHookSentinel) {
			continue
		}

		var entry struct {
			Timestamp string `json:"timestamp"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil || entry.Timestamp == "" {
			continue
		}

		ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
		if err != nil {
			ts, err = time.Parse(time.RFC3339, entry.Timestamp)
			if err != nil {
				continue
			}
		}
		if ts.After(lowerBound) && ts.Before(upperBound) {
			return true
		}
	}

	return false
}
