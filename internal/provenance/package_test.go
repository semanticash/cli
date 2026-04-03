package provenance

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/semanticash/cli/internal/store/blobs"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

func TestToRepoRelative(t *testing.T) {
	cases := []struct {
		path     string
		repoRoot string
		want     string
	}{
		{"/repo/src/main.go", "/repo", "src/main.go"},
		{"/repo", "/repo", "."},
		{"/other/file.go", "/repo", ""},                // absolute outside repo
		{"src/main.go", "/repo", "src/main.go"},        // already relative inside
		{"../escape.go", "/repo", ""},                  // relative escape
		{"../../other/file.go", "/repo", ""},           // deeper relative escape
		{"src/../src/main.go", "/repo", "src/main.go"}, // cleaned but inside
		{"", "/repo", ""},
	}
	for _, tc := range cases {
		got := toRepoRelative(tc.path, tc.repoRoot)
		if got != tc.want {
			t.Errorf("toRepoRelative(%q, %q) = %q, want %q", tc.path, tc.repoRoot, got, tc.want)
		}
	}
}

func TestExtractRepoRelativeFilePaths(t *testing.T) {
	toolUsesJSON := `{"tools":[{"name":"Write","file_path":"/repo/src/new.go"},{"name":"Edit","file_path":"/repo/src/edit.go"},{"name":"Bash"}]}`
	paths := extractRepoRelativeFilePaths(toolUsesJSON, "/repo")

	if len(paths) != 2 {
		t.Fatalf("got %d paths, want 2", len(paths))
	}
	if paths[0] != "src/new.go" {
		t.Errorf("paths[0] = %q, want src/new.go", paths[0])
	}
	if paths[1] != "src/edit.go" {
		t.Errorf("paths[1] = %q, want src/edit.go", paths[1])
	}
}

func TestExtractRepoRelativeFilePaths_DropsOutsideRepo(t *testing.T) {
	toolUsesJSON := `{"tools":[{"name":"Write","file_path":"/other/secret.go"},{"name":"Edit","file_path":"/repo/ok.go"}]}`
	paths := extractRepoRelativeFilePaths(toolUsesJSON, "/repo")

	if len(paths) != 1 {
		t.Fatalf("got %d paths, want 1 (outside-repo path should be dropped)", len(paths))
	}
	if paths[0] != "ok.go" {
		t.Errorf("paths[0] = %q, want ok.go", paths[0])
	}
}

func TestExtractRepoRelativeFilePaths_DropsRelativeEscape(t *testing.T) {
	toolUsesJSON := `{"tools":[{"name":"Write","file_path":"../escape.go"},{"name":"Edit","file_path":"src/ok.go"}]}`
	paths := extractRepoRelativeFilePaths(toolUsesJSON, "/repo")

	if len(paths) != 1 {
		t.Fatalf("got %d paths, want 1 (relative escape should be dropped)", len(paths))
	}
	if paths[0] != "src/ok.go" {
		t.Errorf("paths[0] = %q, want src/ok.go", paths[0])
	}
}

// toolUsesJSON builds a tool_uses JSON string with a single tool entry.
func toolUsesJSON(toolName, filePath, fileOp string) sql.NullString {
	if toolName == "" {
		return sql.NullString{}
	}
	type tool struct {
		Name     string `json:"name"`
		FilePath string `json:"file_path,omitempty"`
		FileOp   string `json:"file_op,omitempty"`
	}
	type payload struct {
		ContentTypes []string `json:"content_types"`
		Tools        []tool   `json:"tools"`
	}
	b, _ := json.Marshal(payload{
		ContentTypes: []string{"tool_use"},
		Tools:        []tool{{Name: toolName, FilePath: filePath, FileOp: fileOp}},
	})
	return sql.NullString{String: string(b), Valid: true}
}

func makeStep(eventID, toolName, toolUseID, source string) sqldb.ListStepEventsForTurnRow {
	return sqldb.ListStepEventsForTurnRow{
		EventID:     eventID,
		Ts:          1000,
		ToolName:    sql.NullString{String: toolName, Valid: toolName != ""},
		ToolUseID:   sql.NullString{String: toolUseID, Valid: toolUseID != ""},
		EventSource: source,
	}
}

