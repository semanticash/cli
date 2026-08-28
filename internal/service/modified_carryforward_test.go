package service

import (
	"reflect"
	"testing"
)

// mkScore builds a file score for selector tests.
func mkScore(path string, exact, formatted, modified, providerOnly int) *fileScore {
	return &fileScore{
		path:              path,
		exactLines:        exact,
		formattedLines:    formatted,
		modifiedLines:     modified,
		providerOnlyLines: providerOnly,
	}
}

func approvedSet(paths ...string) map[string]bool {
	m := make(map[string]bool, len(paths))
	for _, p := range paths {
		m[p] = true
	}
	return m
}

// An approved path with no current attribution and historical line evidence
// carries forward.
func TestSelectModifiedCF_ApprovedUnattributedCarries(t *testing.T) {
	approved := approvedSet("a.go")
	current := map[string]*fileScore{}                                       // no current attribution
	historical := map[string]*fileScore{"a.go": mkScore("a.go", 3, 0, 0, 0)} // 3 exact AI lines
	if got := selectModifiedCarryForwardPaths(approved, current, historical); !reflect.DeepEqual(got, []string{"a.go"}) {
		t.Fatalf("got %v, want [a.go]", got)
	}
}

// Current line evidence prevents carry-forward.
func TestSelectModifiedCF_CurrentEvidenceWins(t *testing.T) {
	approved := approvedSet("a.go")
	current := map[string]*fileScore{"a.go": mkScore("a.go", 1, 0, 0, 0)}
	historical := map[string]*fileScore{"a.go": mkScore("a.go", 5, 0, 0, 0)}
	if got := selectModifiedCarryForwardPaths(approved, current, historical); got != nil {
		t.Fatalf("got %v, want none (current wins)", got)
	}
}

// Provider-presence-only historical evidence cannot carry.
func TestSelectModifiedCF_ProviderOnlyHistoryCannotCarry(t *testing.T) {
	approved := approvedSet("a.go")
	current := map[string]*fileScore{}
	historical := map[string]*fileScore{"a.go": mkScore("a.go", 0, 0, 0, 4)} // provider-only lines
	if got := selectModifiedCarryForwardPaths(approved, current, historical); got != nil {
		t.Fatalf("got %v, want none (provider presence alone cannot carry)", got)
	}
}

// Unapproved paths do not carry forward.
func TestSelectModifiedCF_UnapprovedPathIgnored(t *testing.T) {
	approved := approvedSet("a.go")
	current := map[string]*fileScore{}
	historical := map[string]*fileScore{
		"a.go":     mkScore("a.go", 0, 0, 0, 0), // approved but no line evidence
		"other.go": mkScore("other.go", 9, 0, 0, 0),
	}
	if got := selectModifiedCarryForwardPaths(approved, current, historical); got != nil {
		t.Fatalf("got %v, want none (a.go has no history, other.go not approved)", got)
	}
}

// A path with no historical score does not carry.
func TestSelectModifiedCF_MissingHistoryNoCarry(t *testing.T) {
	approved := approvedSet("a.go")
	current := map[string]*fileScore{}
	historical := map[string]*fileScore{}
	if got := selectModifiedCarryForwardPaths(approved, current, historical); got != nil {
		t.Fatalf("got %v, want none", got)
	}
}

// Formatted and modified historical evidence can carry forward.
func TestSelectModifiedCF_FormattedAndModifiedCarry(t *testing.T) {
	approved := approvedSet("f.go", "m.go")
	current := map[string]*fileScore{}
	historical := map[string]*fileScore{
		"f.go": mkScore("f.go", 0, 2, 0, 0),
		"m.go": mkScore("m.go", 0, 0, 1, 0),
	}
	got := selectModifiedCarryForwardPaths(approved, current, historical)
	if !reflect.DeepEqual(got, []string{"f.go", "m.go"}) {
		t.Fatalf("got %v, want [f.go m.go]", got)
	}
}

// A current score without AI lines does not block carry-forward.
func TestSelectModifiedCF_CurrentZeroAIDoesNotBlock(t *testing.T) {
	approved := approvedSet("a.go")
	current := map[string]*fileScore{"a.go": mkScore("a.go", 0, 0, 0, 2)} // provider-only, no AI lines
	historical := map[string]*fileScore{"a.go": mkScore("a.go", 4, 0, 0, 0)}
	if got := selectModifiedCarryForwardPaths(approved, current, historical); !reflect.DeepEqual(got, []string{"a.go"}) {
		t.Fatalf("got %v, want [a.go]", got)
	}
}

// Empty inputs produce no selection.
func TestSelectModifiedCF_EmptyInputsNoChange(t *testing.T) {
	hist := map[string]*fileScore{"a.go": mkScore("a.go", 3, 0, 0, 0)}
	if got := selectModifiedCarryForwardPaths(nil, nil, hist); got != nil {
		t.Fatalf("nil approved: got %v, want none", got)
	}
	if got := selectModifiedCarryForwardPaths(approvedSet(""), nil, hist); got != nil {
		t.Fatalf("empty path: got %v, want none", got)
	}
	if got := selectModifiedCarryForwardPaths(approvedSet("a.go"), nil, nil); got != nil {
		t.Fatalf("nil historical: got %v, want none", got)
	}
}

// Nil score entries fail closed.
func TestSelectModifiedCF_NilScoreEntriesFailClosed(t *testing.T) {
	approved := approvedSet("a.go", "b.go")
	current := map[string]*fileScore{"a.go": nil}
	historical := map[string]*fileScore{"a.go": mkScore("a.go", 3, 0, 0, 0), "b.go": nil}
	if got := selectModifiedCarryForwardPaths(approved, current, historical); got != nil {
		t.Fatalf("got %v, want none (nil entries fail closed)", got)
	}
}

// A score whose path differs from its map key fails closed.
func TestSelectModifiedCF_KeyPathMismatchFailsClosed(t *testing.T) {
	approved := approvedSet("a.go", "b.go")
	current := map[string]*fileScore{"a.go": mkScore("other.go", 0, 0, 0, 0)}
	historical := map[string]*fileScore{
		"a.go": mkScore("a.go", 3, 0, 0, 0),
		"b.go": mkScore("elsewhere.go", 3, 0, 0, 0),
	}
	if got := selectModifiedCarryForwardPaths(approved, current, historical); got != nil {
		t.Fatalf("got %v, want none (key/path mismatch fails closed)", got)
	}
}

// The result is sorted for deterministic application.
func TestSelectModifiedCF_Deterministic(t *testing.T) {
	approved := approvedSet("c.go", "a.go", "b.go")
	current := map[string]*fileScore{}
	historical := map[string]*fileScore{
		"a.go": mkScore("a.go", 1, 0, 0, 0),
		"b.go": mkScore("b.go", 1, 0, 0, 0),
		"c.go": mkScore("c.go", 1, 0, 0, 0),
	}
	got := selectModifiedCarryForwardPaths(approved, current, historical)
	if !reflect.DeepEqual(got, []string{"a.go", "b.go", "c.go"}) {
		t.Fatalf("got %v, want sorted [a.go b.go c.go]", got)
	}
}
