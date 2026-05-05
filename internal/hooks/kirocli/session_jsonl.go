package kirocli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// kiroSessionToolCall is one tool call extracted from a Kiro CLI session
// JSONL file. Unsupported tool kinds are skipped during parsing, so callers
// can dispatch on Name directly. Line is the 1-based source position of the
// originating AssistantMessage entry.
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

// readKiroSessionJSONL parses a Kiro CLI session JSONL file and returns
// accepted tool calls plus a safe resume offset.
//
// The offset is the highest line number whose JSON parsed cleanly (or was
// empty). Trailing partial lines are excluded so a still-flushing line
// completes on a later pass without losing the call it describes.
//
// ToolResults entries are matched to AssistantMessage tool uses by toolUseId.
// Orphan ToolResults are ignored.
func readKiroSessionJSONL(path string) ([]kiroSessionToolCall, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("open kiro session jsonl: %w", err)
	}
	defer func() { _ = f.Close() }()

	return parseKiroSessionJSONL(f)
}

// parseKiroSessionJSONL is the io.Reader-based core of the reader. Split out
// so tests can drive it from in-memory buffers.
func parseKiroSessionJSONL(r io.Reader) ([]kiroSessionToolCall, int, error) {
	scanner := bufio.NewScanner(r)
	// Subagent stages can write large content blocks (file creates with
	// hundreds of lines). 8 MiB matches the ceiling used by the Gemini
	// transcript reader.
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)

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
			// Skip without advancing the safe offset: a trailing
			// partial line will become valid on a later pass.
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
