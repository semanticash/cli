package service

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	attrevents "github.com/semanticash/cli/internal/attribution/events"
	"github.com/semanticash/cli/internal/carryforward"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// modifiedCarryForwardScanCap bounds workspace-observation lookup.
const modifiedCarryForwardScanCap = 64

// candidateMaps holds candidate state shared with the caller.
type candidateMaps struct {
	aiLines              map[string]map[string]struct{}
	lineProviders        map[string]map[string]map[string]struct{}
	providerTouchedFiles map[string]string
	fileProvider         map[string]string
	providerModel        map[string]string
	aiTouchedFiles       map[string]bool
}

// applyModifiedCarryForward merges historical line and tool-delta evidence for
// modified paths whose committed content matches a workspace observation. It
// returns paths that received line attribution. Invalid inputs make no changes.
func applyModifiedCarryForward(
	ctx context.Context,
	h *sqlstore.Handle,
	bs *blobs.Store,
	cp sqldb.Checkpoint,
	repoRoot string,
	dr diffResult,
	v2 bool,
	lineStamps map[string]map[string][]attrevents.LineStamp,
	deltas *attrevents.DeltaCandidates,
	cm candidateMaps,
) map[string]bool {
	commit, ok := loadParsedManifest(ctx, bs, cp.ManifestHash)
	if !ok {
		return nil
	}
	modified := modifiedPaths(dr)
	if len(modified) == 0 {
		return nil
	}
	anchor := carryforward.SelectContinuousPaths(commit, modified, loadWorkspaceObservations(ctx, h, bs, cp))
	if anchor == nil {
		return nil
	}
	anchorCP, err := h.Queries.GetCheckpointByID(ctx, anchor.CheckpointID)
	if err != nil {
		return nil
	}
	var lower *sqldb.Checkpoint
	if prev, e := h.Queries.GetPreviousCompletedCheckpoint(ctx, sqldb.GetPreviousCompletedCheckpointParams{
		RepositoryID: cp.RepositoryID, RepositorySequence: anchorCP.RepositorySequence,
	}); e == nil {
		lower = &prev
	}

	// Score the current and observed windows with the active attribution version.
	approved := make(map[string]bool, len(anchor.Paths))
	for _, p := range anchor.Paths {
		approved[p] = true
	}
	current := aiCandidates{
		aiLines: cm.aiLines, lineProviders: cm.lineProviders, providerTouchedFiles: cm.providerTouchedFiles,
		fileProvider: cm.fileProvider, providerModel: cm.providerModel,
	}
	var currentScores []fileScore
	if v2 {
		currentScores, _ = scoreDiffPerFileV2(dr, current, lineStamps, deltaClaims(deltas))
	} else {
		currentScores, _ = scoreDiffPerFile(dr, current)
	}

	histInput := ComputeAIPercentInput{RepoRoot: repoRoot, RepoID: cp.RepositoryID, Window: windowBetween(lower, anchorCP)}
	var hist attrevents.Candidates
	haveEvents := false
	if histEvents, herr := loadWindowEvents(ctx, h, histInput); herr == nil {
		hist, _ = attrevents.BuildCandidatesFromRows(toEventRows(ctx, bs, histEvents), repoRoot, approved)
		haveEvents = true
	} else if !errors.Is(herr, ErrNoEventsInWindow) {
		return nil
	}
	var histDeltas *attrevents.DeltaCandidates
	if v2 {
		hd, dErr := LoadDeltaCandidates(ctx, h, bs, histInput)
		if dErr != nil {
			return nil
		}
		histDeltas = hd
		if haveEvents {
			attrevents.SuppressInferredDeletions(&hist, histDeltas)
		}
		filterDeltaCandidatesForCF(histDeltas, approved)
	}
	if !haveEvents && (histDeltas == nil || len(histDeltas.Claims) == 0) {
		return nil
	}

	histCands := aiCandidates{
		aiLines: hist.AILines, lineProviders: hist.LineProviders, providerTouchedFiles: hist.ProviderTouchedFiles,
		fileProvider: hist.FileProvider, providerModel: hist.ProviderModel,
	}
	var histScores []fileScore
	if v2 {
		histScores, _ = scoreDiffPerFileV2(dr, histCands, hist.LineStamps, deltaClaims(histDeltas))
	} else {
		histScores, _ = scoreDiffPerFile(dr, histCands)
	}

	selected := selectModifiedCarryForwardPaths(approved, scoresByPath(currentScores), scoresByPath(histScores))
	if len(selected) == 0 {
		return nil
	}
	selectedSet := make(map[string]bool, len(selected))
	for _, fp := range selected {
		selectedSet[fp] = true
	}

	out := make(map[string]bool, len(selected))
	for _, fp := range selected {
		out[fp] = true
		cm.aiTouchedFiles[fp] = true
		if prov := hist.FileProvider[fp]; prov != "" {
			cm.fileProvider[fp] = prov
		}
		if cm.aiLines[fp] == nil {
			cm.aiLines[fp] = make(map[string]struct{})
		}
		for line := range hist.AILines[fp] {
			cm.aiLines[fp][line] = struct{}{}
		}
		for line, provs := range hist.LineProviders[fp] {
			if cm.lineProviders[fp] == nil {
				cm.lineProviders[fp] = make(map[string]map[string]struct{})
			}
			if cm.lineProviders[fp][line] == nil {
				cm.lineProviders[fp][line] = make(map[string]struct{})
			}
			for p := range provs {
				cm.lineProviders[fp][line][p] = struct{}{}
			}
		}
		for prov, model := range hist.ProviderModel {
			if _, exists := cm.providerModel[prov]; !exists {
				cm.providerModel[prov] = model
			}
		}
	}
	// Include selected historical deltas in the final score.
	if v2 && histDeltas != nil {
		filterDeltaCandidatesForCF(histDeltas, selectedSet)
		mergeHistoricalDeltas(deltas, histDeltas)
		for fp := range mergeDeltaInvolvement(cm.providerTouchedFiles, histDeltas) {
			cm.aiTouchedFiles[fp] = true
		}
	}
	return out
}

