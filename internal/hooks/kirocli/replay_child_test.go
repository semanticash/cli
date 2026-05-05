package kirocli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeKiroChildJSONL builds a minimal child session pair on disk:
// the .jsonl with the requested per-line records and a .json header
// stamped with the cwd. Returns the .jsonl path.
func writeKiroChildJSONL(t *testing.T, dir, sessionID, cwd string, lines ...string) string {
	t.Helper()
	jsonlPath := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(jsonlPath, []byte(kiroSessionLines(lines...)), 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}

	header := `{"session_id":"` + sessionID + `","cwd":"` + cwd + `"}`
	jsonPath := filepath.Join(dir, sessionID+".json")
	if err := os.WriteFile(jsonPath, []byte(header), 0o644); err != nil {
		t.Fatalf("write json header: %v", err)
	}
	return jsonlPath
}

func TestReadFromOffset_JSONLRef_EmitsWriteEvent(t *testing.T) {
	dir := t.TempDir()
	path := writeKiroChildJSONL(t, dir, "child-uuid-1", "/repo",
		kiroLineWriteAssistant,
		kiroLineWriteResult,
	)

	p := &Provider{}
	events, newOffset, err := p.ReadFromOffset(context.Background(), path, 0, nil)
	if err != nil {
		t.Fatalf("ReadFromOffset: %v", err)
	}
	if newOffset != 2 {
		t.Errorf("newOffset = %d, want 2 (two JSONL lines)", newOffset)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.ToolName != "Write" {
		t.Errorf("ToolName = %q, want Write", ev.ToolName)
	}
	if ev.EventSource != "transcript" {
		t.Errorf("EventSource = %q, want transcript", ev.EventSource)
	}
	if ev.ProviderSessionID != "child-uuid-1" {
		t.Errorf("ProviderSessionID = %q, want child-uuid-1", ev.ProviderSessionID)
	}
	if ev.ToolUseID != "tooluse_write_1" {
		t.Errorf("ToolUseID = %q, want tooluse_write_1 (Kiro's own id)", ev.ToolUseID)
	}
	// File path was already absolute in the fixture, preserved.
	if len(ev.FilePaths) != 1 || ev.FilePaths[0] != "/repo/x.md" {
		t.Errorf("FilePaths = %v, want [/repo/x.md]", ev.FilePaths)
	}
}

func TestReadFromOffset_JSONLRef_EmitsBashEvent(t *testing.T) {
	dir := t.TempDir()
	path := writeKiroChildJSONL(t, dir, "child-uuid-2", "/repo",
		kiroLineShellAssistant,
		kiroLineShellResult,
	)

	p := &Provider{}
	events, _, err := p.ReadFromOffset(context.Background(), path, 0, nil)
	if err != nil {
		t.Fatalf("ReadFromOffset: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].ToolName != "Bash" {
		t.Errorf("ToolName = %q, want Bash", events[0].ToolName)
	}
	if events[0].EventSource != "transcript" {
		t.Errorf("EventSource = %q, want transcript", events[0].EventSource)
	}
}

func TestReadFromOffset_JSONLRef_OffsetSkipsAlreadyEmitted(t *testing.T) {
	// First read consumes both calls. Second read from the saved
	// offset produces no events, which protects retry paths from
	// re-emitting the same calls.
	dir := t.TempDir()
	path := writeKiroChildJSONL(t, dir, "child-uuid-3", "/repo",
		kiroLineWriteAssistant,
		kiroLineWriteResult,
		kiroLineShellAssistant,
		kiroLineShellResult,
	)

	p := &Provider{}
	events, off1, err := p.ReadFromOffset(context.Background(), path, 0, nil)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("first read events = %d, want 2", len(events))
	}
	if off1 != 4 {
		t.Errorf("off1 = %d, want 4", off1)
	}

	events2, off2, err := p.ReadFromOffset(context.Background(), path, off1, nil)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if len(events2) != 0 {
		t.Errorf("second read events = %d, want 0 (offset must skip emitted calls)", len(events2))
	}
	if off2 != 4 {
		t.Errorf("off2 = %d, want 4 (offset stays at total line count)", off2)
	}
}

func TestReadFromOffset_JSONLRef_PartialFileEmitsWithoutResponse(t *testing.T) {
	// AssistantMessage on disk but ToolResults not yet flushed. The
	// reader still emits the call so attribution does not stall on
	// a slow flush.
	dir := t.TempDir()
	path := writeKiroChildJSONL(t, dir, "child-uuid-4", "/repo",
		kiroLineWriteAssistant,
	)

	p := &Provider{}
	events, off, err := p.ReadFromOffset(context.Background(), path, 0, nil)
	if err != nil {
		t.Fatalf("ReadFromOffset: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if off != 1 {
		t.Errorf("off = %d, want 1", off)
	}
}

func TestReadFromOffset_JSONLRef_ResolvesRelativePathAgainstChildCWD(t *testing.T) {
	// A relative tool_input path resolves against the child's own cwd
	// from the .json header, not the parent's cwd.
	dir := t.TempDir()
	relativeWrite := `{"version":"v1","kind":"AssistantMessage","data":{"message_id":"msg-rel","content":[{"kind":"toolUse","data":{"toolUseId":"tooluse_rel","name":"write","input":{"command":"create","path":"sub/y.md","content":"hi"}}}]}}`
	path := writeKiroChildJSONL(t, dir, "child-uuid-5", "/child/repo",
		relativeWrite,
	)

	p := &Provider{}
	events, _, err := p.ReadFromOffset(context.Background(), path, 0, nil)
	if err != nil {
		t.Fatalf("ReadFromOffset: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	// resolveKiroFilePath uses filepath.Join, which produces
	// platform-native separators. The test pins "child cwd was
	// used", not a specific slash style, so build the expected
	// value the same way the resolver does.
	want := filepath.Join("/child/repo", "sub/y.md")
	if len(events[0].FilePaths) != 1 || events[0].FilePaths[0] != want {
		t.Errorf("FilePaths = %v, want [%s]", events[0].FilePaths, want)
	}
}

func TestReadFromOffset_SQLiteRef_StaysNoOp(t *testing.T) {
	// The parent path has not changed: SQLite-form refs return zero
	// events. Direct hooks own parent capture; replay would
	// duplicate.
	p := &Provider{}
	events, _, err := p.ReadFromOffset(context.Background(), "/no/such/db.sqlite#conv-x", 0, nil)
	if err != nil {
		t.Fatalf("ReadFromOffset: %v", err)
	}
	if events != nil {
		t.Errorf("events = %v, want nil (parent ref must stay no-op)", events)
	}
}

func TestReadFromOffset_JSONLRef_TrailingPartialSurvivesNextPass(t *testing.T) {
	// First pass: the trailing line is mid-write and fails to parse.
	// The reader returns the events for the complete lines and an
	// offset pointing at the last good line, not past the partial.
	// Second pass: the trailing line has flushed and now parses;
	// its tool call is emitted because the offset did not advance
	// past it.
	dir := t.TempDir()
	path := filepath.Join(dir, "child-uuid-flush.jsonl")
	jsonHeader := `{"session_id":"child-uuid-flush","cwd":"/repo"}`
	if err := os.WriteFile(filepath.Join(dir, "child-uuid-flush.json"), []byte(jsonHeader), 0o644); err != nil {
		t.Fatalf("write header: %v", err)
	}

	// Pass 1 disk state: write+result complete, then a truncated
	// AssistantMessage that mid-flush fails to unmarshal.
	pass1 := kiroLineWriteAssistant + "\n" +
		kiroLineWriteResult + "\n" +
		`{"version":"v1","kind":"AssistantMessage","data":{` + "\n"
	if err := os.WriteFile(path, []byte(pass1), 0o644); err != nil {
		t.Fatalf("write pass1: %v", err)
	}

	p := &Provider{}
	events1, off1, err := p.ReadFromOffset(context.Background(), path, 0, nil)
	if err != nil {
		t.Fatalf("pass1: %v", err)
	}
	if len(events1) != 1 || events1[0].ToolName != "Write" {
		t.Fatalf("pass1 events = %+v, want one Write", events1)
	}
	if off1 != 2 {
		t.Errorf("pass1 offset = %d, want 2 (must not include malformed line 3)", off1)
	}

	// Pass 2 disk state: the partial line completed and now carries
	// a real shell tool call.
	pass2 := kiroLineWriteAssistant + "\n" +
		kiroLineWriteResult + "\n" +
		kiroLineShellAssistant + "\n" +
		kiroLineShellResult + "\n"
	if err := os.WriteFile(path, []byte(pass2), 0o644); err != nil {
		t.Fatalf("write pass2: %v", err)
	}

	events2, off2, err := p.ReadFromOffset(context.Background(), path, off1, nil)
	if err != nil {
		t.Fatalf("pass2: %v", err)
	}
	if len(events2) != 1 || events2[0].ToolName != "Bash" {
		t.Fatalf("pass2 events = %+v, want one Bash (the now-complete line)", events2)
	}
	if off2 != 4 {
		t.Errorf("pass2 offset = %d, want 4", off2)
	}
}

func TestLooksLikeKiroChildJSONLRef(t *testing.T) {
	cases := []struct {
		ref  string
		want bool
	}{
		{"/abs/path/abc-123.jsonl", true},
		{"/path/to/db.sqlite3#conv-abc", false},
		{"", false},
		{"plain.jsonl", true},
		{"weird.json", false},
	}
	for _, tc := range cases {
		if got := looksLikeKiroChildJSONLRef(tc.ref); got != tc.want {
			t.Errorf("looksLikeKiroChildJSONLRef(%q) = %v, want %v", tc.ref, got, tc.want)
		}
	}
}
