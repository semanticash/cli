package kirocli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// kiroSessionToolCall is one tool call extracted from a Kiro CLI
// session JSONL file. Unsupported tool kinds are skipped during
// parsing, so callers can dispatch on Name directly.
//
// Line is the 1-based AssistantMessage line that originated the
// call. Callers use it for resume watermarks and event ordering.
type kiroSessionToolCall struct {
	ID       string
	Name     string
	Input    json.RawMessage
	Response json.RawMessage
	Line     int
}

// kiroSessionLine is the per-line envelope shared by every JSONL
// entry. The Kind field decides how Data is interpreted.
type kiroSessionLine struct {
	Version string          `json:"version"`
	Kind    string          `json:"kind"`
	Data    json.RawMessage `json:"data"`
}

// kiroAssistantMessage carries the assistant turn's content blocks.
// Only blocks with kind=="toolUse" are interesting to us.
type kiroAssistantMessage struct {
	MessageID string                 `json:"message_id"`
	Content   []kiroAssistantContent `json:"content"`
}

type kiroAssistantContent struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type kiroAssistantToolUse struct {
	ToolUseID string          `json:"toolUseId"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

// kiroToolResults carries the tool output for one or more
// preceding toolUse blocks, addressed by toolUseId.
type kiroToolResults struct {
	MessageID string               `json:"message_id"`
	Content   []kiroToolResContent `json:"content"`
}

type kiroToolResContent struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// kiroToolResultBody is the inner shape of a ToolResults content
// entry. Status is preserved verbatim so callers can distinguish
// success from failure if they care.
type kiroToolResultBody struct {
	ToolUseID string          `json:"toolUseId"`
	Content   json.RawMessage `json:"content"`
	Status    string          `json:"status"`
}

// kiroSessionToolNameAccepted is the set of tool names whose input
// the scorer can use. Other names (read, summary, list_directory,
// etc.) are skipped at parse time.
var kiroSessionToolNameAccepted = map[string]bool{
	"write": true,
	"shell": true,
}

// readKiroSessionJSONL parses a Kiro CLI session JSONL file and
// returns accepted tool calls plus a safe resume offset.
//
// The returned offset is the highest line number whose JSON parsed
// successfully (or was empty). Trailing lines that failed to parse
// are intentionally excluded so a still-flushing partial line on one
// pass becomes readable on the next pass without losing the call it
// describes. Malformed lines in the middle of the stream do not
// suppress later lines from advancing the offset, only their own
// position.
//
// The reader matches ToolResults to AssistantMessage tool uses by
// toolUseId. If a ToolResults line appears without a preceding
// accepted tool use, it is ignored.
func readKiroSessionJSONL(path string) ([]kiroSessionToolCall, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("open kiro session jsonl: %w", err)
	}
	defer func() { _ = f.Close() }()

	return parseKiroSessionJSONL(f)
}

// parseKiroSessionJSONL is the io.Reader-based core of the reader.
// Split out from readKiroSessionJSONL so tests can drive it from
// in-memory buffers without touching the filesystem.
func parseKiroSessionJSONL(r io.Reader) ([]kiroSessionToolCall, int, error) {
	scanner := bufio.NewScanner(r)
	// Subagent stages can produce large content blocks. Use the
	// same per-line ceiling as the Gemini transcript reader.
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)

	// Tool uses are recorded by id as they are seen. ToolResults
	// are matched against this map by toolUseId.
	type pending struct {
		index int // position in the calls slice
	}
	pendingByID := map[string]pending{}
	var calls []kiroSessionToolCall

	lineNum := 0
	lastGoodLine := 0
	for scanner.Scan() {
		lineNum++
		raw := scanner.Bytes()
		if len(raw) == 0 {
			lastGoodLine = lineNum
			continue
		}

		var line kiroSessionLine
		if err := json.Unmarshal(raw, &line); err != nil {
			// Skip malformed lines without aborting and without
			// advancing the safe offset. A trailing partial line
			// will become valid on a later pass; advancing past
			// it now would skip the call it carries.
			continue
		}
		lastGoodLine = lineNum

		switch line.Kind {
		case "AssistantMessage":
			var msg kiroAssistantMessage
			if err := json.Unmarshal(line.Data, &msg); err != nil {
				continue
			}
			for _, block := range msg.Content {
				if block.Kind != "toolUse" {
					continue
				}
				var tu kiroAssistantToolUse
				if err := json.Unmarshal(block.Data, &tu); err != nil {
					continue
				}
				if !kiroSessionToolNameAccepted[tu.Name] {
					continue
				}
				if tu.ToolUseID == "" {
					continue
				}
				idx := len(calls)
				calls = append(calls, kiroSessionToolCall{
					ID:    tu.ToolUseID,
					Name:  tu.Name,
					Input: tu.Input,
					Line:  lineNum,
				})
				pendingByID[tu.ToolUseID] = pending{index: idx}
			}

		case "ToolResults":
			var res kiroToolResults
			if err := json.Unmarshal(line.Data, &res); err != nil {
				continue
			}
			for _, block := range res.Content {
				if block.Kind != "toolResult" {
					continue
				}
				var body kiroToolResultBody
				if err := json.Unmarshal(block.Data, &body); err != nil {
					continue
				}
				p, ok := pendingByID[body.ToolUseID]
				if !ok {
					continue
				}
				calls[p.index].Response = block.Data
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, lastGoodLine, fmt.Errorf("scan kiro session jsonl: %w", err)
	}

	return calls, lastGoodLine, nil
}
