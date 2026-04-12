// Package service implements the core business logic for the semantica CLI.
package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	attrcf "github.com/semanticash/cli/internal/attribution/carryforward"
	attrevents "github.com/semanticash/cli/internal/attribution/events"
	attrreporting "github.com/semanticash/cli/internal/attribution/reporting"
	attrscoring "github.com/semanticash/cli/internal/attribution/scoring"
	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"
)

// AttributionService computes AI vs human attribution for git commits.
type AttributionService struct{}

// NewAttributionService returns a ready-to-use AttributionService.
func NewAttributionService() *AttributionService { return &AttributionService{} }

// BlameInput holds the parameters for a blame request.
type BlameInput struct {
	RepoPath string // path to the git repository (defaults to ".")
	Ref      string // commit hash or checkpoint ID/prefix
}

// AttributionInput holds the parameters for an attribution request.
type AttributionInput struct {
	RepoPath   string // path to the git repository (defaults to ".")
	CommitHash string // full or abbreviated commit SHA
}

// FileAttribution holds per-file line attribution counts using four
// deterministic categories:
//   - AI-Exact: line matches AI tool output (trimmed)
//   - AI-Formatted: matches after whitespace normalization (formatter/linter)
//   - AI-Modified: in a contiguous group overlapping AI output, but changed
//   - Human: no overlap with any AI output
type FileAttribution struct {
	Path             string  `json:"path"`
	AIExactLines     int     `json:"ai_exact_lines"`
	AIFormattedLines int     `json:"ai_formatted_lines"`
	AIModifiedLines  int     `json:"ai_modified_lines"`
	HumanLines       int     `json:"human_lines"`
	TotalLines       int     `json:"total_lines"`
	DeletedNonBlank  int     `json:"deleted_non_blank"` // deleted non-blank lines (not attributed, display only)
	AIPercent        float64 `json:"ai_percentage"`     // (exact + formatted + modified) / total * 100
}

// FileChange records a file that was created, edited, or deleted in a commit,
// along with whether the change was performed by an AI agent.
type FileChange struct {
	Path string `json:"path"`
	AI   bool   `json:"ai"`
}

// AttributionDiagnostics provides transparency into why a particular
// AI percentage was computed. Useful when AI% is 0 and the user wants
// to understand which stage of the pipeline had no data.
type AttributionDiagnostics struct {
	EventsConsidered  int    `json:"events_considered"`
	EventsAssistant   int    `json:"events_assistant"`
	PayloadsLoaded    int    `json:"payloads_loaded"`
	AIToolEvents      int    `json:"ai_tool_events"`
	ExactMatches      int    `json:"exact_matches"`
	NormalizedMatches int    `json:"normalized_matches"`
	ModifiedMatches   int    `json:"modified_matches"`
	Note              string `json:"note,omitempty"`
}

// AttributionResult is the full attribution breakdown for a single commit.
type AttributionResult struct {
	CommitHash       string                 `json:"commit_hash"`
	CheckpointID     string                 `json:"checkpoint_id"`
	AIExactLines     int                    `json:"ai_exact_lines"`
	AIFormattedLines int                    `json:"ai_formatted_lines"`
	AIModifiedLines  int                    `json:"ai_modified_lines"`
	AILines          int                    `json:"ai_lines"` // exact + formatted + modified (headline number)
	HumanLines       int                    `json:"human_lines"`
	TotalLines       int                    `json:"total_lines"`
	AIPercentage     float64                `json:"ai_percentage"` // (exact + formatted + modified) / total * 100
	FilesAITouched   int                    `json:"files_ai_touched"`
	FilesTotal       int                    `json:"files_total"`
	FilesCreated     []FileChange           `json:"files_created,omitempty"`
	FilesEdited      []FileChange           `json:"files_edited,omitempty"`
	FilesDeleted     []FileChange           `json:"files_deleted,omitempty"`
	Files            []FileAttribution      `json:"files,omitempty"`
	ProviderDetails  []ProviderAttribution  `json:"provider_details,omitempty"`
	Diagnostics      AttributionDiagnostics `json:"diagnostics"`
}

