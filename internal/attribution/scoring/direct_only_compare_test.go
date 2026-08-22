package scoring

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// These fixtures compare v1 and v2 scoring without tool-delta evidence.
// Expected differences are pinned to complete scores and match statistics.

// expectedDivergence defines an accepted v1/v2 difference for one file.
type expectedDivergence struct {
	file    string
	class   string
	v1Score FileScore
	v2Score FileScore
	v1Stats MatchStats
	v2Stats MatchStats
}

// directOnlyCase is one fixture scored by both algorithms with no deltas.
type directOnlyCase struct {
	name            string
	diff            string
	aiLines         map[string]map[string]struct{}
	providerTouched map[string]string
	fileProvider    map[string]string
	// stamps contains the per-line witnesses used by production scoring.
	stamps map[string]map[string][]LineStamp
	// lineProviders supports the unstamped fallback fixture.
	lineProviders map[string]map[string]map[string]struct{}
	// expect is nil when both scores must match.
	expect *expectedDivergence
}

func unified(path string, added ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\n--- /dev/null\n+++ b/%s\n@@ -0,0 +1,%d @@\n", path, path, path, len(added))
	for _, l := range added {
		b.WriteString("+" + l + "\n")
	}
	return b.String()
}

func lineSet(lines ...string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, l := range lines {
		m[l] = struct{}{}
	}
	return m
}

// witness returns a deterministic line stamp.
func witness(provider string, ts int64) LineStamp {
	return LineStamp{Provider: provider, Ts: ts, InsertSeq: ts, EventID: fmt.Sprintf("%s-%d", provider, ts)}
}

// lineProvidersFromStamps derives provider sets from line witnesses.
func lineProvidersFromStamps(s map[string]map[string][]LineStamp) map[string]map[string]map[string]struct{} {
	out := make(map[string]map[string]map[string]struct{}, len(s))
	for path, lines := range s {
		out[path] = make(map[string]map[string]struct{}, len(lines))
		for line, ws := range lines {
			out[path][line] = map[string]struct{}{}
			for _, w := range ws {
				out[path][line][w.Provider] = struct{}{}
			}
		}
	}
	return out
}

