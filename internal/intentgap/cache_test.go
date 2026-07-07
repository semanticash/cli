package intentgap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newSampleCache(head string) *AnalysisCache {
	return &AnalysisCache{
		SchemaVersion:         AnalysisCacheSchemaVersion,
		FindingSchemaVersion:  FindingSchemaVersion,
		HeadSHA:               head,
		BaseSHA:               "base-sha",
		RequestedBase:         "origin/main",
		PRNumber:              42,
		RepositoryID:          "repo-1",
		PromptTemplateVersion: PromptTemplateVersion,
		Provider:              "claude_code",
		Model:                 "claude-opus-4-7",
		AnalyzedAt:            time.Unix(1_700_000_000, 0).UTC(),
		Findings:              json.RawMessage("[]"),
		CoverageSummary:       json.RawMessage(`{"pr_commits_total":1}`),
	}
}

func sampleKey(head string) AnalysisCacheKey {
	return AnalysisCacheKey{
		HeadSHA:               head,
		BaseSHA:               "base-sha",
		PromptTemplateVersion: PromptTemplateVersion,
		FindingSchemaVersion:  FindingSchemaVersion,
		RepositoryID:          "repo-1",
		PRNumber:              42,
		RequestedBase:         "origin/main",
	}
}

// Round-tripping a cache entry preserves every field the upload path
// needs to rebuild the canonical body.
func TestAnalysisCache_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := newSampleCache("deadbeef")
	if err := WriteAnalysisCache(dir, src); err != nil {
		t.Fatalf("WriteAnalysisCache: %v", err)
	}
	got, hit, err := ReadAnalysisCache(dir, sampleKey("deadbeef"))
	if err != nil {
		t.Fatalf("ReadAnalysisCache: %v", err)
	}
	if !hit {
		t.Fatalf("expected cache hit")
	}
	if got.BaseSHA != "base-sha" || got.PRNumber != 42 || got.Provider != "claude_code" || got.RequestedBase != "origin/main" {
		t.Errorf("round-trip altered fields: %+v", got)
	}
	// Round-trip is semantic, not byte-identical: the on-disk file is
	// indented for human inspection, so compare decoded values.
	var gotCov map[string]any
	if err := json.Unmarshal(got.CoverageSummary, &gotCov); err != nil {
		t.Fatalf("coverage_summary not parseable: %v", err)
	}
	if v, _ := gotCov["pr_commits_total"].(float64); int(v) != 1 {
		t.Errorf("coverage_summary lost in round-trip: %v", gotCov)
	}
}

// A different prompt template version invalidates the cache so a CLI
// upgrade that changes prompt phrasing forces a re-analysis instead
// of replaying an outdated result.
func TestAnalysisCache_PromptTemplateMismatchIsMiss(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAnalysisCache(dir, newSampleCache("deadbeef")); err != nil {
		t.Fatalf("WriteAnalysisCache: %v", err)
	}
	key := sampleKey("deadbeef")
	key.PromptTemplateVersion = "different-template-version"
	got, hit, err := ReadAnalysisCache(dir, key)
	if err != nil {
		t.Fatalf("ReadAnalysisCache: %v", err)
	}
	if hit || got != nil {
		t.Errorf("template mismatch should miss; got hit=%v entry=%+v", hit, got)
	}
}

// Finding IDs are stamped with repository_id, so reusing a cache
// entry under a different connected repo would upload IDs for the
// different repository.
// The reader treats a mismatch as a miss to force a fresh stamp.
func TestAnalysisCache_RepositoryMismatchIsMiss(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAnalysisCache(dir, newSampleCache("deadbeef")); err != nil {
		t.Fatalf("WriteAnalysisCache: %v", err)
	}
	key := sampleKey("deadbeef")
	key.RepositoryID = "repo-2"
	got, hit, _ := ReadAnalysisCache(dir, key)
	if hit || got != nil {
		t.Errorf("repository_id mismatch should miss; got hit=%v", hit)
	}
}

// Same rule as repository_id: finding IDs are PR-scoped so a different
// PR number should miss even when head_sha is shared.
func TestAnalysisCache_PRNumberMismatchIsMiss(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAnalysisCache(dir, newSampleCache("deadbeef")); err != nil {
		t.Fatalf("WriteAnalysisCache: %v", err)
	}
	key := sampleKey("deadbeef")
	key.PRNumber = 99
	got, hit, _ := ReadAnalysisCache(dir, key)
	if hit || got != nil {
		t.Errorf("pr_number mismatch should miss; got hit=%v", hit)
	}
}