// AttributeCommit computes the AI attribution breakdown for a single commit.
func (s *AttributionService) AttributeCommit(ctx context.Context, in AttributionInput) (*AttributionResult, error) {
	if strings.TrimSpace(in.CommitHash) == "" {
		return nil, fmt.Errorf("commit_hash is required")
	}

	repoPath := in.RepoPath
	if strings.TrimSpace(repoPath) == "" {
		repoPath = "."
	}

	repo, err := git.OpenRepo(repoPath)
	if err != nil {
		return nil, err
	}
	repoRoot := repo.Root()

	semDir := filepath.Join(repoRoot, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")
	objectsDir := filepath.Join(semDir, "objects")

	if !util.IsEnabled(semDir) {
		return nil, fmt.Errorf("semantica is disabled. run `semantica enable` to re-enable")
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return nil, err
	}
	defer func() { _ = sqlstore.Close(h) }()

	link, err := h.Queries.GetCommitLinkByCommitHash(ctx, in.CommitHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return commitWithNoDelta(ctx, in.CommitHash, repo)
		}
		return nil, fmt.Errorf("lookup commit link: %w", err)
	}

	cp, err := h.Queries.GetCheckpointByID(ctx, link.CheckpointID)
	if err != nil {
		return nil, fmt.Errorf("get checkpoint: %w", err)
	}

	// Use the previous commit-linked checkpoint so intermediate manual or
	// baseline checkpoints do not shrink the attribution window.
	var afterTs int64
	prev, prevErr := h.Queries.GetPreviousCommitLinkedCheckpoint(ctx, sqldb.GetPreviousCommitLinkedCheckpointParams{
		RepositoryID: cp.RepositoryID,
		CreatedAt:    cp.CreatedAt,
	})
	if prevErr == nil {
		afterTs = prev.CreatedAt
	}

	bs, err := blobs.NewStore(objectsDir)
	if err != nil {
		return nil, fmt.Errorf("init blob store: %w", err)
	}

	windowRows, err := h.Queries.ListEventsInWindow(ctx, sqldb.ListEventsInWindowParams{
		RepositoryID: cp.RepositoryID,
		AfterTs:      afterTs,
		UpToTs:       cp.CreatedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("list events in window: %w", err)
	}

	eventRows := toEventRows(ctx, bs, windowRows)
	cands, evStats := attrevents.BuildCandidatesFromRows(eventRows, repoRoot, nil)

	// Use local names for the candidate sets used by scoring and reporting.
	aiLines := cands.AILines
	providerTouchedFiles := cands.ProviderTouchedFiles
	fileProvider := cands.FileProvider
	providerModel := cands.ProviderModel

	// Derive aiTouchedFiles from ProviderTouchedFiles keys.
	// ProviderTouchedFiles includes all touched paths: provider file-touch,
	// line-level Claude writes/edits, and bash deletion paths.
	aiTouchedFiles := make(map[string]bool, len(providerTouchedFiles))
	for fp := range providerTouchedFiles {
		aiTouchedFiles[fp] = true
	}

	var diag AttributionDiagnostics
	diag.EventsConsidered = evStats.EventsConsidered
	diag.EventsAssistant = evStats.EventsAssistant
	diag.PayloadsLoaded = evStats.PayloadsLoaded
	diag.AIToolEvents = evStats.AIToolEvents

	diffBytes, err := repo.DiffForCommit(ctx, in.CommitHash)
	if err != nil {
		return nil, fmt.Errorf("get diff: %w", err)
	}

	dr := parseDiff(diffBytes)

	// Per-file carry-forward: for created files with 0 AI in the current
	// window that exist in the previous checkpoint's manifest, query
	// historical events and merge their AI candidates.
	if prevErr == nil {
		manifestFiles := loadManifestForCheckpoint(ctx, bs, prev.ManifestHash, semDir)
		cfCandidates := carryForwardCandidates(dr, manifestFiles)
		if len(cfCandidates) > 0 {
			// Score candidate files against the diff using current-window
			// candidates. Only carry-forward files that score 0 AI lines,
			// matching the gate logic in attributeWithCarryForward.
			currentCands := aiCandidates{
				aiLines:              aiLines,
				providerTouchedFiles: providerTouchedFiles,
				fileProvider:         fileProvider,
				providerModel:        providerModel,
			}
			currentScores, _ := scoreDiffPerFile(dr, currentCands)
			scoredAI := make(map[string]bool, len(currentScores))
			for _, fs := range currentScores {
				if fileScoreAILines(&fs) > 0 {
					scoredAI[fs.path] = true
				}
			}
			needsCF := make(map[string]bool)
			for path := range cfCandidates {
				if !scoredAI[path] {
					needsCF[path] = true
				}
			}
			if len(needsCF) > 0 {
				util.AppendActivityLog(semDir,
					"carry-forward: retrying historical attribution for %d deferred created file(s)", len(needsCF))
				histInput := ComputeAIPercentInput{
					RepoRoot: repoRoot,
					RepoID:   cp.RepositoryID,
					AfterTs:  0,
					UpToTs:   prev.CreatedAt,
				}
				if histEvents, histErr := loadWindowEvents(ctx, h, histInput); histErr == nil {
					histRows := toEventRows(ctx, bs, histEvents)
					histNewCands, _ := attrevents.BuildCandidatesFromRows(histRows, repoRoot, needsCF)
					// Merge historical candidates into the main maps.
					var cfLines int
					for fp, lines := range histNewCands.AILines {
						aiTouchedFiles[fp] = true
						if histNewCands.FileProvider[fp] != "" {
							fileProvider[fp] = histNewCands.FileProvider[fp]
						}
						if aiLines[fp] == nil {
							aiLines[fp] = make(map[string]struct{})
						}
						for line := range lines {
							aiLines[fp][line] = struct{}{}
							cfLines++
						}
					}
					for fp, prov := range histNewCands.ProviderTouchedFiles {
						providerTouchedFiles[fp] = prov
						aiTouchedFiles[fp] = true
					}
					for prov, model := range histNewCands.ProviderModel {
						if _, exists := providerModel[prov]; !exists {
							providerModel[prov] = model
						}
					}
					if cfLines > 0 {
						util.AppendActivityLog(semDir,
							"carry-forward: attributed %d AI line(s) from historical events", cfLines)
					}
				}
			}
		}
	}

	// Score the diff against the merged candidates.
	finalCands := aiCandidates{
		aiLines:              aiLines,
		providerTouchedFiles: providerTouchedFiles,
		fileProvider:         fileProvider,
		providerModel:        providerModel,
	}
	scores, matchStats := scoreDiffPerFile(dr, finalCands)

	// Assemble the commit result from the scored files and diff metadata.
	commitInput := buildCommitResultInput(scores, dr, aiTouchedFiles, providerModel)
	cr := attrreporting.BuildCommitResult(commitInput)
	result := fromCommitResult(cr, in.CommitHash, link.CheckpointID)

	// Populate diagnostics.
	diag.ExactMatches = matchStats.ExactMatches
	diag.NormalizedMatches = matchStats.NormalizedMatches
	diag.ModifiedMatches = matchStats.ModifiedMatches
	diag.Note = attrreporting.RenderDiagnosticNote(attrreporting.DiagnosticsInput{
		EventStats: attrreporting.EventStatsInput{
			EventsConsidered: diag.EventsConsidered,
			EventsAssistant:  diag.EventsAssistant,
			PayloadsLoaded:   diag.PayloadsLoaded,
			AIToolEvents:     diag.AIToolEvents,
		},
		MatchStats: attrreporting.MatchStatsInput{
			ExactMatches:      matchStats.ExactMatches,
			NormalizedMatches: matchStats.NormalizedMatches,
			ModifiedMatches:   matchStats.ModifiedMatches,
		},
		AIPercent: result.AIPercentage,
	})
	result.Diagnostics = diag

	return result, nil
}

// Blame resolves a generic ref (commit hash or checkpoint ID/prefix) and
// computes AI attribution. For commits, it produces full line-level
// attribution against the commit diff. For checkpoints without a commit,
// it reports AI activity (events and files touched) in the checkpoint's
// delta window.
func (s *AttributionService) Blame(ctx context.Context, in BlameInput) (*AttributionResult, error) {
	if strings.TrimSpace(in.Ref) == "" {
		return nil, fmt.Errorf("ref is required")
	}

	repoPath := in.RepoPath
	if strings.TrimSpace(repoPath) == "" {
		repoPath = "."
	}

	repo, err := git.OpenRepo(repoPath)
	if err != nil {
		return nil, err
	}
	repoRoot := repo.Root()

	semDir := filepath.Join(repoRoot, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")

	if !util.IsEnabled(semDir) {
		return nil, fmt.Errorf("semantica is disabled. run `semantica enable` to re-enable")
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return nil, err
	}
	defer func() { _ = sqlstore.Close(h) }()

	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, repoRoot)
	if err != nil {
		return nil, fmt.Errorf("repository not found for path %s", repoRoot)
	}
	repoID := repoRow.RepositoryID

	// Resolve git refs (HEAD, HEAD~1, branch names, tags) to full hashes
	// before DB lookups. If resolution fails, proceed with the raw ref
	// (it might be a checkpoint ID or commit prefix).
	ref := in.Ref
	if resolved, gitErr := repo.ResolveRef(ctx, ref); gitErr == nil {
		ref = resolved
	}

	// Try resolving as a commit hash first (exact match, then prefix).
	link, err := h.Queries.GetCommitLinkByCommitHash(ctx, ref)
	if err == nil {
		return s.AttributeCommit(ctx, AttributionInput{
			RepoPath:   in.RepoPath,
			CommitHash: link.CommitHash,
		})
	}
	matches, _ := h.Queries.ResolveCommitLinkByPrefix(ctx, sqldb.ResolveCommitLinkByPrefixParams{
		CommitHash:   ref + "%",
		RepositoryID: repoID,
	})
	if len(matches) == 1 {
		return s.AttributeCommit(ctx, AttributionInput{
			RepoPath:   in.RepoPath,
			CommitHash: matches[0],
		})
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("commit prefix %q is ambiguous, provide more characters", ref)
	}

	// Not a commit - try resolving as a checkpoint ID/prefix.
	resolvedID, cpErr := sqlstore.ResolveCheckpointID(ctx, h.Queries, repoID, ref)
	if cpErr != nil {
		return nil, fmt.Errorf("ref %q is not a known commit or checkpoint", in.Ref)
	}

	// If the checkpoint has a linked commit, use full commit attribution.
	cpLinks, _ := h.Queries.GetCommitLinksByCheckpoint(ctx, resolvedID)
	if len(cpLinks) > 0 {
		return s.AttributeCommit(ctx, AttributionInput{
			RepoPath:   in.RepoPath,
			CommitHash: cpLinks[0].CommitHash,
		})
	}

	return s.blameCheckpoint(ctx, h, repoID, resolvedID, repoRoot, semDir)
}

// ErrNoEventsInWindow is returned by ComputeAIPercentFromDiff when no agent
// events exist in the delta window. The caller can use this to decide whether
// to attempt an inline ingest before retrying.
var ErrNoEventsInWindow = fmt.Errorf("no agent events in delta window")

// ProviderAttribution holds per-provider AI line counts for trailer output.
type ProviderAttribution struct {
	Provider string
	Model    string // empty if unknown
	AILines  int
}

// AIPercentResult contains the full attribution breakdown returned by
// ComputeAIPercentFromDiff. The Percent field is the headline number;
// the remaining fields support richer commit trailers and diagnostics.
type AIPercentResult struct {
	Percent        float64
	TotalLines     int
	AILines        int
	ExactLines     int // tier 1: exact trimmed match
	ModifiedLines  int // tier 0 with hunk overlap
	FormattedLines int // tier 2: whitespace-normalized match
	FilesTouched   int // unique files in the diff
	Providers      []ProviderAttribution
}

// ComputeAIPercentInput holds parameters for the lightweight AI% computation.
type ComputeAIPercentInput struct {
	RepoRoot string
	RepoID   string
	AfterTs  int64 // lower bound of delta window (exclusive)
	UpToTs   int64 // upper bound of delta window (inclusive, checkpoint created_at)
}

// fileScore stores internal per-file attribution counts.
type fileScore struct {
	path           string
	totalLines     int
	exactLines     int
	formattedLines int
	modifiedLines  int
	humanLines     int
	providerLines  map[string]int // provider -> AI lines for this file
}

// aiCandidates holds the AI line sets and provider metadata extracted from events.
type aiCandidates struct {
	aiLines              map[string]map[string]struct{} // file -> set of trimmed lines
	providerTouchedFiles map[string]string              // file -> provider (file-level, e.g. Cursor)
	fileProvider         map[string]string              // file -> provider (line-level, e.g. Claude)
	providerModel        map[string]string              // provider -> model
}

// carryForwardResult carries the aggregate result and whether both windows
// were empty.
type carryForwardResult struct {
	result   AIPercentResult
	noEvents bool // true only if BOTH current window AND historical had no events
}

// ComputeAIPercentFromDiff computes attribution from a git diff against agent
// events in a time window. Returns a rich AIPercentResult with per-provider
// breakdown and tier diagnostics for commit trailers.
//
// Returns ErrNoEventsInWindow when no events exist, allowing the caller to
// attempt ingestion and retry.
func (s *AttributionService) ComputeAIPercentFromDiff(
	ctx context.Context,
	h *sqlstore.Handle,
	bs *blobs.Store,
	diffBytes []byte,
	in ComputeAIPercentInput,
) (AIPercentResult, error) {
	if len(diffBytes) == 0 {
		return AIPercentResult{}, nil
	}

	rows, err := loadWindowEvents(ctx, h, in)
	if err != nil {
		return AIPercentResult{}, err
	}

	eventRows := toEventRows(ctx, bs, rows)
	cands, _ := attrevents.BuildCandidatesFromRows(eventRows, in.RepoRoot, nil)

	if len(cands.AILines) == 0 && len(cands.ProviderTouchedFiles) == 0 {
		return AIPercentResult{}, nil
	}

	// Adapt candidate data for the existing scoring helpers.
	oldCands := aiCandidates{
		aiLines:              cands.AILines,
		providerTouchedFiles: cands.ProviderTouchedFiles,
		fileProvider:         cands.FileProvider,
		providerModel:        cands.ProviderModel,
	}

	dr := parseDiff(diffBytes)
	scores, _ := scoreDiffPerFile(dr, oldCands)

	return aggregateFileScores(scores, oldCands.providerModel, len(dr.files)), nil
}

// loadWindowEvents queries events in the delta window. Returns
// ErrNoEventsInWindow when no events exist.
func loadWindowEvents(ctx context.Context, h *sqlstore.Handle, in ComputeAIPercentInput) ([]sqldb.ListEventsInWindowRow, error) {
	events, err := h.Queries.ListEventsInWindow(ctx, sqldb.ListEventsInWindowParams{
		RepositoryID: in.RepoID,
		AfterTs:      in.AfterTs,
		UpToTs:       in.UpToTs,
	})
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	if len(events) == 0 {
		return nil, ErrNoEventsInWindow
	}
	return events, nil
}

// toEventRows converts DB rows into events.EventRow with pre-loaded payloads.
// It keeps storage access in the service layer while passing plain event data
// to the events package.
//
// Payloads are only loaded for assistant rows with Edit/Write/Bash tool usage,
// matching the old code's filter chain. This avoids loading blobs for irrelevant
// events (user messages, tool results, non-file-modifying assistant responses).
func toEventRows(ctx context.Context, bs *blobs.Store, rows []sqldb.ListEventsInWindowRow) []attrevents.EventRow {
	out := make([]attrevents.EventRow, len(rows))
	for i, r := range rows {
		out[i] = attrevents.EventRow{
			Provider:    r.Provider,
			Role:        r.Role.String,
			ToolUses:    r.ToolUses.String,
			PayloadHash: r.PayloadHash.String,
			Model:       r.Model.String,
		}
		// Only load payloads for assistant events with file-modifying tools.
		// Provider file-touch events (Cursor, Copilot, etc.) don't need payloads.
		if r.Role.String != "assistant" {
			continue
		}
		if !r.PayloadHash.Valid || r.PayloadHash.String == "" || bs == nil {
			continue
		}
		hasBash := r.ToolUses.Valid && strings.Contains(r.ToolUses.String, `"Bash"`)
		if !attrevents.HasEditOrWrite(r.ToolUses.String) && !hasBash {
			continue
		}
		raw, err := bs.Get(ctx, r.PayloadHash.String)
		if err == nil {
			out[i].Payload = raw
		}
	}
	return out
}

// scoreDiffPerFile delegates to attrscoring.ScoreFiles and converts the result
// back to the local fileScore type used by aggregation helpers.
func scoreDiffPerFile(dr diffResult, cands aiCandidates) ([]fileScore, attrscoring.MatchStats) {
	// Convert diffResult to scoring.DiffResult.
	sDiff := toScoringDiff(dr)

	newScores, stats := attrscoring.ScoreFiles(sDiff, cands.aiLines, cands.providerTouchedFiles, cands.fileProvider)

	// Convert scoring.FileScore back to fileScore.
	out := make([]fileScore, len(newScores))
	for i, s := range newScores {
		out[i] = fileScore{
			path:           s.Path,
			totalLines:     s.TotalLines,
			exactLines:     s.ExactLines,
			formattedLines: s.FormattedLines,
			modifiedLines:  s.ModifiedLines,
			humanLines:     s.HumanLines,
			providerLines:  s.ProviderLines,
		}
	}
	return out, stats
}

// toScoringDiff converts the internal diffResult to attrscoring.DiffResult.
func toScoringDiff(dr diffResult) attrscoring.DiffResult {
	files := make([]attrscoring.FileDiff, len(dr.files))
	for i, f := range dr.files {
		groups := make([]attrscoring.AddedGroup, len(f.groups))
		for j, g := range f.groups {
			groups[j] = attrscoring.AddedGroup{Lines: g.lines}
		}
		files[i] = attrscoring.FileDiff{
			Path:            f.path,
			Groups:          groups,
			DeletedNonBlank: f.deletedNonBlank,
		}
	}
	return attrscoring.DiffResult{
		Files:        files,
		FilesCreated: dr.filesCreated,
		FilesDeleted: dr.filesDeleted,
	}
}

// aggregateFileScores delegates to attrreporting.AggregatePercent and
// converts the result back to the local AIPercentResult type.
func aggregateFileScores(scores []fileScore, providerModel map[string]string, filesTouched int) AIPercentResult {
	inputs := make([]attrreporting.FileScoreInput, len(scores))
	for i, fs := range scores {
		inputs[i] = attrreporting.FileScoreInput{
			Path:           fs.path,
			TotalLines:     fs.totalLines,
			ExactLines:     fs.exactLines,
			FormattedLines: fs.formattedLines,
			ModifiedLines:  fs.modifiedLines,
			HumanLines:     fs.humanLines,
			ProviderLines:  fs.providerLines,
		}
	}
	r := attrreporting.AggregatePercent(inputs, providerModel, filesTouched)
	providers := make([]ProviderAttribution, len(r.Providers))
	for i, p := range r.Providers {
		providers[i] = ProviderAttribution{
			Provider: p.Provider,
			Model:    p.Model,
			AILines:  p.AILines,
		}
	}
	return AIPercentResult{
		Percent:        r.Percent,
		TotalLines:     r.TotalLines,
		AILines:        r.AILines,
		ExactLines:     r.ExactLines,
		ModifiedLines:  r.ModifiedLines,
		FormattedLines: r.FormattedLines,
		FilesTouched:   r.FilesTouched,
		Providers:      providers,
	}
}

// carryForwardCandidates delegates to attrcf.IdentifyCandidates and maps
// blobs.ManifestFile to the carryforward-local ManifestEntry type.
func carryForwardCandidates(dr diffResult, manifestFiles []blobs.ManifestFile) map[string]bool {
	entries := make([]attrcf.ManifestEntry, len(manifestFiles))
	for i, mf := range manifestFiles {
		entries[i] = attrcf.ManifestEntry{Path: mf.Path}
	}
	return attrcf.IdentifyCandidates(dr.filesCreated, entries)
}

// loadManifestForCheckpoint loads the manifest for a checkpoint's manifest hash.
// Returns nil on any failure, logging to activity.log for debuggability.
func loadManifestForCheckpoint(ctx context.Context, bs *blobs.Store, manifestHash sql.NullString, semDir string) []blobs.ManifestFile {
	if !manifestHash.Valid || manifestHash.String == "" {
		return nil
	}
	raw, err := bs.Get(ctx, manifestHash.String)
	if err != nil {
		util.AppendActivityLog(semDir,
			"carry-forward: load manifest failed: %v", err)
		return nil
	}
	var m blobs.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		util.AppendActivityLog(semDir,
			"carry-forward: unmarshal manifest failed: %v", err)
		return nil
	}
	return m.Files
}

