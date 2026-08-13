package scoring

import (
	"reflect"
	"strings"
	"testing"
)

func tiers(res []AlignedLine) []int {
	out := make([]int, len(res))
	for i, r := range res {
		out[i] = r.Tier
	}
	return out
}

func claimIdxs(res []AlignedLine) []int {
	out := make([]int, len(res))
	for i, r := range res {
		out[i] = r.ClaimIdx
	}
	return out
}

// Repeated text is matched once per claim.
func TestAlignOrdered_RepeatedLinesConsumedOnce(t *testing.T) {
	claims := NewClaimLines([]string{"return nil", "return nil"})
	added := []string{"return nil", "return nil", "return nil"}
	res, ok := AlignOrdered(claims, added)
	if !ok {
		t.Fatal("budget refused small input")
	}
	if got := tiers(res); !reflect.DeepEqual(got, []int{AlignExact, AlignExact, AlignNone}) {
		t.Fatalf("tiers = %v, want two exact and one unmatched", got)
	}
	if got := claimIdxs(res); !reflect.DeepEqual(got, []int{0, 1, -1}) {
		t.Fatalf("claims = %v, want earliest-position pairing", got)
	}
}

// Matches preserve claim and added-line order.
func TestAlignOrdered_OrderPreserved(t *testing.T) {
	claims := NewClaimLines([]string{"alpha", "beta"})
	added := []string{"beta", "alpha", "beta"}
	res, ok := AlignOrdered(claims, added)
	if !ok {
		t.Fatal(ok)
	}
	if got := claimIdxs(res); !reflect.DeepEqual(got, []int{-1, 0, 1}) {
		t.Fatalf("claims = %v, want ordered alpha,beta alignment", got)
	}
}

func TestAlignOrdered_InterleavedHumanLines(t *testing.T) {
	claims := NewClaimLines([]string{"a := 1", "b := 2"})
	added := []string{"a := 1", "// human note", "b := 2", "// more human"}
	res, ok := AlignOrdered(claims, added)
	if !ok {
		t.Fatal(ok)
	}
	if got := tiers(res); !reflect.DeepEqual(got, []int{AlignExact, AlignNone, AlignExact, AlignNone}) {
		t.Fatalf("tiers = %v", got)
	}
}

// Normalized matches cannot cross exact anchors.
func TestAlignOrdered_NormalizedConfinedToGaps(t *testing.T) {
	claims := NewClaimLines([]string{"x :=1", "anchor", "y  :=  2"})
	added := []string{"x := 1", "anchor", "y := 2"}
	res, ok := AlignOrdered(claims, added)
	if !ok {
		t.Fatal(ok)
	}
	want := []AlignedLine{
		{ClaimIdx: 0, Tier: AlignNormalized},
		{ClaimIdx: 1, Tier: AlignExact},
		{ClaimIdx: 2, Tier: AlignNormalized},
	}
	if !reflect.DeepEqual(res, want) {
		t.Fatalf("res = %+v, want %+v", res, want)
	}

	claims = NewClaimLines([]string{"anchor", "z:=3"})
	added = []string{"z := 3", "anchor"}
	res, ok = AlignOrdered(claims, added)
	if !ok {
		t.Fatal(ok)
	}
	if res[0].Tier != AlignNone || res[1].Tier != AlignExact {
		t.Fatalf("res = %+v, want the pre-anchor line unmatched", res)
	}
}

// Crossing ties prefer the earliest added-line position.
func TestAlignOrdered_CrossingTiePrefersEarliestAddedPosition(t *testing.T) {
	claims := NewClaimLines([]string{"A", "B"})
	added := []string{"B", "A"}
	res, ok := AlignOrdered(claims, added)
	if !ok {
		t.Fatal(ok)
	}
	want := []AlignedLine{
		{ClaimIdx: 1, Tier: AlignExact},
		{ClaimIdx: -1, Tier: AlignNone},
	}
	if !reflect.DeepEqual(res, want) {
		t.Fatalf("res = %+v, want B matched at position 0", res)
	}
}

func TestAlignOrdered_BlankLinesNeverMatch(t *testing.T) {
	claims := NewClaimLines([]string{"", "code()"})
	added := []string{"", "code()", ""}
	res, ok := AlignOrdered(claims, added)
	if !ok {
		t.Fatal(ok)
	}
	if got := tiers(res); !reflect.DeepEqual(got, []int{AlignNone, AlignExact, AlignNone}) {
		t.Fatalf("tiers = %v", got)
	}
}

