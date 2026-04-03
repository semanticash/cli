// Package service implements the core business logic for the semantica CLI.
package service

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

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

	events, err := h.Queries.ListEventsInWindow(ctx, sqldb.ListEventsInWindowParams{
		RepositoryID: cp.RepositoryID,
		AfterTs:      afterTs,
		UpToTs:       cp.CreatedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("list events in window: %w", err)
	}

	aiLines := make(map[string]map[string]struct{})
	aiTouchedFiles := make(map[string]bool)
	providerTouchedFiles := make(map[string]string)
	fileProvider := make(map[string]string)
	providerModel := make(map[string]string)

	var diag AttributionDiagnostics
	diag.EventsConsidered = len(events)

	for _, ev := range events {
		if ev.Model.Valid && ev.Model.String != "" {
			providerModel[ev.Provider] = ev.Model.String
		}

		// Some providers report file touches on tool-role events.
		if hasProviderFileEdit(ev.ToolUses) {
			diag.AIToolEvents++
			for _, fp := range extractProviderFileTouches(ev.ToolUses) {
				providerTouchedFiles[fp] = ev.Provider
				aiTouchedFiles[fp] = true
			}
			continue
		}

		if ev.Role.String != "assistant" {
			continue
		}
		diag.EventsAssistant++

		if !ev.PayloadHash.Valid || ev.PayloadHash.String == "" {
			continue
		}

		hasBash := ev.ToolUses.Valid && strings.Contains(ev.ToolUses.String, `"Bash"`)
		if !hasEditOrWrite(ev.ToolUses) && !hasBash {
			continue
		}
		diag.AIToolEvents++

		raw, err := bs.Get(ctx, ev.PayloadHash.String)
		if err != nil {
			continue
		}
		diag.PayloadsLoaded++

		fileLines, bashCommands := extractClaudeActions(raw, repoRoot)
		for fp, lines := range fileLines {
			aiTouchedFiles[fp] = true
			fileProvider[fp] = ev.Provider
			if aiLines[fp] == nil {
				aiLines[fp] = make(map[string]struct{})
			}
			for line := range lines {
				aiLines[fp][line] = struct{}{}
			}
		}

		for _, cmd := range bashCommands {
			for _, fp := range extractDeletedPaths(cmd, repoRoot) {
				aiTouchedFiles[fp] = true
			}
		}
	}

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
			currentScores := scoreDiffPerFile(dr, currentCands)
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
					histCands := buildAICandidates(ctx, bs, histEvents, repoRoot, needsCF)
					// Merge historical candidates into the main maps.
					var cfLines int
					for fp, lines := range histCands.aiLines {
						aiTouchedFiles[fp] = true
						if histCands.fileProvider[fp] != "" {
							fileProvider[fp] = histCands.fileProvider[fp]
						}
						if aiLines[fp] == nil {
							aiLines[fp] = make(map[string]struct{})
						}
						for line := range lines {
							aiLines[fp][line] = struct{}{}
							cfLines++
						}
					}
					for fp, prov := range histCands.providerTouchedFiles {
						providerTouchedFiles[fp] = prov
						aiTouchedFiles[fp] = true
					}
					for prov, model := range histCands.providerModel {
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

	aiLinesNorm := buildNormalizedSet(aiLines)

	result := &AttributionResult{
		CommitHash:   in.CommitHash,
		CheckpointID: link.CheckpointID,
	}

	createdSet := make(map[string]bool, len(dr.filesCreated))
	for _, f := range dr.filesCreated {
		createdSet[f] = true
	}

	filesWithAI := make(map[string]bool)
	providerLines := make(map[string]int) // provider -> AI lines

	for _, fd := range dr.files {
		fa := FileAttribution{Path: fd.path, DeletedNonBlank: fd.deletedNonBlank}

		provider, isProviderFile := providerTouchedFiles[fd.path]
		isProviderOnly := isProviderFile && aiLines[fd.path] == nil
		if isProviderOnly {
			for _, group := range fd.groups {
				for _, line := range group.lines {
					trimmed := strings.TrimSpace(line)
					if trimmed == "" {
						continue
					}
					fa.TotalLines++
					fa.AIModifiedLines++
					diag.ModifiedMatches++
					providerLines[provider]++
				}
			}
			if fa.TotalLines > 0 {
				fa.AIPercent = float64(fa.AIModifiedLines) / float64(fa.TotalLines) * 100
				filesWithAI[fd.path] = true
			}
			result.AIModifiedLines += fa.AIModifiedLines
			result.AILines += fa.AIModifiedLines // Cursor: modified counts in headline
			result.TotalLines += fa.TotalLines
			result.Files = append(result.Files, fa)

			isAI := true
			if createdSet[fd.path] {
				result.FilesCreated = append(result.FilesCreated, FileChange{Path: fd.path, AI: isAI})
			} else if fa.TotalLines > 0 {
				result.FilesEdited = append(result.FilesEdited, FileChange{Path: fd.path, AI: isAI})
			}
			continue
		}

		prov := fileProvider[fd.path]
		if prov == "" && isProviderFile {
			prov = provider
		}

		for _, group := range fd.groups {
			// Pass 1: classify each line via Tier-1/Tier-2, track overlap.
			type lineClass struct {
				tier int // 0=unmatched, 1=exact, 2=normalized
			}
			var classes []lineClass
			hasOverlap := false

			for _, line := range group.lines {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" {
					continue
				}
				lc := lineClass{}

				// Tier 1: exact trimmed match.
				if fileSet, ok := aiLines[fd.path]; ok {
					if _, found := fileSet[trimmed]; found {
						lc.tier = 1
						hasOverlap = true
					}
				}
				// Tier 2: whitespace-stripped match (only if Tier 1 missed).
				if lc.tier == 0 {
					norm := normalizeWhitespace(trimmed)
					if normSet, ok := aiLinesNorm[fd.path]; ok {
						if _, found := normSet[norm]; found {
							lc.tier = 2
							hasOverlap = true
						}
					}
				}
				classes = append(classes, lc)
			}

			// Pass 2: assign final categories based on group overlap.
			for _, lc := range classes {
				fa.TotalLines++
				switch {
				case lc.tier == 1:
					fa.AIExactLines++
					diag.ExactMatches++
					providerLines[prov]++
				case lc.tier == 2:
					fa.AIFormattedLines++
					diag.NormalizedMatches++
					providerLines[prov]++
				case hasOverlap:
					fa.AIModifiedLines++
					diag.ModifiedMatches++
					providerLines[prov]++
				default:
					fa.HumanLines++
				}
			}
		}

		aiAuthored := fa.AIExactLines + fa.AIFormattedLines + fa.AIModifiedLines
		if fa.TotalLines > 0 && aiAuthored > 0 {
			fa.AIPercent = float64(aiAuthored) / float64(fa.TotalLines) * 100
			filesWithAI[fd.path] = true
		}

		result.AIExactLines += fa.AIExactLines
		result.AIFormattedLines += fa.AIFormattedLines
		result.AIModifiedLines += fa.AIModifiedLines
		result.AILines += aiAuthored
		result.HumanLines += fa.HumanLines
		result.TotalLines += fa.TotalLines
		result.Files = append(result.Files, fa)

		isAI := filesWithAI[fd.path] || aiTouchedFiles[fd.path]
		if createdSet[fd.path] {
			result.FilesCreated = append(result.FilesCreated, FileChange{Path: fd.path, AI: isAI})
		} else if fa.TotalLines > 0 {
			result.FilesEdited = append(result.FilesEdited, FileChange{Path: fd.path, AI: isAI})
		}
	}

	for _, f := range dr.filesDeleted {
		result.FilesDeleted = append(result.FilesDeleted, FileChange{Path: f, AI: aiTouchedFiles[f]})
	}

	result.FilesTotal = len(result.FilesCreated) + len(result.FilesEdited)
	result.FilesAITouched = len(filesWithAI)
	if result.TotalLines > 0 {
		result.AIPercentage = float64(result.AILines) / float64(result.TotalLines) * 100
	}

	for prov, lines := range providerLines {
		if lines > 0 {
			result.ProviderDetails = append(result.ProviderDetails, ProviderAttribution{
				Provider: prov,
				Model:    providerModel[prov],
				AILines:  lines,
			})
		}
	}
	sort.Slice(result.ProviderDetails, func(i, j int) bool {
		if result.ProviderDetails[i].AILines != result.ProviderDetails[j].AILines {
			return result.ProviderDetails[i].AILines > result.ProviderDetails[j].AILines
		}
		return result.ProviderDetails[i].Provider < result.ProviderDetails[j].Provider
	})

	if result.AIPercentage == 0 {
		switch {
		case diag.EventsConsidered == 0:
			diag.Note = "No agent events found in the delta window between checkpoints."
		case diag.AIToolEvents == 0:
			diag.Note = "Agent events found but none contained file-modifying tool calls (Edit/Write)."
		case diag.PayloadsLoaded == 0:
			diag.Note = "Agent tool calls found but payloads could not be loaded from blob store."
		default:
			diag.Note = "Agent tool calls found but no added lines in the commit matched AI-produced output."
		}
	} else if diag.NormalizedMatches > 0 || diag.ModifiedMatches > 0 {
		parts := []string{fmt.Sprintf("%d exact", diag.ExactMatches)}
		if diag.NormalizedMatches > 0 {
			parts = append(parts, fmt.Sprintf("%d normalized", diag.NormalizedMatches))
		}
		if diag.ModifiedMatches > 0 {
			parts = append(parts, fmt.Sprintf("%d modified", diag.ModifiedMatches))
		}
		diag.Note = fmt.Sprintf("AI matches: %s.", strings.Join(parts, ", "))
	}
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

	events, err := loadWindowEvents(ctx, h, in)
	if err != nil {
		return AIPercentResult{}, err
	}

	cands := buildAICandidates(ctx, bs, events, in.RepoRoot, nil)

	if len(cands.aiLines) == 0 && len(cands.providerTouchedFiles) == 0 {
		return AIPercentResult{}, nil
	}

	dr := parseDiff(diffBytes)
	scores := scoreDiffPerFile(dr, cands)

	return aggregateFileScores(scores, cands.providerModel, len(dr.files)), nil
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

// buildAICandidates extracts AI line sets and provider metadata from events.
// When eligibleFiles is non-nil, only files in the set contribute to candidate
// maps (gating happens at extraction time, not in the diff loop).
func buildAICandidates(ctx context.Context, bs *blobs.Store, events []sqldb.ListEventsInWindowRow, repoRoot string, eligibleFiles map[string]bool) aiCandidates {
	c := aiCandidates{
		aiLines:              make(map[string]map[string]struct{}),
		providerTouchedFiles: make(map[string]string),
		fileProvider:         make(map[string]string),
		providerModel:        make(map[string]string),
	}

	for _, ev := range events {
		if ev.Model.Valid && ev.Model.String != "" {
			c.providerModel[ev.Provider] = ev.Model.String
		}

		if hasProviderFileEdit(ev.ToolUses) {
			for _, fp := range extractProviderFileTouches(ev.ToolUses) {
				if eligibleFiles != nil && !eligibleFiles[fp] {
					continue
				}
				c.providerTouchedFiles[fp] = ev.Provider
			}
			continue
		}

		if ev.Role.String != "assistant" {
			continue
		}

		if !ev.PayloadHash.Valid || ev.PayloadHash.String == "" {
			continue
		}
		if !hasEditOrWrite(ev.ToolUses) {
			continue
		}
		raw, err := bs.Get(ctx, ev.PayloadHash.String)
		if err != nil {
			continue
		}
		fileLines, _ := extractClaudeActions(raw, repoRoot)
		for fp, lines := range fileLines {
			if eligibleFiles != nil && !eligibleFiles[fp] {
				continue
			}
			c.fileProvider[fp] = ev.Provider
			if c.aiLines[fp] == nil {
				c.aiLines[fp] = make(map[string]struct{})
			}
			for line := range lines {
				c.aiLines[fp][line] = struct{}{}
			}
		}
	}

	return c
}

// scoreDiffPerFile matches AI candidate maps against a parsed diff and returns
// per-file scores. Each fileScore contains that file's total/exact/formatted/
// modified/human line counts and per-provider breakdown.
func scoreDiffPerFile(dr diffResult, cands aiCandidates) []fileScore {
	aiLinesNorm := buildNormalizedSet(cands.aiLines)

	var scores []fileScore

	for _, fd := range dr.files {
		fs := fileScore{
			path:          fd.path,
			providerLines: make(map[string]int),
		}

		provider, isProviderFile := cands.providerTouchedFiles[fd.path]
		isProviderOnly := isProviderFile && cands.aiLines[fd.path] == nil
		if isProviderOnly {
			for _, group := range fd.groups {
				for _, line := range group.lines {
					trimmed := strings.TrimSpace(line)
					if trimmed == "" {
						continue
					}
					fs.totalLines++
					fs.modifiedLines++
					fs.providerLines[provider]++
				}
			}
			scores = append(scores, fs)
			continue
		}

		prov := cands.fileProvider[fd.path]
		if prov == "" && isProviderFile {
			prov = provider
		}

		for _, group := range fd.groups {
			type lc struct{ tier int }
			var classes []lc
			hasOverlap := false

			for _, line := range group.lines {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" {
					continue
				}
				c := lc{}
				if fileSet, ok := cands.aiLines[fd.path]; ok {
					if _, found := fileSet[trimmed]; found {
						c.tier = 1
						hasOverlap = true
					}
				}
				if c.tier == 0 {
					norm := normalizeWhitespace(trimmed)
					if normSet, ok := aiLinesNorm[fd.path]; ok {
						if _, found := normSet[norm]; found {
							c.tier = 2
							hasOverlap = true
						}
					}
				}
				classes = append(classes, c)
			}

			for _, c := range classes {
				fs.totalLines++
				switch {
				case c.tier == 1:
					fs.exactLines++
					fs.providerLines[prov]++
				case c.tier == 2:
					fs.formattedLines++
					fs.providerLines[prov]++
				case c.tier == 0 && hasOverlap:
					fs.modifiedLines++
					fs.providerLines[prov]++
				default:
					fs.humanLines++
				}
			}
		}

		scores = append(scores, fs)
	}

	return scores
}

