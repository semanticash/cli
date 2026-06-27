package intentgap

import "sort"

// TrajectoryCandidate is a sequence of agent actions touching the same
// scope whose net effect is not present in the diff. It marks a
// possible add-then-remove sequence: the data needed to classify a
// finding as deferred, without semantic interpretation.
//
// Detection is mechanical: a file (and optionally a line range) must
// be touched by at least two actions, and the cumulative diff must
// show no change at that scope. The analyzer or downstream consumer
// is responsible for matching the candidate against a captured prompt
// before treating it as a deferred finding.
//
// LineStart and LineEnd are zero when the cluster carries no usable
// line range; consumers should treat that as a file-level scope.
type TrajectoryCandidate struct {
	File      string
	LineStart int
	LineEnd   int
	ActionIDs []string
}

// DetectEditRevertTrajectories walks the bundle's agent actions and
// returns scopes that were touched repeatedly but do not appear as
// changes in the cumulative diff.
//
// Best-effort fields are handled conservatively:
//   - Actions with an empty FilePath cannot be grouped with other
//     actions and never become a trajectory.
//   - Actions with no line range contribute to a file-level scope. A
//     file-level trajectory only fires when the diff contains no
//     change for the file at all.
//   - Actions with explicit line ranges cluster by overlapping ranges;
//     each cluster of two or more actions becomes a candidate when
//     the diff has no overlap at the merged range.
func DetectEditRevertTrajectories(bundle Bundle) []TrajectoryCandidate {
	if len(bundle.AgentActions) < 2 {
		return nil
	}
	diffByFile := parseChangedRegions(bundle.Diff)

	type byFileBuckets struct {
		ranged    []BundleAgentAction
		fileLevel []BundleAgentAction
	}
	byFile := map[string]*byFileBuckets{}
	files := []string{}
	for _, a := range bundle.AgentActions {
		if a.FilePath == "" {
			continue
		}
		if byFile[a.FilePath] == nil {
			byFile[a.FilePath] = &byFileBuckets{}
			files = append(files, a.FilePath)
		}
		if a.LineRangeStart > 0 && a.LineRangeEnd > 0 {
			byFile[a.FilePath].ranged = append(byFile[a.FilePath].ranged, a)
		} else {
			byFile[a.FilePath].fileLevel = append(byFile[a.FilePath].fileLevel, a)
		}
	}

	// Sort the file list so the output is stable across runs.
	sort.Strings(files)

	var out []TrajectoryCandidate
	for _, file := range files {
		bucket := byFile[file]
		diffRegions := diffByFile[file]
		fileHasDiff := len(diffRegions) > 0
		hasFileLevel := len(bucket.fileLevel) > 0
		totalActions := len(bucket.fileLevel) + len(bucket.ranged)

		// File-level: any file-level action plus at least one other
		// action on the same file, with no surviving change. The
		// file-level action lacks line precision, so even one ranged
		// neighbour gets reported at file-level scope rather than
		// missed. When this candidate fires it subsumes any ranged
		// clusters on the same file, so the line-narrowed pass is
		// skipped to avoid duplicates.
		if hasFileLevel && totalActions >= 2 && !fileHasDiff {
			all := make([]BundleAgentAction, 0, totalActions)
			all = append(all, bucket.fileLevel...)
			all = append(all, bucket.ranged...)
			out = append(out, TrajectoryCandidate{
				File:      file,
				ActionIDs: actionIDsByTS(all),
			})
			continue
		}

		// Line-narrowed: cluster by overlapping ranges; each cluster
		// with two or more actions and no diff overlap is a candidate.
		for _, cluster := range clusterByOverlappingRanges(bucket.ranged) {
			if len(cluster.actions) < 2 {
				continue
			}
			if lineRangeIntersects(lineRange{cluster.start, cluster.end}, diffRegions) {
				continue
			}
			out = append(out, TrajectoryCandidate{
				File:      file,
				LineStart: cluster.start,
				LineEnd:   cluster.end,
				ActionIDs: actionIDsByTS(cluster.actions),
			})
		}
	}
	return out
}

// rangeCluster is one merged span of overlapping action line ranges.
type rangeCluster struct {
	start, end int
	actions    []BundleAgentAction
}

// clusterByOverlappingRanges sorts actions by start line and merges
// neighbours whose ranges overlap. Adjacent-but-disjoint ranges (e.g.
// 10-20 and 21-30) do not merge; only spans that share at least one
// line collapse into the same cluster. Each returned cluster reports
// the merged span and the actions that contributed to it.
func clusterByOverlappingRanges(actions []BundleAgentAction) []rangeCluster {
	if len(actions) == 0 {
		return nil
	}
	sorted := make([]BundleAgentAction, len(actions))
	copy(sorted, actions)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].LineRangeStart < sorted[j].LineRangeStart
	})

	var clusters []rangeCluster
	cur := rangeCluster{
		start:   sorted[0].LineRangeStart,
		end:     sorted[0].LineRangeEnd,
		actions: []BundleAgentAction{sorted[0]},
	}
	for _, a := range sorted[1:] {
		if a.LineRangeStart <= cur.end {
			if a.LineRangeEnd > cur.end {
				cur.end = a.LineRangeEnd
			}
			cur.actions = append(cur.actions, a)
		} else {
			clusters = append(clusters, cur)
			cur = rangeCluster{
				start:   a.LineRangeStart,
				end:     a.LineRangeEnd,
				actions: []BundleAgentAction{a},
			}
		}
	}
	clusters = append(clusters, cur)
	return clusters
}

// actionIDsByTS extracts ActionIDs in chronological (TS) order so
// consumers reading the listed IDs see the sequence as the agent
// actually performed it. Stable on ties so the result is
// deterministic across re-runs.
func actionIDsByTS(actions []BundleAgentAction) []string {
	sorted := make([]BundleAgentAction, len(actions))
	copy(sorted, actions)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].TS < sorted[j].TS
	})
	ids := make([]string, 0, len(sorted))
	for _, a := range sorted {
		ids = append(ids, a.ActionID)
	}
	return ids
}
