package kirocli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/agents/api"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
)

// looksLikeKiroChildJSONLRef reports whether a transcript ref points at a
// subagent JSONL file. Parent refs use the SQLite composite form
// <dbPath>#<conv-id> and never end in .jsonl.
func looksLikeKiroChildJSONLRef(ref string) bool {
	return strings.HasSuffix(ref, ".jsonl")
}

// readChildJSONL replays a subagent JSONL transcript starting at the given
// line offset. Calls with Line > offset are emitted; the returned offset is
// the parser's safe resume point so a trailing partial line can complete on
// the next pass without losing the call it carries.
//
// File paths resolve against the child's own cwd from its .json companion.
// ProviderSessionID is the child's Kiro session UUID. The lifecycle stamps
// ParentSessionID and TurnID after this returns.
func readChildJSONL(ctx context.Context, path string, offset int, bs api.BlobPutter) ([]broker.RawEvent, int, error) {
	calls, lineCount, err := readKiroSessionJSONL(path)
	if err != nil {
		return nil, offset, fmt.Errorf("read kiro child jsonl: %w", err)
	}

	var fresh []kiroSessionToolCall
	for _, c := range calls {
		if c.Line > offset {
			fresh = append(fresh, c)
		}
	}
	if len(fresh) == 0 {
		return nil, lineCount, nil
	}

	headerPath := strings.TrimSuffix(path, ".jsonl") + ".json"
	childCWD := ""
	if hdr, err := readKiroSessionHeader(headerPath); err == nil {
		childCWD = hdr.CWD
	}

	// Kiro does not stamp tool lines with a per-event timestamp; the
	// .jsonl mtime is the closest monotonic wall-clock proxy.
	var ts int64
	if info, err := os.Stat(path); err == nil {
		ts = info.ModTime().UnixMilli()
	}

	childUUID := strings.TrimSuffix(filepath.Base(path), ".jsonl")

	var events []broker.RawEvent
	for _, c := range fresh {
		toolName := normalizeKiroToolName(c.Name, c.Input)
		if toolName == "" {
			continue
		}
		synth := &hooks.Event{
			Type:         hooks.ToolStepCompleted,
			SessionID:    childUUID,
			CWD:          childCWD,
			Timestamp:    ts,
			ToolName:     toolName,
			ToolInput:    c.Input,
			ToolResponse: c.Response,
			ToolUseID:    c.ID,
		}
		built, err := buildStepEvent(ctx, synth, bs)
		if err != nil {
			return nil, offset, fmt.Errorf("build child step %s: %w", toolName, err)
		}
		for i := range built {
			built[i].EventSource = "transcript"
		}
		events = append(events, built...)
	}
	return events, lineCount, nil
}