// aggregateFileScores reduces per-file scores into a single AIPercentResult.
func aggregateFileScores(scores []fileScore, providerModel map[string]string, filesTouched int) AIPercentResult {
	var totalLines, aiAuthored int
	var exactLines, formattedLines, modifiedLines int
	providerLines := make(map[string]int)

	for _, fs := range scores {
		totalLines += fs.totalLines
		exactLines += fs.exactLines
		formattedLines += fs.formattedLines
		modifiedLines += fs.modifiedLines
		aiAuthored += fs.exactLines + fs.formattedLines + fs.modifiedLines
		for prov, lines := range fs.providerLines {
			providerLines[prov] += lines
		}
	}

	if totalLines == 0 {
		return AIPercentResult{}
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

	return AIPercentResult{
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

// carryForwardCandidates returns files eligible for historical lookback:
// created in the current diff AND present in the previous commit-linked
// checkpoint's manifest.
func carryForwardCandidates(dr diffResult, manifestFiles []blobs.ManifestFile) map[string]bool {
	if len(dr.filesCreated) == 0 || len(manifestFiles) == 0 {
		return nil
	}

	manifestSet := make(map[string]bool, len(manifestFiles))
	for _, mf := range manifestFiles {
		manifestSet[mf.Path] = true
	}

	var result map[string]bool
	for _, path := range dr.filesCreated {
		if manifestSet[path] {
			if result == nil {
				result = make(map[string]bool)
			}
			result[path] = true
		}
	}
	return result
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

// fileScoreAILines returns the total AI lines (exact + formatted + modified)
// for a single file score.
func fileScoreAILines(fs *fileScore) int {
	return fs.exactLines + fs.formattedLines + fs.modifiedLines
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
		currentCands = buildAICandidates(ctx, bs, events, in.RepoRoot, nil)
		if len(currentCands.aiLines) > 0 || len(currentCands.providerTouchedFiles) > 0 {
			currentScores = scoreDiffPerFile(dr, currentCands)
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
			histCands = buildAICandidates(ctx, bs, histEvents, in.RepoRoot, needsCF)
			if len(histCands.aiLines) > 0 || len(histCands.providerTouchedFiles) > 0 {
				historicalScores = scoreDiffPerFile(dr, histCands)
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

	aiTouchedFiles := make(map[string]bool)
	var diag AttributionDiagnostics
	diag.EventsConsidered = len(events)

	for _, ev := range events {
		// File-level attribution from tool_uses metadata (Cursor, Copilot).
		if hasProviderFileEdit(ev.ToolUses) {
			diag.AIToolEvents++
			for _, fp := range extractProviderFileTouches(ev.ToolUses) {
				aiTouchedFiles[fp] = true
			}
			continue
		}

		if ev.Role.String != "assistant" {
			continue
		}
		diag.EventsAssistant++

		// Claude path.
		if !ev.PayloadHash.Valid || ev.PayloadHash.String == "" {
			continue
		}

		hasBash := ev.ToolUses.Valid && strings.Contains(ev.ToolUses.String, `"Bash"`)
		if !hasEditOrWrite(ev.ToolUses) && !hasBash {
			continue
		}
		diag.AIToolEvents++

		raw, err := bs.Get(ctx, ev.PayloadHash.String)
		if err != nil {
			continue
		}
		diag.PayloadsLoaded++

		fileLines, bashCommands := extractClaudeActions(raw, repoRoot)
		for fp := range fileLines {
			aiTouchedFiles[fp] = true
		}
		for _, cmd := range bashCommands {
			for _, fp := range extractDeletedPaths(cmd, repoRoot) {
				aiTouchedFiles[fp] = true
			}
		}
	}

	result := &AttributionResult{
		CheckpointID:   checkpointID,
		FilesAITouched: len(aiTouchedFiles),
	}

	for fp := range aiTouchedFiles {
		result.FilesEdited = append(result.FilesEdited, FileChange{Path: fp, AI: true})
	}
	result.FilesTotal = len(aiTouchedFiles)

	if diag.EventsConsidered == 0 {
		diag.Note = "No agent events found in the delta window."
	} else if diag.AIToolEvents == 0 {
		diag.Note = "Agent events found but none contained file-modifying tool calls (Edit/Write)."
	}
	result.Diagnostics = diag

	return result, nil
}

// hasEditOrWrite is a fast pre-filter that checks the tool_uses JSON column
// for Edit or Write tool names without fully parsing the JSON.
func hasEditOrWrite(toolUses sql.NullString) bool {
	if !toolUses.Valid || toolUses.String == "" {
		return false
	}
	s := toolUses.String
	return strings.Contains(s, `"Edit"`) || strings.Contains(s, `"Write"`)
}

// hasProviderFileEdit checks if the tool_uses column indicates a provider
// file edit event. Matches tool names from Cursor, Copilot, and Gemini
// that represent file modifications without line-level payload content.
func hasProviderFileEdit(toolUses sql.NullString) bool {
	if !toolUses.Valid || toolUses.String == "" {
		return false
	}
	s := toolUses.String
	return strings.Contains(s, `"cursor_file_edit"`) ||
		strings.Contains(s, `"cursor_edit"`) ||
		strings.Contains(s, `"copilot_file_edit"`) ||
		strings.Contains(s, `"kiro_file_edit"`) ||
		strings.Contains(s, `"editFile"`) ||
		strings.Contains(s, `"createFile"`) ||
		strings.Contains(s, `"write_file"`) ||
		strings.Contains(s, `"edit_file"`) ||
		strings.Contains(s, `"save_file"`) ||
		strings.Contains(s, `"replace"`)
}

// extractProviderFileTouches parses the tool_uses JSON from a Cursor or
// Copilot event and returns repo-relative file paths that the AI touched.
// Paths are already stored as repo-relative by the ingest pipeline.
func extractProviderFileTouches(toolUses sql.NullString) []string {
	if !toolUses.Valid || toolUses.String == "" {
		return nil
	}
	var payload struct {
		Tools []struct {
			FilePath string `json:"file_path"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(toolUses.String), &payload); err != nil {
		return nil
	}
	var paths []string
	for _, t := range payload.Tools {
		if t.FilePath != "" {
			paths = append(paths, t.FilePath)
		}
	}
	return paths
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

// parseDiff parses a unified diff (as produced by "git diff") into per-file
// added lines, and identifies newly created and deleted files.
//
// It recognizes:
//   - "--- /dev/null" + "+++ b/path" -> file created
//   - "--- a/path" + "+++ /dev/null" -> file deleted
//   - Lines starting with "+" (excluding the +++ header) -> added lines
//
// Diff metadata lines (diff --git, index, @@, new file, deleted file) are
// skipped. Only the "b/" side path is used for file identification.
func parseDiff(diffBytes []byte) diffResult {
	var res diffResult
	var current *fileDiff
	var currentOldPath string
	inAddedRun := false // tracking a contiguous run of "+" lines

	// finalizeGroup closes the current contiguous added-line group (if any).
	finalizeGroup := func() {
		if !inAddedRun || current == nil {
			inAddedRun = false
			return
		}
		inAddedRun = false
	}

	scanner := bufio.NewScanner(bytes.NewReader(diffBytes))
	for scanner.Scan() {
		line := scanner.Text()

		// Track the "from" path (--- a/path or --- /dev/null).
		if strings.HasPrefix(line, "--- ") {
			finalizeGroup()
			currentOldPath = strings.TrimPrefix(line, "--- ")
			continue
		}

		// Track the "to" path and detect creates/deletes.
		if strings.HasPrefix(line, "+++ ") {
			finalizeGroup()
			newPath := strings.TrimPrefix(line, "+++ ")

			if currentOldPath == "/dev/null" && strings.HasPrefix(newPath, "b/") {
				path := strings.TrimPrefix(newPath, "b/")
				res.filesCreated = append(res.filesCreated, path)
			} else if newPath == "/dev/null" && strings.HasPrefix(currentOldPath, "a/") {
				path := strings.TrimPrefix(currentOldPath, "a/")
				res.filesDeleted = append(res.filesDeleted, path)
			}

			if strings.HasPrefix(newPath, "b/") {
				path := strings.TrimPrefix(newPath, "b/")
				res.files = append(res.files, fileDiff{path: path})
				current = &res.files[len(res.files)-1]
			} else if newPath == "/dev/null" && strings.HasPrefix(currentOldPath, "a/") {
				// Deleted file: create a fileDiff so "-" lines are counted.
				path := strings.TrimPrefix(currentOldPath, "a/")
				res.files = append(res.files, fileDiff{path: path})
				current = &res.files[len(res.files)-1]
			} else {
				current = nil
			}
			currentOldPath = ""
			continue
		}

		// Skip diff metadata headers - these also break added-line runs.
		if strings.HasPrefix(line, "diff --git") ||
			strings.HasPrefix(line, "index ") || strings.HasPrefix(line, "@@") ||
			strings.HasPrefix(line, "new file") || strings.HasPrefix(line, "deleted file") {
			finalizeGroup()
			continue
		}

		// Collect added lines (strip the leading "+") into contiguous groups.
		if current != nil && strings.HasPrefix(line, "+") {
			if !inAddedRun {
				// Start a new group.
				current.groups = append(current.groups, addedGroup{})
				inAddedRun = true
			}
			g := &current.groups[len(current.groups)-1]
			g.lines = append(g.lines, line[1:])
		} else if current != nil && strings.HasPrefix(line, "-") {
			finalizeGroup()
			if trimmed := strings.TrimSpace(line[1:]); trimmed != "" {
				current.deletedNonBlank++
			}
		} else {
			// Context line or anything else breaks the run.
			finalizeGroup()
		}
	}

	return res
}

// extractClaudeActions parses an assistant payload blob from the content-
// addressed store and extracts the actions the AI performed:
//
//   - Edit tool calls: lines from the "new_string" field (replacement text)
//   - Write tool calls: lines from the "content" field (full file content)
//   - Bash tool calls: the raw command string (used to detect file deletions)
//
// The payload format is a JSON object with structure:
//
//	{"type": "assistant", "message": {"content": [{"type": "tool_use", "name": "Edit", "input": {...}}]}}
//
// Returns two values:
//   - fileLines: map of repo-relative file paths to sets of trimmed line content
//   - bashCommands: raw command strings from Bash tool calls
func extractClaudeActions(raw []byte, repoRoot string) (fileLines map[string]map[string]struct{}, bashCommands []string) {
	fileLines = make(map[string]map[string]struct{})

	var payload struct {
		Type    string `json:"type"`
		Message struct {
			Content []json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return
	}
	if payload.Type != "assistant" {
		return
	}

	for _, blockRaw := range payload.Message.Content {
		var block struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(blockRaw, &block); err != nil {
			continue
		}
		if block.Type != "tool_use" {
			continue
		}

		switch block.Name {
		case "Edit":
			var inp struct {
				FilePath  string `json:"file_path"`
				NewString string `json:"new_string"`
			}
			if err := json.Unmarshal(block.Input, &inp); err != nil || inp.NewString == "" {
				continue
			}
			relPath := normalizePath(inp.FilePath, repoRoot)
			addLines(fileLines, relPath, inp.NewString)

		case "Write":
			var inp struct {
				FilePath string `json:"file_path"`
				Content  string `json:"content"`
			}
			if err := json.Unmarshal(block.Input, &inp); err != nil || inp.Content == "" {
				continue
			}
			relPath := normalizePath(inp.FilePath, repoRoot)
			addLines(fileLines, relPath, inp.Content)

		case "Bash":
			var inp struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(block.Input, &inp); err != nil || inp.Command == "" {
				continue
			}
			bashCommands = append(bashCommands, inp.Command)
		}
	}

	return
}

// extractDeletedPaths extracts file paths from a shell command that
// contains "rm". It tokenizes the command by whitespace and returns
// repo-relative paths for any token that isn't a flag or the "rm" command
// itself.
func extractDeletedPaths(cmd, repoRoot string) []string {
	if !strings.Contains(cmd, "rm ") {
		return nil
	}

	var paths []string
	for _, token := range strings.Fields(cmd) {
		if token == "rm" || strings.HasPrefix(token, "-") {
			continue
		}
		rel := normalizePath(token, repoRoot)
		if rel != "" && rel != "." {
			paths = append(paths, rel)
		}
	}
	return paths
}

// addLines splits text into lines and inserts each trimmed, non-blank line
// into the set for the given file path. This is the core building block for
// constructing the AI candidate line set.
func addLines(m map[string]map[string]struct{}, filePath, text string) {
	if filePath == "" {
		return
	}
	if m[filePath] == nil {
		m[filePath] = make(map[string]struct{})
	}
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		m[filePath][trimmed] = struct{}{}
	}
}

// normalizePath converts an absolute file path to a repo-relative path
// using forward slashes, matching the format produced by "git diff".
// Falls back to the base name if the path cannot be made relative.
func normalizePath(filePath, repoRoot string) string {
	if filePath == "" {
		return ""
	}
	rel, err := filepath.Rel(repoRoot, filePath)
	if err != nil {
		return filepath.Base(filePath)
	}
	return filepath.ToSlash(rel)
}

// --- Tier-2 and Tier-3 matching helpers ---

// normalizeWhitespace removes all whitespace characters from s.
// Used as a second-tier match when exact trimmed comparison fails,
// catching formatter/linter modifications like:
//
//	"func foo(){" vs "func foo() {"
//	"return   x+y" vs "return x + y"
func normalizeWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if !unicode.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// buildNormalizedSet derives a whitespace-stripped line set from the
// existing AI candidate set. Each trimmed line is stripped of all
// whitespace and stored per file path.
func buildNormalizedSet(aiLines map[string]map[string]struct{}) map[string]map[string]struct{} {
	norm := make(map[string]map[string]struct{}, len(aiLines))
	for fp, lines := range aiLines {
		norm[fp] = make(map[string]struct{}, len(lines))
		for line := range lines {
			norm[fp][normalizeWhitespace(line)] = struct{}{}
		}
	}
	return norm
}
