package service

import (
	"strings"
	"testing"
)

// parsePushedRefs splits git's pre-push stdin protocol. Every other
// test in this file relies on it producing the expected four-field
// shape, so we pin the parser independently.
func TestParsePushedRefs_HappyPath(t *testing.T) {
	in := strings.NewReader(
		"refs/heads/feat/intent-gap deadbeef refs/heads/feat/intent-gap cafebabe\n",
	)
	refs, err := parsePushedRefs(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(refs))
	}
	got := refs[0]
	if got.LocalRef != "refs/heads/feat/intent-gap" {
		t.Errorf("LocalRef = %q", got.LocalRef)
	}
	if got.LocalSHA != "deadbeef" {
		t.Errorf("LocalSHA = %q", got.LocalSHA)
	}
	if got.RemoteRef != "refs/heads/feat/intent-gap" {
		t.Errorf("RemoteRef = %q", got.RemoteRef)
	}
	if got.RemoteSHA != "cafebabe" {
		t.Errorf("RemoteSHA = %q", got.RemoteSHA)
	}
}

// Multi-ref pushes (e.g. `git push origin a b`) produce one stdin
// line per ref. The parser preserves order.
func TestParsePushedRefs_MultipleRefs(t *testing.T) {
	in := strings.NewReader(strings.Join([]string{
		"refs/heads/a aaaa refs/heads/a 0000000000000000000000000000000000000000",
		"refs/heads/b bbbb refs/heads/b 1111111111111111111111111111111111111111",
		"",
	}, "\n"))
	refs, err := parsePushedRefs(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refs))
	}
	if refs[0].LocalRef != "refs/heads/a" || refs[1].LocalRef != "refs/heads/b" {
		t.Errorf("order not preserved: %#v", refs)
	}
}

// Empty stdin (zero pushed refs) is a valid no-op shape - git can
// invoke pre-push with no lines when there's nothing to do. Parser
// returns an empty slice, not an error.
func TestParsePushedRefs_Empty(t *testing.T) {
	refs, err := parsePushedRefs(strings.NewReader(""))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("expected 0 refs, got %d", len(refs))
	}
}

// nil stdin is also valid - in tests we may not pass anything.
func TestParsePushedRefs_NilReader(t *testing.T) {
	refs, err := parsePushedRefs(nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("expected 0 refs, got %d", len(refs))
	}
}

// Malformed lines are caught and surfaced so the hook can log the
// reason via doctor instead of silently mis-triggering.
func TestParsePushedRefs_MalformedLine(t *testing.T) {
	in := strings.NewReader("only three fields\n")
	_, err := parsePushedRefs(in)
	if err == nil {
		t.Fatalf("expected error on malformed line")
	}
}

// shortRefName strips the standard refs/heads/ prefix and leaves
// everything else alone. The current-branch match relies on this so
// pushed refs match the short form `git rev-parse --abbrev-ref HEAD`
// returns.
func TestShortRefName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"refs/heads/main", "main"},
		{"refs/heads/feat/intent-gap", "feat/intent-gap"},
		// Non-branch ref namespaces flow through unchanged so they
		// never match a branch name.
		{"refs/tags/v1.0.0", "refs/tags/v1.0.0"},
		{"refs/remotes/origin/main", "refs/remotes/origin/main"},
		// Edge cases: empty / prefix-only.
		{"", ""},
		{"refs/heads/", ""},
	}
	for _, tc := range cases {
		if got := shortRefName(tc.in); got != tc.want {
			t.Errorf("shortRefName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
