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

	if stats.EventsConsidered != 1 { t.Errorf("EventsConsidered = %d, want 1", stats.EventsConsidered) }
	if stats.EventsAssistant != 1 { t.Errorf("EventsAssistant = %d, want 1", stats.EventsAssistant) }
	if stats.PayloadsLoaded != 1 { t.Errorf("PayloadsLoaded = %d, want 1", stats.PayloadsLoaded) }
	if stats.AIToolEvents != 1 { t.Errorf("AIToolEvents = %d, want 1", stats.AIToolEvents) }

	if len(cands.AILines) != 1 { t.Fatalf("AILines files = %d, want 1", len(cands.AILines)) }
	lines := cands.AILines["main.go"]
	if len(lines) != 2 { t.Errorf("main.go lines = %d, want 2", len(lines)) }
	if _, ok := lines["package main"]; !ok { t.Error("missing 'package main'") }
	if _, ok := lines["func main() {}"]; !ok { t.Error("missing 'func main() {}'") }

	if cands.ProviderModel["claude_code"] != "opus 4.6" {
		t.Errorf("ProviderModel = %v", cands.ProviderModel)
	}
	if cands.FileProvider["main.go"] != "claude_code" {
		t.Errorf("FileProvider = %v", cands.FileProvider)
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

	if stats.AIToolEvents != 1 { t.Errorf("AIToolEvents = %d, want 1", stats.AIToolEvents) }
	if cands.ProviderTouchedFiles["handler.go"] != "cursor" {
		t.Errorf("ProviderTouchedFiles = %v", cands.ProviderTouchedFiles)
	}
	if len(cands.AILines) != 0 { t.Error("expected no AILines for provider file touch") }
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

	if len(cands.AILines) != 1 { t.Fatalf("AILines files = %d, want 1", len(cands.AILines)) }
	if _, ok := cands.AILines["main.go"]; !ok { t.Error("expected main.go in AILines") }
	if _, ok := cands.AILines["other.go"]; ok { t.Error("other.go should be filtered by eligible gate") }
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

	if stats.AIToolEvents != 1 { t.Errorf("AIToolEvents = %d, want 1", stats.AIToolEvents) }
	if stats.PayloadsLoaded != 0 { t.Errorf("PayloadsLoaded = %d, want 0", stats.PayloadsLoaded) }
}

func TestBuildCandidatesFromRows_NonAssistantSkipped(t *testing.T) {
	rows := []EventRow{
		{Provider: "claude_code", Role: "user", ToolUses: `{}`},
	}

	_, stats := BuildCandidatesFromRows(rows, "/test/repo", nil)

	if stats.EventsAssistant != 0 { t.Errorf("EventsAssistant = %d, want 0", stats.EventsAssistant) }
}
