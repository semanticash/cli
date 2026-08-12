package scoring

import (
	"strings"
	"testing"
)

func TestParseDiff_GroupsByContiguousAddedLines(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/main.go b/main.go",
		"--- /dev/null",
		"+++ b/main.go",
		"@@ -0,0 +1,5 @@",
		"+package main",
		"+func main() {",
		"+}",
		"",
	}, "\n")

	dr := ParseDiff([]byte(diff))

	if len(dr.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(dr.Files))
	}
	if dr.Files[0].Path != "main.go" {
		t.Errorf("path = %q, want main.go", dr.Files[0].Path)
	}
	if len(dr.FilesCreated) != 1 || dr.FilesCreated[0] != "main.go" {
		t.Errorf("FilesCreated = %v, want [main.go]", dr.FilesCreated)
	}
	if len(dr.Files[0].Groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(dr.Files[0].Groups))
	}
	if len(dr.Files[0].Groups[0].Lines) != 3 {
		t.Errorf("group lines = %d, want 3", len(dr.Files[0].Groups[0].Lines))
	}
}

func TestParseDiff_DeletedFile(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/old.go b/old.go",
		"--- a/old.go",
		"+++ /dev/null",
		"@@ -1,2 +0,0 @@",
		"-package old",
		"-func Legacy() {}",
		"",
	}, "\n")

	dr := ParseDiff([]byte(diff))

	if len(dr.FilesDeleted) != 1 || dr.FilesDeleted[0] != "old.go" {
		t.Errorf("FilesDeleted = %v, want [old.go]", dr.FilesDeleted)
	}
}

func TestParseDiff_MultipleFiles(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/a.go b/a.go",
		"--- /dev/null",
		"+++ b/a.go",
		"@@ -0,0 +1,1 @@",
		"+package a",
		"diff --git a/b.go b/b.go",
		"--- /dev/null",
		"+++ b/b.go",
		"@@ -0,0 +1,1 @@",
		"+package b",
		"",
	}, "\n")

	dr := ParseDiff([]byte(diff))

	if len(dr.Files) != 2 {
		t.Fatalf("files = %d, want 2", len(dr.Files))
	}
	if dr.Files[0].Path != "a.go" || dr.Files[1].Path != "b.go" {
		t.Errorf("paths = [%s, %s]", dr.Files[0].Path, dr.Files[1].Path)
	}
}

func TestParseDiff_BinaryFiles(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/new.bin b/new.bin",
		"new file mode 100644",
		"Binary files /dev/null and b/new.bin differ",
		"diff --git a/existing.bin b/existing.bin",
		"Binary files a/existing.bin and b/existing.bin differ",
		"diff --git a/old.bin b/old.bin",
		"deleted file mode 100644",
		"Binary files a/old.bin and /dev/null differ",
		"",
	}, "\n")

	dr := ParseDiff([]byte(diff))
	if len(dr.Files) != 3 {
		t.Fatalf("files = %+v, want three binary files", dr.Files)
	}
	wantPaths := []string{"new.bin", "existing.bin", "old.bin"}
	for i, want := range wantPaths {
		if dr.Files[i].Path != want || len(dr.Files[i].Groups) != 0 {
			t.Errorf("Files[%d] = %+v, want zero-line %q", i, dr.Files[i], want)
		}
	}
	if len(dr.FilesCreated) != 1 || dr.FilesCreated[0] != "new.bin" {
		t.Errorf("FilesCreated = %v, want [new.bin]", dr.FilesCreated)
	}
	if len(dr.FilesDeleted) != 1 || dr.FilesDeleted[0] != "old.bin" {
		t.Errorf("FilesDeleted = %v, want [old.bin]", dr.FilesDeleted)
	}
}
