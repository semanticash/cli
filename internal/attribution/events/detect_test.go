package events

import "testing"

func TestHasProviderFileEdit_KiroFileEdit(t *testing.T) {
	tu := `{"content_types":["kiro_file_edit"],"tools":[{"name":"kiro_file_edit","file_path":"main.go","file_op":"edit"}]}`
	if !HasProviderFileEdit(tu) {
		t.Error("expected true for kiro_file_edit")
	}
}

func TestHasProviderFileEdit_UnknownTool(t *testing.T) {
	tu := `{"content_types":["tool_use"],"tools":[{"name":"ReadFile","file_path":"main.go"}]}`
	if HasProviderFileEdit(tu) {
		t.Error("expected false for ReadFile tool")
	}
}

func TestHasProviderFileEdit_Empty(t *testing.T) {
	if HasProviderFileEdit("") {
		t.Error("expected false for empty tool_uses")
	}
}

func TestHasEditOrWrite_Write(t *testing.T) {
	tu := `{"content_types":["tool_use"],"tools":[{"name":"Write","file_path":"main.go"}]}`
	if !HasEditOrWrite(tu) {
		t.Error("expected true for Write tool")
	}
}

func TestHasEditOrWrite_Edit(t *testing.T) {
	tu := `{"content_types":["tool_use"],"tools":[{"name":"Edit","file_path":"main.go"}]}`
	if !HasEditOrWrite(tu) {
		t.Error("expected true for Edit tool")
	}
}

func TestHasEditOrWrite_Empty(t *testing.T) {
	if HasEditOrWrite("") {
		t.Error("expected false for empty tool_uses")
	}
}

func TestExtractProviderFileTouches(t *testing.T) {
	tu := `{"tools":[{"name":"cursor_edit","file_path":"main.go"},{"name":"cursor_edit","file_path":"handler.go"}]}`
	paths := ExtractProviderFileTouches(tu, "")
	if len(paths) != 2 {
		t.Fatalf("got %d paths, want 2", len(paths))
	}
	if paths[0] != "main.go" || paths[1] != "handler.go" {
		t.Errorf("paths = %v", paths)
	}
}

func TestExtractProviderFileTouches_RelativizesAbsolutePathsAgainstRepoRoot(t *testing.T) {
	// A provider that stored absolute paths in tool_uses (e.g. Codex
	// apply_patch with envelope paths joined against the hook payload's
	// cwd) must come out keyed by the repo-relative path so the
	// scorer's diff-keyed lookup matches git's repo-relative file
	// names. Subdir-cwd sessions are the case where this matters:
	// envelope path "main.go" with cwd "/repo/pkg" must surface as
	// "pkg/main.go" at candidate-build time.
	tu := `{"tools":[{"name":"codex_file_edit","file_path":"/repo/pkg/main.go"}]}`
	paths := ExtractProviderFileTouches(tu, "/repo")
	if len(paths) != 1 || paths[0] != "pkg/main.go" {
		t.Errorf("paths = %v, want [pkg/main.go]", paths)
	}
}

func TestExtractProviderFileTouches_LeavesRelativePathsUntouched(t *testing.T) {
	// Providers whose hook payload cwd matched the repo root at emit
	// time stored already-relative paths. Calling NormalizePath on a
	// relative path with an absolute repoRoot would lose subdirectory
	// components (filepath.Rel errors out and falls through to
	// filepath.Base). The relativization gate must be limited to
	// paths that look absolute.
	tu := `{"tools":[{"name":"cursor_edit","file_path":"pkg/sub/handler.go"}]}`
	paths := ExtractProviderFileTouches(tu, "/repo")
	if len(paths) != 1 || paths[0] != "pkg/sub/handler.go" {
		t.Errorf("paths = %v, want [pkg/sub/handler.go]", paths)
	}
}
