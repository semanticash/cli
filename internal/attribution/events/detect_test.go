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
	paths := ExtractProviderFileTouches(tu)
	if len(paths) != 2 {
		t.Fatalf("got %d paths, want 2", len(paths))
	}
	if paths[0] != "main.go" || paths[1] != "handler.go" {
		t.Errorf("paths = %v", paths)
	}
}
