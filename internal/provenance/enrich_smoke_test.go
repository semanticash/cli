package provenance

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// fakeCompanionQuerier implements the companionQuerier interface without a real DB.
type fakeCompanionQuerier struct {
	results  map[string][]sqldb.ListStepCompanionResultsRow
	temporal map[string]sqldb.GetNextToolResultAfterRow // key: session|turnID
}

func (f *fakeCompanionQuerier) ListStepCompanionResults(_ context.Context, arg sqldb.ListStepCompanionResultsParams) ([]sqldb.ListStepCompanionResultsRow, error) {
	key := arg.SessionID + "|" + arg.ToolUseID.String
	if rows, ok := f.results[key]; ok {
		return rows, nil
	}
	return nil, nil
}

func (f *fakeCompanionQuerier) GetNextToolResultAfter(_ context.Context, arg sqldb.GetNextToolResultAfterParams) (sqldb.GetNextToolResultAfterRow, error) {
	if f.temporal == nil {
		return sqldb.GetNextToolResultAfterRow{}, sql.ErrNoRows
	}
	key := arg.SessionID + "|" + arg.TurnID.String
	if row, ok := f.temporal[key]; ok && row.Ts > arg.Ts {
		return row, nil
	}
	return sqldb.GetNextToolResultAfterRow{}, sql.ErrNoRows
}

