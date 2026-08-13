package events

import "testing"

// A verified delta deletion supersedes the bash-inferred deletion for
// the same path; file-edit touches and unrelated inferences survive.
func TestSuppressInferredDeletions(t *testing.T) {
	cands := &Candidates{
		ProviderTouchedFiles: map[string]string{
			"deleted-by-both.go":  "claude_code", // rm-inferred, delta-confirmed
			"deleted-by-rm.go":    "claude_code", // rm-inferred only
			"edited-by-cursor.go": "cursor",      // provider file-edit touch
			"edited-then-rm.go":   "claude_code", // cursor's edit touch overwritten by claude's rm inference
		},
		InferredDeletions: map[string]string{
			"deleted-by-both.go": "claude_code",
			"deleted-by-rm.go":   "claude_code",
			"edited-then-rm.go":  "claude_code",
		},
		ExplicitTouches: map[string]LineStamp{
			"edited-by-cursor.go": {Provider: "cursor"},
			"edited-then-rm.go":   {Provider: "cursor"},
		},
	}
	deltas := &DeltaCandidates{
		Deleted: map[string][]string{
			"deleted-by-both.go":  {"claude_code"},
			"edited-by-cursor.go": {"claude_code"}, // never removes an edit touch
			"edited-then-rm.go":   {"claude_code"},
		},
	}
	SuppressInferredDeletions(cands, deltas)

	if _, ok := cands.ProviderTouchedFiles["deleted-by-both.go"]; ok {
		t.Fatal("delta-confirmed rm inference not suppressed")
	}
	if cands.ProviderTouchedFiles["deleted-by-rm.go"] != "claude_code" {
		t.Fatal("unconfirmed rm inference removed")
	}
	if cands.ProviderTouchedFiles["edited-by-cursor.go"] != "cursor" {
		t.Fatal("provider file-edit touch removed")
	}
	if cands.ProviderTouchedFiles["edited-then-rm.go"] != "cursor" {
		t.Fatalf("explicit editor not restored: touch = %q, want cursor",
			cands.ProviderTouchedFiles["edited-then-rm.go"])
	}
	if _, ok := cands.InferredDeletions["deleted-by-both.go"]; ok {
		t.Fatal("consumed inference marker retained")
	}
	if _, ok := cands.InferredDeletions["edited-then-rm.go"]; ok {
		t.Fatal("consumed inference marker retained for explicit touch")
	}
}
