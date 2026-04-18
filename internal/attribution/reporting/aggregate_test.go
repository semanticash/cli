package reporting

import "testing"

func TestAggregatePercent_SumsCorrectly(t *testing.T) {
	scores := []FileScoreInput{
		{
			Path:           "a.go",
			TotalLines:     10,
			ExactLines:     5,
			FormattedLines: 2,
			ModifiedLines:  1,
			HumanLines:     2,
			ProviderLines:  map[string]int{"claude_code": 8},
		},
		{
			Path:           "b.go",
			TotalLines:     5,
			ExactLines:     3,
			FormattedLines: 0,
			ModifiedLines:  0,
			HumanLines:     2,
			ProviderLines:  map[string]int{"cursor": 3},
		},
	}
	provModel := map[string]string{
		"claude_code": "opus 4.6",
		"cursor":      "",
	}

	result := AggregatePercent(scores, provModel, 2)

	if result.TotalLines != 15 {
		t.Errorf("TotalLines = %d, want 15", result.TotalLines)
	}
	if result.AILines != 11 { // 5+2+1 + 3+0+0
		t.Errorf("AILines = %d, want 11", result.AILines)
	}
	if result.ExactLines != 8 {
		t.Errorf("ExactLines = %d, want 8", result.ExactLines)
	}
	if result.FormattedLines != 2 {
		t.Errorf("FormattedLines = %d, want 2", result.FormattedLines)
	}
	if result.ModifiedLines != 1 {
		t.Errorf("ModifiedLines = %d, want 1", result.ModifiedLines)
	}
	if result.FilesTouched != 2 {
		t.Errorf("FilesTouched = %d, want 2", result.FilesTouched)
	}
	wantPercent := float64(11) / float64(15) * 100
	if result.Percent != wantPercent {
		t.Errorf("Percent = %f, want %f", result.Percent, wantPercent)
	}
	if len(result.Providers) != 2 {
		t.Fatalf("Providers count = %d, want 2", len(result.Providers))
	}
	// claude_code (8) > cursor (3)
	if result.Providers[0].Provider != "claude_code" {
		t.Errorf("Providers[0].Provider = %q, want claude_code", result.Providers[0].Provider)
	}
	if result.Providers[0].Model != "opus 4.6" {
		t.Errorf("Providers[0].Model = %q, want opus 4.6", result.Providers[0].Model)
	}
	if result.Providers[0].AILines != 8 {
		t.Errorf("Providers[0].AILines = %d, want 8", result.Providers[0].AILines)
	}
	if result.Providers[1].Provider != "cursor" {
		t.Errorf("Providers[1].Provider = %q, want cursor", result.Providers[1].Provider)
	}
	if result.Providers[1].AILines != 3 {
		t.Errorf("Providers[1].AILines = %d, want 3", result.Providers[1].AILines)
	}
}

func TestAggregatePercent_ZeroTotalLines(t *testing.T) {
	result := AggregatePercent(nil, nil, 0)

	if result.Percent != 0 {
		t.Errorf("Percent = %f, want 0", result.Percent)
	}
	if result.TotalLines != 0 {
		t.Errorf("TotalLines = %d, want 0", result.TotalLines)
	}
}

func TestAggregatePercent_NilProviderModel(t *testing.T) {
	scores := []FileScoreInput{
		{
			TotalLines:    4,
			ExactLines:    2,
			ModifiedLines: 1,
			HumanLines:    1,
			ProviderLines: map[string]int{"copilot": 3},
		},
	}

	result := AggregatePercent(scores, nil, 1)

	if len(result.Providers) != 1 {
		t.Fatalf("Providers count = %d, want 1", len(result.Providers))
	}
	if result.Providers[0].Model != "" {
		t.Errorf("Model = %q, want empty", result.Providers[0].Model)
	}
}

