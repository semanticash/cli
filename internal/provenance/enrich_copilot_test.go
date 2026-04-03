package provenance

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCopilotEnricher_CanEnrich(t *testing.T) {
	e := &copilotEnricher{}
	for _, tool := range []string{"bash", "Bash", "edit", "Edit", "Write", "copilot_file_edit"} {
		if !e.CanEnrich("copilot", tool) {
			t.Errorf("should enrich copilot %s", tool)
		}
	}
	for _, tool := range []string{"view", "ask_user", "Read"} {
		if e.CanEnrich("copilot", tool) {
			t.Errorf("should not enrich copilot %s", tool)
		}
	}
	if e.CanEnrich("claude_code", "bash") {
		t.Error("should not enrich claude_code")
	}
}

func TestCopilotEnricher_BashWithCompanion(t *testing.T) {
	bs := testBlobStore(t)

	// Copilot assistant.message with a bash tool request.
	payload := mustJSON(t, map[string]any{
		"type": "assistant.message",
		"data": map[string]any{
			"toolRequests": []map[string]any{
				{
					"toolCallId": "call_bash1",
					"name":       "bash",
					"arguments":  map[string]any{"command": "npm test"},
				},
			},
		},
	})
	payloadHash := putBlob(t, bs, payload)

	// Companion tool.execution_complete with output.
	companion := mustJSON(t, map[string]any{
		"type": "tool.execution_complete",
		"data": map[string]any{
			"toolCallId":       "call_bash1",
			"textResultForLlm": "PASS all tests\n",
		},
	})
	companionHash := putBlob(t, bs, companion)

	e := &copilotEnricher{}
	blob, err := e.Enrich(context.Background(), EnrichInput{
		Provider:    "copilot",
		ToolName:    "bash",
		ToolUseID:   "call_bash1",
		PayloadHash: payloadHash,
		Companions: []CompanionEvidence{
			{PayloadHash: companionHash, Role: "tool", Kind: "tool_result"},
		},
		BlobStore: bs,
	})
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if blob == nil {
		t.Fatal("expected non-nil blob")
	}

	// Verify shape matches storeRedactedBashPayload: tool_input.command + tool_response.textResultForLlm.
	var prov map[string]json.RawMessage
	_ = json.Unmarshal(blob, &prov)

	var input map[string]string
	_ = json.Unmarshal(prov["tool_input"], &input)
	if input["command"] != "npm test" {
		t.Errorf("command = %q", input["command"])
	}

	var resp map[string]string
	_ = json.Unmarshal(prov["tool_response"], &resp)
	if resp["textResultForLlm"] == "" {
		t.Error("expected textResultForLlm from companion")
	}

	// Should NOT have top-level tool_name (Copilot Bash uses simple shape).
	if _, ok := prov["tool_name"]; ok {
		t.Error("Copilot Bash should not have top-level tool_name")
	}
}

func TestCopilotEnricher_BashWithLiveTranscriptCompanion(t *testing.T) {
	bs := testBlobStore(t)

	// assistant.message with bash tool request.
	payload := mustJSON(t, map[string]any{
		"type": "assistant.message",
		"data": map[string]any{
			"toolRequests": []map[string]any{
				{
					"toolCallId": "call_bash_live",
					"name":       "bash",
					"arguments":  map[string]any{"command": "go test ./..."},
				},
			},
		},
	})
	payloadHash := putBlob(t, bs, payload)

	// Live transcript companion uses data.result.content / data.result.detailedContent,
	// NOT data.textResultForLlm. This is the shape produced by real Copilot
	// tool.execution_complete events.
	companion := mustJSON(t, map[string]any{
		"type": "tool.execution_complete",
		"data": map[string]any{
			"toolCallId": "call_bash_live",
			"result": map[string]any{
				"content":         "ok  \texample.com/pkg\t0.4s",
				"detailedContent": "=== RUN   TestFoo\n--- PASS: TestFoo (0.00s)\nPASS\nok  \texample.com/pkg\t0.4s\n",
			},
		},
	})
	companionHash := putBlob(t, bs, companion)

	e := &copilotEnricher{}
	blob, err := e.Enrich(context.Background(), EnrichInput{
		Provider:    "copilot",
		ToolName:    "bash",
		ToolUseID:   "call_bash_live",
		PayloadHash: payloadHash,
		Companions: []CompanionEvidence{
			{PayloadHash: companionHash, Role: "tool", Kind: "tool_result"},
		},
		BlobStore: bs,
	})
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if blob == nil {
		t.Fatal("expected non-nil blob")
	}

	var prov map[string]json.RawMessage
	_ = json.Unmarshal(blob, &prov)

	var input map[string]string
	_ = json.Unmarshal(prov["tool_input"], &input)
	if input["command"] != "go test ./..." {
		t.Errorf("command = %q", input["command"])
	}

	// The enricher should have extracted output from data.result.detailedContent.
	var resp map[string]string
	_ = json.Unmarshal(prov["tool_response"], &resp)
	if resp["textResultForLlm"] == "" {
		t.Error("expected textResultForLlm populated from data.result.detailedContent")
	}
	if !strings.Contains(resp["textResultForLlm"], "PASS") {
		t.Errorf("textResultForLlm should contain test output, got %q", resp["textResultForLlm"])
	}
}