// fileScoreAILines delegates to attrreporting.AILines.
func fileScoreAILines(fs *fileScore) int {
	input := attrreporting.FileScoreInput{
		ExactLines:     fs.exactLines,
		FormattedLines: fs.formattedLines,
		ModifiedLines:  fs.modifiedLines,
	}
	return attrreporting.AILines(&input)
}

// buildCommitResultInput converts local types to the reporting package's
// CommitResultInput. It maps fileScore -> FileScoreInput and carries diff
// metadata and the AI-touched file set.
func buildCommitResultInput(scores []fileScore, dr diffResult, aiTouchedFiles map[string]bool, providerModel map[string]string) attrreporting.CommitResultInput {
	deletedNonBlank := make(map[string]int, len(dr.files))
	for _, fd := range dr.files {
		deletedNonBlank[fd.path] = fd.deletedNonBlank
	}

	fsInputs := make([]attrreporting.FileScoreInput, len(scores))
	for i, fs := range scores {
		fsInputs[i] = attrreporting.FileScoreInput{
			Path:            fs.path,
			TotalLines:      fs.totalLines,
			ExactLines:      fs.exactLines,
			FormattedLines:  fs.formattedLines,
			ModifiedLines:   fs.modifiedLines,
			HumanLines:      fs.humanLines,
			ProviderLines:   fs.providerLines,
			DeletedNonBlank: deletedNonBlank[fs.path],
		}
	}
	return attrreporting.CommitResultInput{
		FileScores:     fsInputs,
		FilesCreated:   dr.filesCreated,
		FilesDeleted:   dr.filesDeleted,
		TouchedFiles:   aiTouchedFiles,
		ProviderModels: providerModel,
	}
}