// modifiedPaths returns diff paths that are neither created nor deleted.
func modifiedPaths(dr diffResult) []string {
	skip := make(map[string]bool, len(dr.filesCreated)+len(dr.filesDeleted))
	for _, p := range dr.filesCreated {
		skip[p] = true
	}
	for _, p := range dr.filesDeleted {
		skip[p] = true
	}
	var out []string
	for _, f := range dr.files {
		if !skip[f.path] {
			out = append(out, f.path)
		}
	}
	return out
}

// loadParsedManifest loads and validates a manifest blob by CAS hash.
func loadParsedManifest(ctx context.Context, bs *blobs.Store, manifestHash sql.NullString) (blobs.Manifest, bool) {
	if !manifestHash.Valid || manifestHash.String == "" {
		return blobs.Manifest{}, false
	}
	data, err := bs.Get(ctx, manifestHash.String)
	if err != nil {
		return blobs.Manifest{}, false
	}
	m, err := blobs.ParseManifest(data)
	if err != nil {
		return blobs.Manifest{}, false
	}
	return m, true
}

// loadWorkspaceObservations returns the newest unlinked workspace observation.
// An invalid newest observation blocks fallback to older observations.
func loadWorkspaceObservations(ctx context.Context, h *sqlstore.Handle, bs *blobs.Store, cp sqldb.Checkpoint) []carryforward.Observation {
	rows, err := h.Queries.ListCompletedManifestCheckpointsBefore(ctx, sqldb.ListCompletedManifestCheckpointsBeforeParams{
		RepositoryID: cp.RepositoryID, RepositorySequence: cp.RepositorySequence, Limit: modifiedCarryForwardScanCap,
	})
	if err != nil {
		return nil
	}
	// Rows are newest-first. Commit-linked checkpoints are not observations; the
	// first unlinked one is the newest observation and the only anchor considered.
	for _, r := range rows {
		if r.CommitLinkCount != 0 {
			continue
		}
		m, ok := loadParsedManifest(ctx, bs, r.ManifestHash)
		if !ok || !m.IsWorkspaceScoped() {
			return nil
		}
		return []carryforward.Observation{{
			CheckpointID: r.CheckpointID, Sequence: r.RepositorySequence,
			EventCursor: r.EventCursor.Int64, EventCursorValid: r.EventCursor.Valid, Manifest: m,
		}}
	}
	return nil
}

func deltaClaims(d *attrevents.DeltaCandidates) map[string][]attrevents.DeltaClaimGroup {
	if d == nil {
		return nil
	}
	return d.Claims
}

func scoresByPath(scores []fileScore) map[string]*fileScore {
	m := make(map[string]*fileScore, len(scores))
	for i := range scores {
		m[scores[i].path] = &scores[i]
	}
	return m
}

// selectModifiedCarryForwardPaths returns approved paths with historical AI lines
// and no current AI lines. Invalid scores and provider-only evidence are ignored.
func selectModifiedCarryForwardPaths(approved map[string]bool, current, historical map[string]*fileScore) []string {
	var out []string
	for path, ok := range approved {
		if !ok || path == "" {
			continue
		}
		if cur, seen := current[path]; seen {
			// A malformed current score cannot establish an attribution gap.
			if cur == nil || cur.path != path {
				continue
			}
			if fileScoreAILines(cur) > 0 {
				continue
			}
		}
		hist, seen := historical[path]
		if !seen || hist == nil || hist.path != path || fileScoreAILines(hist) == 0 {
			continue
		}
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}