// --base changes the diff regions the analyzer considered. A cached
// run produced against the default base should not be reused when the
// caller asked for a different base, even at the same head SHA.
func TestAnalysisCache_RequestedBaseMismatchIsMiss(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAnalysisCache(dir, newSampleCache("deadbeef")); err != nil {
		t.Fatalf("WriteAnalysisCache: %v", err)
	}
	key := sampleKey("deadbeef")
	key.RequestedBase = "release/v2"
	got, hit, _ := ReadAnalysisCache(dir, key)
	if hit || got != nil {
		t.Errorf("requested_base mismatch should miss; got hit=%v", hit)
	}
}

// Base SHA captures merge-base movement. When origin/main advances
// between runs the cache should miss even if head_sha and the requested
// base ref are unchanged - the diff the analyzer would see is different.
func TestAnalysisCache_BaseSHAMismatchIsMiss(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAnalysisCache(dir, newSampleCache("deadbeef")); err != nil {
		t.Fatalf("WriteAnalysisCache: %v", err)
	}
	key := sampleKey("deadbeef")
	key.BaseSHA = "different-base-sha"
	got, hit, _ := ReadAnalysisCache(dir, key)
	if hit || got != nil {
		t.Errorf("base_sha mismatch should miss; got hit=%v", hit)
	}
}

// Finding schema version mismatch should miss so a future schema bump
// cannot replay findings shaped for an older wire contract.
func TestAnalysisCache_FindingSchemaVersionMismatchIsMiss(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAnalysisCache(dir, newSampleCache("deadbeef")); err != nil {
		t.Fatalf("WriteAnalysisCache: %v", err)
	}
	key := sampleKey("deadbeef")
	key.FindingSchemaVersion = "2"
	got, hit, _ := ReadAnalysisCache(dir, key)
	if hit || got != nil {
		t.Errorf("finding_schema_version mismatch should miss; got hit=%v", hit)
	}
}

// Cached findings that no longer satisfy the schema are treated as a
// miss. This also handles validation tightening between CLI versions.
func TestAnalysisCache_SchemaInvalidFindingsIsMiss(t *testing.T) {
	dir := t.TempDir()
	src := newSampleCache("deadbeef")
	src.Findings = json.RawMessage(`[{"kind":"under_impl","title":"shape is invalid"}]`)
	if err := WriteAnalysisCache(dir, src); err != nil {
		t.Fatalf("WriteAnalysisCache: %v", err)
	}
	got, hit, _ := ReadAnalysisCache(dir, sampleKey("deadbeef"))
	if hit || got != nil {
		t.Errorf("schema-invalid cached findings should miss; got hit=%v", hit)
	}
}

// A missing cache file is a clean miss, not an error.
func TestAnalysisCache_MissingFileIsCleanMiss(t *testing.T) {
	got, hit, err := ReadAnalysisCache(t.TempDir(), sampleKey("never-written"))
	if err != nil {
		t.Fatalf("ReadAnalysisCache: %v", err)
	}
	if hit || got != nil {
		t.Errorf("missing file should miss; got hit=%v entry=%+v", hit, got)
	}
}

// A cache-schema mismatch is also a miss; a future bump to the cache
// file format should not surface as a hit.
func TestAnalysisCache_SchemaVersionMismatchIsMiss(t *testing.T) {
	dir := t.TempDir()
	src := newSampleCache("deadbeef")
	src.SchemaVersion = "999"
	// Bypass WriteAnalysisCache because it normalizes the version.
	if err := os.MkdirAll(CacheDir(dir), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data, _ := json.Marshal(src)
	if err := os.WriteFile(filepath.Join(CacheDir(dir), "deadbeef.json"), data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	got, hit, _ := ReadAnalysisCache(dir, sampleKey("deadbeef"))
	if hit || got != nil {
		t.Errorf("schema-version mismatch should miss; got hit=%v", hit)
	}
}

// Corrupt JSON in a cache file is treated as a miss so the next
// successful analysis can replace it.
func TestAnalysisCache_CorruptFileIsMiss(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(CacheDir(dir), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(CacheDir(dir), "deadbeef.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	got, hit, err := ReadAnalysisCache(dir, sampleKey("deadbeef"))
	if err != nil {
		t.Fatalf("ReadAnalysisCache: %v", err)
	}
	if hit || got != nil {
		t.Errorf("corrupt cache should miss; got hit=%v", hit)
	}
}
