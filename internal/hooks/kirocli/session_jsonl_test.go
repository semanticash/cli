package kirocli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// kiroSessionLines composes one JSONL document from a list of
// pre-built per-line records. Tests use this so the fixtures stay
// readable and the reader's tolerance for blank lines is checked
// implicitly.
func kiroSessionLines(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

const (
	// Representative AgentCrew session lines. Kept inline so each
	// test can compose only the records it needs.
	kiroLinePrompt = `{"version":"v1","kind":"Prompt","data":{"message_id":"msg-1","content":[{"kind":"text","data":"Create the file"}],"meta":{"timestamp":1700000000}}}`

	kiroLineWriteAssistant = `{"version":"v1","kind":"AssistantMessage","data":{"message_id":"msg-2","content":[{"kind":"text","data":""},{"kind":"toolUse","data":{"toolUseId":"tooluse_write_1","name":"write","input":{"command":"create","path":"/repo/x.md","content":"hello\n"}}}]}}`

	kiroLineWriteResult = `{"version":"v1","kind":"ToolResults","data":{"message_id":"msg-3","content":[{"kind":"toolResult","data":{"toolUseId":"tooluse_write_1","content":[{"kind":"text","data":"Successfully created /repo/x.md (1 lines)."}],"status":"success"}}]}}`

	kiroLineShellAssistant = `{"version":"v1","kind":"AssistantMessage","data":{"message_id":"msg-4","content":[{"kind":"toolUse","data":{"toolUseId":"tooluse_shell_1","name":"shell","input":{"command":"wc -l /repo/x.md","working_dir":"/repo","__tool_use_purpose":"Get line count"}}}]}}`

	kiroLineShellResult = `{"version":"v1","kind":"ToolResults","data":{"message_id":"msg-5","content":[{"kind":"toolResult","data":{"toolUseId":"tooluse_shell_1","content":[{"kind":"text","data":"       1 /repo/x.md"}],"status":"success"}}]}}`

	// A read tool call is not in the accepted set and is skipped.
	kiroLineReadAssistant = `{"version":"v1","kind":"AssistantMessage","data":{"message_id":"msg-r","content":[{"kind":"toolUse","data":{"toolUseId":"tooluse_read_1","name":"read","input":{"path":"/repo/x.md"}}}]}}`
)

func TestReadKiroSessionJSONL_WriteOnly(t *testing.T) {
	src := kiroSessionLines(
		kiroLinePrompt,
		kiroLineWriteAssistant,
		kiroLineWriteResult,
	)
	calls, lineCount, err := parseKiroSessionJSONL(strings.NewReader(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if lineCount != 3 {
		t.Errorf("lineCount = %d, want 3", lineCount)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	got := calls[0]
	if got.ID != "tooluse_write_1" {
		t.Errorf("ID = %q", got.ID)
	}
	if got.Name != "write" {
		t.Errorf("Name = %q", got.Name)
	}
	// AssistantMessage is the 2nd line in the fixture (after Prompt).
	if got.Line != 2 {
		t.Errorf("Line = %d, want 2", got.Line)
	}

	// Input must be the raw tool_input bytes, parseable back into
	// the canonical fsWriteInput shape.
	var inp fsWriteInput
	if err := json.Unmarshal(got.Input, &inp); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if inp.Command != "create" || inp.Path != "/repo/x.md" || inp.Content != "hello\n" {
		t.Errorf("input fields = %+v", inp)
	}

	// Response was attached from the matching ToolResults entry.
	if len(got.Response) == 0 {
		t.Error("Response should be populated from matched ToolResults")
	}
}

func TestReadKiroSessionJSONL_WriteAndShell(t *testing.T) {
	// Order matters: the reader returns calls in the order their
	// AssistantMessage entries appear, regardless of where their
	// ToolResults land.
	src := kiroSessionLines(
		kiroLineWriteAssistant,
		kiroLineShellAssistant,
		kiroLineWriteResult,
		kiroLineShellResult,
	)
	calls, lineCount, err := parseKiroSessionJSONL(strings.NewReader(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if lineCount != 4 {
		t.Errorf("lineCount = %d, want 4", lineCount)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	if calls[0].Name != "write" || calls[1].Name != "shell" {
		t.Errorf("order = [%q,%q], want [write,shell]", calls[0].Name, calls[1].Name)
	}
	if len(calls[0].Response) == 0 || len(calls[1].Response) == 0 {
		t.Error("both calls should have Response attached from their ToolResults")
	}
	// Line tracks the AssistantMessage source position regardless of
	// where the matching ToolResults landed downstream.
	if calls[0].Line != 1 || calls[1].Line != 2 {
		t.Errorf("Lines = [%d,%d], want [1,2]", calls[0].Line, calls[1].Line)
	}

	// Confirm shell input round-trips back to bashInput cleanly,
	// including the natural-language hint.
	var sh bashInput
	if err := json.Unmarshal(calls[1].Input, &sh); err != nil {
		t.Fatalf("unmarshal shell input: %v", err)
	}
	if sh.Purpose != "Get line count" {
		t.Errorf("Purpose = %q, want 'Get line count'", sh.Purpose)
	}
}

func TestReadKiroSessionJSONL_SkipsUnacceptedToolNames(t *testing.T) {
	// A read tool call is interleaved between two accepted ones.
	// It must be skipped without affecting the order or matching.
	src := kiroSessionLines(
		kiroLineWriteAssistant,
		kiroLineReadAssistant,
		kiroLineWriteResult,
		kiroLineShellAssistant,
		kiroLineShellResult,
	)
	calls, _, err := parseKiroSessionJSONL(strings.NewReader(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2 (read skipped)", len(calls))
	}
	for _, c := range calls {
		if c.Name == "read" {
			t.Errorf("read tool call should not have been returned: %+v", c)
		}
	}
}

func TestReadKiroSessionJSONL_PartialFile(t *testing.T) {
	// Sub-agent is still flushing: the AssistantMessage with the
	// tool use is on disk but the matching ToolResults has not
	// landed yet. The reader returns the call without a Response
	// instead of waiting or erroring.
	src := kiroSessionLines(
		kiroLineWriteAssistant,
	)
	calls, lineCount, err := parseKiroSessionJSONL(strings.NewReader(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if lineCount != 1 {
		t.Errorf("lineCount = %d, want 1", lineCount)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if len(calls[0].Response) != 0 {
		t.Error("Response should be empty when ToolResults has not been written yet")
	}
}

func TestReadKiroSessionJSONL_OrphanToolResultIsDropped(t *testing.T) {
	// Result-only lines are ignored unless they match an accepted
	// AssistantMessage tool use.
	orphan := `{"version":"v1","kind":"ToolResults","data":{"message_id":"orphan","content":[{"kind":"toolResult","data":{"toolUseId":"tooluse_unknown","content":[{"kind":"text","data":"x"}],"status":"success"}}]}}`
	src := kiroSessionLines(orphan)
	calls, _, err := parseKiroSessionJSONL(strings.NewReader(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(calls) != 0 {
		t.Errorf("calls = %d, want 0 (orphan ToolResults must not produce a call)", len(calls))
	}
}

func TestReadKiroSessionJSONL_TrailingMalformedDoesNotAdvanceOffset(t *testing.T) {
	// Kiro is in the middle of writing the third line. The valid
	// lines on disk parse cleanly, but the trailing partial line
	// fails json.Unmarshal. The reader keeps the returned offset at
	// the last complete line so the tool call can be read after the
	// line finishes flushing.
	src := kiroSessionLines(
		kiroLineWriteAssistant,
		kiroLineWriteResult,
		`{"version":"v1","kind":"AssistantMessage","data":{`, // truncated mid-write
	)
	calls, lineCount, err := parseKiroSessionJSONL(strings.NewReader(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if lineCount != 2 {
		t.Errorf("lineCount = %d, want 2 (must not advance past trailing malformed line)", lineCount)
	}
}

func TestReadKiroSessionJSONL_MalformedInMiddleAdvancesPastIt(t *testing.T) {
	// A malformed line followed by valid lines does not keep the
	// offset at the line before the malformed one. The safe-offset
	// rule only affects trailing partial lines.
	src := kiroSessionLines(
		kiroLineWriteAssistant,
		`not valid json`,
		kiroLineWriteResult,
	)
	_, lineCount, err := parseKiroSessionJSONL(strings.NewReader(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if lineCount != 3 {
		t.Errorf("lineCount = %d, want 3 (later valid line must advance offset)", lineCount)
	}
}

func TestReadKiroSessionJSONL_TolerantOfMalformedLines(t *testing.T) {
	// A malformed line in the middle of the stream does not abort
	// the read; the surrounding valid lines parse and the
	// AssistantMessage still gets matched to its ToolResults.
	src := kiroSessionLines(
		kiroLineWriteAssistant,
		`not valid json`,
		kiroLineWriteResult,
	)
	calls, lineCount, err := parseKiroSessionJSONL(strings.NewReader(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if lineCount != 3 {
		t.Errorf("lineCount = %d, want 3 (later valid line advances past malformed)", lineCount)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if len(calls[0].Response) == 0 {
		t.Error("Response should still be matched after the malformed line")
	}
}

func TestReadKiroSessionJSONL_FromFile(t *testing.T) {
	// Round-trip the public file-based reader against a temp file
	// to confirm the open/scan plumbing works end to end.
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	src := kiroSessionLines(
		kiroLineWriteAssistant,
		kiroLineWriteResult,
	)
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	calls, _, err := readKiroSessionJSONL(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(calls) != 1 || calls[0].Name != "write" {
		t.Errorf("calls = %+v", calls)
	}
}

func TestReadKiroSessionJSONL_MissingFile(t *testing.T) {
	_, _, err := readKiroSessionJSONL("/nonexistent/path/session.jsonl")
	if err == nil {
		t.Error("expected error for missing file")
	}
}