func TestAggregatePercent_ProviderSortTiebreaker(t *testing.T) {
	scores := []FileScoreInput{
		{
			TotalLines: 6,
			ExactLines: 6,
			ProviderLines: map[string]int{
				"cursor":      3,
				"claude_code": 3,
			},
		},
	}

	result := AggregatePercent(scores, nil, 1)

	if len(result.Providers) != 2 {
		t.Fatalf("Providers count = %d, want 2", len(result.Providers))
	}
	// Same AI lines -> alphabetical: claude_code < cursor
	if result.Providers[0].Provider != "claude_code" {
		t.Errorf("Providers[0].Provider = %q, want claude_code", result.Providers[0].Provider)
	}
	if result.Providers[1].Provider != "cursor" {
		t.Errorf("Providers[1].Provider = %q, want cursor", result.Providers[1].Provider)
	}
}

func TestAILines(t *testing.T) {
	fs := FileScoreInput{
		ExactLines:     5,
		FormattedLines: 2,
		ModifiedLines:  1,
		HumanLines:     3,
	}
	got := AILines(&fs)
	if got != 8 {
		t.Errorf("AILines = %d, want 8", got)
	}
}

func TestBuildCommitResult_FullAssembly(t *testing.T) {
	in := CommitResultInput{
		FileScores: []FileScoreInput{
			{
				Path:            "main.go",
				TotalLines:      10,
				ExactLines:      5,
				FormattedLines:  2,
				ModifiedLines:   1,
				HumanLines:      2,
				ProviderLines:   map[string]int{"claude_code": 8},
				DeletedNonBlank: 3,
			},
			{
				Path:          "handler.go",
				TotalLines:    4,
				ModifiedLines: 4,
				ProviderLines: map[string]int{"cursor": 4},
			},
		},
		FilesCreated:   []string{"main.go"},
		FilesDeleted:   []string{"old.go"},
		TouchedFiles:   map[string]bool{"main.go": true, "handler.go": true, "old.go": true},
		ProviderModels: map[string]string{"claude_code": "opus 4.6", "cursor": ""},
	}

	r := BuildCommitResult(in)

	// Headline totals.
	if r.TotalLines != 14 {
		t.Errorf("TotalLines = %d, want 14", r.TotalLines)
	}
	if r.AILines != 12 { // 5+2+1 + 4
		t.Errorf("AILines = %d, want 12", r.AILines)
	}
	if r.AIExactLines != 5 {
		t.Errorf("AIExactLines = %d, want 5", r.AIExactLines)
	}
	if r.AIFormattedLines != 2 {
		t.Errorf("AIFormattedLines = %d, want 2", r.AIFormattedLines)
	}
	if r.AIModifiedLines != 5 { // 1 + 4
		t.Errorf("AIModifiedLines = %d, want 5", r.AIModifiedLines)
	}
	if r.HumanLines != 2 {
		t.Errorf("HumanLines = %d, want 2", r.HumanLines)
	}
	wantPct := float64(12) / float64(14) * 100
	if r.AIPercentage != wantPct {
		t.Errorf("AIPercentage = %f, want %f", r.AIPercentage, wantPct)
	}

	// Per-file attribution rows. BuildCommitResult guarantees a row in
	// r.Files for every path in FilesDeleted even when the caller does
	// not include them in FileScores, so a pure-deletion path shows up
	// here alongside the two scored files with EvidenceDeletion.
	if len(r.Files) != 3 {
		t.Fatalf("Files = %d, want 3 (2 scored + 1 deletion)", len(r.Files))
	}
	if r.Files[0].Path != "main.go" {
		t.Errorf("Files[0].Path = %q, want main.go", r.Files[0].Path)
	}
	if r.Files[0].DeletedNonBlank != 3 {
		t.Errorf("Files[0].DeletedNonBlank = %d, want 3", r.Files[0].DeletedNonBlank)
	}
	mainPct := float64(8) / float64(10) * 100
	if r.Files[0].AIPercent != mainPct {
		t.Errorf("Files[0].AIPercent = %f, want %f", r.Files[0].AIPercent, mainPct)
	}
	handlerPct := float64(4) / float64(4) * 100
	if r.Files[1].AIPercent != handlerPct {
		t.Errorf("Files[1].AIPercent = %f, want %f", r.Files[1].AIPercent, handlerPct)
	}
	// Deletion row: zero line counts, EvidenceDeletion (old.go is in
	// TouchedFiles, so it resolves to an AI-origin deletion).
	if r.Files[2].Path != "old.go" {
		t.Errorf("Files[2].Path = %q, want old.go", r.Files[2].Path)
	}
	if r.Files[2].TotalLines != 0 || r.Files[2].AIExactLines != 0 {
		t.Errorf("Files[2] should have zero line counts, got %+v", r.Files[2])
	}
	if r.Files[2].PrimaryEvidence != EvidenceDeletion {
		t.Errorf("Files[2].PrimaryEvidence = %q, want %q (AI-touched pure deletion)",
			r.Files[2].PrimaryEvidence, EvidenceDeletion)
	}

	// File changes.
	if len(r.FilesCreated) != 1 {
		t.Fatalf("FilesCreated = %d, want 1", len(r.FilesCreated))
	}
	if r.FilesCreated[0].Path != "main.go" || !r.FilesCreated[0].AI {
		t.Errorf("FilesCreated[0] = %+v, want {main.go, AI:true}", r.FilesCreated[0])
	}
	if len(r.FilesEdited) != 1 {
		t.Fatalf("FilesEdited = %d, want 1", len(r.FilesEdited))
	}
	if r.FilesEdited[0].Path != "handler.go" || !r.FilesEdited[0].AI {
		t.Errorf("FilesEdited[0] = %+v, want {handler.go, AI:true}", r.FilesEdited[0])
	}
	if len(r.FilesDeleted) != 1 {
		t.Fatalf("FilesDeleted = %d, want 1", len(r.FilesDeleted))
	}
	if r.FilesDeleted[0].Path != "old.go" || !r.FilesDeleted[0].AI {
		t.Errorf("FilesDeleted[0] = %+v, want {old.go, AI:true}", r.FilesDeleted[0])
	}

	// Counts.
	if r.FilesTotal != 2 { // 1 created + 1 edited
		t.Errorf("FilesTotal = %d, want 2", r.FilesTotal)
	}
	if r.FilesAITouched != 2 { // both files have AI lines > 0
		t.Errorf("FilesAITouched = %d, want 2", r.FilesAITouched)
	}

	// Provider details sorted by AI lines desc.
	if len(r.ProviderDetails) != 2 {
		t.Fatalf("ProviderDetails = %d, want 2", len(r.ProviderDetails))
	}
	if r.ProviderDetails[0].Provider != "claude_code" {
		t.Errorf("ProviderDetails[0].Provider = %q, want claude_code", r.ProviderDetails[0].Provider)
	}
	if r.ProviderDetails[0].AILines != 8 {
		t.Errorf("ProviderDetails[0].AILines = %d, want 8", r.ProviderDetails[0].AILines)
	}
	if r.ProviderDetails[0].Model != "opus 4.6" {
		t.Errorf("ProviderDetails[0].Model = %q, want opus 4.6", r.ProviderDetails[0].Model)
	}
	if r.ProviderDetails[1].Provider != "cursor" {
		t.Errorf("ProviderDetails[1].Provider = %q, want cursor", r.ProviderDetails[1].Provider)
	}
	if r.ProviderDetails[1].AILines != 4 {
		t.Errorf("ProviderDetails[1].AILines = %d, want 4", r.ProviderDetails[1].AILines)
	}
}

