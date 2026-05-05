package kiroide

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/agents/api"
	agentKiro "github.com/semanticash/cli/internal/agents/kiro"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
)

var providerName = agentKiro.ProviderNameIDE

// Provider implements hooks.HookProvider for Kiro IDE.
type Provider struct {
	// resolveSession overrides session lookup in tests.
	resolveSession func(workspacePath string) (sessionID, historyPath string, err error)
}

func init() {
	hooks.RegisterProvider(&Provider{})
}

func (p *Provider) Name() string        { return providerName }
func (p *Provider) DisplayName() string { return "Kiro IDE" }

func (p *Provider) IsAvailable() bool {
	_, err := agentKiro.KiroGlobalStorageDir()
	return err == nil
}

const semanticaMarker = "semantica capture kiro-ide"

type kiroHook struct {
	ID          string       `json:"id"`
	Enabled     bool         `json:"enabled"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Version     string       `json:"version"`
	When        kiroHookWhen `json:"when"`
	Then        kiroHookThen `json:"then"`
}

type kiroHookWhen struct {
	Type string `json:"type"`
}

type kiroHookThen struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

func (p *Provider) InstallHooks(ctx context.Context, repoRoot string, binaryPath string) (int, error) {
	hooksDir := filepath.Join(repoRoot, ".kiro", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return 0, fmt.Errorf("create .kiro/hooks: %w", err)
	}

	bin := binaryPath
	if bin == "" {
		bin = "semantica"
	}

	hookDefs := []kiroHook{
		{
			ID:          "semantica-prompt-submit",
			Enabled:     true,
			Name:        "Semantica Capture Prompt",
			Description: "Pin the Kiro session when the user submits a prompt",
			Version:     "1",
			When:        kiroHookWhen{Type: "promptSubmit"},
			Then:        kiroHookThen{Type: "runCommand", Command: hooks.GuardedCommand(bin, "capture kiro-ide prompt-submit"), Timeout: 10},
		},
		{
			ID:          "semantica-agent-stop",
			Enabled:     true,
			Name:        "Semantica Capture Stop",
			Description: "Capture Kiro execution after agent completes",
			Version:     "1",
			When:        kiroHookWhen{Type: "agentStop"},
			Then:        kiroHookThen{Type: "runCommand", Command: hooks.GuardedCommand(bin, "capture kiro-ide stop"), Timeout: 60},
		},
	}

	count := 0
	for _, def := range hookDefs {
		hookPath := filepath.Join(hooksDir, def.ID+".kiro.hook")

		if data, err := os.ReadFile(hookPath); err == nil {
			if strings.Contains(string(data), semanticaMarker) {
				count++
				continue
			}
		}

		data, err := hooks.MarshalSettingsJSON(def)
		if err != nil {
			return 0, fmt.Errorf("marshal hook %s: %w", def.ID, err)
		}
		if err := os.WriteFile(hookPath, data, 0o644); err != nil {
			return 0, fmt.Errorf("write hook %s: %w", def.ID, err)
		}
		count++
	}

	return count, nil
}

func (p *Provider) UninstallHooks(ctx context.Context, repoRoot string) error {
	hooksDir := filepath.Join(repoRoot, ".kiro", "hooks")
	entries, err := os.ReadDir(hooksDir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".kiro.hook") {
			continue
		}
		path := filepath.Join(hooksDir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), semanticaMarker) {
			_ = os.Remove(path)
		}
	}
	return nil
}

func (p *Provider) AreHooksInstalled(ctx context.Context, repoRoot string) bool {
	hooksDir := filepath.Join(repoRoot, ".kiro", "hooks")
	entries, err := os.ReadDir(hooksDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".kiro.hook") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(hooksDir, e.Name()))
		if err != nil {
			continue
		}
		if strings.Contains(string(data), semanticaMarker) {
			return true
		}
	}
	return false
}

func (p *Provider) HookBinary(ctx context.Context, repoRoot string) (string, error) {
	hooksDir := filepath.Join(repoRoot, ".kiro", "hooks")
	entries, err := os.ReadDir(hooksDir)
	if err != nil {
		return "", fmt.Errorf("read hooks dir: %w", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".kiro.hook") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(hooksDir, e.Name()))
		if err != nil {
			continue
		}
		var h kiroHook
		if json.Unmarshal(data, &h) != nil {
			continue
		}
		if strings.Contains(h.Then.Command, semanticaMarker) {
			if bin := hooks.ExtractBinary(h.Then.Command); bin != "" {
				return bin, nil
			}
		}
	}
	return "", fmt.Errorf("no semantica hooks found in .kiro/hooks")
}

func workspaceKey(absPath string) string {
	return agentKiro.WorkspaceKey("kiroide", absPath)
}

// ParseHookEvent resolves the active Kiro session for the current workspace.
// Prompt hooks pin the session history reference for the matching stop hook.
func (p *Provider) ParseHookEvent(ctx context.Context, hookName string, stdin io.Reader) (*hooks.Event, error) {
	var eventType hooks.EventType
	switch hookName {
	case "prompt-submit":
		eventType = hooks.PromptSubmitted
	case "stop":
		eventType = hooks.AgentCompleted
	default:
		return nil, fmt.Errorf("unknown kiro-ide hook: %s", hookName)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolve cwd: %w", err)
	}

	wsKey := workspaceKey(cwd)

	// Reuse the pinned session history reference when it is available.
	if eventType == hooks.AgentCompleted {
		if state, err := hooks.LoadCaptureStateByKey(wsKey); err == nil {
			return &hooks.Event{
				Type:          eventType,
				SessionID:     wsKey,
				TranscriptRef: state.TranscriptRef,
				Timestamp:     time.Now().UnixMilli(),
				CWD:           cwd,
			}, nil
		}
	}

	resolve := p.resolveSession
	if resolve == nil {
		resolve = agentKiro.ResolveLatestSession
	}
	_, historyPath, err := resolve(cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve kiro session: %w", err)
	}

	return &hooks.Event{
		Type:          eventType,
		SessionID:     wsKey,
		TranscriptRef: historyPath,
		Timestamp:     time.Now().UnixMilli(),
		CWD:           cwd,
	}, nil
}

// DeriveProviderSessionID resolves the actual session UUID from the transcript
// ref (session history JSON). The workspace-keyed session ID used in capture
// state doesn't match the provider_session_id stored in the DB by ReadFromOffset.
func (p *Provider) DeriveProviderSessionID(transcriptRef string) string {
	history, err := agentKiro.ParseSessionHistory(transcriptRef)
	if err != nil {
		return ""
	}
	return history.SessionID
}

// TranscriptOffset returns the current execution count from the session
// history. The lifecycle stores this value, while ReadFromOffset discovers
// file operations from the execution trace store.
func (p *Provider) TranscriptOffset(ctx context.Context, transcriptRef string) (int, error) {
	history, err := agentKiro.ParseSessionHistory(transcriptRef)
	if err != nil {
		return 0, err
	}
	return agentKiro.CountExecutionEntries(history), nil
}

// ReadFromOffset scans the execution trace store and emits file-modifying
// actions for the current Kiro session. The lifecycle offset is accepted for
// interface compatibility; duplicate writes are avoided by stable event IDs.
func (p *Provider) ReadFromOffset(ctx context.Context, transcriptRef string, offset int, bs api.BlobPutter) ([]broker.RawEvent, int, error) {
	history, err := agentKiro.ParseSessionHistory(transcriptRef)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, offset, nil
		}
		return nil, offset, err
	}

	globalDir, err := agentKiro.KiroGlobalStorageDir()
	if err != nil {
		// Without Kiro global storage, there are no traces to read. Return the
		// current history count so lifecycle state can still advance.
		return nil, agentKiro.CountExecutionEntries(history), nil
	}

	workspaceDir := history.WorkspaceDirectory
	if workspaceDir == "" {
		workspaceDir, _ = os.Getwd()
	}

	// Scan all known executions and filter them by chat session after loading
	// each trace.
	allExecs := scanExecutionIndex(globalDir)
	if len(allExecs) == 0 {
		return nil, agentKiro.CountExecutionEntries(history), nil
	}

	var events []broker.RawEvent

	for _, exec := range allExecs {
		trace := findExecutionTrace(globalDir, exec.ExecutionID)
		if trace == nil {
			continue
		}

		// Keep only traces that belong to the current chat session.
		if trace.ChatSessionID != "" && trace.ChatSessionID != history.SessionID {
			continue
		}

		sessionStartedAtTs := sessionStartedAt(transcriptRef, history.SessionID)
		ops := agentKiro.ExtractFileOps(trace)
		for _, op := range ops {
			ev, ok := buildEventForOp(ctx, op, exec.ExecutionID, trace.StartTime, history.SessionID, sessionStartedAtTs, transcriptRef, workspaceDir, bs)
			if ok {
				events = append(events, ev)
			}
		}

	}

	newOffset := agentKiro.CountExecutionEntries(history)
	return events, newOffset, nil
}

// --- Execution index scanning ---

type execMeta struct {
	ExecutionID string
	StartTime   int64
}

// scanExecutionIndex returns execution metadata from Kiro index files in a
// stable order. Session filtering happens after the matching trace is loaded.
func scanExecutionIndex(globalDir string) []execMeta {
	var all []execMeta
	entries, err := os.ReadDir(globalDir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "workspace-sessions" || e.Name() == "dev_data" ||
			e.Name() == "index" || e.Name() == "default" || e.Name() == ".migrations" {
			continue
		}
		hashDir := filepath.Join(globalDir, e.Name())
		// Execution index files live alongside the trace directories.
		subEntries, err := os.ReadDir(hashDir)
		if err != nil {
			continue
		}
		for _, sub := range subEntries {
			if sub.IsDir() {
				continue
			}
			indexPath := filepath.Join(hashDir, sub.Name())
			var idx agentKiro.ExecutionIndex
			data, err := os.ReadFile(indexPath)
			if err != nil {
				continue
			}
			if json.Unmarshal(data, &idx) != nil || len(idx.Executions) == 0 {
				continue
			}
			for _, ex := range idx.Executions {
				all = append(all, execMeta{
					ExecutionID: ex.ExecutionID,
					StartTime:   ex.StartTime,
				})
			}
		}
	}

	// Order by start time and use the execution ID to break ties.
	sort.Slice(all, func(i, j int) bool {
		if all[i].StartTime != all[j].StartTime {
			return all[i].StartTime < all[j].StartTime
		}
		return all[i].ExecutionID < all[j].ExecutionID
	})

	return all
}

// execsAfterID returns the executions that follow lastID in the ordered list.
// If lastID is missing, it returns the full set.
func execsAfterID(execs []execMeta, lastID string) []execMeta {
	if lastID == "" {
		return execs
	}
	for i, ex := range execs {
		if ex.ExecutionID == lastID {
			return execs[i+1:]
		}
	}
	// If the saved ID is no longer present, process the full set.
	return execs
}

// findExecutionTrace scans the globalStorage hash directories for a trace
// matching the given execution ID.
func findExecutionTrace(globalDir, executionID string) *agentKiro.ExecutionTrace {
	entries, err := os.ReadDir(globalDir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "workspace-sessions" || e.Name() == "dev_data" ||
			e.Name() == "index" || e.Name() == "default" || e.Name() == ".migrations" {
			continue
		}
		hashDir := filepath.Join(globalDir, e.Name())
		subEntries, err := os.ReadDir(hashDir)
		if err != nil {
			continue
		}
		for _, sub := range subEntries {
			if !sub.IsDir() {
				continue
			}
			traceDir := filepath.Join(hashDir, sub.Name())
			traceFiles, err := os.ReadDir(traceDir)
			if err != nil {
				continue
			}
			for _, tf := range traceFiles {
				if tf.IsDir() {
					continue
				}
				tracePath := filepath.Join(traceDir, tf.Name())
				trace, err := agentKiro.ParseExecutionTrace(tracePath)
				if err != nil {
					continue
				}
				if trace.ExecutionID == executionID {
					return trace
				}
			}
		}
	}
	return nil
}

// actionTimestamp uses the per-action timestamp when it is present and falls
// back to the execution start time otherwise.
func actionTimestamp(emittedAt, execStartTime int64) int64 {
	if emittedAt > 0 {
		return emittedAt
	}
	return execStartTime
}

// buildEventForOp converts one Kiro file operation into a RawEvent.
// Unknown action types return ok=false so callers do not emit partial rows.
//
// Tool names choose the attribution path:
//   - create -> Write, with full content payload
//   - replace/append -> Edit, with old/new payload
//   - smartRelocate -> kiro_file_edit, file-touch only
func buildEventForOp(
	ctx context.Context,
	op agentKiro.FileOperation,
	executionID string,
	traceStart int64,
	sessionID string,
	sessionStartedAtTs int64,
	transcriptRef string,
	workspaceDir string,
	bs api.BlobPutter,
) (broker.RawEvent, bool) {
	event := broker.RawEvent{
		Provider:          providerName,
		SourceKey:         transcriptRef,
		Kind:              "assistant",
		Role:              "assistant",
		Timestamp:         actionTimestamp(op.EmittedAt, traceStart),
		ProviderSessionID: sessionID,
		SessionStartedAt:  sessionStartedAtTs,
		SourceProjectPath: workspaceDir,
		EventSource:       "transcript",
	}

	switch op.ActionType {
	case "create":
		event.EventID = hashEventID(executionID, op.ActionType, op.FilePath, op.ActionID)
		event.ToolUsesJSON = agentKiro.BuildToolUsesJSON(agentKiro.ToolNameWrite, op.FilePath, "write").String
		event.ToolName = agentKiro.ToolNameWrite
		event.Summary = fmt.Sprintf("create %s", op.FilePath)

		if op.FilePath != "" {
			event.FilePaths = []string{filepath.Join(workspaceDir, op.FilePath)}
		}

		if bs != nil && op.Content != "" {
			provBlob, _ := json.Marshal(map[string]any{
				"action":    op.ActionType,
				"file_path": op.FilePath,
				"content":   op.Content,
			})
			if h, _, err := bs.Put(ctx, provBlob); err == nil {
				event.ProvenanceHash = h
			}
		}

		if bs != nil && op.FilePath != "" {
			inputJSON, _ := json.Marshal(map[string]any{
				"file_path": op.FilePath,
				"content":   op.Content,
			})
			payloadBlob, _ := json.Marshal(map[string]any{
				"type": "assistant",
				"message": map[string]any{
					"content": []map[string]any{
						{"type": "tool_use", "name": agentKiro.ToolNameWrite, "input": json.RawMessage(inputJSON)},
					},
				},
			})
			if h, _, err := bs.Put(ctx, payloadBlob); err == nil {
				event.PayloadHash = h
			}
		}

	case "replace", "append":
		event.EventID = hashEventID(executionID, op.ActionType, op.FilePath, op.ActionID)
		event.ToolUsesJSON = agentKiro.BuildToolUsesJSON(agentKiro.ToolNameEdit, op.FilePath, "edit").String
		event.ToolName = agentKiro.ToolNameEdit
		event.Summary = fmt.Sprintf("%s %s", op.ActionType, op.FilePath)

		if op.FilePath != "" {
			event.FilePaths = []string{filepath.Join(workspaceDir, op.FilePath)}
		}

		if bs != nil {
			provBlob, _ := json.Marshal(map[string]any{
				"action":           op.ActionType,
				"file_path":        op.FilePath,
				"original_content": op.OriginalContent,
				"modified_content": op.Content,
			})
			if h, _, err := bs.Put(ctx, provBlob); err == nil {
				event.ProvenanceHash = h
			}
		}

		if bs != nil && op.FilePath != "" {
			inputJSON, _ := json.Marshal(map[string]any{
				"file_path":  op.FilePath,
				"old_string": op.OriginalContent,
				"new_string": op.Content,
			})
			payloadBlob, _ := json.Marshal(map[string]any{
				"type": "assistant",
				"message": map[string]any{
					"content": []map[string]any{
						{"type": "tool_use", "name": agentKiro.ToolNameEdit, "input": json.RawMessage(inputJSON)},
					},
				},
			})
			if h, _, err := bs.Put(ctx, payloadBlob); err == nil {
				event.PayloadHash = h
			}
		}

	case "smartRelocate":
		event.EventID = hashEventID(executionID, op.ActionType, op.SourcePath, op.ActionID)
		event.ToolUsesJSON = agentKiro.BuildToolUsesJSON(agentKiro.ToolNameFileEdit, op.DestPath, "rename").String
		event.ToolName = agentKiro.ToolNameWrite
		event.Summary = fmt.Sprintf("rename %s -> %s", op.SourcePath, op.DestPath)

		if op.DestPath != "" {
			event.FilePaths = []string{filepath.Join(workspaceDir, op.DestPath)}
		}

	default:
		return broker.RawEvent{}, false
	}

	return event, true
}

// hashEventID builds a stable replay event ID. ActionID keeps repeated
// same-file edits in one execution distinct; empty ActionID preserves the
// deterministic shape for older traces.
func hashEventID(executionID, actionType, filePath, actionID string) string {
	h := sha256.New()
	h.Write([]byte(executionID))
	h.Write([]byte(actionType))
	h.Write([]byte(filePath))
	h.Write([]byte(actionID))
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// sessionStartedAt reads sessions.json to find the creation time for a session.
func sessionStartedAt(transcriptRef string, sessionID string) int64 {
	sessDir := filepath.Dir(transcriptRef)
	indexPath := filepath.Join(sessDir, "sessions.json")
	sessions, err := agentKiro.ParseSessionIndex(indexPath)
	if err != nil {
		return 0
	}
	for _, s := range sessions {
		if s.SessionID == sessionID {
			ts, _ := strconv.ParseInt(s.DateCreated, 10, 64)
			return ts
		}
	}
	return 0
}
