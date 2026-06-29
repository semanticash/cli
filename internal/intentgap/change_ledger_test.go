package intentgap

import (
	"strings"
	"testing"
)

// An empty diff produces an empty ledger that is still safe to index.
func TestBuildChangeLedger_EmptyDiff(t *testing.T) {
	got := BuildChangeLedger(nil)
	if len(got.Files) != 0 {
		t.Errorf("Files = %v, want empty", got.Files)
	}
	if got.ByPath == nil {
		t.Errorf("ByPath should be a non-nil map for safe indexing")
	}
}

// One additive hunk on one file: parsed into a single ChangedFile with
// one ChangedHunk whose direction is Added (pre-image count is zero).
func TestBuildChangeLedger_SingleAdditiveHunk(t *testing.T) {
	diff := []byte("--- a/foo.go\n+++ b/foo.go\n@@ -0,0 +1,2 @@\n+package foo\n+\n")
	got := BuildChangeLedger(diff)
	if len(got.Files) != 1 {
		t.Fatalf("Files len = %d, want 1", len(got.Files))
	}
	f := got.Files[0]
	if f.Path != "foo.go" {
		t.Errorf("Path = %q, want foo.go", f.Path)
	}
	if f.Category != CatCode {
		t.Errorf("Category = %q, want code", f.Category)
	}
	if len(f.Hunks) != 1 {
		t.Fatalf("Hunks len = %d, want 1", len(f.Hunks))
	}
	h := f.Hunks[0]
	if h.Direction != HunkAdded {
		t.Errorf("Direction = %q, want added", h.Direction)
	}
	if h.StartLine != 1 || h.EndLine != 2 {
		t.Errorf("Lines = %d-%d, want 1-2", h.StartLine, h.EndLine)
	}
	if !strings.Contains(h.Body, "+package foo") {
		t.Errorf("Body missing added line; got %q", h.Body)
	}
	if got.ByPath["foo.go"] == nil {
		t.Errorf("ByPath did not index foo.go")
	}
}

// Pure removal: pre-image count > 0, post-image count == 0. The hunk
// keeps a sensible line range (anchored to the pre-image start line)
// so downstream consumers do not see EndLine < StartLine.
func TestBuildChangeLedger_RemovedHunkAnchorsLineRange(t *testing.T) {
	diff := []byte("--- a/foo.go\n+++ b/foo.go\n@@ -10,2 +9,0 @@\n-line one\n-line two\n")
	got := BuildChangeLedger(diff)
	if len(got.Files) != 1 || len(got.Files[0].Hunks) != 1 {
		t.Fatalf("expected one file with one hunk, got %+v", got)
	}
	h := got.Files[0].Hunks[0]
	if h.Direction != HunkRemoved {
		t.Errorf("Direction = %q, want removed", h.Direction)
	}
	if h.StartLine == 0 || h.EndLine < h.StartLine {
		t.Errorf("Lines invalid: %d-%d", h.StartLine, h.EndLine)
	}
}

// A hunk with non-zero counts on both sides labels as Changed. The
// label is about hunk shape, not semantic intent: the body may add,
// remove, or rewrite lines, and downstream callers must read the body
// to distinguish those cases.
func TestBuildChangeLedger_ChangedHunk(t *testing.T) {
	diff := []byte("--- a/foo.go\n+++ b/foo.go\n@@ -5,3 +5,4 @@\n a\n-b\n+B\n c\n+d\n")
	got := BuildChangeLedger(diff)
	if got.Files[0].Hunks[0].Direction != HunkChanged {
		t.Errorf("Direction = %q, want changed", got.Files[0].Hunks[0].Direction)
	}
}

