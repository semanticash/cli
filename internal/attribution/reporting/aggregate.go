package reporting

import "sort"

// AggregatePercent reduces per-file scores into a single AggregateResult
// with provider breakdown sorted by AI lines (descending), then name.
func AggregatePercent(scores []FileScoreInput, providerModel map[string]string, filesTouched int) AggregateResult {
	var totalLines, aiAuthored int
	var exactLines, formattedLines, modifiedLines int
	providerLines := make(map[string]int)

	for _, fs := range scores {
		totalLines += fs.TotalLines
		exactLines += fs.ExactLines
		formattedLines += fs.FormattedLines
		modifiedLines += fs.ModifiedLines
		aiAuthored += fs.ExactLines + fs.FormattedLines + fs.ModifiedLines
		for prov, lines := range fs.ProviderLines {
			providerLines[prov] += lines
		}
	}

	if totalLines == 0 {
		return AggregateResult{}
	}

	var providers []ProviderAttribution
	for prov, lines := range providerLines {
		if lines > 0 {
			model := ""
			if providerModel != nil {
				model = providerModel[prov]
			}
			providers = append(providers, ProviderAttribution{
				Provider: prov,
				Model:    model,
				AILines:  lines,
			})
		}
	}
	sort.Slice(providers, func(i, j int) bool {
		if providers[i].AILines != providers[j].AILines {
			return providers[i].AILines > providers[j].AILines
		}
		return providers[i].Provider < providers[j].Provider
	})

	return AggregateResult{
		Percent:        float64(aiAuthored) / float64(totalLines) * 100,
		TotalLines:     totalLines,
		AILines:        aiAuthored,
		ExactLines:     exactLines,
		ModifiedLines:  modifiedLines,
		FormattedLines: formattedLines,
		FilesTouched:   filesTouched,
		Providers:      providers,
	}
}

// AILines returns the total AI lines (exact + formatted + modified)
// for a single file score input.
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
	var r CommitResult

	for _, fs := range in.FileScores {
		fa := FileAttributionOutput{
			Path:             fs.Path,
			AIExactLines:     fs.ExactLines,
			AIFormattedLines: fs.FormattedLines,
			AIModifiedLines:  fs.ModifiedLines,
			HumanLines:       fs.HumanLines,
			TotalLines:       fs.TotalLines,
			DeletedNonBlank:  fs.DeletedNonBlank,
		}

		aiAuthored := fs.ExactLines + fs.FormattedLines + fs.ModifiedLines
		if fa.TotalLines > 0 && aiAuthored > 0 {
			fa.AIPercent = float64(aiAuthored) / float64(fa.TotalLines) * 100
			filesWithAI[fs.Path] = true
		}

		r.AIExactLines += fa.AIExactLines
		r.AIFormattedLines += fa.AIFormattedLines
		r.AIModifiedLines += fa.AIModifiedLines
		r.AILines += aiAuthored
		r.HumanLines += fa.HumanLines
		r.TotalLines += fa.TotalLines
		r.Files = append(r.Files, fa)

		isAI := filesWithAI[fs.Path] || in.TouchedFiles[fs.Path]
		if createdSet[fs.Path] {
			r.FilesCreated = append(r.FilesCreated, FileChangeOutput{Path: fs.Path, AI: isAI})
		} else if fa.TotalLines > 0 {
			r.FilesEdited = append(r.FilesEdited, FileChangeOutput{Path: fs.Path, AI: isAI})
		}

		for prov, lines := range fs.ProviderLines {
			providerLines[prov] += lines
		}
	}

	for _, f := range in.FilesDeleted {
		r.FilesDeleted = append(r.FilesDeleted, FileChangeOutput{Path: f, AI: in.TouchedFiles[f]})
	}

	r.FilesTotal = len(r.FilesCreated) + len(r.FilesEdited)
	r.FilesAITouched = len(filesWithAI)
	if r.TotalLines > 0 {
		r.AIPercentage = float64(r.AILines) / float64(r.TotalLines) * 100
	}

	for prov, lines := range providerLines {
		if lines > 0 {
			model := ""
			if in.ProviderModels != nil {
				model = in.ProviderModels[prov]
			}
			r.ProviderDetails = append(r.ProviderDetails, ProviderAttribution{
				Provider: prov,
				Model:    model,
				AILines:  lines,
			})
		}
	}
	sort.Slice(r.ProviderDetails, func(i, j int) bool {
		if r.ProviderDetails[i].AILines != r.ProviderDetails[j].AILines {
			return r.ProviderDetails[i].AILines > r.ProviderDetails[j].AILines
		}
		return r.ProviderDetails[i].Provider < r.ProviderDetails[j].Provider
	})

	return r
}