// TestSmokeEnrichment_ClaudeTranscriptTurn exercises packaging-time enrichment
// for transcript-only Claude steps.
func TestSmokeEnrichment_ClaudeTranscriptTurn(t *testing.T) {
	bs := testBlobStore(t)
	ctx := context.Background()

	// Seed CAS with Claude transcript payloads.

	// Edit tool_use payload.
	editPayload := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant",
			"content": []map[string]any{
				{
					"type": "tool_use",
					"id":   "toolu_edit_1",
					"name": "Edit",
					"input": map[string]any{
						"file_path":  "/workspace/repo/main.go",
						"old_string": "func old() {}",
						"new_string": "func new() {}",
					},
				},
			},
		},
	})
	editHash := putBlob(t, bs, editPayload)

	// Bash tool_use payload.
	bashPayload := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant",
			"content": []map[string]any{
				{
					"type": "tool_use",
					"id":   "toolu_bash_1",
					"name": "Bash",
					"input": map[string]any{
						"command":     "go test ./...",
						"description": "run tests",
					},
				},
			},
		},
	})
	bashHash := putBlob(t, bs, bashPayload)

	// Bash companion tool_result.
	bashCompanion := mustJSON(t, map[string]any{
		"type": "user",
		"message": map[string]any{
			"content": []map[string]any{
				{
					"type":        "tool_result",
					"tool_use_id": "toolu_bash_1",
					"content":     "PASS",
				},
			},
		},
		"toolUseResult": map[string]any{
			"stdout":      "PASS\nok  github.com/test 0.3s",
			"stderr":      "",
			"interrupted": false,
		},
	})
	bashCompanionHash := putBlob(t, bs, bashCompanion)

	// Build step rows as ListStepEventsForTurn would return.
	// Both are transcript-sourced with no provenance_hash.
	steps := []sqldb.ListStepEventsForTurnRow{
		{
			EventID:     "evt-edit-1",
			Ts:          1000,
			ToolName:    sql.NullString{String: "Edit", Valid: true},
			ToolUseID:   sql.NullString{String: "toolu_edit_1", Valid: true},
			PayloadHash: sql.NullString{String: editHash, Valid: true},
			Summary:     sql.NullString{String: "Edit(main.go)", Valid: true},
			EventSource: "transcript",
		},
		{
			EventID:     "evt-bash-1",
			Ts:          2000,
			ToolName:    sql.NullString{String: "Bash", Valid: true},
			ToolUseID:   sql.NullString{String: "toolu_bash_1", Valid: true},
			PayloadHash: sql.NullString{String: bashHash, Valid: true},
			Summary:     sql.NullString{String: "go test ./...", Valid: true},
			EventSource: "transcript",
		},
	}

	// Mock companion querier.
	querier := &fakeCompanionQuerier{
		results: map[string][]sqldb.ListStepCompanionResultsRow{
			"sess-1|toolu_bash_1": {
				{
					EventID:     "evt-bash-result",
					PayloadHash: sql.NullString{String: bashCompanionHash, Valid: true},
					Summary:     sql.NullString{String: "PASS", Valid: true},
					Role:        sql.NullString{String: "user", Valid: true},
					Kind:        "tool_result",
					Ts:          3000,
				},
			},
		},
	}

	// Run enrichment.
	enriched := enrichSteps(ctx, querier, bs, "claude_code", "sess-1", "turn-1", steps)

	// Both steps should now have provenance_hash.
	if len(enriched) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(enriched))
	}

	for _, s := range enriched {
		if !s.ProvenanceHash.Valid || s.ProvenanceHash.String == "" {
			t.Errorf("step %s (%s): missing provenance_hash after enrichment",
				s.EventID, s.ToolName.String)
			continue
		}

		// Read and verify the provenance blob.
		provData, err := bs.Get(ctx, s.ProvenanceHash.String)
		if err != nil {
			t.Errorf("step %s: read provenance: %v", s.EventID, err)
			continue
		}

		var prov map[string]json.RawMessage
		if err := json.Unmarshal(provData, &prov); err != nil {
			t.Errorf("step %s: parse provenance: %v", s.EventID, err)
			continue
		}

		if _, ok := prov["tool_input"]; !ok {
			t.Errorf("step %s: provenance missing tool_input", s.EventID)
		}

		switch s.ToolName.String {
		case "Edit":
			var input map[string]string
			_ = json.Unmarshal(prov["tool_input"], &input)
			if input["file_path"] == "" {
				t.Error("Edit: missing file_path")
			}
			if input["old_string"] != "func old() {}" {
				t.Errorf("Edit: old_string = %q", input["old_string"])
			}
			if input["new_string"] != "func new() {}" {
				t.Errorf("Edit: new_string = %q", input["new_string"])
			}
			// Edit should NOT have tool_name at top level.
			if _, ok := prov["tool_name"]; ok {
				t.Error("Edit: should not have top-level tool_name")
			}

		case "Bash":
			// Bash should have tool_name and tool_use_id at top level.
			var toolName string
			_ = json.Unmarshal(prov["tool_name"], &toolName)
			if toolName != "Bash" {
				t.Errorf("Bash: tool_name = %q", toolName)
			}
			// Should have tool_response with stdout from companion.
			var resp map[string]any
			_ = json.Unmarshal(prov["tool_response"], &resp)
			if resp["stdout"] == nil || resp["stdout"] == "" {
				t.Error("Bash: missing stdout from companion enrichment")
			}
			if _, ok := resp["interrupted"]; !ok {
				t.Error("Bash: missing interrupted field")
			}
		}
	}
}