// fromCommitResult maps reporting.CommitResult back to the service-layer
// AttributionResult, adding commit hash and checkpoint ID.
func fromCommitResult(cr attrreporting.CommitResult, commitHash, checkpointID string) *AttributionResult {
	result := &AttributionResult{
		CommitHash:       commitHash,
		CheckpointID:     checkpointID,
		AIExactLines:     cr.AIExactLines,
		AIFormattedLines: cr.AIFormattedLines,
		AIModifiedLines:  cr.AIModifiedLines,
		AILines:          cr.AILines,
		HumanLines:       cr.HumanLines,
		TotalLines:       cr.TotalLines,
		AIPercentage:     cr.AIPercentage,
		FilesAITouched:   cr.FilesAITouched,
		FilesTotal:       cr.FilesTotal,
	}
	for _, f := range cr.Files {
		result.Files = append(result.Files, FileAttribution{
			Path:             f.Path,
			AIExactLines:     f.AIExactLines,
			AIFormattedLines: f.AIFormattedLines,
			AIModifiedLines:  f.AIModifiedLines,
			HumanLines:       f.HumanLines,
			TotalLines:       f.TotalLines,
			DeletedNonBlank:  f.DeletedNonBlank,
			AIPercent:        f.AIPercent,
		})
	}
	for _, f := range cr.FilesCreated {
		result.FilesCreated = append(result.FilesCreated, FileChange{Path: f.Path, AI: f.AI})
	}
	for _, f := range cr.FilesEdited {
		result.FilesEdited = append(result.FilesEdited, FileChange{Path: f.Path, AI: f.AI})
	}
	for _, f := range cr.FilesDeleted {
		result.FilesDeleted = append(result.FilesDeleted, FileChange{Path: f.Path, AI: f.AI})
	}
	for _, p := range cr.ProviderDetails {
		result.ProviderDetails = append(result.ProviderDetails, ProviderAttribution{
			Provider: p.Provider,
			Model:    p.Model,
			AILines:  p.AILines,
		})
	}
	return result
}