func directOnlyCases() []directOnlyCase {
	return []directOnlyCase{
		{
			name:         "exact_single_provider",
			diff:         unified("main.go", "package main", "func main() {}"),
			aiLines:      map[string]map[string]struct{}{"main.go": lineSet("package main", "func main() {}")},
			fileProvider: map[string]string{"main.go": "claude_code"},
			stamps: map[string]map[string][]LineStamp{"main.go": {
				"package main":   {witness("claude_code", 100)},
				"func main() {}": {witness("claude_code", 100)},
			}},
		},
		{
			name: "normalized_match",
			diff: unified("a.go", "func foo() {"),
			// AI output has different whitespace; both must whitespace-normalize.
			aiLines:      map[string]map[string]struct{}{"a.go": lineSet("func foo(){")},
			fileProvider: map[string]string{"a.go": "claude_code"},
			stamps:       map[string]map[string][]LineStamp{"a.go": {"func foo(){": {witness("claude_code", 100)}}},
		},
		{
			name: "modified_hunk_overlap",
			diff: unified("b.go", "x := compute()", "return x+1"),
			// Only one line matches; the other sits in the same hunk (modified).
			aiLines:      map[string]map[string]struct{}{"b.go": lineSet("x := compute()")},
			fileProvider: map[string]string{"b.go": "claude_code"},
			stamps:       map[string]map[string][]LineStamp{"b.go": {"x := compute()": {witness("claude_code", 100)}}},
		},
		{
			name: "duplicate_lines",
			diff: unified("d.go", "log.Println(1)", "log.Println(1)", "log.Println(1)"),
			// The same content appears three times in the diff and once in AI output.
			aiLines:      map[string]map[string]struct{}{"d.go": lineSet("log.Println(1)")},
			fileProvider: map[string]string{"d.go": "claude_code"},
			stamps:       map[string]map[string][]LineStamp{"d.go": {"log.Println(1)": {witness("claude_code", 100)}}},
		},
		{
			name:    "mixed_providers_distinct_lines",
			diff:    unified("m.go", "alpha", "beta"),
			aiLines: map[string]map[string]struct{}{"m.go": lineSet("alpha", "beta")},
			stamps: map[string]map[string][]LineStamp{"m.go": {
				"alpha": {witness("claude_code", 100)},
				"beta":  {witness("codex", 200)},
			}},
		},
		{
			// v2 selects the later witness; v1 credits both witnesses.
			name:    "shared_line_recency_codex_wins",
			diff:    unified("s1.go", "shared"),
			aiLines: map[string]map[string]struct{}{"s1.go": lineSet("shared")},
			stamps: map[string]map[string][]LineStamp{"s1.go": {
				"shared": {witness("claude_code", 100), witness("codex", 200)},
			}},
			expect: &expectedDivergence{
				file:    "s1.go",
				class:   "provider_narrowed",
				v1Score: sharedScore("s1.go", map[string]int{"claude_code": 1, "codex": 1}, 0),
				v2Score: sharedScore("s1.go", map[string]int{"codex": 1}, 1),
				v1Stats: MatchStats{ExactMatches: 1},
				v2Stats: MatchStats{ExactMatches: 1, ContestedLines: 1},
			},
		},
		{
			// Recency selects claude_code despite its lower lexical order.
			name:    "shared_line_recency_claude_wins",
			diff:    unified("s2.go", "shared"),
			aiLines: map[string]map[string]struct{}{"s2.go": lineSet("shared")},
			stamps: map[string]map[string][]LineStamp{"s2.go": {
				"shared": {witness("claude_code", 300), witness("codex", 200)},
			}},
			expect: &expectedDivergence{
				file:    "s2.go",
				class:   "provider_narrowed",
				v1Score: sharedScore("s2.go", map[string]int{"claude_code": 1, "codex": 1}, 0),
				v2Score: sharedScore("s2.go", map[string]int{"claude_code": 1}, 1),
				v1Stats: MatchStats{ExactMatches: 1},
				v2Stats: MatchStats{ExactMatches: 1, ContestedLines: 1},
			},
		},
		{
			// Unstamped evidence uses the lexicographically greatest provider.
			name:          "shared_line_unstamped_fallback",
			diff:          unified("s3.go", "shared"),
			aiLines:       map[string]map[string]struct{}{"s3.go": lineSet("shared")},
			lineProviders: map[string]map[string]map[string]struct{}{"s3.go": {"shared": {"claude_code": {}, "codex": {}}}},
			expect: &expectedDivergence{
				file:    "s3.go",
				class:   "provider_narrowed",
				v1Score: sharedScore("s3.go", map[string]int{"claude_code": 1, "codex": 1}, 0),
				// The fallback creates one candidate, so the line is not contested.
				v2Score: sharedScore("s3.go", map[string]int{"codex": 1}, 0),
				v1Stats: MatchStats{ExactMatches: 1},
				v2Stats: MatchStats{ExactMatches: 1},
			},
		},
		{
			name:            "provider_touch_only",
			diff:            unified("h.go", "package api", "func Handle() {}"),
			providerTouched: map[string]string{"h.go": "cursor"},
		},
		{
			// Coarse touch origins are assigned after scoring and are tested elsewhere.
			name:            "provider_touch_cursor_agent",
			diff:            unified("c.go", "session line"),
			providerTouched: map[string]string{"c.go": "cursor_agent"},
		},
		{
			name: "no_evidence_all_human",
			diff: unified("plain.go", "just human code", "more of it"),
		},
		{
			// Deleted lines are display-only without carry-forward evidence.
			name: "deletion",
			diff: "diff --git a/gone.go b/gone.go\n--- a/gone.go\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-package gone\n-func Gone() {}\n",
		},
	}
}

// sharedScore returns the expected score for one shared exact line.
func sharedScore(path string, providers map[string]int, contested int) FileScore {
	return FileScore{
		Path:                        path,
		TotalLines:                  1,
		ExactLines:                  1,
		ProviderLines:               providers,
		ProviderOnlyLinesByProvider: map[string]int{},
		ContestedLines:              contested,
	}
}

func indexByPath(scores []FileScore) map[string]FileScore {
	m := make(map[string]FileScore, len(scores))
	for _, s := range scores {
		m[s.Path] = s
	}
	return m
}

