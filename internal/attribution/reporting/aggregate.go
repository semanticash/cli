package reporting

import "sort"

// AggregatePercent reduces per-file scores into a single AggregateResult
// with provider breakdown sorted by AI lines (descending), then name.
//
// Provider-only lines are tracked separately and excluded from the
// headline Percent. The provider breakdown carries them on a
// distinct field (ProviderOnlyLines) so a consumer rendering the
// breakdown can show line-level and provider-only counts side by
// side without conflating evidence strengths.
func AggregatePercent(scores []FileScoreInput, providerModel map[string]string, filesTouched int) AggregateResult {
	var totalLines, aiAuthored int
	var exactLines, formattedLines, modifiedLines, providerOnlyLines int
	providerLines := make(map[string]int)
	providerOnlyByProvider := make(map[string]int)

	for _, fs := range scores {
		totalLines += fs.TotalLines
		exactLines += fs.ExactLines
		formattedLines += fs.FormattedLines
		modifiedLines += fs.ModifiedLines
		providerOnlyLines += fs.ProviderOnlyLines
		aiAuthored += fs.ExactLines + fs.FormattedLines + fs.ModifiedLines
		for prov, lines := range fs.ProviderLines {
			providerLines[prov] += lines
		}
		for prov, lines := range fs.ProviderOnlyLinesByProvider {
			providerOnlyByProvider[prov] += lines
		}
	}

	if totalLines == 0 {
		return AggregateResult{}
	}

	// Build the provider breakdown over the union of both maps so
	// a provider that contributed only provider-only lines still
	// appears in the result.
	provSet := make(map[string]struct{})
	for prov := range providerLines {
		provSet[prov] = struct{}{}
	}
	for prov := range providerOnlyByProvider {
		provSet[prov] = struct{}{}
	}
	var providers []ProviderAttribution
	for prov := range provSet {
		ll := providerLines[prov]
		po := providerOnlyByProvider[prov]
		if ll == 0 && po == 0 {
			continue
		}
		model := ""
		if providerModel != nil {
			model = providerModel[prov]
		}
		providers = append(providers, ProviderAttribution{
			Provider:          prov,
			Model:             model,
			AILines:           ll,
			ProviderOnlyLines: po,
		})
	}
	sort.Slice(providers, func(i, j int) bool {
		if providers[i].AILines != providers[j].AILines {
			return providers[i].AILines > providers[j].AILines
		}
		if providers[i].ProviderOnlyLines != providers[j].ProviderOnlyLines {
			return providers[i].ProviderOnlyLines > providers[j].ProviderOnlyLines
		}
		return providers[i].Provider < providers[j].Provider
	})

	return AggregateResult{
		Percent:           float64(aiAuthored) / float64(totalLines) * 100,
		TotalLines:        totalLines,
		AILines:           aiAuthored,
		ExactLines:        exactLines,
		ModifiedLines:     modifiedLines,
		FormattedLines:    formattedLines,
		ProviderOnlyLines: providerOnlyLines,
		FilesTouched:      filesTouched,
		Providers:         providers,
	}
}

// AILines returns the total AI lines (exact + formatted + modified)
// for a single file score input. ProviderOnlyLines is deliberately
// excluded; callers that need the full provider-touched count
// read ProviderOnlyLines directly.
func AILines(fs *FileScoreInput) int {
	return fs.ExactLines + fs.FormattedLines + fs.ModifiedLines
}

