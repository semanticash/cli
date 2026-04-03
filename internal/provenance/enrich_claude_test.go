package provenance

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/store/blobs"
)

// testBlobStore creates a temp blob store and returns it with a cleanup.
func testBlobStore(t *testing.T) *blobs.Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "objects")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bs, err := blobs.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return bs
}

// putBlob stores a blob and returns its hash.
func putBlob(t *testing.T, bs *blobs.Store, data []byte) string {
	t.Helper()
	h, _, err := bs.Put(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestClaudeEnricher_WriteStep(t *testing.T) {
	bs := testBlobStore(t)

	// Claude assistant JSONL entry with a Write tool_use block.
	payload := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []map[string]any{
				{
					"type": "tool_use",
					"id":   "toolu_write1",
					"name": "Write",
					"input": map[string]any{
						"file_path": "/repo/src/main.go",
						"content":   "package main\nfunc main() {}\n",
					},
				},
			},
		},
	})
	payloadHash := putBlob(t, bs, payload)

	enricher := &claudeEnricher{}
	blob, err := enricher.Enrich(context.Background(), EnrichInput{
		Provider:    "claude_code",
		ToolName:    "Write",
		ToolUseID:   "toolu_write1",
		PayloadHash: payloadHash,
		BlobStore:   bs,
	})

	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if blob == nil {
		t.Fatal("expected non-nil blob for Write step")
	}

	// Verify it matches the hook-produced shape: {"tool_input": {...}}
	var prov map[string]json.RawMessage
	if err := json.Unmarshal(blob, &prov); err != nil {
		t.Fatalf("unmarshal blob: %v", err)
	}
	if _, ok := prov["tool_input"]; !ok {
		t.Error("blob missing tool_input")
	}
	// Write blobs should NOT have tool_name at top level (matches storeRawHookPayload).
	if _, ok := prov["tool_name"]; ok {
		t.Error("Write blob should not have top-level tool_name")
	}

	// Verify the input contains the file path and content.
	var input map[string]string
	if err := json.Unmarshal(prov["tool_input"], &input); err != nil {
		t.Fatalf("unmarshal tool_input: %v", err)
	}
	if input["file_path"] != "/repo/src/main.go" {
		t.Errorf("file_path = %q", input["file_path"])
	}
	if input["content"] == "" {
		t.Error("content should not be empty")
	}
}

func TestClaudeEnricher_EditStep(t *testing.T) {
	bs := testBlobStore(t)

	payload := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []map[string]any{
				{
					"type": "tool_use",
					"id":   "toolu_edit1",
					"name": "Edit",
					"input": map[string]any{
						"file_path":  "/repo/src/config.go",
						"old_string": "v1",
						"new_string": "v2",
					},
				},
			},
		},
	})
	payloadHash := putBlob(t, bs, payload)

	enricher := &claudeEnricher{}
	blob, err := enricher.Enrich(context.Background(), EnrichInput{
		Provider:    "claude_code",
		ToolName:    "Edit",
		ToolUseID:   "toolu_edit1",
		PayloadHash: payloadHash,
		BlobStore:   bs,
	})

	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if blob == nil {
		t.Fatal("expected non-nil blob for Edit step")
	}

	var prov map[string]json.RawMessage
	if err := json.Unmarshal(blob, &prov); err != nil {
		t.Fatalf("unmarshal blob: %v", err)
	}

	var input map[string]string
	if err := json.Unmarshal(prov["tool_input"], &input); err != nil {
		t.Fatalf("unmarshal tool_input: %v", err)
	}
	if input["old_string"] != "v1" || input["new_string"] != "v2" {
		t.Errorf("edit fields: old=%q new=%q", input["old_string"], input["new_string"])
	}
}

