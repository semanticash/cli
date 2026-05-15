package codex

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// abs builds an absolute path for the test binary's target OS. That
// keeps normalizePatchPath tests on the absolute-path branch on every
// platform.
func abs(parts ...string) string {
	joined := filepath.Join(parts...)
	if filepath.IsAbs(joined) {
		return joined
	}
	if runtime.GOOS == "windows" {
		return filepath.Join("C:\\", joined)
	}
	return string(filepath.Separator) + joined
}

// Shared fixture paths. Computed once via abs() so every test sees
// the same absolute repo root for the target OS.
var (
	fixtureRepo          = abs("tmp", "codex-fixture", "repo")
	fixtureTranscript    = abs("tmp", "codex-fixture", "sessions", "2026", "05", "13", "rollout-test.jsonl")
	fixtureTranscriptAlt = abs("tmp", "codex-fixture", "sessions", "rollout-test.jsonl")
)

func TestParseApplyPatchEnvelope_AddFileEmitsAllLines(t *testing.T) {
	repoRoot := abs("tmp", "repo")
	envelope := strings.Join([]string{
		"*** Begin Patch",
		"*** Add File: " + abs("tmp", "repo", "main.go"),
		"+package main",
		"+",
		"+func main() {",
		"+\tprintln(\"hi\")",
		"+}",
		"*** End Patch",
	}, "\n")

	files := parseApplyPatchEnvelope(envelope, repoRoot)
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1", len(files))
	}
	if files[0].op != applyPatchOpAdd {
		t.Errorf("op = %v, want Add", files[0].op)
	}
	if files[0].path != "main.go" {
		t.Errorf("path = %q, want main.go", files[0].path)
	}
	wantContent := "package main\n\nfunc main() {\n\tprintln(\"hi\")\n}"
	if files[0].content != wantContent {
		t.Errorf("content mismatch:\n got: %q\nwant: %q", files[0].content, wantContent)
	}
}

func TestParseApplyPatchEnvelope_UpdateFileEmitsOnlyAddedLines(t *testing.T) {
	envelope := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: main.go",
		"@@",
		" package main",
		"+",
		"+func main() {",
		"+\tprintln(\"hi\")",
		"+}",
		"*** End Patch",
	}, "\n")

	files := parseApplyPatchEnvelope(envelope, "/tmp/repo")
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1", len(files))
	}
	if files[0].op != applyPatchOpUpdate {
		t.Errorf("op = %v, want Update", files[0].op)
	}
	if files[0].path != "main.go" {
		t.Errorf("path = %q, want main.go", files[0].path)
	}
	wantContent := "\nfunc main() {\n\tprintln(\"hi\")\n}"
	if files[0].content != wantContent {
		t.Errorf("content = %q, want %q", files[0].content, wantContent)
	}
}

func TestParseApplyPatchEnvelope_UpdateSkipsRemovedAndContextLines(t *testing.T) {
	envelope := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: main.go",
		"@@",
		" func main() {",
		"-\tprintln(\"old\")",
		"+\tprintln(\"new\")",
		" }",
		"*** End Patch",
	}, "\n")

	files := parseApplyPatchEnvelope(envelope, "/tmp/repo")
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1", len(files))
	}
	if files[0].content != "\tprintln(\"new\")" {
		t.Errorf("content = %q, want only the + line", files[0].content)
	}
}

func TestParseApplyPatchEnvelope_DeleteEmitsNoContent(t *testing.T) {
	repoRoot := abs("tmp", "repo")
	envelope := strings.Join([]string{
		"*** Begin Patch",
		"*** Delete File: " + abs("tmp", "repo", "legacy.go"),
		"*** End Patch",
	}, "\n")

	files := parseApplyPatchEnvelope(envelope, repoRoot)
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1", len(files))
	}
	if files[0].op != applyPatchOpDelete {
		t.Errorf("op = %v, want Delete", files[0].op)
	}
	if files[0].path != "legacy.go" {
		t.Errorf("path = %q, want legacy.go", files[0].path)
	}
	if files[0].content != "" {
		t.Errorf("delete should not carry content, got %q", files[0].content)
	}
}