// aiTotal is the headline line-level attribution for a file.
func aiTotal(s FileScore) int { return s.ExactLines + s.FormattedLines + s.ModifiedLines }

func providerLineSum(m map[string]int) int {
	sum := 0
	for _, v := range m {
		sum += v
	}
	return sum
}

// gainedCredit reports whether any provider receives additional credit.
func gainedCredit(got, base map[string]int) bool {
	for k, v := range got {
		if v > base[k] {
			return true
		}
	}
	return false
}

// headlineEqual compares the headline line counts.
func headlineEqual(v1, v2 FileScore) bool {
	return v1.ExactLines == v2.ExactLines &&
		v1.FormattedLines == v2.FormattedLines &&
		v1.ModifiedLines == v2.ModifiedLines &&
		v1.ProviderOnlyLines == v2.ProviderOnlyLines &&
		v1.HumanLines == v2.HumanLines &&
		v1.TotalLines == v2.TotalLines
}

// classifyFileDiff classifies a per-file scoring difference.
func classifyFileDiff(v1, v2 FileScore) string {
	if headlineEqual(v1, v2) &&
		reflect.DeepEqual(v1.ProviderLines, v2.ProviderLines) &&
		reflect.DeepEqual(v1.ProviderOnlyLinesByProvider, v2.ProviderOnlyLinesByProvider) {
		return ""
	}
	// Detect additional line or provider-touch attribution.
	if aiTotal(v2) > aiTotal(v1) || v2.ProviderOnlyLines > v1.ProviderOnlyLines {
		return "new_ai_attribution"
	}
	// Detect new or reassigned provider credit.
	if gainedCredit(v2.ProviderLines, v1.ProviderLines) ||
		gainedCredit(v2.ProviderOnlyLinesByProvider, v1.ProviderOnlyLinesByProvider) {
		return "wrong_provider"
	}
	// Detect provider totals that exceed their attributed line counts.
	if providerLineSum(v2.ProviderLines) > aiTotal(v2) ||
		providerLineSum(v2.ProviderOnlyLinesByProvider) > v2.ProviderOnlyLines {
		return "double_credit"
	}
	// An unchanged headline may narrow provider credit without increasing it.
	if headlineEqual(v1, v2) {
		return "provider_narrowed"
	}
	return "unclassifiable"
}