func TestClaudeEnricher_BashWithCompanion(t *testing.T) {
	bs := testBlobStore(t)

	// Assistant message with Bash tool_use.
	payload := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []map[string]any{
				{
					"type": "tool_use",
					"id":   "toolu_bash1",
					"name": "Bash",
					"input": map[string]any{
						"command": "go test ./...",
					},
				},
			},
		},
	})
	payloadHash := putBlob(t, bs, payload)

	// Companion tool_result with structured toolUseResult.
	companion := mustJSON(t, map[string]any{
		"type": "user",
		"message": map[string]any{
			"content": []map[string]any{
				{
					"type":        "tool_result",
					"tool_use_id": "toolu_bash1",
					"content":     "PASS\nok  github.com/test 0.5s\n",
				},
			},
		},
		"toolUseResult": map[string]any{
			"stdout":      "PASS\nok  github.com/test 0.5s\n",
			"stderr":      "warning: unused import\n",
			"interrupted": false,
		},
	})
	companionHash := putBlob(t, bs, companion)

	enricher := &claudeEnricher{}
	blob, err := enricher.Enrich(context.Background(), EnrichInput{
		Provider:    "claude_code",
		ToolName:    "Bash",
		ToolUseID:   "toolu_bash1",
		PayloadHash: payloadHash,
		Companions: []CompanionEvidence{
			{PayloadHash: companionHash, Role: "user", Kind: "tool_result", Ts: 1000},
		},
		BlobStore: bs,
	})

	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if blob == nil {
		t.Fatal("expected non-nil blob for Bash step")
	}

	// Verify it matches storeRedactedBashProvenance shape.
	var prov map[string]json.RawMessage
	if err := json.Unmarshal(blob, &prov); err != nil {
		t.Fatalf("unmarshal blob: %v", err)
	}

	// Bash blobs have tool_name and tool_use_id at top level.
	var toolName string
	if err := json.Unmarshal(prov["tool_name"], &toolName); err != nil {
		t.Fatalf("unmarshal tool_name: %v", err)
	}
	if toolName != "Bash" {
		t.Errorf("tool_name = %q, want Bash", toolName)
	}

	var input map[string]string
	if err := json.Unmarshal(prov["tool_input"], &input); err != nil {
		t.Fatalf("unmarshal tool_input: %v", err)
	}
	if input["command"] != "go test ./..." {
		t.Errorf("command = %q", input["command"])
	}

	var resp map[string]any
	if err := json.Unmarshal(prov["tool_response"], &resp); err != nil {
		t.Fatalf("unmarshal tool_response: %v", err)
	}
	stdout, _ := resp["stdout"].(string)
	if stdout == "" {
		t.Error("expected non-empty stdout from companion")
	}
	stderr, _ := resp["stderr"].(string)
	if stderr == "" {
		t.Error("expected non-empty stderr from structured toolUseResult")
	}
	if _, ok := resp["interrupted"]; !ok {
		t.Error("expected interrupted field in tool_response")
	}
}

func TestClaudeEnricher_BashNoCompanion(t *testing.T) {
	bs := testBlobStore(t)

	payload := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []map[string]any{
				{
					"type": "tool_use",
					"id":   "toolu_bash2",
					"name": "Bash",
					"input": map[string]any{
						"command": "ls -la",
					},
				},
			},
		},
	})
	payloadHash := putBlob(t, bs, payload)

	enricher := &claudeEnricher{}
	blob, err := enricher.Enrich(context.Background(), EnrichInput{
		Provider:    "claude_code",
		ToolName:    "Bash",
		ToolUseID:   "toolu_bash2",
		PayloadHash: payloadHash,
		BlobStore:   bs,
	})

	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if blob == nil {
		t.Fatal("expected non-nil blob even without companion")
	}

	// Should still have the command, just empty stdout/stderr.
	var prov map[string]json.RawMessage
	_ = json.Unmarshal(blob, &prov)
	var input map[string]string
	_ = json.Unmarshal(prov["tool_input"], &input)
	if input["command"] != "ls -la" {
		t.Errorf("command = %q", input["command"])
	}
}