// TestSmokeEnrichment_CopilotTranscriptTurn exercises packaging-time enrichment
// for Copilot edit, bash, and copilot_file_edit transcript steps.
func TestSmokeEnrichment_CopilotTranscriptTurn(t *testing.T) {
	bs := testBlobStore(t)
	ctx := context.Background()

	// Copilot edit payload (uses path/old_str/new_str).
	editPayload := mustJSON(t, map[string]any{
		"type": "assistant.message",
		"data": map[string]any{
			"toolRequests": []map[string]any{
				{
					"toolCallId": "call_edit_1",
					"name":       "edit",
					"arguments": map[string]any{
						"path":    "/repo/src/main.go",
						"old_str": "import \"strings\"",
						"new_str": "import \"fmt\"",
					},
				},
			},
		},
	})
	editHash := putBlob(t, bs, editPayload)

	// Copilot bash payload.
	bashPayload := mustJSON(t, map[string]any{
		"type": "assistant.message",
		"data": map[string]any{
			"toolRequests": []map[string]any{
				{
					"toolCallId": "call_bash_1",
					"name":       "bash",
					"arguments":  map[string]any{"command": "npm test"},
				},
			},
		},
	})
	bashHash := putBlob(t, bs, bashPayload)

	// Bash companion (tool.execution_complete with textResultForLlm).
	bashCompanion := mustJSON(t, map[string]any{
		"type": "tool.execution_complete",
		"data": map[string]any{
			"toolCallId":       "call_bash_1",
			"textResultForLlm": "PASS 12 tests\n",
		},
	})
	bashCompanionHash := putBlob(t, bs, bashCompanion)

	// copilot_file_edit payload (tool.execution_complete with diff).
	fileEditPayload := mustJSON(t, map[string]any{
		"type": "tool.execution_complete",
		"data": map[string]any{
			"toolCallId": "call_fe_1",
			"result": map[string]any{
				"content":         "Created file handler.ts",
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
	fileEditHash := putBlob(t, bs, fileEditPayload)

	steps := []sqldb.ListStepEventsForTurnRow{
		{
			EventID:     "evt-edit",
			Ts:          1000,
			ToolName:    sql.NullString{String: "edit", Valid: true},
			ToolUseID:   sql.NullString{String: "call_edit_1", Valid: true},
			PayloadHash: sql.NullString{String: editHash, Valid: true},
			EventSource: "transcript",
		},
		{
			EventID:     "evt-bash",
			Ts:          2000,
			ToolName:    sql.NullString{String: "bash", Valid: true},
			ToolUseID:   sql.NullString{String: "call_bash_1", Valid: true},
			PayloadHash: sql.NullString{String: bashHash, Valid: true},
			EventSource: "transcript",
		},
		{
			EventID:     "evt-fe",
			Ts:          3000,
			ToolName:    sql.NullString{String: "copilot_file_edit", Valid: true},
			ToolUseID:   sql.NullString{String: "call_fe_1", Valid: true},
			PayloadHash: sql.NullString{String: fileEditHash, Valid: true},
			EventSource: "transcript",
		},
	}

	querier := &fakeCompanionQuerier{
		results: map[string][]sqldb.ListStepCompanionResultsRow{
			"sess-cop|call_bash_1": {
				{
					EventID:     "evt-bash-result",
					PayloadHash: sql.NullString{String: bashCompanionHash, Valid: true},
					Role:        sql.NullString{String: "tool", Valid: true},
					Kind:        "tool_result",
					Ts:          2500,
				},
			},
		},
	}

	enriched := enrichSteps(ctx, querier, bs, "copilot", "sess-cop", "turn-cop", steps)

	if len(enriched) != 3 {
		t.Fatalf("expected 3 steps, got %d", len(enriched))
	}

	// All three should have provenance.
	for _, s := range enriched {
		if !s.ProvenanceHash.Valid || s.ProvenanceHash.String == "" {
			t.Errorf("step %s (%s): missing provenance_hash", s.EventID, s.ToolName.String)
			continue
		}

		provData, err := bs.Get(ctx, s.ProvenanceHash.String)
		if err != nil {
			t.Errorf("step %s: read provenance: %v", s.EventID, err)
			continue
		}

		var prov map[string]json.RawMessage
		if err := json.Unmarshal(provData, &prov); err != nil {
			t.Errorf("step %s: parse provenance: %v", s.EventID, err)
			continue
		}

		switch s.ToolName.String {
		case "edit":
			// Should be normalized to canonical field names.
			var input map[string]string
			_ = json.Unmarshal(prov["tool_input"], &input)
			if input["file_path"] != "/repo/src/main.go" {
				t.Errorf("edit: file_path = %q (should be normalized from path)", input["file_path"])
			}
			if input["old_string"] != "import \"strings\"" {
				t.Errorf("edit: old_string = %q (should be normalized from old_str)", input["old_string"])
			}
			if input["new_string"] != "import \"fmt\"" {
				t.Errorf("edit: new_string = %q (should be normalized from new_str)", input["new_string"])
			}

		case "bash":
			// Should have command in tool_input and textResultForLlm in tool_response.
			var input map[string]string
			_ = json.Unmarshal(prov["tool_input"], &input)
			if input["command"] != "npm test" {
				t.Errorf("bash: command = %q", input["command"])
			}
			var resp map[string]string
			_ = json.Unmarshal(prov["tool_response"], &resp)
			if resp["textResultForLlm"] == "" {
				t.Error("bash: missing textResultForLlm from companion")
			}

		case "copilot_file_edit":
			// Should have file_path and diff.
			var input map[string]any
			_ = json.Unmarshal(prov["tool_input"], &input)
			if input["file_path"] != "/repo/src/handler.ts" {
				t.Errorf("copilot_file_edit: file_path = %v", input["file_path"])
			}
			if _, ok := prov["tool_response"]; !ok {
				t.Error("copilot_file_edit: missing tool_response with diff")
			} else {
				var resp map[string]string
				_ = json.Unmarshal(prov["tool_response"], &resp)
				if resp["diff"] == "" {
					t.Error("copilot_file_edit: missing diff in tool_response")
				}
			}
		}
	}
}

// TestSmokeEnrichment_ClaudeTemporalFallback verifies that when tool_result
// rows lack tool_use_id (as in real Claude transcripts), the enricher falls
// back to temporal matching and still gets the companion output.
func TestSmokeEnrichment_ClaudeTemporalFallback(t *testing.T) {
	bs := testBlobStore(t)
	ctx := context.Background()

	bashPayload := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []map[string]any{
				{
					"type": "tool_use",
					"id":   "toolu_bash_tf",
					"name": "Bash",
					"input": map[string]any{
						"command": "make build",
					},
				},
			},
		},
	})
	bashHash := putBlob(t, bs, bashPayload)

	bashCompanion := mustJSON(t, map[string]any{
		"type": "user",
		"message": map[string]any{
			"content": []map[string]any{
				{
					"type":        "tool_result",
					"tool_use_id": "toolu_bash_tf",
					"content":     "Build succeeded",
				},
			},
		},
		"toolUseResult": map[string]any{
			"stdout":      "Build succeeded\n",
			"stderr":      "",
			"interrupted": false,
		},
	})
	companionHash := putBlob(t, bs, bashCompanion)

	steps := []sqldb.ListStepEventsForTurnRow{
		{
			EventID:     "evt-bash-tf",
			Ts:          5000,
			ToolName:    sql.NullString{String: "Bash", Valid: true},
			ToolUseID:   sql.NullString{String: "toolu_bash_tf", Valid: true},
			PayloadHash: sql.NullString{String: bashHash, Valid: true},
			EventSource: "transcript",
		},
	}

	// Simulate the real scenario: ListStepCompanionResults returns nothing
	// (companion tool_result has empty tool_use_id in DB), but temporal
	// fallback finds the tool_result at ts=5500.
	querier := &fakeCompanionQuerier{
		results: map[string][]sqldb.ListStepCompanionResultsRow{},
		temporal: map[string]sqldb.GetNextToolResultAfterRow{
			"sess-tf|turn-tf": {
				EventID:     "evt-bash-result-tf",
				PayloadHash: sql.NullString{String: companionHash, Valid: true},
				Summary:     sql.NullString{String: "Build succeeded", Valid: true},
				Role:        sql.NullString{String: "user", Valid: true},
				Kind:        "tool_result",
				Ts:          5500,
			},
		},
	}

	enriched := enrichSteps(ctx, querier, bs, "claude_code", "sess-tf", "turn-tf", steps)

	if len(enriched) != 1 {
		t.Fatalf("expected 1 step, got %d", len(enriched))
	}
	if !enriched[0].ProvenanceHash.Valid || enriched[0].ProvenanceHash.String == "" {
		t.Fatal("Bash step missing provenance after temporal fallback enrichment")
	}

	provData, _ := bs.Get(ctx, enriched[0].ProvenanceHash.String)
	var prov map[string]json.RawMessage
	_ = json.Unmarshal(provData, &prov)
	var resp map[string]any
	_ = json.Unmarshal(prov["tool_response"], &resp)
	if resp["stdout"] == nil || resp["stdout"] == "" {
		t.Error("Bash stdout should be populated from temporal fallback companion")
	}
}

// TestSmokeEnrichment_HookBackedStepsUnchanged verifies that steps with
// existing provenance_hash are not modified by enrichment.
func TestSmokeEnrichment_HookBackedStepsUnchanged(t *testing.T) {
	bs := testBlobStore(t)
	ctx := context.Background()

	originalHash := "original_provenance_hash_1234"

	steps := []sqldb.ListStepEventsForTurnRow{
		{
			EventID:        "evt-hook-1",
			Ts:             1000,
			ToolName:       sql.NullString{String: "Write", Valid: true},
			ToolUseID:      sql.NullString{String: "toolu_w1", Valid: true},
			ProvenanceHash: sql.NullString{String: originalHash, Valid: true},
			PayloadHash:    sql.NullString{String: "some_payload", Valid: true},
			EventSource:    "hook",
		},
	}

	querier := &fakeCompanionQuerier{}
	enriched := enrichSteps(ctx, querier, bs, "claude_code", "sess-1", "turn-1", steps)

	if enriched[0].ProvenanceHash.String != originalHash {
		t.Errorf("hook-backed step provenance_hash changed from %q to %q",
			originalHash, enriched[0].ProvenanceHash.String)
	}
}

// TestSmokeEnrichment_UnsupportedProviderNoOp verifies that steps from
// providers without enrichers are unchanged.
func TestSmokeEnrichment_UnsupportedProviderNoOp(t *testing.T) {
	bs := testBlobStore(t)
	ctx := context.Background()

	steps := []sqldb.ListStepEventsForTurnRow{
		{
			EventID:     "evt-kiro-1",
			Ts:          1000,
			ToolName:    sql.NullString{String: "Write", Valid: true},
			ToolUseID:   sql.NullString{String: "toolu_k1", Valid: true},
			PayloadHash: sql.NullString{String: "some_hash", Valid: true},
			EventSource: "transcript",
		},
	}

	querier := &fakeCompanionQuerier{}
	enriched := enrichSteps(ctx, querier, bs, "gemini_cli", "sess-1", "turn-1", steps)

	if enriched[0].ProvenanceHash.Valid {
		t.Error("expected no provenance_hash for unsupported provider")
	}
}

// TestSmokeEnrichment_StepOrderPreserved verifies enrichment never
// changes the order or count of steps.
func TestSmokeEnrichment_StepOrderPreserved(t *testing.T) {
	bs := testBlobStore(t)
	ctx := context.Background()

	payload := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []map[string]any{
				{"type": "tool_use", "id": "t1", "name": "Write", "input": map[string]any{"file_path": "/a.go", "content": "x"}},
			},
		},
	})
	hash := putBlob(t, bs, payload)

	steps := []sqldb.ListStepEventsForTurnRow{
		{EventID: "a", Ts: 1, ToolName: sql.NullString{String: "Write", Valid: true}, ToolUseID: sql.NullString{String: "t1", Valid: true}, PayloadHash: sql.NullString{String: hash, Valid: true}, EventSource: "transcript"},
		{EventID: "b", Ts: 2, ToolName: sql.NullString{String: "Read", Valid: true}, EventSource: "transcript"},
		{EventID: "c", Ts: 3, ToolName: sql.NullString{String: "Edit", Valid: true}, EventSource: "transcript"},
	}

	querier := &fakeCompanionQuerier{}
	enriched := enrichSteps(ctx, querier, bs, "claude_code", "sess-1", "turn-1", steps)

	if len(enriched) != 3 {
		t.Fatalf("step count changed: %d -> %d", 3, len(enriched))
	}
	for i, id := range []string{"a", "b", "c"} {
		if enriched[i].EventID != id {
			t.Errorf("step %d: EventID = %q, want %q", i, enriched[i].EventID, id)
		}
	}
}
