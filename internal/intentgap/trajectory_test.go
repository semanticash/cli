package intentgap

import "testing"

// diffWithChangeAt builds a minimal unified diff that records a
// single-line change at the given file and 1-indexed start line.
// Used by tests that need parseChangedRegions to populate a real
// entry for a file.
func diffWithChangeAt(file string, start int) []byte {
	header := "--- a/" + file + "\n+++ b/" + file + "\n"
	hunk := "@@ -" + itoa(start) + ",1 +" + itoa(start) + ",2 @@\n line\n+added\n"
	return []byte(header + hunk)
}

// itoa is a small stringifier so the test helpers stay dependency-free.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// A bundle with no captured actions produces no trajectories.
func TestDetectEditRevertTrajectories_EmptyBundle(t *testing.T) {
	if got := DetectEditRevertTrajectories(Bundle{}); got != nil {
		t.Errorf("empty bundle returned trajectories: %+v", got)
	}
}

// A single captured action cannot describe an add-then-remove
// sequence, so the detector returns nothing.
func TestDetectEditRevertTrajectories_SingleActionNoTrajectory(t *testing.T) {
	b := Bundle{
		AgentActions: []BundleAgentAction{
			{ActionID: "a1", FilePath: "a.go", LineRangeStart: 10, LineRangeEnd: 20},
		},
	}
	if got := DetectEditRevertTrajectories(b); got != nil {
		t.Errorf("single action returned trajectories: %+v", got)
	}
}

// Two actions on the same file at overlapping ranges with no diff
// change in that range is the canonical edit-then-revert candidate.
func TestDetectEditRevertTrajectories_OverlappingRangesNoDiffEmitsCandidate(t *testing.T) {
	b := Bundle{
		AgentActions: []BundleAgentAction{
			{ActionID: "a1", FilePath: "a.go", LineRangeStart: 10, LineRangeEnd: 20},
			{ActionID: "a2", FilePath: "a.go", LineRangeStart: 15, LineRangeEnd: 25},
		},
		// Diff exists for a different file, so a.go has no recorded change.
		Diff: diffWithChangeAt("other.go", 1),
	}
	got := DetectEditRevertTrajectories(b)
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	c := got[0]
	if c.File != "a.go" || c.LineStart != 10 || c.LineEnd != 25 {
		t.Errorf("merged range = %s:%d-%d, want a.go:10-25", c.File, c.LineStart, c.LineEnd)
	}
	if len(c.ActionIDs) != 2 || c.ActionIDs[0] != "a1" || c.ActionIDs[1] != "a2" {
		t.Errorf("ActionIDs = %v, want [a1 a2]", c.ActionIDs)
	}
}

// When the diff intersects the action range, the change survived and
// the sequence is not a trajectory.
func TestDetectEditRevertTrajectories_OverlappingDiffSuppressesCandidate(t *testing.T) {
	b := Bundle{
		AgentActions: []BundleAgentAction{
			{ActionID: "a1", FilePath: "a.go", LineRangeStart: 10, LineRangeEnd: 20},
			{ActionID: "a2", FilePath: "a.go", LineRangeStart: 15, LineRangeEnd: 25},
		},
		Diff: diffWithChangeAt("a.go", 12),
	}
	if got := DetectEditRevertTrajectories(b); got != nil {
		t.Errorf("diff at the scope should suppress candidate; got %+v", got)
	}
}

// Two actions on the same file at disjoint ranges produce two
// separate clusters, each with one action, so no trajectory candidate
// is emitted. The detector requires at least two actions per cluster.
func TestDetectEditRevertTrajectories_DisjointRangesProduceNoCandidate(t *testing.T) {
	b := Bundle{
		AgentActions: []BundleAgentAction{
			{ActionID: "a1", FilePath: "a.go", LineRangeStart: 10, LineRangeEnd: 20},
			{ActionID: "a2", FilePath: "a.go", LineRangeStart: 50, LineRangeEnd: 60},
		},
	}
	if got := DetectEditRevertTrajectories(b); got != nil {
		t.Errorf("disjoint ranges should not cluster; got %+v", got)
	}
}

// Actions on different files do not cluster together.
func TestDetectEditRevertTrajectories_DifferentFilesNoCandidate(t *testing.T) {
	b := Bundle{
		AgentActions: []BundleAgentAction{
			{ActionID: "a1", FilePath: "a.go", LineRangeStart: 10, LineRangeEnd: 20},
			{ActionID: "a2", FilePath: "b.go", LineRangeStart: 10, LineRangeEnd: 20},
		},
	}
	if got := DetectEditRevertTrajectories(b); got != nil {
		t.Errorf("cross-file actions should not cluster; got %+v", got)
	}
}