// fromCheckpointResult maps reporting.CheckpointResult back to the
// service-layer AttributionResult.
func fromCheckpointResult(cr attrreporting.CheckpointResult) *AttributionResult {
	result := &AttributionResult{
		CheckpointID:   cr.CheckpointID,
		FilesAITouched: cr.FilesAITouched,
		FilesTotal:     cr.FilesTotal,
	}
	for _, f := range cr.FilesEdited {
		result.FilesEdited = append(result.FilesEdited, FileChange{Path: f.Path, AI: f.AI})
	}
	result.Diagnostics = AttributionDiagnostics{
		EventsConsidered: cr.Diagnostics.EventsConsidered,
		EventsAssistant:  cr.Diagnostics.EventsAssistant,
		PayloadsLoaded:   cr.Diagnostics.PayloadsLoaded,
		AIToolEvents:     cr.Diagnostics.AIToolEvents,
		Note:             cr.Diagnostics.Note,
	}
	return result
}

// attributeWithCarryForward scores the current window and applies bounded
// carry-forward for eligible created files. If prevCP is nil, it uses only the
// current window. noEvents is true only when neither window contains events.
func attributeWithCarryForward(
	ctx context.Context,
	h *sqlstore.Handle,
	bs *blobs.Store,
	diffBytes []byte,
	in ComputeAIPercentInput,
	prevCP *sqldb.Checkpoint,
	semDir string,
) (carryForwardResult, error) {
	if len(diffBytes) == 0 {
		return carryForwardResult{}, nil
	}

	dr := parseDiff(diffBytes)

	// Score the current window.
	currentNoEvents := false
	var currentScores []fileScore
	var currentCands aiCandidates

	events, err := loadWindowEvents(ctx, h, in)
	if errors.Is(err, ErrNoEventsInWindow) {
		currentNoEvents = true
	} else if err != nil {
		return carryForwardResult{}, err
	} else {
		eventRows := toEventRows(ctx, bs, events)
		newCands, _ := attrevents.BuildCandidatesFromRows(eventRows, in.RepoRoot, nil)
		currentCands = aiCandidates{
			aiLines: newCands.AILines, providerTouchedFiles: newCands.ProviderTouchedFiles,
			fileProvider: newCands.FileProvider, providerModel: newCands.ProviderModel,
		}
		if len(currentCands.aiLines) > 0 || len(currentCands.providerTouchedFiles) > 0 {
			currentScores, _ = scoreDiffPerFile(dr, currentCands)
		}
	}

	// If no previous checkpoint, skip carry-forward entirely.
	if prevCP == nil {
		if currentNoEvents {
			return carryForwardResult{noEvents: true}, ErrNoEventsInWindow
		}
		return carryForwardResult{
			result: aggregateFileScores(currentScores, currentCands.providerModel, len(dr.files)),
		}, nil
	}

	// Load previous manifest for candidate identification.
	manifestFiles := loadManifestForCheckpoint(ctx, bs, prevCP.ManifestHash, semDir)

	// Identify carry-forward candidates: created in diff and present in the
	// previous manifest.
	cfCandidates := carryForwardCandidates(dr, manifestFiles)

	// Filter to files that scored 0 AI in the current window.
	currentByPath := make(map[string]*fileScore, len(currentScores))
	for i := range currentScores {
		currentByPath[currentScores[i].path] = &currentScores[i]
	}

	needsCF := make(map[string]bool)
	for path := range cfCandidates {
		if fs, ok := currentByPath[path]; ok && fileScoreAILines(fs) == 0 {
			needsCF[path] = true
		} else if !ok {
			// No current score means this file had no attributable current-window
			// output, even if the window contained other events.
			needsCF[path] = true
		}
	}

	// Run historical scoring for eligible files.
	historicalNoEvents := true
	var historicalScores []fileScore
	var histCands aiCandidates

	if len(needsCF) > 0 {
		util.AppendActivityLog(semDir,
			"carry-forward: retrying historical attribution for %d deferred created file(s)", len(needsCF))

		histInput := ComputeAIPercentInput{
			RepoRoot: in.RepoRoot,
			RepoID:   in.RepoID,
			AfterTs:  0,
			UpToTs:   prevCP.CreatedAt,
		}
		histEvents, histErr := loadWindowEvents(ctx, h, histInput)
		if histErr == nil {
			historicalNoEvents = false
			histRows := toEventRows(ctx, bs, histEvents)
			histNewCands, _ := attrevents.BuildCandidatesFromRows(histRows, in.RepoRoot, needsCF)
			histCands = aiCandidates{
				aiLines: histNewCands.AILines, providerTouchedFiles: histNewCands.ProviderTouchedFiles,
				fileProvider: histNewCands.FileProvider, providerModel: histNewCands.ProviderModel,
			}
			if len(histCands.aiLines) > 0 || len(histCands.providerTouchedFiles) > 0 {
				historicalScores, _ = scoreDiffPerFile(dr, histCands)
			}
		} else if !errors.Is(histErr, ErrNoEventsInWindow) {
			util.AppendActivityLog(semDir,
				"carry-forward: historical event query failed: %v", histErr)
		}
	}

	// Merge: replace 0-AI scores with historical scores.
	historicalByPath := make(map[string]*fileScore, len(historicalScores))
	for i := range historicalScores {
		historicalByPath[historicalScores[i].path] = &historicalScores[i]
	}

	merged := currentScores
	for i := range merged {
		if hs, ok := historicalByPath[merged[i].path]; ok && fileScoreAILines(hs) > 0 {
			merged[i] = *hs
		}
	}
	// Add files that only appear in the historical result.
	for _, hs := range historicalScores {
		if _, inCurrent := currentByPath[hs.path]; !inCurrent && fileScoreAILines(&hs) > 0 {
			merged = append(merged, hs)
		}
	}

	// Log carry-forward success.
	var cfAILines int
	for path := range needsCF {
		if hs, ok := historicalByPath[path]; ok {
			cfAILines += fileScoreAILines(hs)
		}
	}
	if cfAILines > 0 {
		util.AppendActivityLog(semDir,
			"carry-forward: attributed %d AI line(s) from historical events", cfAILines)
	}

	// Collect providerModel from both windows.
	provModel := make(map[string]string)
	for k, v := range currentCands.providerModel {
		provModel[k] = v
	}
	for k, v := range histCands.providerModel {
		if _, exists := provModel[k]; !exists {
			provModel[k] = v
		}
	}

	resultNoEvents := currentNoEvents && historicalNoEvents

	if resultNoEvents {
		return carryForwardResult{noEvents: true}, ErrNoEventsInWindow
	}

	return carryForwardResult{
		result: aggregateFileScores(merged, provModel, len(dr.files)),
	}, nil
}