func makeFileStep(eventID, toolName, toolUseID, source, filePath string) sqldb.ListStepEventsForTurnRow {
	s := makeStep(eventID, toolName, toolUseID, source)
	s.ToolUses = toolUsesJSON(toolName, filePath, "edit")
	return s
}

// makeBashStep creates a Bash step with a provenance blob containing the command.
func makeBashStep(t *testing.T, bs *blobs.Store, eventID, toolName, toolUseID, source, command string) sqldb.ListStepEventsForTurnRow {
	t.Helper()
	s := makeStep(eventID, toolName, toolUseID, source)
	if command != "" {
		blob, _ := json.Marshal(map[string]any{
			"tool_input":    map[string]string{"command": command},
			"tool_response": map[string]string{"textResultForLlm": ""},
		})
		hash := putBlob(t, bs, blob)
		s.ProvenanceHash = sql.NullString{String: hash, Valid: true}
	}
	return s
}

func TestFilterCopilotDuplicateSteps(t *testing.T) {
	tests := []struct {
		name    string
		steps   func(t *testing.T, bs *blobs.Store) []sqldb.ListStepEventsForTurnRow
		wantIDs []string
	}{
		{
			name: "file path match suppresses transcript edit+create+copilot_file_edit",
			steps: func(t *testing.T, bs *blobs.Store) []sqldb.ListStepEventsForTurnRow {
				return []sqldb.ListStepEventsForTurnRow{
					makeFileStep("h1", "Edit", "copilot-step-aaa1", "hook", "src/main.go"),
					makeFileStep("h2", "Write", "copilot-step-aaa2", "hook", "src/new.go"),
					makeFileStep("t1", "edit", "call_edit_1", "transcript", "src/main.go"),
					makeFileStep("t2", "create", "call_create_1", "transcript", "src/new.go"),
					makeFileStep("t3", "copilot_file_edit", "call_fe_1", "transcript", "src/new.go"),
				}
			},
			wantIDs: []string{"h1", "h2"},
		},
		{
			name: "bash command match suppresses transcript",
			steps: func(t *testing.T, bs *blobs.Store) []sqldb.ListStepEventsForTurnRow {
				return []sqldb.ListStepEventsForTurnRow{
					makeBashStep(t, bs, "h1", "Bash", "copilot-step-bbb1", "hook", "npm test"),
					makeBashStep(t, bs, "t1", "bash", "call_bash_1", "transcript", "npm test"),
				}
			},
			wantIDs: []string{"h1"},
		},
		{
			name: "different bash commands both kept",
			steps: func(t *testing.T, bs *blobs.Store) []sqldb.ListStepEventsForTurnRow {
				return []sqldb.ListStepEventsForTurnRow{
					makeBashStep(t, bs, "h1", "Bash", "copilot-step-ccc1", "hook", "npm test"),
					makeBashStep(t, bs, "t1", "bash", "call_bash_1", "transcript", "npm run build"),
				}
			},
			wantIDs: []string{"h1", "t1"},
		},
		{
			name: "transcript bash without provenance kept",
			steps: func(t *testing.T, bs *blobs.Store) []sqldb.ListStepEventsForTurnRow {
				return []sqldb.ListStepEventsForTurnRow{
					makeBashStep(t, bs, "h1", "Bash", "copilot-step-ddd1", "hook", "go test"),
					makeStep("t1", "bash", "call_bash_1", "transcript"),
				}
			},
			wantIDs: []string{"h1", "t1"},
		},
		{
			name: "bash partial coverage: matched suppressed, unmatched kept",
			steps: func(t *testing.T, bs *blobs.Store) []sqldb.ListStepEventsForTurnRow {
				return []sqldb.ListStepEventsForTurnRow{
					makeBashStep(t, bs, "h1", "Bash", "copilot-step-eee1", "hook", "npm test"),
					makeBashStep(t, bs, "t1", "bash", "call_bash_1", "transcript", "npm test"),
					makeBashStep(t, bs, "t2", "bash", "call_bash_2", "transcript", "npm run build"),
				}
			},
			wantIDs: []string{"h1", "t2"},
		},
		{
			name: "repeated command on transcript side is ambiguous, all kept",
			steps: func(t *testing.T, bs *blobs.Store) []sqldb.ListStepEventsForTurnRow {
				return []sqldb.ListStepEventsForTurnRow{
					makeBashStep(t, bs, "h1", "Bash", "copilot-step-fff1", "hook", "npm test"),
					makeBashStep(t, bs, "t1", "bash", "call_bash_1", "transcript", "npm test"),
					makeBashStep(t, bs, "t2", "bash", "call_bash_2", "transcript", "npm test"),
				}
			},
			wantIDs: []string{"h1", "t1", "t2"},
		},
		{
			name: "repeated command on both sides is ambiguous, all kept",
			steps: func(t *testing.T, bs *blobs.Store) []sqldb.ListStepEventsForTurnRow {
				return []sqldb.ListStepEventsForTurnRow{
					makeBashStep(t, bs, "h1", "Bash", "copilot-step-ggg1", "hook", "npm test"),
					makeBashStep(t, bs, "h2", "Bash", "copilot-step-ggg2", "hook", "npm test"),
					makeBashStep(t, bs, "t1", "bash", "call_bash_1", "transcript", "npm test"),
					makeBashStep(t, bs, "t2", "bash", "call_bash_2", "transcript", "npm test"),
					makeBashStep(t, bs, "t3", "bash", "call_bash_3", "transcript", "npm test"),
				}
			},
			wantIDs: []string{"h1", "h2", "t1", "t2", "t3"},
		},
		{
			name: "partial file path overlap: matched suppressed, unmatched kept",
			steps: func(t *testing.T, bs *blobs.Store) []sqldb.ListStepEventsForTurnRow {
				return []sqldb.ListStepEventsForTurnRow{
					makeFileStep("h1", "Edit", "copilot-step-ggg1", "hook", "src/a.go"),
					makeFileStep("h2", "Edit", "copilot-step-ggg2", "hook", "src/b.go"),
					makeFileStep("t1", "edit", "call_edit_1", "transcript", "src/a.go"),
					makeFileStep("t2", "edit", "call_edit_2", "transcript", "src/b.go"),
					makeFileStep("t3", "edit", "call_edit_3", "transcript", "src/c.go"),
				}
			},
			wantIDs: []string{"h1", "h2", "t3"},
		},
		{
			name: "repeated file path on transcript side is ambiguous, all kept",
			steps: func(t *testing.T, bs *blobs.Store) []sqldb.ListStepEventsForTurnRow {
				return []sqldb.ListStepEventsForTurnRow{
					makeFileStep("h1", "Write", "copilot-step-hhh1", "hook", "src/x.go"),
					makeFileStep("t1", "create", "call_create_1", "transcript", "src/x.go"),
					makeFileStep("t2", "create", "call_create_2", "transcript", "src/x.go"),
				}
			},
			wantIDs: []string{"h1", "t1", "t2"},
		},
		{
			name: "non-mutation tools (Read) always kept",
			steps: func(t *testing.T, bs *blobs.Store) []sqldb.ListStepEventsForTurnRow {
				return []sqldb.ListStepEventsForTurnRow{
					makeFileStep("h1", "Edit", "copilot-step-iii1", "hook", "src/a.go"),
					makeFileStep("t1", "edit", "call_edit_1", "transcript", "src/a.go"),
					makeStep("t2", "Read", "call_read_1", "transcript"),
				}
			},
			wantIDs: []string{"h1", "t2"},
		},
		{
			name: "no hook coverage keeps all transcript steps",
			steps: func(t *testing.T, bs *blobs.Store) []sqldb.ListStepEventsForTurnRow {
				return []sqldb.ListStepEventsForTurnRow{
					makeFileStep("t1", "edit", "call_edit_1", "transcript", "src/a.go"),
					makeStep("t2", "bash", "call_bash_1", "transcript"),
				}
			},
			wantIDs: []string{"t1", "t2"},
		},
		{
			name: "copilot_file_edit directly matches hook Write",
			steps: func(t *testing.T, bs *blobs.Store) []sqldb.ListStepEventsForTurnRow {
				return []sqldb.ListStepEventsForTurnRow{
					makeFileStep("h1", "Write", "copilot-step-jjj1", "hook", "src/handler.ts"),
					makeFileStep("t1", "copilot_file_edit", "call_fe_1", "transcript", "src/handler.ts"),
				}
			},
			wantIDs: []string{"h1"},
		},
		{
			name: "copilot_file_edit kept when twin create is unmatched",
			steps: func(t *testing.T, bs *blobs.Store) []sqldb.ListStepEventsForTurnRow {
				return []sqldb.ListStepEventsForTurnRow{
					makeFileStep("h1", "Edit", "copilot-step-mmm1", "hook", "src/other.go"),
					makeFileStep("t1", "edit", "call_edit_1", "transcript", "src/other.go"),
					makeFileStep("t2", "create", "call_create_1", "transcript", "src/y.go"),
					makeFileStep("t3", "copilot_file_edit", "call_fe_1", "transcript", "src/y.go"),
				}
			},
			wantIDs: []string{"h1", "t2", "t3"},
		},
		{
			name: "copilot_file_edit follows ambiguous twin, not independently matched",
			steps: func(t *testing.T, bs *blobs.Store) []sqldb.ListStepEventsForTurnRow {
				return []sqldb.ListStepEventsForTurnRow{
					makeFileStep("h1", "Write", "copilot-step-nnn1", "hook", "src/y.go"),
					makeFileStep("t1", "create", "call_create_1", "transcript", "src/y.go"),
					makeFileStep("t2", "create", "call_create_2", "transcript", "src/y.go"),
					makeFileStep("t3", "copilot_file_edit", "call_fe_1", "transcript", "src/y.go"),
				}
			},
			wantIDs: []string{"h1", "t1", "t2", "t3"},
		},
		{
			name: "full mirrored turn: all transcript suppressed",
			steps: func(t *testing.T, bs *blobs.Store) []sqldb.ListStepEventsForTurnRow {
				return []sqldb.ListStepEventsForTurnRow{
					makeBashStep(t, bs, "h1", "Bash", "copilot-step-kkk1", "hook", "go test ./..."),
					makeFileStep("h2", "Edit", "copilot-step-kkk2", "hook", "src/main.go"),
					makeFileStep("h3", "Write", "copilot-step-kkk3", "hook", "src/new.go"),
					makeBashStep(t, bs, "t1", "bash", "call_bash_1", "transcript", "go test ./..."),
					makeFileStep("t2", "edit", "call_edit_1", "transcript", "src/main.go"),
					makeFileStep("t3", "create", "call_create_1", "transcript", "src/new.go"),
					makeFileStep("t4", "copilot_file_edit", "call_fe_1", "transcript", "src/new.go"),
				}
			},
			wantIDs: []string{"h1", "h2", "h3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			bs := testBlobStore(t)
			steps := tt.steps(t, bs)

			filtered := filterCopilotDuplicateSteps(ctx, bs, "copilot", steps)

			gotIDs := stepIDs(filtered)
			if len(gotIDs) != len(tt.wantIDs) {
				t.Fatalf("got %d steps %v, want %d %v", len(gotIDs), gotIDs, len(tt.wantIDs), tt.wantIDs)
			}
			want := make(map[string]bool, len(tt.wantIDs))
			for _, id := range tt.wantIDs {
				want[id] = true
			}
			for _, id := range gotIDs {
				if !want[id] {
					t.Errorf("unexpected step %s", id)
				}
			}
		})
	}
}

func TestFilterCopilotDuplicateSteps_NoopForNonCopilot(t *testing.T) {
	ctx := context.Background()
	bs := testBlobStore(t)
	steps := []sqldb.ListStepEventsForTurnRow{
		makeFileStep("h1", "Write", "toolu_abc", "hook", "src/a.go"),
		makeFileStep("t1", "Write", "toolu_abc", "transcript", "src/a.go"),
	}

	filtered := filterCopilotDuplicateSteps(ctx, bs, "claude_code", steps)

	if len(filtered) != 2 {
		t.Fatalf("got %d steps, want 2 (non-copilot should not filter)", len(filtered))
	}
}

func stepIDs(steps []sqldb.ListStepEventsForTurnRow) []string {
	ids := make([]string, len(steps))
	for i, s := range steps {
		ids[i] = s.EventID
	}
	return ids
}
