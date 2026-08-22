package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/doctor"
)

func rec(phase, tu string, dur int64, outcome string, leaf map[string]int64) hookBenchRecord {
	return hookBenchRecord{
		TS: "2026-08-22T10:00:00Z", Kind: "toolwindow", Phase: phase,
		SessionID: "s1", TurnID: "t1", ToolUseID: tu,
		DurationMS: dur, Outcome: outcome, StageLeafMS: leaf,
	}
}

func TestSummarizeHookBench(t *testing.T) {
	recs := []hookBenchRecord{
		rec("pre", "a", 40, "registered", map[string]int64{"hash": 10}),
		rec("post", "a", 200, "closed_complete", map[string]int64{"hash": 20, "ref_release": 24}),
		rec("pre", "b", 60, "registered", nil),
		rec("post", "b", 220, "closed_complete", nil),
		rec("pre", "c", 50, "registered", nil), // unpaired pre
		rec("post", "d", 210, "closed_complete", nil), // unpaired post
	}
	s := summarizeHookBench(recs)
	if s.Records != 6 || s.Paired != 2 || s.UnpairedPre != 1 || s.UnpairedPost != 1 {
		t.Fatalf("pairing = %+v", s)
	}
	if s.Pre.P50 != 50 { // [40,50,60]
		t.Errorf("pre p50 = %d, want 50", s.Pre.P50)
	}
	if s.Post.P50 != 210 { // [200,210,220]
		t.Errorf("post p50 = %d, want 210", s.Post.P50)
	}
	if s.Combined.P50 != 240 { // pairs a=240, b=280 -> [240,280]
		t.Errorf("combined p50 = %d, want 240", s.Combined.P50)
	}
	if len(s.Stages) == 0 || s.Stages[0].Name != "ref_release" || s.Stages[0].P50 != 24 {
		t.Errorf("top stage = %+v, want ref_release/24", s.Stages)
	}
	if s.Outcomes["closed_complete"] != 3 || s.Outcomes["registered"] != 3 {
		t.Errorf("outcomes = %v", s.Outcomes)
	}
}

// A replayed identity must form two distinct pairs, not one inflated pair.
func TestSummarizeHookBenchRepeatedIdentity(t *testing.T) {
	recs := []hookBenchRecord{
		rec("pre", "a", 40, "registered", nil),
		rec("post", "a", 200, "closed_complete", nil),
		rec("pre", "a", 50, "registered", nil),
		rec("post", "a", 210, "closed_complete", nil),
	}
	s := summarizeHookBench(recs)
	if s.Paired != 2 || s.UnpairedPre != 0 || s.UnpairedPost != 0 {
		t.Fatalf("pairing = %+v, want 2 paired / 0 unpaired", s)
	}
	// Occurrence pairing: 40+200 and 50+210 -> [240, 260], not one 90+410.
	if s.Combined.P50 != 240 || s.Combined.Max != 260 {
		t.Errorf("combined = %+v, want p50 240 max 260", s.Combined)
	}
}

// An extra unmatched post with a repeated key is counted as unpaired.
func TestSummarizeHookBenchExtraPost(t *testing.T) {
	recs := []hookBenchRecord{
		rec("pre", "a", 40, "registered", nil),
		rec("post", "a", 200, "closed_complete", nil),
		rec("post", "a", 210, "closed_complete", nil), // no matching pre
	}
	s := summarizeHookBench(recs)
	if s.Paired != 1 || s.UnpairedPost != 1 || s.UnpairedPre != 0 {
		t.Fatalf("pairing = %+v, want 1 paired / 1 unpaired post", s)
	}
}

func TestFilterSince(t *testing.T) {
	now := time.Now().UTC()
	recs := []hookBenchRecord{
		{TS: now.Format(time.RFC3339Nano), Kind: "toolwindow", Phase: "pre"},
		{TS: now.Add(-48 * time.Hour).Format(time.RFC3339Nano), Kind: "toolwindow", Phase: "pre"},
	}
	got, err := filterSince(recs, "24h")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("since 24h kept %d, want 1", len(got))
	}
	if _, err := filterSince(recs, "nope"); err == nil {
		t.Error("expected error for bad --since")
	}
}

func writeBenchLog(t *testing.T, repo string, recs []hookBenchRecord) {
	t.Helper()
	path := doctor.BenchLogPath(repo)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	for _, r := range recs {
		line, _ := json.Marshal(r)
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHookBenchCommandJSON(t *testing.T) {
	repo := gitInitRepo(t)
	writeBenchLog(t, repo, []hookBenchRecord{
		rec("pre", "a", 40, "registered", map[string]int64{"hash": 10}),
		rec("post", "a", 200, "closed_complete", map[string]int64{"ref_release": 24}),
	})
	cmd := newDoctorHookBenchCmd(&RootOptions{RepoPath: repo})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	var s hookBenchSummary
	if err := json.Unmarshal(out.Bytes(), &s); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if s.Records != 2 || s.Paired != 1 || s.Combined.P50 != 240 {
		t.Fatalf("summary = %+v", s)
	}
}

// --json must emit valid JSON even with no records, carrying recording status.
func TestHookBenchCommandJSONEmpty(t *testing.T) {
	repo := gitInitRepo(t) // no records, bench disabled
	cmd := newDoctorHookBenchCmd(&RootOptions{RepoPath: repo})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	var s hookBenchSummary
	if err := json.Unmarshal(out.Bytes(), &s); err != nil {
		t.Fatalf("--json empty is not valid JSON: %v\n%s", err, out.String())
	}
	if s.Records != 0 || s.RecordingEnabled {
		t.Errorf("summary = %+v, want records 0, recording disabled", s)
	}
}

func TestHookBenchCommandNoRecordsDisabled(t *testing.T) {
	repo := gitInitRepo(t) // no bench.enabled, no bench.jsonl
	cmd := newDoctorHookBenchCmd(&RootOptions{RepoPath: repo})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "disabled") || !strings.Contains(got, "bench.enabled") {
		t.Errorf("expected disabled + enable instructions, got:\n%s", got)
	}
}

func TestHookBenchCommandTextRenders(t *testing.T) {
	repo := gitInitRepo(t)
	writeBenchLog(t, repo, []hookBenchRecord{
		rec("pre", "a", 48, "registered", map[string]int64{"hash": 66, "tree_write": 42}),
		rec("post", "a", 221, "closed_complete", map[string]int64{"ref_release": 24}),
	})
	cmd := newDoctorHookBenchCmd(&RootOptions{RepoPath: repo})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got := out.String()
	for _, want := range []string{"Hook benchmark:", "Combined", "Top stages", "Outcomes:", "ref_release"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}
