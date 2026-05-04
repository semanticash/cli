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

// looksLikeKiroChildJSONLRef reports whether a transcript ref points
// at a subagent JSONL file. Parent refs use the SQLite composite
// form <dbPath>#<conv-id> and never end in .jsonl, so the suffix is
// sufficient to tell the two shapes apart.
func looksLikeKiroChildJSONLRef(ref string) bool {
	return strings.HasSuffix(ref, ".jsonl")
}

// readChildJSONL replays a subagent JSONL transcript starting at
// the given line offset. Each accepted tool call is rendered as a
// RawEvent using the same builders as direct hook capture, then
// marked as transcript-sourced.
//
// The returned offset is the parser's safe resume offset. Filtering
// later reads with call.Line > offset prevents duplicate events while
// still allowing a trailing partial line to complete on the next pass.
//
// File paths are resolved against the child's own cwd (read from the
// .json companion). The ProviderSessionID is the child's .jsonl
// stem, which is the Kiro CLI session UUID. Lifecycle is responsible
// for stamping ParentSessionID and TurnID after this function
// returns; both pieces of information live in the parent's capture
// state rather than in the child transcript.
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

	// Kiro does not stamp tool lines with a per-event timestamp, so
	// the .jsonl mtime is used as a wall-clock proxy. It is monotonic
	// across appends, which is the property the broker actually
	// cares about for ordering replay events within a session.
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