func TestBuildCommitResult_AIFlagFromTouchedFiles(t *testing.T) {
	// File has 0 AI scored lines but is in TouchedFiles -> AI=true.
	in := CommitResultInput{
		FileScores: []FileScoreInput{
			{
				Path:       "touched.go",
				TotalLines: 3,
				HumanLines: 3,
			},
		},
		FilesCreated: []string{"touched.go"},
		TouchedFiles: map[string]bool{"touched.go": true},
	}

	r := BuildCommitResult(in)

	if len(r.FilesCreated) != 1 {
		t.Fatalf("FilesCreated = %d, want 1", len(r.FilesCreated))
	}
	if !r.FilesCreated[0].AI {
		t.Error("expected AI=true for touched file with 0 scored lines")
	}
	if r.FilesAITouched != 0 {
		t.Errorf("FilesAITouched = %d, want 0 (only counts files with scored AI lines)", r.FilesAITouched)
	}
}

func TestBuildCommitResult_ZeroInput(t *testing.T) {
	r := BuildCommitResult(CommitResultInput{})

	if r.TotalLines != 0 {
		t.Errorf("TotalLines = %d, want 0", r.TotalLines)
	}
	if r.AIPercentage != 0 {
		t.Errorf("AIPercentage = %f, want 0", r.AIPercentage)
	}
	if r.FilesTotal != 0 {
		t.Errorf("FilesTotal = %d, want 0", r.FilesTotal)
	}
}

