package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"
)

// Protocol tests.

func TestHandleInitialize(t *testing.T) {
	s := NewServer("/tmp/test")

	req := &jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "initialize",
	}

	resp := s.handleRequest(t.Context(), req)
	if resp == nil {
		t.Fatal("expected response")
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatal("result is not a map")
	}

	if result["protocolVersion"] != "2024-11-05" {
		t.Errorf("protocolVersion = %v", result["protocolVersion"])
	}

	info, ok := result["serverInfo"].(map[string]any)
	if !ok {
		t.Fatal("serverInfo is not a map")
	}
	if info["name"] != "semantica" {
		t.Errorf("name = %v", info["name"])
	}
}

func TestHandleToolsList(t *testing.T) {
	s := NewServer("/tmp/test")

	req := &jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  "tools/list",
	}

	resp := s.handleRequest(t.Context(), req)
	if resp == nil {
		t.Fatal("expected response")
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatal("result is not a map")
	}

	tools, ok := result["tools"].([]toolDef)
	if !ok {
		t.Fatal("tools is not a []toolDef")
	}

	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Name] = true
	}

	for _, expected := range []string{"semantica_explain"} {
		if !names[expected] {
			t.Errorf("missing tool: %s", expected)
		}
	}
}

func TestHandleUnknownMethod(t *testing.T) {
	s := NewServer("/tmp/test")

	req := &jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`3`),
		Method:  "unknown/method",
	}

	resp := s.handleRequest(t.Context(), req)
	if resp == nil {
		t.Fatal("expected response")
	}
	if resp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("error code = %d, want -32601", resp.Error.Code)
	}
}

func TestHandleNotification_NoResponse(t *testing.T) {
	s := NewServer("/tmp/test")

	// notifications/initialized has no ID - should return nil.
	req := &jsonrpcRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	}

	resp := s.handleRequest(t.Context(), req)
	if resp != nil {
		t.Error("notifications should not produce a response")
	}
}

// tools/call error path tests.

func TestHandleToolsCall_UnknownTool(t *testing.T) {
	s := NewServer("/tmp/test")

	params, _ := json.Marshal(map[string]any{
		"name":      "nonexistent_tool",
		"arguments": map[string]any{},
	})

	req := &jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`10`),
		Method:  "tools/call",
		Params:  params,
	}

	resp := s.handleRequest(t.Context(), req)
	if resp == nil {
		t.Fatal("expected response")
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatal("result is not a map")
	}
	if result["isError"] != true {
		t.Error("expected isError: true for unknown tool")
	}
}

func TestHandleToolsCall_InvalidParams(t *testing.T) {
	s := NewServer("/tmp/test")

	req := &jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`11`),
		Method:  "tools/call",
		Params:  json.RawMessage(`not json`),
	}

	resp := s.handleRequest(t.Context(), req)
	if resp == nil {
		t.Fatal("expected response")
	}
	if resp.Error == nil {
		t.Fatal("expected JSON-RPC error for invalid params")
	}
	if resp.Error.Code != -32602 {
		t.Errorf("error code = %d, want -32602", resp.Error.Code)
	}
}

func TestHandleToolsCall_ExplainBadRepo(t *testing.T) {
	s := NewServer("/tmp/nonexistent-repo-path")

	params, _ := json.Marshal(map[string]any{
		"name":      "semantica_explain",
		"arguments": map[string]any{"ref": "abc123"},
	})

	req := &jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`13`),
		Method:  "tools/call",
		Params:  params,
	}

	resp := s.handleRequest(t.Context(), req)
	if resp == nil {
		t.Fatal("expected response")
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatal("result is not a map")
	}
	if result["isError"] != true {
		t.Error("expected isError: true for bad repo")
	}
}

// tools/call success path tests.
//
// These tests create a real git repo with .semantica/, run migrations,
// seed the DB with test data, and call each tool through the server.