func TestClaudeEnricher_BashFailedWithStringToolUseResult(t *testing.T) {
	bs := testBlobStore(t)

	payload := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []map[string]any{
				{
					"type": "tool_use",
					"id":   "toolu_bash_fail",
					"name": "Bash",
					"input": map[string]any{
						"command": "go build ./...",
					},
				},
			},
		},
	})
	payloadHash := putBlob(t, bs, payload)

	// Failed Bash: toolUseResult is a plain string, not a structured object.
	companion := mustJSON(t, map[string]any{
		"type": "user",
		"message": map[string]any{
			"content": []map[string]any{
				{
					"type":        "tool_result",
					"tool_use_id": "toolu_bash_fail",
					"content":     "Error: Exit code 1",
				},
			},
		},
		"toolUseResult": "Error: Exit code 1\n./main.go:10: undefined: foo\n",
	})
	companionHash := putBlob(t, bs, companion)

	enricher := &claudeEnricher{}
	blob, err := enricher.Enrich(context.Background(), EnrichInput{
		Provider:    "claude_code",
		ToolName:    "Bash",
		ToolUseID:   "toolu_bash_fail",
		PayloadHash: payloadHash,
		Companions: []CompanionEvidence{
			{PayloadHash: companionHash, Role: "user", Kind: "tool_result"},
		},
		BlobStore: bs,
	})

	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if blob == nil {
		t.Fatal("expected non-nil blob for failed Bash")
	}

	var prov map[string]json.RawMessage
	_ = json.Unmarshal(blob, &prov)
	var resp map[string]any
	_ = json.Unmarshal(prov["tool_response"], &resp)

	stdout, _ := resp["stdout"].(string)
	if stdout == "" {
		t.Error("stdout should contain the error output from string toolUseResult")
	}
	if !strings.Contains(stdout, "Exit code 1") {
		t.Errorf("stdout = %q, expected to contain error output", stdout)
	}
}

func TestClaudeEnricher_HookBackedStepNotEnriched(t *testing.T) {
	enricher := &claudeEnricher{}
	if !enricher.CanEnrich("claude_code", "Write") {
		t.Fatal("should be able to enrich claude_code Write")
	}
	if enricher.CanEnrich("copilot", "Write") {
		t.Error("should not enrich copilot")
	}
	if enricher.CanEnrich("claude_code", "Read") {
		t.Error("should not enrich Read tool")
	}
}

func TestClaudeEnricher_MissingPayload(t *testing.T) {
	bs := testBlobStore(t)

	enricher := &claudeEnricher{}
	blob, err := enricher.Enrich(context.Background(), EnrichInput{
		Provider:    "claude_code",
		ToolName:    "Write",
		ToolUseID:   "toolu_missing",
		PayloadHash: "nonexistent_hash_1234567890",
		BlobStore:   bs,
	})

	if err == nil && blob != nil {
		t.Error("expected nil blob or error for missing payload")
	}
}

// Regression tests for the blob shapes consumed by the backend diff parser.

func TestClaudeEnricher_E2E_WriteWithToolResponse(t *testing.T) {
	bs := testBlobStore(t)

	// Realistic Claude transcript: Write tool_use + companion tool_result.
	payload := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant",
			"content": []map[string]any{
				{
					"type": "tool_use",
					"id":   "toolu_w1",
					"name": "Write",
					"input": map[string]any{
						"file_path": "/workspace/repo/src/handler.go",
						"content":   "package main\n\nfunc Handle() error {\n\treturn nil\n}\n",
					},
				},
			},
		},
	})
	payloadHash := putBlob(t, bs, payload)

	companion := mustJSON(t, map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]any{
				{
					"type":        "tool_result",
					"tool_use_id": "toolu_w1",
					"content":     "File written successfully",
				},
			},
		},
	})
	companionHash := putBlob(t, bs, companion)

	enricher := &claudeEnricher{}
	blob, err := enricher.Enrich(context.Background(), EnrichInput{
		Provider:    "claude_code",
		ToolName:    "Write",
		ToolUseID:   "toolu_w1",
		PayloadHash: payloadHash,
		Companions: []CompanionEvidence{
			{PayloadHash: companionHash, Role: "user", Kind: "tool_result"},
		},
		BlobStore: bs,
	})
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}

	// Parse as the backend would (parseDiffFromProvenance).
	var prov map[string]json.RawMessage
	if err := json.Unmarshal(blob, &prov); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Backend extracts tool_input for Write: file_path + content.
	var input map[string]any
	if err := json.Unmarshal(prov["tool_input"], &input); err != nil {
		t.Fatalf("unmarshal tool_input: %v", err)
	}
	if input["file_path"] != "/workspace/repo/src/handler.go" {
		t.Errorf("file_path = %v", input["file_path"])
	}
	if input["content"] == nil || input["content"] == "" {
		t.Error("content should be present for Write")
	}

	// tool_response should be the raw companion content (string).
	if _, ok := prov["tool_response"]; !ok {
		t.Error("expected tool_response from companion")
	}

	// No top-level tool_name (matches storeRawHookPayload shape).
	if _, ok := prov["tool_name"]; ok {
		t.Error("Write blob should not have top-level tool_name")
	}
}