// A pure-deletion file that the caller did NOT include in FileScores
// still gets a row in r.Files. Evidence resolves from TouchedFiles:
// AI-touched deletion -> EvidenceDeletion, human deletion -> EvidenceNone.
func TestBuildCommitResult_PureDeletionProducesFilesRow(t *testing.T) {
	in := CommitResultInput{
		FilesDeleted: []string{"ai-deleted.go", "human-deleted.go"},
		TouchedFiles: map[string]bool{"ai-deleted.go": true},
	}
	r := BuildCommitResult(in)

	if len(r.Files) != 2 {
		t.Fatalf("Files = %d, want 2", len(r.Files))
	}
	if r.Files[0].Path != "ai-deleted.go" || r.Files[0].PrimaryEvidence != EvidenceDeletion {
		t.Errorf("Files[0] = %+v, want {Path: ai-deleted.go, PrimaryEvidence: deletion}", r.Files[0])
	}
	if r.Files[1].Path != "human-deleted.go" || r.Files[1].PrimaryEvidence != EvidenceNone {
		t.Errorf("Files[1] = %+v, want {Path: human-deleted.go, PrimaryEvidence: none}", r.Files[1])
	}
	// Line counts are zero for pure deletions.
	for i, f := range r.Files {
		if f.TotalLines != 0 || f.AIExactLines != 0 || f.AIFormattedLines != 0 || f.AIModifiedLines != 0 {
			t.Errorf("Files[%d] should have zero line counts, got %+v", i, f)
		}
	}
	// FilesDeleted is also populated (existing contract).
	if len(r.FilesDeleted) != 2 {
		t.Fatalf("FilesDeleted = %d, want 2", len(r.FilesDeleted))
	}
	if !r.FilesDeleted[0].AI || r.FilesDeleted[1].AI {
		t.Errorf("FilesDeleted AI flags wrong: %+v", r.FilesDeleted)
	}
}

// When a caller DOES include a deleted path in FileScores (production
// path via ScoreFiles), BuildCommitResult does not double-emit it:
// one row in r.Files, not two.
func TestBuildCommitResult_DeletionAlreadyInFileScoresNotDuplicated(t *testing.T) {
	in := CommitResultInput{
		FileScores: []FileScoreInput{
			{Path: "deleted-scored.go"}, // zero-line entry from ScoreFiles
		},
		FilesDeleted:     []string{"deleted-scored.go"},
		TouchedFiles:     map[string]bool{"deleted-scored.go": true},
		FileTouchOrigins: map[string]TouchOrigin{"deleted-scored.go": TouchOriginDeletion},
	}
	r := BuildCommitResult(in)

	if len(r.Files) != 1 {
		t.Fatalf("Files = %d, want 1 (no duplication)", len(r.Files))
	}
	if r.Files[0].PrimaryEvidence != EvidenceDeletion {
		t.Errorf("Files[0].PrimaryEvidence = %q, want %q", r.Files[0].PrimaryEvidence, EvidenceDeletion)
	}
}

// FilesTotal and FilesAITouched should not be inflated by the defensive
// deletion pass - those counters are about created/edited scored files
// and AI-line-producing files, respectively. A pure deletion adds to
// neither.
func TestBuildCommitResult_DeletionDoesNotInflateCounters(t *testing.T) {
	in := CommitResultInput{
		FileScores: []FileScoreInput{
			{Path: "kept.go", TotalLines: 5, ExactLines: 5, ProviderLines: map[string]int{"claude_code": 5}},
		},
		FilesCreated: []string{"kept.go"},
		FilesDeleted: []string{"gone.go"},
		TouchedFiles: map[string]bool{"kept.go": true, "gone.go": true},
	}
	r := BuildCommitResult(in)

	if r.FilesTotal != 1 {
		t.Errorf("FilesTotal = %d, want 1 (created only, deletion excluded)", r.FilesTotal)
	}
	if r.FilesAITouched != 1 {
		t.Errorf("FilesAITouched = %d, want 1 (only scored AI files count)", r.FilesAITouched)
	}
}