// Multiple files in one diff: each gets its own ChangedFile and the
// hunk bodies do not bleed across file boundaries.
func TestBuildChangeLedger_MultipleFilesDoNotBleed(t *testing.T) {
	diff := []byte("" +
		"--- a/one.go\n+++ b/one.go\n@@ -0,0 +1,1 @@\n+marker-one\n" +
		"--- a/two.go\n+++ b/two.go\n@@ -0,0 +1,1 @@\n+marker-two\n",
	)
	got := BuildChangeLedger(diff)
	if len(got.Files) != 2 {
		t.Fatalf("Files len = %d, want 2", len(got.Files))
	}
	if !strings.Contains(got.Files[0].Hunks[0].Body, "marker-one") ||
		strings.Contains(got.Files[0].Hunks[0].Body, "marker-two") {
		t.Errorf("hunk body bled across files: %q", got.Files[0].Hunks[0].Body)
	}
	if !strings.Contains(got.Files[1].Hunks[0].Body, "marker-two") ||
		strings.Contains(got.Files[1].Hunks[0].Body, "marker-one") {
		t.Errorf("hunk body bled across files: %q", got.Files[1].Hunks[0].Body)
	}
}

// Pure-deletion files (+++ /dev/null) DO enter the ledger, keyed on
// the old-side path with Deleted=true. Retrieval needs to see the
// deletion so an intent like "delete X" can match the diff without
// emitting a Track A "nothing addressed this ask" diagnostic. The
// adjudicator separately enforces that final under_impl findings cite
// regions in the new-side diff; that constraint is upstream of this
// ledger.
func TestBuildChangeLedger_DeletedFilesEnterLedgerWithOldPath(t *testing.T) {
	diff := []byte("--- a/gone.go\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-line one\n-line two\n")
	got := BuildChangeLedger(diff)
	if len(got.Files) != 1 {
		t.Fatalf("Files len = %d, want 1 (deletion must appear)", len(got.Files))
	}
	f := got.Files[0]
	if f.Path != "gone.go" {
		t.Errorf("Path = %q, want gone.go (the old-side path)", f.Path)
	}
	if !f.Deleted {
		t.Errorf("Deleted must be true for a +++ /dev/null entry")
	}
	if len(f.Hunks) != 1 || f.Hunks[0].Direction != HunkRemoved {
		t.Errorf("expected one HunkRemoved; got %+v", f.Hunks)
	}
	if got.ByPath["gone.go"] == nil {
		t.Errorf("ByPath did not index the deleted file's old path")
	}
}

// Hunk body lines whose content begins with `++ ` or `-- ` are
// rendered in the diff as `+++ ...` and `--- ...`. A parser that
// recognizes file headers regardless of context would prematurely
// finalize the current file and bleed body text into a phantom
// successor. The parser MUST keep treating those lines as body until
// the open hunk has consumed its declared pre/post line counts.
//
// This test plants both a `+++ ...` added line and a `--- ...`
// removed line inside one hunk and asserts the parser keeps the
// whole body on one ChangedFile.
func TestBuildChangeLedger_BodyLinesLookingLikeHeadersStayInsideHunk(t *testing.T) {
	diff := []byte("" +
		"--- a/foo.go\n" +
		"+++ b/foo.go\n" +
		"@@ -10,3 +10,4 @@\n" +
		" some context\n" +
		"+++ not-a-file-header\n" +
		"--- also-not-a-file-header\n" +
		"+legit added\n",
	)
	got := BuildChangeLedger(diff)
	if len(got.Files) != 1 {
		t.Fatalf("Files len = %d, want 1 (the +++/--- body lines must NOT split into a second file)", len(got.Files))
	}
	if len(got.Files[0].Hunks) != 1 {
		t.Fatalf("Hunks len = %d, want 1 (the +++/--- body lines must NOT start a second hunk)", len(got.Files[0].Hunks))
	}
	body := got.Files[0].Hunks[0].Body
	for _, want := range []string{
		"+++ not-a-file-header",
		"--- also-not-a-file-header",
		"+legit added",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("hunk body lost %q; got:\n%s", want, body)
		}
	}
}

// A standalone --- a/<path> with no matching +++ line (truncated
// diffs, malformed input) must not enter the ledger. The parser
// drops the file entirely instead of leaving a partial ChangedFile
// dangling.
func TestBuildChangeLedger_OrphanOldHeaderIsDiscarded(t *testing.T) {
	diff := []byte("--- a/orphan.go\n")
	got := BuildChangeLedger(diff)
	if len(got.Files) != 0 {
		t.Errorf("orphan --- header must not produce a ledger entry; got %+v", got.Files)
	}
}