// initTestRepo creates a git repo with .semantica/ fully initialised,
// a checkpoint linked to a real commit, a playbook summary, and an FTS entry.
// It returns the repo root and the commit hash of the seeded commit.
func initTestRepo(t *testing.T) (repoRoot string, commitHash string) {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	// Create a real git repo with one commit.
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "main.go"},
		{"commit", "-m", "add main.go"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Get the commit hash.
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.TrimSpace(string(out))

	// Create .semantica directory and settings.
	semDir := filepath.Join(dir, ".semantica")
	if err := os.MkdirAll(filepath.Join(semDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := util.WriteSettings(semDir, util.Settings{
		Enabled: true,
		Version: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// Run migrations and seed the DB.
	ctx := context.Background()
	dbPath := filepath.Join(semDir, "lineage.db")
	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatal(err)
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()

	repoID, err := sqlstore.EnsureRepository(ctx, h.Queries, dir)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UnixMilli()
	cpID := uuid.NewString()

	// Insert a completed checkpoint.
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpID,
		RepositoryID: repoID,
		Kind:         "auto",
		Status:       "complete",
		ManifestHash: sql.NullString{String: "fakehash", Valid: true},
		CreatedAt:    now,
		CompletedAt:  sql.NullInt64{Int64: now, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	// Link the commit to the checkpoint.
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash:   hash,
		RepositoryID: repoID,
		CheckpointID: cpID,
		LinkedAt:     now,
	}); err != nil {
		t.Fatal(err)
	}

	// Save a playbook summary on the checkpoint.
	summaryJSON := `{"title":"Add main entry point","intent":"Bootstrap the project","outcome":"Created main.go","learnings":["Go modules"],"friction":[],"open_items":[],"keywords":["main","bootstrap"]}`
	if err := h.Queries.SaveCheckpointSummary(ctx, sqldb.SaveCheckpointSummaryParams{
		CheckpointID: cpID,
		SummaryJson:  sql.NullString{String: summaryJSON, Valid: true},
		SummaryModel: sql.NullString{String: "test-model", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	return dir, hash
}

// toolCallResponse is a helper to decode the MCP content array from a tools/call result.
func toolCallResponse(t *testing.T, resp *jsonrpcResponse) (text string, isError bool) {
	t.Helper()
	if resp == nil {
		t.Fatal("nil response")
	}
	if resp.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: %v", resp.Error)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatal("result is not a map")
	}

	if ie, ok := result["isError"]; ok {
		isError, _ = ie.(bool)
	}

	content, ok := result["content"].([]map[string]any)
	if !ok || len(content) == 0 {
		t.Fatal("expected content array")
	}
	text, _ = content[0]["text"].(string)
	return text, isError
}

func TestHandleToolsCall_ExplainSuccess(t *testing.T) {
	repoRoot, commitHash := initTestRepo(t)
	s := NewServer(repoRoot)

	params, _ := json.Marshal(map[string]any{
		"name":      "semantica_explain",
		"arguments": map[string]any{"ref": commitHash},
	})

	req := &jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`22`),
		Method:  "tools/call",
		Params:  params,
	}

	resp := s.handleRequest(t.Context(), req)
	text, isError := toolCallResponse(t, resp)

	if isError {
		t.Fatalf("expected success, got error: %s", text)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatalf("result is not valid JSON: %v\ntext: %s", err, text)
	}

	if result["commit_hash"] != commitHash {
		t.Errorf("commit_hash = %v, want %s", result["commit_hash"], commitHash)
	}
	if _, ok := result["ai_percentage"]; !ok {
		t.Error("result missing ai_percentage field")
	}
	if _, ok := result["files_changed"]; !ok {
		t.Error("result missing files_changed field")
	}

	// Should include the persisted summary.
	if result["summary"] == nil {
		t.Error("expected summary to be present")
	}
}

func TestHandleToolsCall_ExplainByPrefix(t *testing.T) {
	repoRoot, commitHash := initTestRepo(t)
	s := NewServer(repoRoot)

	// Use an 8-char prefix instead of the full hash.
	prefix := commitHash[:8]

	params, _ := json.Marshal(map[string]any{
		"name":      "semantica_explain",
		"arguments": map[string]any{"ref": prefix},
	})

	req := &jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`24`),
		Method:  "tools/call",
		Params:  params,
	}

	resp := s.handleRequest(t.Context(), req)
	text, isError := toolCallResponse(t, resp)

	if isError {
		t.Fatalf("expected success with prefix, got error: %s", text)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	if result["commit_hash"] != commitHash {
		t.Errorf("commit_hash = %v, want %s", result["commit_hash"], commitHash)
	}
}

// tool definition tests.

func TestToolDefinitions_HasRequiredFields(t *testing.T) {
	tools := toolDefinitions()

	for _, tool := range tools {
		if tool.Name == "" {
			t.Error("tool has empty name")
		}
		if tool.Description == "" {
			t.Errorf("tool %s has empty description", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %s has nil input schema", tool.Name)
		}
	}
}
