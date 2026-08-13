package events

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func makePayload(repoRoot, filePath, content string) []byte {
	payload := fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"%s/%s","content":"%s"}}]}}`,
		repoRoot, filePath, strings.ReplaceAll(content, "\n", `\n`))
	return []byte(payload)
}

func TestBuildCandidatesFromRows_ClaudeLineLevel(t *testing.T) {
	repoRoot := "/test/repo"
	rows := []EventRow{
		{
			Provider:    "claude_code",
			Role:        "assistant",
			ToolUses:    `{"content_types":["tool_use"],"tools":[{"name":"Write","file_path":"main.go","file_op":"write"}]}`,
			PayloadHash: "hash1",
			Payload:     makePayload(repoRoot, "main.go", "package main\nfunc main() {}\n"),
			Model:       "opus 4.6",
		},
	}

	cands, stats := BuildCandidatesFromRows(rows, repoRoot, nil)

	if stats.EventsConsidered != 1 {
		t.Errorf("EventsConsidered = %d, want 1", stats.EventsConsidered)
	}
	if stats.EventsAssistant != 1 {
		t.Errorf("EventsAssistant = %d, want 1", stats.EventsAssistant)
	}
	if stats.PayloadsLoaded != 1 {
		t.Errorf("PayloadsLoaded = %d, want 1", stats.PayloadsLoaded)
	}
	if stats.AIToolEvents != 1 {
		t.Errorf("AIToolEvents = %d, want 1", stats.AIToolEvents)
	}

	if len(cands.AILines) != 1 {
		t.Fatalf("AILines files = %d, want 1", len(cands.AILines))
	}
	lines := cands.AILines["main.go"]
	if len(lines) != 2 {
		t.Errorf("main.go lines = %d, want 2", len(lines))
	}
	if _, ok := lines["package main"]; !ok {
		t.Error("missing 'package main'")
	}
	if _, ok := lines["func main() {}"]; !ok {
		t.Error("missing 'func main() {}'")
	}

	if cands.ProviderModel["claude_code"] != "opus 4.6" {
		t.Errorf("ProviderModel = %v", cands.ProviderModel)
	}
	if cands.FileProvider["main.go"] != "claude_code" {
		t.Errorf("FileProvider = %v", cands.FileProvider)
	}
	if cands.ExplicitTouches["main.go"].Provider != "claude_code" {
		t.Errorf("ExplicitTouches = %v, want the Edit/Write editor recorded", cands.ExplicitTouches)
	}
}

// Multiple events that emit one line retain separate witnesses.
func TestBuildCandidatesFromRows_LineStampsAllWitnesses(t *testing.T) {
	repoRoot := "/test/repo"
	toolUses := `{"content_types":["tool_use"],"tools":[{"name":"Write","file_path":"main.go","file_op":"write"}]}`
	row := func(provider, eventID string, ts, seq int64) EventRow {
		return EventRow{
			Provider: provider, Role: "assistant", ToolUses: toolUses,
			PayloadHash: "h", Payload: makePayload(repoRoot, "main.go", "shared line\n"),
			EventID: eventID, Ts: ts, InsertSeq: seq,
		}
	}
	rows := []EventRow{
		row("claude_code", "evt-late", 200, 1),
		row("codex", "evt-early", 100, 9),
	}
	cands, _ := BuildCandidatesFromRows(rows, repoRoot, nil)
	got := cands.LineStamps["main.go"]["shared line"]
	want := []LineStamp{
		{Provider: "claude_code", Ts: 200, InsertSeq: 1, EventID: "evt-late"},
		{Provider: "codex", Ts: 100, InsertSeq: 9, EventID: "evt-early"},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("stamps = %+v, want all witnesses in event order %+v", got, want)
	}
}

// Explicit touches use event recency rather than input order.
func TestBuildCandidatesFromRows_ExplicitTouchLatestWitness(t *testing.T) {
	repoRoot := "/test/repo"
	toolUses := `{"content_types":["tool_use"],"tools":[{"name":"Write","file_path":"main.go","file_op":"write"}]}`
	row := func(provider, eventID string, ts, seq int64) EventRow {
		return EventRow{
			Provider: provider, Role: "assistant", ToolUses: toolUses,
			PayloadHash: "h", Payload: makePayload(repoRoot, "main.go", "x\n"),
			EventID: eventID, Ts: ts, InsertSeq: seq,
		}
	}
	// Later insert_seq arrives first in input order.
	rows := []EventRow{
		row("codex", "evt-b", 100, 9),
		row("claude_code", "evt-a", 100, 1),
	}
	cands, _ := BuildCandidatesFromRows(rows, repoRoot, nil)
	if got := cands.ExplicitTouches["main.go"]; got.Provider != "codex" || got.EventID != "evt-b" {
		t.Fatalf("explicit touch = %+v, want the later insert_seq witness", got)
	}
	// File-level attribution uses the same winner.
	if got := cands.ProviderTouchedFiles["main.go"]; got != "codex" {
		t.Fatalf("ProviderTouchedFiles = %q, want the recency winner codex", got)
	}
}

func TestBuildCandidatesFromRows_ProviderFileTouchOnly(t *testing.T) {
	rows := []EventRow{
		{
			Provider: "cursor",
			Role:     "assistant",
			ToolUses: `{"content_types":["cursor_file_edit"],"tools":[{"name":"cursor_edit","file_path":"handler.go","file_op":"edit"}]}`,
		},
	}

	cands, stats := BuildCandidatesFromRows(rows, "/test/repo", nil)

	if stats.AIToolEvents != 1 {
		t.Errorf("AIToolEvents = %d, want 1", stats.AIToolEvents)
	}
	if cands.ProviderTouchedFiles["handler.go"] != "cursor" {
		t.Errorf("ProviderTouchedFiles = %v", cands.ProviderTouchedFiles)
	}
	if len(cands.AILines) != 0 {
		t.Error("expected no AILines for provider file touch")
	}
}

func TestBuildCandidatesFromRows_EligibleFileGating(t *testing.T) {
	repoRoot := "/test/repo"
	rows := []EventRow{
		{
			Provider:    "claude_code",
			Role:        "assistant",
			ToolUses:    `{"content_types":["tool_use"],"tools":[{"name":"Write","file_path":"main.go","file_op":"write"}]}`,
			PayloadHash: "hash1",
			Payload:     makePayload(repoRoot, "main.go", "package main\n"),
		},
		{
			Provider:    "claude_code",
			Role:        "assistant",
			ToolUses:    `{"content_types":["tool_use"],"tools":[{"name":"Write","file_path":"other.go","file_op":"write"}]}`,
			PayloadHash: "hash2",
			Payload:     makePayload(repoRoot, "other.go", "package other\n"),
		},
	}

	// Only main.go is eligible.
	eligible := map[string]bool{"main.go": true}
	cands, _ := BuildCandidatesFromRows(rows, repoRoot, eligible)

	if len(cands.AILines) != 1 {
		t.Fatalf("AILines files = %d, want 1", len(cands.AILines))
	}
	if _, ok := cands.AILines["main.go"]; !ok {
		t.Error("expected main.go in AILines")
	}
	if _, ok := cands.AILines["other.go"]; ok {
		t.Error("other.go should be filtered by eligible gate")
	}
}

func TestBuildCandidatesFromRows_DeletionPath(t *testing.T) {
	repoRoot := "/test/repo"
	payload, _ := json.Marshal(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{
					"type":  "tool_use",
					"name":  "Bash",
					"input": map[string]any{"command": "rm " + repoRoot + "/old.go"},
				},
			},
		},
	})

	rows := []EventRow{
		{
			Provider:    "claude_code",
			Role:        "assistant",
			ToolUses:    `{"content_types":["tool_use"],"tools":[{"name":"Bash"}]}`,
			PayloadHash: "hash1",
			Payload:     payload,
		},
	}

	cands, _ := BuildCandidatesFromRows(rows, repoRoot, nil)

	if cands.ProviderTouchedFiles["old.go"] != "claude_code" {
		t.Errorf("expected old.go in ProviderTouchedFiles, got %v", cands.ProviderTouchedFiles)
	}
}

func TestBuildCandidatesFromRows_NilPayloadSkipped(t *testing.T) {
	rows := []EventRow{
		{
			Provider:    "claude_code",
			Role:        "assistant",
			ToolUses:    `{"content_types":["tool_use"],"tools":[{"name":"Write","file_path":"main.go"}]}`,
			PayloadHash: "hash1",
			Payload:     nil, // not loaded
		},
	}

	_, stats := BuildCandidatesFromRows(rows, "/test/repo", nil)

	if stats.AIToolEvents != 1 {
		t.Errorf("AIToolEvents = %d, want 1", stats.AIToolEvents)
	}
	if stats.PayloadsLoaded != 0 {
		t.Errorf("PayloadsLoaded = %d, want 0", stats.PayloadsLoaded)
	}
}

func TestBuildCandidatesFromRows_NonAssistantSkipped(t *testing.T) {
	rows := []EventRow{
		{Provider: "claude_code", Role: "user", ToolUses: `{}`},
	}

	_, stats := BuildCandidatesFromRows(rows, "/test/repo", nil)

	if stats.EventsAssistant != 0 {
		t.Errorf("EventsAssistant = %d, want 0", stats.EventsAssistant)
	}
}

// Multiple providers retain ownership of their respective lines.
func TestBuildCandidatesFromRows_LineProvidersMultiProviderSameFile(t *testing.T) {
	repoRoot := "/test/repo"
	rows := []EventRow{
		{
			Provider:    "claude_code",
			Role:        "assistant",
			ToolUses:    `{"content_types":["tool_use"],"tools":[{"name":"Write","file_path":"main.go","file_op":"write"}]}`,
			PayloadHash: "hash-claude",
			Payload:     makePayload(repoRoot, "main.go", "package main\nfunc main() {}\n"),
		},
		{
			Provider:    "codex",
			Role:        "assistant",
			ToolUses:    `{"content_types":["tool_use"],"tools":[{"name":"Write","file_path":"main.go","file_op":"write"}]}`,
			PayloadHash: "hash-codex",
			Payload:     makePayload(repoRoot, "main.go", "// added by codex\n"),
		},
	}

	cands, _ := BuildCandidatesFromRows(rows, repoRoot, nil)

	// AILines contains lines from both providers.
	if len(cands.AILines["main.go"]) != 3 {
		t.Errorf("AILines[main.go] = %d, want 3 (union of both providers)", len(cands.AILines["main.go"]))
	}
	if cands.FileProvider["main.go"] != "codex" {
		t.Errorf("FileProvider[main.go] = %q, want %q (last-writer-wins by design)",
			cands.FileProvider["main.go"], "codex")
	}

	// Each line retains its provider.
	perLine, ok := cands.LineProviders["main.go"]
	if !ok {
		t.Fatalf("LineProviders missing main.go entry; got %v", cands.LineProviders)
	}
	for _, line := range []string{"package main", "func main() {}"} {
		provs := perLine[line]
		if _, ok := provs["claude_code"]; !ok {
			t.Errorf("LineProviders[main.go][%q] missing claude_code; got %v", line, provs)
		}
		if _, ok := provs["codex"]; ok {
			t.Errorf("LineProviders[main.go][%q] should not include codex; got %v", line, provs)
		}
	}
	codexLine := perLine["// added by codex"]
	if _, ok := codexLine["codex"]; !ok {
		t.Errorf("LineProviders[main.go][// added by codex] missing codex; got %v", codexLine)
	}
	if _, ok := codexLine["claude_code"]; ok {
		t.Errorf("LineProviders[main.go][// added by codex] should not include claude_code; got %v", codexLine)
	}
}