// ByPath must point at the same struct readers see when iterating
// Files, after the underlying slice has been grown by append several
// times. A naive implementation that stores &ledger.Files[len-1]
// during the parse loop would fail this test once Files reallocates
// its backing array.
func TestBuildChangeLedger_ByPathPointsAtLiveFilesAfterAppends(t *testing.T) {
	var b strings.Builder
	const fileCount = 32
	for i := 0; i < fileCount; i++ {
		b.WriteString("--- a/file")
		b.WriteString(itoa(i))
		b.WriteString(".go\n+++ b/file")
		b.WriteString(itoa(i))
		b.WriteString(".go\n@@ -0,0 +1,1 @@\n+marker-")
		b.WriteString(itoa(i))
		b.WriteString("\n")
	}
	got := BuildChangeLedger([]byte(b.String()))
	if len(got.Files) != fileCount {
		t.Fatalf("Files len = %d, want %d", len(got.Files), fileCount)
	}
	// Every ByPath pointer should reference the corresponding entry
	// in the final Files slice, not an entry from an older backing
	// array. Equality of *ChangedFile via pointer identity is the
	// contract callers rely on.
	for i := range got.Files {
		path := got.Files[i].Path
		if got.ByPath[path] != &got.Files[i] {
			t.Fatalf("ByPath[%q] does not point at &Files[%d]; pointer identity violated", path, i)
		}
	}
}

// Files with only a +++ header and no hunks (e.g. rename-only or
// mode-only diffs) are not retained: the ledger's job is to surface
// content changes for retrieval and verification.
func TestBuildChangeLedger_HeaderOnlyFileExcluded(t *testing.T) {
	diff := []byte("--- a/renamed.go\n+++ b/renamed.go\n")
	got := BuildChangeLedger(diff)
	if len(got.Files) != 0 {
		t.Errorf("header-only file should not enter the ledger; got %+v", got.Files)
	}
}

// Each path-to-category rule has its own table entry so additions can
// land alongside the existing rules without rewriting test bodies.
func TestCategorize_RulesByPath(t *testing.T) {
	cases := []struct {
		path string
		want FileCategory
	}{
		{"internal/intentgap/analyzer.go", CatCode},
		{"internal/intentgap/analyzer_test.go", CatTest},
		{"internal/intentgap/citeordrop_test.go", CatTest},
		{"tests/python/test_run.py", CatTest},
		{"src/components/Login.spec.tsx", CatTest},
		{"src/components/Login.test.ts", CatTest},
		{"README.md", CatDoc},
		{"docs/architecture/overview.md", CatDoc},
		{"docs/notes.txt", CatDoc},
		{"package.json", CatConfig},
		{".github/workflows/release.yml", CatConfig},
		{"pyproject.toml", CatConfig},
		{".env.local", CatConfig},
		{"migrations/0001_init.sql", CatSchema},
		{"api/docs/schemas/intent_gap.schema.json", CatSchema},
		{"internal/intentgap/schema.go", CatSchema}, // basename contains "schema"
		{"cmd/server/main.go", CatCode},
	}
	for _, tc := range cases {
		if got := categorize(tc.path); got != tc.want {
			t.Errorf("categorize(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// "testify" should NOT classify as a test file: the segment match
// only fires on exact-segment "test" or "tests" components, not on
// any path that contains the substring "test".
func TestCategorize_TestifyIsNotTest(t *testing.T) {
	if got := categorize("vendor/github.com/stretchr/testify/assert/doc.go"); got == CatTest {
		t.Errorf("testify path was misclassified as test: %q", got)
	}
}

// ByPath always indexes Files by their forward-slash path so callers
// can look up a changed file without re-scanning the slice.
func TestBuildChangeLedger_ByPathIndex(t *testing.T) {
	diff := []byte("" +
		"--- a/one.go\n+++ b/one.go\n@@ -0,0 +1,1 @@\n+x\n" +
		"--- a/two.go\n+++ b/two.go\n@@ -0,0 +1,1 @@\n+y\n",
	)
	got := BuildChangeLedger(diff)
	if got.ByPath["one.go"] == nil || got.ByPath["two.go"] == nil {
		t.Errorf("ByPath missing entries: %+v", got.ByPath)
	}
	if got.ByPath["never-existed"] != nil {
		t.Errorf("ByPath should not invent entries")
	}
}