// TestClassifyFileDiff covers each difference class.
func TestClassifyFileDiff(t *testing.T) {
	base := FileScore{Path: "f", TotalLines: 2, ExactLines: 1, HumanLines: 1,
		ProviderLines: map[string]int{"claude_code": 1}, ProviderOnlyLinesByProvider: map[string]int{}}
	pl := func(m map[string]int) map[string]int {
		if m == nil {
			return map[string]int{}
		}
		return m
	}
	// fs builds a score from explicit line-class counts.
	fs := func(exact, formatted, human, provOnly int, lines, provOnlyBy map[string]int) FileScore {
		return FileScore{Path: "f", TotalLines: exact + formatted + human + provOnly,
			ExactLines: exact, FormattedLines: formatted, HumanLines: human, ProviderOnlyLines: provOnly,
			ProviderLines: pl(lines), ProviderOnlyLinesByProvider: pl(provOnlyBy)}
	}
	tests := []struct {
		name string
		v1   FileScore
		v2   FileScore
		want string
	}{
		{"identical", base, base, ""},
		{"new_ai", base, fs(2, 0, 0, 0, map[string]int{"claude_code": 2}, nil), "new_ai_attribution"},
		{"new_provider_only", base, fs(1, 0, 0, 1, map[string]int{"claude_code": 1}, map[string]int{"cursor": 1}), "new_ai_attribution"},
		{"wrong_provider_swap", base, fs(1, 0, 1, 0, map[string]int{"codex": 1}, nil), "wrong_provider"},
		{"wrong_provider_added", fs(1, 0, 1, 0, map[string]int{"claude_code": 1}, nil), fs(1, 0, 1, 0, map[string]int{"claude_code": 1, "codex": 1}, nil), "wrong_provider"},
		// Codex gains the credit that claude_code lost.
		{"wrong_provider_reassigned", fs(3, 0, 0, 0, map[string]int{"claude_code": 2, "codex": 1}, nil), fs(3, 0, 0, 0, map[string]int{"claude_code": 1, "codex": 2}, nil), "wrong_provider"},
		// Provider credit exceeds the attributed line count.
		{"double_credit", fs(2, 0, 0, 0, map[string]int{"claude_code": 1, "codex": 1}, nil), fs(1, 0, 1, 0, map[string]int{"claude_code": 1, "codex": 1}, nil), "double_credit"},
		{"provider_narrowed", fs(1, 0, 1, 0, map[string]int{"claude_code": 1, "codex": 1}, nil), fs(1, 0, 1, 0, map[string]int{"codex": 1}, nil), "provider_narrowed"},
		// Line reclassified exact->formatted with the same provider and totals.
		{"unclassifiable", base, fs(0, 1, 1, 0, map[string]int{"claude_code": 1}, nil), "unclassifiable"},
	}
	for _, tc := range tests {
		if got := classifyFileDiff(tc.v1, tc.v2); got != tc.want {
			t.Errorf("%s: classifyFileDiff = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDirectOnlyV1V2Parity(t *testing.T) {
	for _, c := range directOnlyCases() {
		t.Run(c.name, func(t *testing.T) {
			lineProviders := c.lineProviders
			if c.stamps != nil {
				lineProviders = lineProvidersFromStamps(c.stamps)
			}
			dr := ParseDiff([]byte(c.diff))
			v1Scores, v1Stats := ScoreFiles(dr, c.aiLines, c.providerTouched, c.fileProvider, lineProviders)
			v2Scores, v2Stats := ScoreFilesWithDeltas(dr, c.aiLines, c.providerTouched, c.fileProvider, lineProviders, c.stamps, nil)

			m1, m2 := indexByPath(v1Scores), indexByPath(v2Scores)
			if len(m1) != len(m2) {
				t.Fatalf("file count differs: v1=%d v2=%d", len(m1), len(m2))
			}
			for path, a := range m1 {
				b, ok := m2[path]
				if !ok {
					t.Errorf("%s present in v1 but not v2", path)
					continue
				}
				if c.expect != nil && c.expect.file == path {
					checkExpectedDivergence(t, path, classifyFileDiff(a, b), a, b, c.expect)
					continue
				}
				// Parity cases compare the complete score.
				if !reflect.DeepEqual(a, b) {
					t.Errorf("%s: unexpected divergence [%s]\n v1=%+v\n v2=%+v", path, classifyFileDiff(a, b), a, b)
				}
			}
			// Expected differences pin statistics; other cases require equality.
			if c.expect != nil {
				if !reflect.DeepEqual(v1Stats, c.expect.v1Stats) {
					t.Errorf("v1 stats = %+v, want %+v", v1Stats, c.expect.v1Stats)
				}
				if !reflect.DeepEqual(v2Stats, c.expect.v2Stats) {
					t.Errorf("v2 stats = %+v, want %+v", v2Stats, c.expect.v2Stats)
				}
			} else if !reflect.DeepEqual(v1Stats, v2Stats) {
				t.Errorf("match stats differ:\n v1=%+v\n v2=%+v", v1Stats, v2Stats)
			}
		})
	}
}

// checkExpectedDivergence verifies a complete expected difference.
func checkExpectedDivergence(t *testing.T, path, class string, v1, v2 FileScore, e *expectedDivergence) {
	t.Helper()
	if blockers := map[string]bool{"new_ai_attribution": true, "wrong_provider": true, "double_credit": true, "unclassifiable": true}; blockers[class] {
		t.Errorf("%s: approved divergence became a blocker [%s]\n v1=%+v\n v2=%+v", path, class, v1, v2)
		return
	}
	if class != e.class {
		t.Errorf("%s: classification = %q, want pinned %q (divergence changed or disappeared)", path, class, e.class)
	}
	if !reflect.DeepEqual(v1, e.v1Score) {
		t.Errorf("%s: v1 score = %+v, want pinned %+v", path, v1, e.v1Score)
	}
	if !reflect.DeepEqual(v2, e.v2Score) {
		t.Errorf("%s: v2 score = %+v, want pinned %+v", path, v2, e.v2Score)
	}
}