func TestCopilotEnricher_BashNoCompanion(t *testing.T) {
	bs := testBlobStore(t)

	payload := mustJSON(t, map[string]any{
		"type": "assistant.message",
		"data": map[string]any{
			"toolRequests": []map[string]any{
				{
					"toolCallId": "call_bash2",
					"name":       "bash",
					"arguments":  map[string]any{"command": "ls -la"},
				},
			},
		},
	})
	payloadHash := putBlob(t, bs, payload)

	e := &copilotEnricher{}
	blob, err := e.Enrich(context.Background(), EnrichInput{
		Provider:    "copilot",
		ToolName:    "bash",
		ToolUseID:   "call_bash2",
		PayloadHash: payloadHash,
		BlobStore:   bs,
	})
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if blob == nil {
		t.Fatal("expected non-nil blob even without companion")
	}

	var prov map[string]json.RawMessage
	_ = json.Unmarshal(blob, &prov)
	var input map[string]string
	_ = json.Unmarshal(prov["tool_input"], &input)
	if input["command"] != "ls -la" {
		t.Errorf("command = %q", input["command"])
	}
}

func TestCopilotEnricher_EditWithArguments(t *testing.T) {
	bs := testBlobStore(t)

	// Real Copilot uses "path", "old_str", "new_str" -- not "file_path", "old_string", "new_string".
	payload := mustJSON(t, map[string]any{
		"type": "assistant.message",
		"data": map[string]any{
			"toolRequests": []map[string]any{
				{
					"toolCallId": "call_edit1",
					"name":       "edit",
					"arguments": map[string]any{
						"path":    "/repo/src/main.go",
						"old_str": "import (\n\t\"fmt\"\n\t\"strings\"\n)",
						"new_str": "import (\n\t\"fmt\"\n)",
					},
				},
			},
		},
	})
	payloadHash := putBlob(t, bs, payload)

	e := &copilotEnricher{}
	blob, err := e.Enrich(context.Background(), EnrichInput{
		Provider:    "copilot",
		ToolName:    "edit",
		ToolUseID:   "call_edit1",
		PayloadHash: payloadHash,
		BlobStore:   bs,
	})
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if blob == nil {
		t.Fatal("expected non-nil blob for edit with arguments")
	}

	// Verify normalized to canonical field names.
	var prov map[string]json.RawMessage
	_ = json.Unmarshal(blob, &prov)
	var input map[string]string
	_ = json.Unmarshal(prov["tool_input"], &input)
	if input["file_path"] != "/repo/src/main.go" {
		t.Errorf("file_path = %q (should be normalized from path)", input["file_path"])
	}
	if input["old_string"] == "" {
		t.Error("old_string should be normalized from old_str")
	}
	if input["new_string"] == "" {
		t.Error("new_string should be normalized from new_str")
	}
}