func TestParseApplyPatchEnvelope_MoveTracksBothPaths(t *testing.T) {
	envelope := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: old/path.go",
		"*** Move to: new/path.go",
		"@@",
		"+package newpath",
		"*** End Patch",
	}, "\n")

	files := parseApplyPatchEnvelope(envelope, "/tmp/repo")
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1", len(files))
	}
	if files[0].op != applyPatchOpMove {
		t.Errorf("op = %v, want Move", files[0].op)
	}
	if files[0].path != "old/path.go" {
		t.Errorf("path = %q, want old/path.go", files[0].path)
	}
	if files[0].movedTo != "new/path.go" {
		t.Errorf("movedTo = %q, want new/path.go", files[0].movedTo)
	}
	if files[0].content != "package newpath" {
		t.Errorf("content = %q, want body retained", files[0].content)
	}
}

func TestParseApplyPatchEnvelope_MultipleFilesInOneEnvelope(t *testing.T) {
	envelope := strings.Join([]string{
		"*** Begin Patch",
		"*** Delete File: main.go",
		"*** Add File: main.go",
		"+package main",
		"+",
		"+func main() {}",
		"*** End Patch",
	}, "\n")

	files := parseApplyPatchEnvelope(envelope, "/tmp/repo")
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2", len(files))
	}
	if files[0].op != applyPatchOpDelete || files[0].path != "main.go" {
		t.Errorf("first entry not delete-main.go: %+v", files[0])
	}
	if files[1].op != applyPatchOpAdd || files[1].path != "main.go" {
		t.Errorf("second entry not add-main.go: %+v", files[1])
	}
	if files[1].content != "package main\n\nfunc main() {}" {
		t.Errorf("add content mismatch: %q", files[1].content)
	}
}

func TestNormalizePatchPath_RelativizesAbsoluteUnderRepo(t *testing.T) {
	repoRoot := abs("tmp", "repo")
	cases := []struct {
		name     string
		raw      string
		repoRoot string
		want     string
	}{
		{
			name:     "absolute inside repo",
			raw:      abs("tmp", "repo", "pkg", "main.go"),
			repoRoot: repoRoot,
			want:     "pkg/main.go",
		},
		{
			name:     "relative passes through unchanged",
			raw:      "pkg/main.go",
			repoRoot: repoRoot,
			want:     "pkg/main.go",
		},
		{
			name:     "absolute outside repo retained as-is",
			raw:      abs("etc", "passwd"),
			repoRoot: repoRoot,
			// filepath.Clean canonicalizes path separators per OS;
			// ToSlash then renders forward slashes on every OS.
			want: filepath.ToSlash(abs("etc", "passwd")),
		},
		{
			name:     "no repoRoot leaves absolute path",
			raw:      abs("tmp", "repo", "main.go"),
			repoRoot: "",
			want:     filepath.ToSlash(abs("tmp", "repo", "main.go")),
		},
		{
			name:     "trailing whitespace stripped",
			raw:      "  pkg/main.go  ",
			repoRoot: repoRoot,
			want:     "pkg/main.go",
		},
		{
			// Names that merely start with two dots are still valid
			// repo-internal relative paths. Only a literal ".." segment
			// escapes the repo.
			name:     "name starting with dotdot stays inside repo",
			raw:      "..generated/file.go",
			repoRoot: repoRoot,
			want:     "..generated/file.go",
		},
		{
			name:     "actual parent-escape returns absolute fallback",
			raw:      abs("etc", "hosts"),
			repoRoot: repoRoot,
			want:     filepath.ToSlash(abs("etc", "hosts")),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizePatchPath(tc.raw, tc.repoRoot)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