// Two file-level actions (no line range) where the diff records no
// change for the file produce a file-level trajectory. Without line
// data, the detector cannot narrow further; the entire file scope is
// reported.
func TestDetectEditRevertTrajectories_FileLevelTrajectory(t *testing.T) {
	b := Bundle{
		AgentActions: []BundleAgentAction{
			{ActionID: "a1", FilePath: "a.go"},
			{ActionID: "a2", FilePath: "a.go"},
		},
		Diff: diffWithChangeAt("other.go", 1),
	}
	got := DetectEditRevertTrajectories(b)
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	c := got[0]
	if c.File != "a.go" || c.LineStart != 0 || c.LineEnd != 0 {
		t.Errorf("file-level candidate = %s:%d-%d, want a.go:0-0", c.File, c.LineStart, c.LineEnd)
	}
}

// A file-level cluster is suppressed when the diff records any change
// on the file. The detector cannot prove every action was reverted in
// that case, so it does not emit a candidate.
func TestDetectEditRevertTrajectories_FileLevelSuppressedWhenDiffPresent(t *testing.T) {
	b := Bundle{
		AgentActions: []BundleAgentAction{
			{ActionID: "a1", FilePath: "a.go"},
			{ActionID: "a2", FilePath: "a.go"},
		},
		Diff: diffWithChangeAt("a.go", 12),
	}
	if got := DetectEditRevertTrajectories(b); got != nil {
		t.Errorf("file-level trajectory should be suppressed by file-level diff; got %+v", got)
	}
}

// Actions whose FilePath is empty (e.g. an unknown-path Bash fallback)
// are skipped: they cannot be grouped with the file-keyed buckets and
// cannot anchor a trajectory candidate.
func TestDetectEditRevertTrajectories_EmptyFilePathSkipped(t *testing.T) {
	b := Bundle{
		AgentActions: []BundleAgentAction{
			{ActionID: "a1", FilePath: ""},
			{ActionID: "a2", FilePath: ""},
		},
	}
	if got := DetectEditRevertTrajectories(b); got != nil {
		t.Errorf("empty-path actions should not produce trajectories; got %+v", got)
	}
}

// File ordering in the output is sorted alphabetically so re-runs
// over the same bundle produce identical results. This matters for
// stable downstream artifacts (cite-or-drop reasons, prompt output).
func TestDetectEditRevertTrajectories_OutputOrderingIsStable(t *testing.T) {
	b := Bundle{
		AgentActions: []BundleAgentAction{
			{ActionID: "b1", FilePath: "b.go", LineRangeStart: 10, LineRangeEnd: 20},
			{ActionID: "b2", FilePath: "b.go", LineRangeStart: 15, LineRangeEnd: 25},
			{ActionID: "a1", FilePath: "a.go", LineRangeStart: 10, LineRangeEnd: 20},
			{ActionID: "a2", FilePath: "a.go", LineRangeStart: 15, LineRangeEnd: 25},
		},
	}
	got := DetectEditRevertTrajectories(b)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	if got[0].File != "a.go" || got[1].File != "b.go" {
		t.Errorf("order = [%s %s], want [a.go b.go]", got[0].File, got[1].File)
	}
}

// A file-level action paired with a ranged action on the same file
// must produce a file-level candidate when the diff records no
// surviving change. Without this case, mixed evidence such as a
// file-level Bash action plus a ranged Edit would be omitted even
// though both actions touched the same file.
func TestDetectEditRevertTrajectories_MixedFileLevelAndRangedEmitsFileLevelCandidate(t *testing.T) {
	b := Bundle{
		AgentActions: []BundleAgentAction{
			{ActionID: "a_file", FilePath: "added.go"},
			{ActionID: "a_ranged", FilePath: "added.go", LineRangeStart: 10, LineRangeEnd: 20},
		},
		// Diff covers only other.go so added.go is recorded as untouched.
		Diff: diffWithChangeAt("other.go", 1),
	}
	got := DetectEditRevertTrajectories(b)
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	c := got[0]
	if c.File != "added.go" || c.LineStart != 0 || c.LineEnd != 0 {
		t.Errorf("candidate = %s:%d-%d, want added.go:0-0 (file-level)", c.File, c.LineStart, c.LineEnd)
	}
	if len(c.ActionIDs) != 2 {
		t.Errorf("ActionIDs len = %d, want 2 (both actions covered): %v", len(c.ActionIDs), c.ActionIDs)
	}
}

// ActionIDs in a candidate are ordered by TS so consumers can read
// the sequence as the agent performed it. Insertion order on the
// bundle is intentionally not chronological in this test to prove
// the detector sorts independently.
func TestDetectEditRevertTrajectories_ActionIDsOrderedByTimestamp(t *testing.T) {
	b := Bundle{
		AgentActions: []BundleAgentAction{
			// Inserted out of chronological order.
			{ActionID: "a_second", FilePath: "x.go", TS: 200, LineRangeStart: 10, LineRangeEnd: 20},
			{ActionID: "a_first", FilePath: "x.go", TS: 100, LineRangeStart: 15, LineRangeEnd: 25},
		},
	}
	got := DetectEditRevertTrajectories(b)
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	if got[0].ActionIDs[0] != "a_first" || got[0].ActionIDs[1] != "a_second" {
		t.Errorf("ActionIDs = %v, want [a_first a_second] (chronological)", got[0].ActionIDs)
	}
}