// BuildCommitResult assembles a full commit attribution result from scored
// file data, diff metadata, and candidate metadata. It builds per-file
// attribution rows, headline totals, file change lists, and provider details.
func BuildCommitResult(in CommitResultInput) CommitResult {
	createdSet := make(map[string]bool, len(in.FilesCreated))
	for _, f := range in.FilesCreated {
		createdSet[f] = true
	}

	filesWithAI := make(map[string]bool)
	providerLines := make(map[string]int)
	providerOnlyByProvider := make(map[string]int)
	filesSeen := make(map[string]bool, len(in.FileScores)+len(in.FilesDeleted))
	var r CommitResult

	for _, fs := range in.FileScores {
		fa := FileAttributionOutput{
			Path:                fs.Path,
			AIExactLines:        fs.ExactLines,
			AIFormattedLines:    fs.FormattedLines,
			AIModifiedLines:     fs.ModifiedLines,
			AIProviderOnlyLines: fs.ProviderOnlyLines,
			HumanLines:          fs.HumanLines,
			TotalLines:          fs.TotalLines,
			DeletedNonBlank:     fs.DeletedNonBlank,
		}

		aiAuthored := fs.ExactLines + fs.FormattedLines + fs.ModifiedLines
		// "AI touched this file" includes provider-only signal so
		// downstream change lists still mark the file as AI even
		// though its lines are excluded from AIPercent.
		hasAIEvidence := aiAuthored > 0 || fs.ProviderOnlyLines > 0
		if fa.TotalLines > 0 && aiAuthored > 0 {
			fa.AIPercent = float64(aiAuthored) / float64(fa.TotalLines) * 100
		}
		if hasAIEvidence {
			filesWithAI[fs.Path] = true
		}

		// Derive evidence classification.
		touch := in.FileTouchOrigins[fs.Path]
		isCF := in.CarryForwardFiles[fs.Path]
		fa.PrimaryEvidence = ResolveFileEvidence(fs, touch, isCF)
		fa.AllEvidence = CollectFileEvidence(fs, touch, isCF)

		r.AIExactLines += fa.AIExactLines
		r.AIFormattedLines += fa.AIFormattedLines
		r.AIModifiedLines += fa.AIModifiedLines
		r.AIProviderOnlyLines += fa.AIProviderOnlyLines
		r.AILines += aiAuthored
		r.HumanLines += fa.HumanLines
		r.TotalLines += fa.TotalLines
		r.Files = append(r.Files, fa)
		filesSeen[fa.Path] = true

		isAI := filesWithAI[fs.Path] || in.TouchedFiles[fs.Path]
		var provs []string
		if isAI {
			provs = in.FileProviders[fs.Path]
		}
		if createdSet[fs.Path] {
			r.FilesCreated = append(r.FilesCreated, FileChangeOutput{Path: fs.Path, AI: isAI, Providers: provs})
		} else if fa.TotalLines > 0 {
			r.FilesEdited = append(r.FilesEdited, FileChangeOutput{Path: fs.Path, AI: isAI, Providers: provs})
		}

		for prov, lines := range fs.ProviderLines {
			providerLines[prov] += lines
		}
		for prov, lines := range fs.ProviderOnlyLinesByProvider {
			providerOnlyByProvider[prov] += lines
		}
	}

	// Pure-deletion pass. Each path in FilesDeleted must also appear in
	// r.Files so downstream consumers can inspect per-file evidence.
	// Production scoring already emits zero-line entries for deleted
	// paths, so this branch mainly protects callers that provide
	// FilesDeleted without matching FileScores. The appended rows keep
	// zero line counts and resolve evidence from the same touch metadata
	// used by scored deletions.
	for _, f := range in.FilesDeleted {
		var provs []string
		if in.TouchedFiles[f] {
			provs = in.FileProviders[f]
		}
		r.FilesDeleted = append(r.FilesDeleted, FileChangeOutput{Path: f, AI: in.TouchedFiles[f], Providers: provs})
		if filesSeen[f] {
			continue
		}
		touch := in.FileTouchOrigins[f]
		if touch == "" && in.TouchedFiles[f] {
			// Caller populated TouchedFiles but not FileTouchOrigins -
			// an AI-touched pure deletion with no explicit origin is
			// by definition a deletion-origin touch.
			touch = TouchOriginDeletion
		}
		emptyFS := FileScoreInput{Path: f}
		r.Files = append(r.Files, FileAttributionOutput{
			Path:            f,
			PrimaryEvidence: ResolveFileEvidence(emptyFS, touch, false),
			AllEvidence:     CollectFileEvidence(emptyFS, touch, false),
		})
		filesSeen[f] = true
	}

	r.FilesTotal = len(r.FilesCreated) + len(r.FilesEdited)
	r.FilesAITouched = len(filesWithAI)
	if r.TotalLines > 0 {
		r.AIPercentage = float64(r.AILines) / float64(r.TotalLines) * 100
	}

	// Provider breakdown over the union of line-level and
	// provider-only attribution. A provider that contributed
	// only provider-only lines still appears so the breakdown
	// is honest about who touched what.
	provSet := make(map[string]struct{})
	for prov := range providerLines {
		provSet[prov] = struct{}{}
	}
	for prov := range providerOnlyByProvider {
		provSet[prov] = struct{}{}
	}
	for prov := range provSet {
		ll := providerLines[prov]
		po := providerOnlyByProvider[prov]
		if ll == 0 && po == 0 {
			continue
		}
		model := ""
		if in.ProviderModels != nil {
			model = in.ProviderModels[prov]
		}
		r.ProviderDetails = append(r.ProviderDetails, ProviderAttribution{
			Provider:          prov,
			Model:             model,
			AILines:           ll,
			ProviderOnlyLines: po,
		})
	}
	sort.Slice(r.ProviderDetails, func(i, j int) bool {
		if r.ProviderDetails[i].AILines != r.ProviderDetails[j].AILines {
			return r.ProviderDetails[i].AILines > r.ProviderDetails[j].AILines
		}
		if r.ProviderDetails[i].ProviderOnlyLines != r.ProviderDetails[j].ProviderOnlyLines {
			return r.ProviderDetails[i].ProviderOnlyLines > r.ProviderDetails[j].ProviderOnlyLines
		}
		return r.ProviderDetails[i].Provider < r.ProviderDetails[j].Provider
	})

	r.Evidence, r.FallbackCount = CommitEvidence(r.Files)

	return r
}

// BuildCheckpointResult assembles a checkpoint-only attribution result.
// Checkpoint blame has no diff and no line-level scoring - it reports
// which files were touched by AI and event-level diagnostics.
func BuildCheckpointResult(in CheckpointResultInput) CheckpointResult {
	var files []FileChangeOutput
	for fp := range in.TouchedFiles {
		files = append(files, FileChangeOutput{Path: fp, AI: true})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})

	var note string
	if in.EventStats.EventsConsidered == 0 {
		note = "No agent events found in the delta window."
	} else if in.EventStats.AIToolEvents == 0 {
		note = "Agent events found but none contained file-modifying tool calls (Edit/Write)."
	}

	return CheckpointResult{
		CheckpointID:   in.CheckpointID,
		FilesAITouched: len(in.TouchedFiles),
		FilesTotal:     len(in.TouchedFiles),
		FilesEdited:    files,
		Diagnostics: CheckpointDiagnostics{
			EventsConsidered: in.EventStats.EventsConsidered,
			EventsAssistant:  in.EventStats.EventsAssistant,
			PayloadsLoaded:   in.EventStats.PayloadsLoaded,
			AIToolEvents:     in.EventStats.AIToolEvents,
			Notes:            AssembleCheckpointNotes(note),
		},
	}
}