// blameCheckpoint computes AI activity for a checkpoint that has no
// associated commit. Without a commit diff, line-level attribution is not
// possible - instead we report which files the AI touched and event
// diagnostics.
func (s *AttributionService) blameCheckpoint(
	ctx context.Context,
	h *sqlstore.Handle,
	repoID, checkpointID, repoRoot, semDir string,
) (*AttributionResult, error) {
	cp, err := h.Queries.GetCheckpointByID(ctx, checkpointID)
	if err != nil {
		return nil, fmt.Errorf("get checkpoint: %w", err)
	}

	// Delta window: previous commit-linked checkpoint -> this checkpoint.
	var afterTs int64
	prev, prevErr := h.Queries.GetPreviousCommitLinkedCheckpoint(ctx, sqldb.GetPreviousCommitLinkedCheckpointParams{
		RepositoryID: repoID,
		CreatedAt:    cp.CreatedAt,
	})
	if prevErr == nil {
		afterTs = prev.CreatedAt
	}

	events, err := h.Queries.ListEventsInWindow(ctx, sqldb.ListEventsInWindowParams{
		RepositoryID: repoID,
		AfterTs:      afterTs,
		UpToTs:       cp.CreatedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("list events in window: %w", err)
	}

	objectsDir := filepath.Join(semDir, "objects")
	bs, err := blobs.NewStore(objectsDir)
	if err != nil {
		return nil, fmt.Errorf("init blob store: %w", err)
	}

	eventRows := toEventRows(ctx, bs, events)
	candidates, evStats := attrevents.BuildCandidatesFromRows(eventRows, repoRoot, nil)

	touchedFiles := make(map[string]bool, len(candidates.ProviderTouchedFiles))
	for fp := range candidates.ProviderTouchedFiles {
		touchedFiles[fp] = true
	}

	cr := attrreporting.BuildCheckpointResult(attrreporting.CheckpointResultInput{
		CheckpointID: checkpointID,
		TouchedFiles: touchedFiles,
		EventStats: attrreporting.EventStatsInput{
			EventsConsidered: evStats.EventsConsidered,
			EventsAssistant:  evStats.EventsAssistant,
			PayloadsLoaded:   evStats.PayloadsLoaded,
			AIToolEvents:     evStats.AIToolEvents,
		},
	})

	return fromCheckpointResult(cr), nil
}

// commitWithNoDelta builds an attribution result for a commit that has no
// linked checkpoint. All lines are attributed to humans (0% AI).
func commitWithNoDelta(ctx context.Context, hash string, repo *git.Repo) (*AttributionResult, error) {
	diffBytes, err := repo.DiffForCommit(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("get diff: %w", err)
	}

	dr := parseDiff(diffBytes)
	result := &AttributionResult{
		CommitHash: hash,
	}

	createdSet := make(map[string]bool, len(dr.filesCreated))
	for _, f := range dr.filesCreated {
		createdSet[f] = true
	}

	for _, fd := range dr.files {
		fa := FileAttribution{Path: fd.path, DeletedNonBlank: fd.deletedNonBlank}
		for _, group := range fd.groups {
			for _, line := range group.lines {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" {
					continue
				}
				fa.TotalLines++
				fa.HumanLines++
			}
		}
		result.HumanLines += fa.HumanLines
		result.TotalLines += fa.TotalLines
		result.Files = append(result.Files, fa)

		if createdSet[fd.path] {
			result.FilesCreated = append(result.FilesCreated, FileChange{Path: fd.path})
		} else if fa.TotalLines > 0 {
			result.FilesEdited = append(result.FilesEdited, FileChange{Path: fd.path})
		}
	}
	for _, f := range dr.filesDeleted {
		result.FilesDeleted = append(result.FilesDeleted, FileChange{Path: f})
	}

	result.FilesTotal = len(dr.files)
	result.Diagnostics = AttributionDiagnostics{
		Note: "No linked checkpoint found. This commit was made without a tracked agent session.",
	}
	return result, nil
}

