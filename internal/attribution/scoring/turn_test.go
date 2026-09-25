package scoring

import "testing"

func TestTurnSnapshotFallback(t *testing.T) {
	diff := deltaDiff(t, "+direct", "+generated")
	turn := groupsFor(group("codex", 9, "turn", "direct", "generated"))
	scores, stats := ScoreFilesWithDeltas(diff,
		map[string]map[string]struct{}{"f.go": {"direct": {}}}, nil,
		map[string]string{"f.go": "claude_code"}, nil, nil, nil, turn)
	// Direct matches win; snapshot matches precede hunk inheritance.
	if stats.TurnSnapshotMatches != 1 || scores[0].ProviderLines["claude_code"] != 1 || scores[0].ProviderLines["codex"] != 1 {
		t.Fatalf("turn replaced direct evidence: %+v, %+v", scores, stats)
	}
	scores, stats = ScoreFilesWithDeltas(diff, nil, nil, nil, nil, nil,
		groupsFor(group("claude_code", 1, "tool", "direct")), turn)
	if stats.TurnSnapshotMatches != 1 || scores[0].ProviderLines["claude_code"] != 1 || scores[0].ProviderLines["codex"] != 1 {
		t.Fatalf("turn replaced tool delta or lost unmatched line: %+v, %+v", scores, stats)
	}
}

func TestTurnSnapshotDoesNotInheritOrDuplicateCredit(t *testing.T) {
	diff := deltaDiff(t, "+generated", "+generated", "+unrelated")
	scores, stats := ScoreFilesWithDeltas(diff, nil, nil, nil, nil, nil, nil,
		groupsFor(group("codex", 1, "turn", "generated")))
	if stats.TurnSnapshotMatches != 1 || scores[0].HumanLines != 2 {
		t.Fatalf("turn invented additional lines: %+v, %+v", scores, stats)
	}
}

func TestTurnSnapshotAmbiguousAndIncomplete(t *testing.T) {
	diff := deltaDiff(t, "+generated")
	claims := groupsFor(group("codex", 1, "a", "generated"), group("claude_code", 2, "b", "generated"))
	_, stats := ScoreFilesWithDeltas(diff, nil, nil, nil, nil, nil, nil, claims)
	if stats.TurnSnapshotMatches != 0 {
		t.Fatal("competing providers received credit")
	}
	diff.Complete = false
	_, stats = ScoreFilesWithDeltas(diff, nil, nil, nil, nil, nil, nil, groupsFor(group("codex", 1, "a", "generated")))
	if stats.TurnSnapshotMatches != 0 {
		t.Fatal("incomplete diff received credit")
	}
}