func TestBuildCheckpointResult_WithTouchedFiles(t *testing.T) {
	cr := BuildCheckpointResult(CheckpointResultInput{
		CheckpointID: "cp-123",
		TouchedFiles: map[string]bool{"a.go": true, "b.go": true},
		EventStats: EventStatsInput{
			EventsConsidered: 5,
			EventsAssistant:  3,
			PayloadsLoaded:   2,
			AIToolEvents:     2,
		},
	})

	if cr.CheckpointID != "cp-123" {
		t.Errorf("CheckpointID = %q, want cp-123", cr.CheckpointID)
	}
	if cr.FilesAITouched != 2 {
		t.Errorf("FilesAITouched = %d, want 2", cr.FilesAITouched)
	}
	if cr.FilesTotal != 2 {
		t.Errorf("FilesTotal = %d, want 2", cr.FilesTotal)
	}
	if len(cr.FilesEdited) != 2 {
		t.Fatalf("FilesEdited = %d, want 2", len(cr.FilesEdited))
	}
	for _, f := range cr.FilesEdited {
		if !f.AI {
			t.Errorf("FilesEdited %q: AI = false, want true", f.Path)
		}
	}
	if cr.Diagnostics.EventsConsidered != 5 {
		t.Errorf("Diagnostics.EventsConsidered = %d, want 5", cr.Diagnostics.EventsConsidered)
	}
	if len(cr.Diagnostics.Notes) != 0 {
		t.Errorf("Diagnostics.Notes = %v, want empty (events found with tool calls)", cr.Diagnostics.Notes)
	}
}

func TestBuildCheckpointResult_NoEvents(t *testing.T) {
	cr := BuildCheckpointResult(CheckpointResultInput{
		CheckpointID: "cp-456",
		EventStats:   EventStatsInput{EventsConsidered: 0},
	})

	if cr.FilesAITouched != 0 {
		t.Errorf("FilesAITouched = %d, want 0", cr.FilesAITouched)
	}
	want := "No agent events found in the delta window."
	if len(cr.Diagnostics.Notes) != 1 || cr.Diagnostics.Notes[0] != want {
		t.Errorf("Diagnostics.Notes = %v, want [%q]", cr.Diagnostics.Notes, want)
	}
}

func TestBuildCheckpointResult_EventsButNoToolCalls(t *testing.T) {
	cr := BuildCheckpointResult(CheckpointResultInput{
		CheckpointID: "cp-789",
		EventStats: EventStatsInput{
			EventsConsidered: 3,
			EventsAssistant:  2,
			AIToolEvents:     0,
		},
	})

	want := "Agent events found but none contained file-modifying tool calls (Edit/Write)."
	if len(cr.Diagnostics.Notes) != 1 || cr.Diagnostics.Notes[0] != want {
		t.Errorf("Diagnostics.Notes = %v, want [%q]", cr.Diagnostics.Notes, want)
	}
}

func TestBuildCommitResult_EditedOnlyWithTotalLines(t *testing.T) {
	// Non-created file with TotalLines=0 should NOT appear in FilesEdited.
	in := CommitResultInput{
		FileScores: []FileScoreInput{
			{Path: "empty.go", TotalLines: 0},
			{Path: "real.go", TotalLines: 5, ExactLines: 5, ProviderLines: map[string]int{"claude_code": 5}},
		},
	}

	r := BuildCommitResult(in)

	if len(r.FilesEdited) != 1 {
		t.Fatalf("FilesEdited = %d, want 1", len(r.FilesEdited))
	}
	if r.FilesEdited[0].Path != "real.go" {
		t.Errorf("FilesEdited[0].Path = %q, want real.go", r.FilesEdited[0].Path)
	}
}