func TestClaudeEnricher_E2E_EditMatchesBackendShape(t *testing.T) {
	bs := testBlobStore(t)

	payload := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant",
			"content": []map[string]any{
				{
					"type": "tool_use",
					"id":   "toolu_e1",
					"name": "Edit",
					"input": map[string]any{
						"file_path":  "/workspace/repo/src/config.go",
						"old_string": "const Version = \"1.0\"",
						"new_string": "const Version = \"2.0\"",
					},
				},
			},
		},
	})
	payloadHash := putBlob(t, bs, payload)

	enricher := &claudeEnricher{}
	blob, _ := enricher.Enrich(context.Background(), EnrichInput{
		Provider:    "claude_code",
		ToolName:    "Edit",
		ToolUseID:   "toolu_e1",
		PayloadHash: payloadHash,
		BlobStore:   bs,
	})

	// Backend parses Edit: file_path + old_string + new_string.
	var prov map[string]json.RawMessage
	_ = json.Unmarshal(blob, &prov)
	var input map[string]any
	_ = json.Unmarshal(prov["tool_input"], &input)

	if input["file_path"] != "/workspace/repo/src/config.go" {
		t.Errorf("file_path = %v", input["file_path"])
	}
	if input["old_string"] != "const Version = \"1.0\"" {
		t.Errorf("old_string = %v", input["old_string"])
	}
	if input["new_string"] != "const Version = \"2.0\"" {
		t.Errorf("new_string = %v", input["new_string"])
	}
}

func TestClaudeEnricher_E2E_BashMatchesBackendShape(t *testing.T) {
	bs := testBlobStore(t)

	payload := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant",
			"content": []map[string]any{
				{
					"type": "tool_use",
					"id":   "toolu_b1",
					"name": "Bash",
					"input": map[string]any{
						"command":     "go test -v ./internal/service/...",
						"description": "Run service tests",
					},
				},
			},
		},
	})
	payloadHash := putBlob(t, bs, payload)

	companion := mustJSON(t, map[string]any{
		"type": "user",
		"message": map[string]any{
			"content": []map[string]any{
				{
					"type":        "tool_result",
					"tool_use_id": "toolu_b1",
					"content":     "PASS",
				},
			},
		},
		"toolUseResult": map[string]any{
			"stdout":      "=== RUN TestService\n--- PASS: TestService (0.01s)\nPASS",
			"stderr":      "",
			"interrupted": false,
		},
	})
	companionHash := putBlob(t, bs, companion)

	enricher := &claudeEnricher{}
	blob, _ := enricher.Enrich(context.Background(), EnrichInput{
		Provider:    "claude_code",
		ToolName:    "Bash",
		ToolUseID:   "toolu_b1",
		PayloadHash: payloadHash,
		Companions: []CompanionEvidence{
			{PayloadHash: companionHash, Role: "user", Kind: "tool_result"},
		},
		BlobStore: bs,
	})

	// Backend parses Bash: tool_name, tool_use_id, tool_input.command,
	// tool_response.stdout/stderr/interrupted.
	var prov map[string]json.RawMessage
	_ = json.Unmarshal(blob, &prov)

	// Must have tool_name and tool_use_id at top level.
	var toolName string
	_ = json.Unmarshal(prov["tool_name"], &toolName)
	if toolName != "Bash" {
		t.Errorf("tool_name = %q, want Bash", toolName)
	}
	var toolUseID string
	_ = json.Unmarshal(prov["tool_use_id"], &toolUseID)
	if toolUseID != "toolu_b1" {
		t.Errorf("tool_use_id = %q", toolUseID)
	}

	// tool_input must have command and description.
	var input map[string]string
	_ = json.Unmarshal(prov["tool_input"], &input)
	if input["command"] == "" {
		t.Error("command should not be empty")
	}
	if _, ok := input["description"]; !ok {
		t.Error("description field should be present")
	}

	// tool_response must have stdout, stderr, interrupted.
	var resp map[string]any
	_ = json.Unmarshal(prov["tool_response"], &resp)
	if resp["stdout"] == nil || resp["stdout"] == "" {
		t.Error("stdout should be present from toolUseResult")
	}
	if _, ok := resp["stderr"]; !ok {
		t.Error("stderr field should be present")
	}
	if _, ok := resp["interrupted"]; !ok {
		t.Error("interrupted field should be present")
	}
}