func TestAlignOrdered_Deterministic(t *testing.T) {
	claims := NewClaimLines([]string{"dup", "dup", "mid", "dup"})
	added := []string{"dup", "mid", "dup", "dup", "mid"}
	first, ok := AlignOrdered(claims, added)
	if !ok {
		t.Fatal(ok)
	}
	for i := 0; i < 10; i++ {
		again, ok := AlignOrdered(claims, added)
		if !ok || !reflect.DeepEqual(first, again) {
			t.Fatalf("run %d diverged: %+v vs %+v", i, again, first)
		}
	}
}

func TestAlignOrdered_BudgetRefusal(t *testing.T) {
	n := 2100 // 2100*2100 > 1<<22
	lines := make([]string, n)
	for i := range lines {
		lines[i] = "line"
	}
	if _, ok := AlignOrdered(NewClaimLines(lines), lines); ok {
		t.Fatal("over-budget alignment accepted")
	}
}

// Coordinates remain correct across hunks and deletions.
func TestParseDiff_GroupCoordinates(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/f.go b/f.go",
		"index 111..222 100644",
		"--- a/f.go",
		"+++ b/f.go",
		"@@ -1,4 +1,5 @@",
		" ctx1",
		"+added2",
		"+added3",
		" ctx4",
		"-removed",
		"+added5",
		"@@ -20,3 +21,4 @@",
		" ctx21",
		"+added22",
		`\ No newline at end of file`,
		"",
	}, "\n")
	res := ParseDiff([]byte(diff))
	if len(res.Files) != 1 {
		t.Fatalf("files = %+v", res.Files)
	}
	groups := res.Files[0].Groups
	if len(groups) != 3 {
		t.Fatalf("groups = %+v, want three", groups)
	}
	check := func(i, start int, lines ...string) {
		t.Helper()
		if groups[i].NewStart != start || !reflect.DeepEqual(groups[i].Lines, lines) {
			t.Fatalf("group %d = %+v, want start %d lines %v", i, groups[i], start, lines)
		}
	}
	check(0, 2, "added2", "added3")
	check(1, 5, "added5")
	check(2, 22, "added22")
	if !res.Complete {
		t.Fatal("well-formed diff reported incomplete")
	}
}

// Malformed headers invalidate coordinates until the next valid header.
func TestParseDiff_MalformedHunkInvalidatesCoordinates(t *testing.T) {
	diff := strings.Join([]string{
		"--- a/f.go",
		"+++ b/f.go",
		"@@ garbage @@",
		" ctx",
		" ctx",
		"+added",
		"@@ -10,2 +11,3 @@",
		" ctx11",
		"+added12",
		"",
	}, "\n")
	res := ParseDiff([]byte(diff))
	groups := res.Files[0].Groups
	if len(groups) != 2 {
		t.Fatalf("groups = %+v", groups)
	}
	if groups[0].NewStart != 0 {
		t.Fatalf("group after malformed header has NewStart %d, want 0", groups[0].NewStart)
	}
	if groups[1].NewStart != 12 {
		t.Fatalf("group after recovery has NewStart %d, want 12", groups[1].NewStart)
	}
}

func TestHunkNewStart_StrictShape(t *testing.T) {
	valid := map[string]int{
		"@@ -1,4 +1,5 @@":              1,
		"@@ -1 +2 @@":                  2,
		"@@ -0,0 +1,5 @@ func heading": 1,
		"@@ -10,2 +11,3 @@":            11,
	}
	for line, want := range valid {
		if got, ok := hunkNewStart(line); !ok || got != want {
			t.Errorf("hunkNewStart(%q) = (%d, %v), want (%d, true)", line, got, ok, want)
		}
	}
	invalid := []string{
		"@@ garbage +12 garbage",
		"@@ garbage +12 garbage @@",
		"@@ -1,2 +99999999999999999999,3 @@", // overflow
		"@@ -1,2 +3,99999999999999999999 @@", // count overflow
		"@@@ -1,2 -3,4 +5,6 @@@",             // combined diff
		"@@ -1,2 @@",                         // missing new range
		"@@ +1,2 -3,4 @@",                    // swapped signs
		"@@ -1,2 +3,4",                       // unterminated
		"@@ -1,2 ++3,4 @@",                   // malformed digits
		"@@ -1,2 +3, @@",                     // empty count
	}
	for _, line := range invalid {
		if got, ok := hunkNewStart(line); ok {
			t.Errorf("hunkNewStart(%q) = (%d, true), want rejection", line, got)
		}
	}
}

// Oversized lines produce an incomplete parse.
func TestParseDiff_TruncatedInputReportsIncomplete(t *testing.T) {
	long := strings.Repeat("x", 5*1024*1024)
	diff := "--- a/f.go\n+++ b/f.go\n@@ -1,1 +1,2 @@\n ctx\n+" + long + "\n"
	res := ParseDiff([]byte(diff))
	if res.Complete {
		t.Fatal("truncated parse reported complete")
	}
}