func TestCopilotEnricher_EditWithoutPath(t *testing.T) {
	bs := testBlobStore(t)

	// Edit arguments without path or file_path -- should return nil.
	payload := mustJSON(t, map[string]any{
		"type": "assistant.message",
		"data": map[string]any{
			"toolRequests": []map[string]any{
				{
					"toolCallId": "call_edit2",
					"name":       "edit",
					"arguments":  map[string]any{"content": "some text"},
				},
			},
		},
	})
	payloadHash := putBlob(t, bs, payload)

	e := &copilotEnricher{}
	blob, _ := e.Enrich(context.Background(), EnrichInput{
		Provider:    "copilot",
		ToolName:    "edit",
		ToolUseID:   "call_edit2",
		PayloadHash: payloadHash,
		BlobStore:   bs,
	})
	if blob != nil {
		t.Error("expected nil blob for edit without path")
	}
}

func TestCopilotEnricher_CopilotFileEditWithDiff(t *testing.T) {
	bs := testBlobStore(t)

	// Real copilot_file_edit from tool.execution_complete with rich data.
	payload := mustJSON(t, map[string]any{
		"type": "tool.execution_complete",
		"data": map[string]any{
			"toolCallId": "call_fe1",
			"result": map[string]any{
				"content":         "Created file /repo/src/handler.ts with 24 characters",
				"detailedContent": "diff --git a/handler.ts b/handler.ts\n+export function handle() {}\n",
			},
			"toolTelemetry": map[string]any{
				"properties": map[string]any{
					"command":   "create",
					"filePaths": `["/repo/src/handler.ts"]`,
				},
			},
		},
	})
	payloadHash := putBlob(t, bs, payload)

	e := &copilotEnricher{}
	blob, err := e.Enrich(context.Background(), EnrichInput{
		Provider:    "copilot",
		ToolName:    "copilot_file_edit",
		ToolUseID:   "call_fe1",
		PayloadHash: payloadHash,
		BlobStore:   bs,
	})
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if blob == nil {
		t.Fatal("expected non-nil blob for copilot_file_edit")
	}

	var prov map[string]json.RawMessage
	_ = json.Unmarshal(blob, &prov)

	// Should have file_path in tool_input.
	var input map[string]any
	_ = json.Unmarshal(prov["tool_input"], &input)
	if input["file_path"] != "/repo/src/handler.ts" {
		t.Errorf("file_path = %v", input["file_path"])
	}

	// Should have diff in tool_response.
	if _, ok := prov["tool_response"]; !ok {
		t.Error("expected tool_response with diff")
	}
	var resp map[string]string
	_ = json.Unmarshal(prov["tool_response"], &resp)
	if resp["diff"] == "" {
		t.Error("expected diff in tool_response")
	}
}

func TestCopilotEnricher_CopilotFileEditMinimal(t *testing.T) {
	bs := testBlobStore(t)

	// Minimal tool.execution_complete with only telemetry, no result.
	payload := mustJSON(t, map[string]any{
		"type": "tool.execution_complete",
		"data": map[string]any{
			"toolCallId": "call_fe2",
			"toolTelemetry": map[string]any{
				"properties": map[string]any{
					"filePaths": `["/repo/src/util.ts"]`,
				},
			},
		},
	})
	payloadHash := putBlob(t, bs, payload)

	e := &copilotEnricher{}
	blob, _ := e.Enrich(context.Background(), EnrichInput{
		Provider:    "copilot",
		ToolName:    "copilot_file_edit",
		ToolUseID:   "call_fe2",
		PayloadHash: payloadHash,
		BlobStore:   bs,
	})
	if blob == nil {
		t.Fatal("expected non-nil blob even for minimal copilot_file_edit")
	}

	var prov map[string]json.RawMessage
	_ = json.Unmarshal(blob, &prov)
	var input map[string]any
	_ = json.Unmarshal(prov["tool_input"], &input)
	if input["file_path"] != "/repo/src/util.ts" {
		t.Errorf("file_path = %v", input["file_path"])
	}
	// No tool_response when no detailedContent.
	if _, ok := prov["tool_response"]; ok {
		t.Error("should not have tool_response without detailedContent")
	}
}
