package testutil

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Happy path: the on-disk set exactly matches the declared cases.
// Both diff slices come back empty.
func TestDiffFixtureSet_ExactMatch(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "alpha.golden.json"), "{}")
	mustWriteFile(t, filepath.Join(dir, "beta.golden.json"), "{}")

	cases := []Case{{Name: "alpha"}, {Name: "beta"}}
	missing, extra, err := diffFixtureSet(dir, cases)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want empty", missing)
	}
	if len(extra) != 0 {
		t.Errorf("extra = %v, want empty", extra)
	}
}

// A case is declared in code but no matching golden file exists.
// diffFixtureSet returns it in missing so the caller can fail the
// test or create it on -update.
func TestDiffFixtureSet_MissingFixture(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "alpha.golden.json"), "{}")

	cases := []Case{{Name: "alpha"}, {Name: "beta"}}
	missing, extra, err := diffFixtureSet(dir, cases)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"beta.golden.json"}
	if !reflect.DeepEqual(missing, want) {
		t.Errorf("missing = %v, want %v", missing, want)
	}
	if len(extra) != 0 {
		t.Errorf("extra = %v, want empty", extra)
	}
}

// A stale golden file exists on disk for a case that has been
// removed or renamed. This is the silent-coverage-loss scenario the
// drift protection exists to catch.
func TestDiffFixtureSet_StaleFixture(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "alpha.golden.json"), "{}")
	mustWriteFile(t, filepath.Join(dir, "obsolete.golden.json"), "{}")

	cases := []Case{{Name: "alpha"}}
	missing, extra, err := diffFixtureSet(dir, cases)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want empty", missing)
	}
	want := []string{"obsolete.golden.json"}
	if !reflect.DeepEqual(extra, want) {
		t.Errorf("extra = %v, want %v", extra, want)
	}
}

// Both sides of the diff surface together when the cases and the
// on-disk fixtures disagree in multiple places.
func TestDiffFixtureSet_BothMissingAndStale(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "alpha.golden.json"), "{}")
	mustWriteFile(t, filepath.Join(dir, "obsolete.golden.json"), "{}")

	cases := []Case{{Name: "alpha"}, {Name: "new_case"}}
	missing, extra, err := diffFixtureSet(dir, cases)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantMissing := []string{"new_case.golden.json"}
	if !reflect.DeepEqual(missing, wantMissing) {
		t.Errorf("missing = %v, want %v", missing, wantMissing)
	}
	wantExtra := []string{"obsolete.golden.json"}
	if !reflect.DeepEqual(extra, wantExtra) {
		t.Errorf("extra = %v, want %v", extra, wantExtra)
	}
}

// Files that do not end in .golden.json must not trigger the stale-
// fixture branch. Test data directories can legitimately hold other
// artifacts (README fragments, schema files, etc.) without failing
// the harness.
func TestDiffFixtureSet_IgnoresUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "alpha.golden.json"), "{}")
	mustWriteFile(t, filepath.Join(dir, "README.md"), "notes")
	mustWriteFile(t, filepath.Join(dir, "alpha.input.json"), "{}")       // not a golden
	mustWriteFile(t, filepath.Join(dir, "alpha.golden.json.bak"), "{}") // leftover from manual edit

	cases := []Case{{Name: "alpha"}}
	missing, extra, err := diffFixtureSet(dir, cases)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want empty", missing)
	}
	if len(extra) != 0 {
		t.Errorf("extra = %v, want empty (unrelated files should be ignored)", extra)
	}
}

// Subdirectories are ignored even if their names would otherwise
// match a golden filename. A directory cannot be a golden fixture.
func TestDiffFixtureSet_IgnoresSubdirectories(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "alpha.golden.json"), "{}")
	if err := os.MkdirAll(filepath.Join(dir, "nested.golden.json"), 0o755); err != nil {
		t.Fatalf("create nested dir: %v", err)
	}

	cases := []Case{{Name: "alpha"}}
	missing, extra, err := diffFixtureSet(dir, cases)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(missing) != 0 || len(extra) != 0 {
		t.Errorf("subdirectory leaked into diff: missing=%v extra=%v", missing, extra)
	}
}

// When the directory itself does not exist, every declared case is
// returned as missing. This is the case when a new provider runs
// its first `go test -update`: no testdata/ exists yet, so every
// fixture-to-write shows up as missing. RunGolden special-cases
// -update to create the directory, so this is purely informational
// for the comparison path.
func TestDiffFixtureSet_NonexistentDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	cases := []Case{{Name: "alpha"}, {Name: "beta"}}

	missing, extra, err := diffFixtureSet(dir, cases)
	if err != nil {
		t.Fatalf("nonexistent dir should not error, got: %v", err)
	}
	want := []string{"alpha.golden.json", "beta.golden.json"}
	if !reflect.DeepEqual(missing, want) {
		t.Errorf("missing = %v, want %v", missing, want)
	}
	if len(extra) != 0 {
		t.Errorf("extra = %v, want empty", extra)
	}
}

// No cases declared and no directory on disk: both sides are empty
// and no error. A provider that has not yet added any golden tests
// should not fail the harness by existing.
func TestDiffFixtureSet_EmptyCasesNoDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "empty")
	missing, extra, err := diffFixtureSet(dir, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want empty", missing)
	}
	if len(extra) != 0 {
		t.Errorf("extra = %v, want empty", extra)
	}
}

func mustWriteFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