// diffResult holds the parsed output of a unified diff.
type diffResult struct {
	files        []fileDiff // all files present in the diff
	filesCreated []string   // paths that went from /dev/null -> b/path (new files)
	filesDeleted []string   // paths that went from a/path -> /dev/null (removed files)
}

// addedGroup is a contiguous block of added lines within a diff hunk.
// Context and removal lines break groups, so each group represents lines
// that are adjacent in the output file.
type addedGroup struct {
	lines []string // "+" lines with prefix stripped
}

// fileDiff holds the added lines for a single file in a unified diff,
// grouped into contiguous runs. Groups are broken by context lines,
// removal lines, or @@ headers.
type fileDiff struct {
	path            string       // repo-relative file path
	groups          []addedGroup // contiguous groups of added lines
	deletedNonBlank int          // count of deleted non-blank lines
}

// parseDiff delegates to attrscoring.ParseDiff and converts the result to the
// local diffResult type used by the remaining service helpers.
func parseDiff(diffBytes []byte) diffResult {
	sd := attrscoring.ParseDiff(diffBytes)
	return fromScoringDiff(sd)
}

// fromScoringDiff converts attrscoring.DiffResult to the internal diffResult.
func fromScoringDiff(sd attrscoring.DiffResult) diffResult {
	files := make([]fileDiff, len(sd.Files))
	for i, f := range sd.Files {
		groups := make([]addedGroup, len(f.Groups))
		for j, g := range f.Groups {
			groups[j] = addedGroup{lines: g.Lines}
		}
		files[i] = fileDiff{
			path:            f.Path,
			groups:          groups,
			deletedNonBlank: f.DeletedNonBlank,
		}
	}
	return diffResult{
		files:        files,
		filesCreated: sd.FilesCreated,
		filesDeleted: sd.FilesDeleted,
	}
}
