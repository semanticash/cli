package provenance

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// initGitRepoWithIgnore creates a temp git repo with a .gitignore.
func initGitRepoWithIgnore(t *testing.T, ignorePatterns string) string {
	t.Helper()
	dir := t.TempDir()

	for _, args := range [][]string{
		{"init"},
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@test.local"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(ignorePatterns), 0o644); err != nil {
		t.Fatal(err)
	}
	// Need at least one commit for git check-ignore to work reliably.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "."},
		{"commit", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	return dir
}

func TestCheckGitIgnored_BasicPatterns(t *testing.T) {
	dir := initGitRepoWithIgnore(t, "node_modules/\n*.log\n.env\n")

	ignored := checkGitIgnored(context.Background(), dir, []string{
		"src/main.go",
		"node_modules/lodash/index.js",
		"debug.log",
		".env",
		"src/handler.go",
	})

	if !ignored["node_modules/lodash/index.js"] {
		t.Error("expected node_modules path to be ignored")
	}
	if !ignored["debug.log"] {
		t.Error("expected .log file to be ignored")
	}
	if !ignored[".env"] {
		t.Error("expected .env to be ignored")
	}
	if ignored["src/main.go"] {
		t.Error("src/main.go should not be ignored")
	}
	if ignored["src/handler.go"] {
		t.Error("src/handler.go should not be ignored")
	}
}

func TestCheckGitIgnored_EmptyInput(t *testing.T) {
	dir := initGitRepoWithIgnore(t, "node_modules/\n")
	ignored := checkGitIgnored(context.Background(), dir, nil)
	if len(ignored) != 0 {
		t.Errorf("expected empty set for nil input, got %v", ignored)
	}
}

func TestCheckGitIgnored_NoMatches(t *testing.T) {
	dir := initGitRepoWithIgnore(t, "node_modules/\n")
	ignored := checkGitIgnored(context.Background(), dir, []string{"src/main.go", "README.md"})
	if len(ignored) != 0 {
		t.Errorf("expected empty set when no paths match, got %v", ignored)
	}
}

func TestCheckGitIgnored_FailOpen(t *testing.T) {
	// Non-git directory should fail open with empty set.
	dir := t.TempDir()
	ignored := checkGitIgnored(context.Background(), dir, []string{"src/main.go"})
	if len(ignored) != 0 {
		t.Errorf("expected empty set for non-git dir (fail-open), got %v", ignored)
	}
}

func TestExtractPrimaryFile_ToolInputFilePath(t *testing.T) {
	blob, _ := json.Marshal(map[string]any{
		"tool_input": map[string]string{"file_path": "/repo/src/main.go"},
	})
	if got := extractPrimaryFile(blob); got != "/repo/src/main.go" {
		t.Errorf("got %q, want /repo/src/main.go", got)
	}
}

func TestExtractPrimaryFile_ToolInputPath(t *testing.T) {
	// Copilot uses "path" in tool_input.
	blob, _ := json.Marshal(map[string]any{
		"tool_input": map[string]string{"path": "/repo/src/handler.ts"},
	})
	if got := extractPrimaryFile(blob); got != "/repo/src/handler.ts" {
		t.Errorf("got %q, want /repo/src/handler.ts", got)
	}
}

func TestExtractPrimaryFile_TopLevel(t *testing.T) {
	blob, _ := json.Marshal(map[string]any{
		"file_path": "/repo/src/config.go",
	})
	if got := extractPrimaryFile(blob); got != "/repo/src/config.go" {
		t.Errorf("got %q", got)
	}
}

func TestExtractPrimaryFile_Empty(t *testing.T) {
	blob, _ := json.Marshal(map[string]any{
		"tool_input": map[string]string{"command": "go test"},
	})
	if got := extractPrimaryFile(blob); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

// filterIgnoredSteps tests.

func TestFilterIgnoredSteps_IgnoredFileDropped(t *testing.T) {
	dir := initGitRepoWithIgnore(t, "node_modules/\n")
	bs := testBlobStore(t)
	ctx := context.Background()

	steps := []sqldb.ListStepEventsForTurnRow{
		{
			EventID:     "evt-1",
			ToolName:    sql.NullString{String: "Write", Valid: true},
			ToolUses:    toolUsesJSON("Write", "node_modules/lodash/index.js", "write"),
			EventSource: "hook",
		},
	}

	filtered := filterIgnoredSteps(ctx, dir, steps, bs)
	if len(filtered) != 0 {
		t.Errorf("expected 0 steps (all gitignored), got %d", len(filtered))
	}
}

func TestFilterIgnoredSteps_VisibleFileKept(t *testing.T) {
	dir := initGitRepoWithIgnore(t, "node_modules/\n")
	bs := testBlobStore(t)
	ctx := context.Background()

	steps := []sqldb.ListStepEventsForTurnRow{
		{
			EventID:     "evt-1",
			ToolName:    sql.NullString{String: "Edit", Valid: true},
			ToolUses:    toolUsesJSON("Edit", "src/main.go", "edit"),
			EventSource: "hook",
		},
	}

	filtered := filterIgnoredSteps(ctx, dir, steps, bs)
	if len(filtered) != 1 {
		t.Fatalf("expected 1 step, got %d", len(filtered))
	}
	if len(filtered[0].FilePaths) != 1 || filtered[0].FilePaths[0] != "src/main.go" {
		t.Errorf("file_paths = %v, want [src/main.go]", filtered[0].FilePaths)
	}
}

func TestFilterIgnoredSteps_MixedWithVisiblePrimary(t *testing.T) {
	dir := initGitRepoWithIgnore(t, "node_modules/\n")
	bs := testBlobStore(t)
	ctx := context.Background()

	// Provenance blob with visible primary file.
	provBlob, _ := json.Marshal(map[string]any{
		"tool_input": map[string]string{"file_path": filepath.Join(dir, "src/handler.go")},
	})
	provHash := putBlob(t, bs, provBlob)

	steps := []sqldb.ListStepEventsForTurnRow{
		{
			EventID:        "evt-1",
			ToolName:       sql.NullString{String: "Edit", Valid: true},
			ToolUses:       multiToolUsesJSON("Edit", []string{"src/handler.go", "node_modules/express/index.js"}),
			ProvenanceHash: sql.NullString{String: provHash, Valid: true},
			EventSource:    "hook",
		},
	}

	filtered := filterIgnoredSteps(ctx, dir, steps, bs)
	if len(filtered) != 1 {
		t.Fatalf("expected 1 step, got %d", len(filtered))
	}
	// file_paths should be filtered to visible only.
	if len(filtered[0].FilePaths) != 1 || filtered[0].FilePaths[0] != "src/handler.go" {
		t.Errorf("file_paths = %v, want [src/handler.go]", filtered[0].FilePaths)
	}
	// Provenance should be preserved (primary file is visible).
	if !filtered[0].Row.ProvenanceHash.Valid {
		t.Error("provenance_hash should be preserved when primary file is visible")
	}
}

func TestFilterIgnoredSteps_MixedNoPrimaryClearsProvenance(t *testing.T) {
	dir := initGitRepoWithIgnore(t, "node_modules/\n")
	bs := testBlobStore(t)
	ctx := context.Background()

	steps := []sqldb.ListStepEventsForTurnRow{
		{
			EventID:        "evt-1",
			ToolName:       sql.NullString{String: "Edit", Valid: true},
			ToolUses:       multiToolUsesJSON("Edit", []string{"src/main.go", "node_modules/lodash/index.js"}),
			ProvenanceHash: sql.NullString{String: "some_hash", Valid: true},
			EventSource:    "transcript",
			// No provenance blob in CAS (can't determine primary file).
		},
	}

	filtered := filterIgnoredSteps(ctx, dir, steps, bs)
	if len(filtered) != 1 {
		t.Fatalf("expected 1 step (mixed, has visible), got %d", len(filtered))
	}
	if filtered[0].Row.ProvenanceHash.Valid {
		t.Error("provenance_hash should be cleared when no primary file determinable and mixed paths")
	}
	if len(filtered[0].FilePaths) != 1 || filtered[0].FilePaths[0] != "src/main.go" {
		t.Errorf("file_paths = %v, want [src/main.go]", filtered[0].FilePaths)
	}
}

func TestFilterIgnoredSteps_PathlessBashUnchanged(t *testing.T) {
	dir := initGitRepoWithIgnore(t, "node_modules/\n")
	bs := testBlobStore(t)
	ctx := context.Background()

	steps := []sqldb.ListStepEventsForTurnRow{
		{
			EventID:     "evt-1",
			ToolName:    sql.NullString{String: "Bash", Valid: true},
			EventSource: "hook",
		},
	}

	filtered := filterIgnoredSteps(ctx, dir, steps, bs)
	if len(filtered) != 1 {
		t.Fatalf("expected 1 step (pathless Bash kept), got %d", len(filtered))
	}
}

func TestFilterIgnoredSteps_IgnoredPrimaryEmptyToolUses(t *testing.T) {
	dir := initGitRepoWithIgnore(t, "node_modules/\n")
	bs := testBlobStore(t)
	ctx := context.Background()

	// Step has provenance blob with file_path in node_modules, but no tool_uses.
	// This should still be dropped because the primary file is gitignored.
	provBlob, _ := json.Marshal(map[string]any{
		"tool_input": map[string]string{"file_path": filepath.Join(dir, "node_modules/lodash/index.js")},
	})
	provHash := putBlob(t, bs, provBlob)

	steps := []sqldb.ListStepEventsForTurnRow{
		{
			EventID:        "evt-1",
			ToolName:       sql.NullString{String: "Write", Valid: true},
			ProvenanceHash: sql.NullString{String: provHash, Valid: true},
			EventSource:    "hook",
			// No ToolUses set -- file path only in provenance blob.
		},
	}

	filtered := filterIgnoredSteps(ctx, dir, steps, bs)
	if len(filtered) != 0 {
		t.Errorf("expected 0 steps (ignored primary with empty tool_uses), got %d", len(filtered))
	}
}

func TestFilterIgnoredSteps_VisiblePrimaryEmptyToolUses(t *testing.T) {
	dir := initGitRepoWithIgnore(t, "node_modules/\n")
	bs := testBlobStore(t)
	ctx := context.Background()

	// Step has provenance blob with visible file_path, but no tool_uses.
	// Should be kept.
	provBlob, _ := json.Marshal(map[string]any{
		"tool_input": map[string]string{"file_path": filepath.Join(dir, "src/main.go")},
	})
	provHash := putBlob(t, bs, provBlob)

	steps := []sqldb.ListStepEventsForTurnRow{
		{
			EventID:        "evt-1",
			ToolName:       sql.NullString{String: "Write", Valid: true},
			ProvenanceHash: sql.NullString{String: provHash, Valid: true},
			EventSource:    "hook",
		},
	}

	filtered := filterIgnoredSteps(ctx, dir, steps, bs)
	if len(filtered) != 1 {
		t.Fatalf("expected 1 step (visible primary with empty tool_uses), got %d", len(filtered))
	}
}

func TestFilterIgnoredSteps_StepCountReduced(t *testing.T) {
	dir := initGitRepoWithIgnore(t, "node_modules/\nvendor/\n")
	bs := testBlobStore(t)
	ctx := context.Background()

	steps := []sqldb.ListStepEventsForTurnRow{
		{EventID: "keep-1", ToolName: sql.NullString{String: "Edit", Valid: true}, ToolUses: toolUsesJSON("Edit", "src/main.go", "edit"), EventSource: "hook"},
		{EventID: "drop-1", ToolName: sql.NullString{String: "Write", Valid: true}, ToolUses: toolUsesJSON("Write", "node_modules/pkg/a.js", "write"), EventSource: "hook"},
		{EventID: "keep-2", ToolName: sql.NullString{String: "Bash", Valid: true}, EventSource: "hook"},
		{EventID: "drop-2", ToolName: sql.NullString{String: "Edit", Valid: true}, ToolUses: toolUsesJSON("Edit", "vendor/lib/b.go", "edit"), EventSource: "hook"},
	}

	filtered := filterIgnoredSteps(ctx, dir, steps, bs)
	if len(filtered) != 2 {
		ids := make([]string, len(filtered))
		for i, f := range filtered {
			ids[i] = f.Row.EventID
		}
		t.Fatalf("expected 2 steps, got %d: %v", len(filtered), ids)
	}
	if filtered[0].Row.EventID != "keep-1" || filtered[1].Row.EventID != "keep-2" {
		t.Errorf("wrong steps kept: %s, %s", filtered[0].Row.EventID, filtered[1].Row.EventID)
	}
}

// multiToolUsesJSON builds a tool_uses JSON with multiple file paths.
func multiToolUsesJSON(toolName string, filePaths []string) sql.NullString {
	type tool struct {
		Name     string `json:"name"`
		FilePath string `json:"file_path,omitempty"`
		FileOp   string `json:"file_op,omitempty"`
	}
	type payload struct {
		ContentTypes []string `json:"content_types"`
		Tools        []tool   `json:"tools"`
	}
	var tools []tool
	for _, fp := range filePaths {
		tools = append(tools, tool{Name: toolName, FilePath: fp, FileOp: "edit"})
	}
	b, _ := json.Marshal(payload{
		ContentTypes: []string{"tool_use"},
		Tools:        tools,
	})
	return sql.NullString{String: string(b), Valid: true}
}
