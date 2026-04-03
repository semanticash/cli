package service

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"

	// Register hook providers via init().
	_ "github.com/semanticash/cli/internal/hooks/claude"
	_ "github.com/semanticash/cli/internal/hooks/cursor"
	_ "github.com/semanticash/cli/internal/hooks/gemini"
	_ "github.com/semanticash/cli/internal/hooks/kirocli"
	_ "github.com/semanticash/cli/internal/hooks/kiroide"
)

type CommitMsgHookService struct {
	RepoPath string
}

func NewCommitMsgHookService(repoPath string) *CommitMsgHookService {
	return &CommitMsgHookService{RepoPath: repoPath}
}

func (s *CommitMsgHookService) Run(ctx context.Context, msgFile string) error {
	if msgFile == "" {
		return fmt.Errorf("commit message file path is required")
	}

	repo, err := git.OpenRepo(s.RepoPath)
	if err != nil {
		// never block commits
		return nil
	}
	repoRoot := repo.Root()

	semDir := filepath.Join(repoRoot, ".semantica")
	if !util.IsEnabled(semDir) {
		return nil
	}
	dbPath := filepath.Join(semDir, "lineage.db")

	// If the handoff file is missing (for example, with --no-verify),
	// checkpointID stays empty and attribution becomes best-effort only.
	var checkpointID string
	handoffPath := util.PreCommitCheckpointPath(semDir)
	if raw, err := os.ReadFile(handoffPath); err == nil {
		parts := strings.SplitN(strings.TrimSpace(string(raw)), "|", 2)
		checkpointID = strings.TrimSpace(parts[0])
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.OpenOptions{
		BusyTimeout: 200 * time.Millisecond,
		Synchronous: "NORMAL",
	})
	if err == nil {
		defer func() { _ = sqlstore.Close(h) }()
	} else if checkpointID != "" {
		util.AppendActivityLog(semDir,
			"commit-msg warning: db open failed: %v", err)
	}

	// Best-effort attribution: use existing DB events first, then try a
	// targeted active-session flush before retrying. Hard cap: 1 second.
	var attrResult *commitAttrResult
	if checkpointID != "" && h != nil {
		attrResult = s.computeAttribution(ctx, h, repo, repoRoot, semDir, checkpointID)
	}

	// Write a summary file for post-commit to read and display.
	writeAttributionSummary(semDir, attrResult)

	if checkpointID == "" && attrResult == nil {
		return nil
	}

	orig, err := os.ReadFile(msgFile)
	if err != nil {
		// never block commits
		return nil
	}
	text := string(orig)

	// Avoid duplicate trailers - scan line-by-line to handle whitespace/\r\n variants.
	hasCheckpoint, hasAttribution, hasDiagnostics := scanForTrailers(text)

	// Trailer config: checkpoint is always on; attribution and diagnostics are configurable.
	trailersOn := util.TrailersEnabled(semDir)

	var trailers []string
	if checkpointID != "" && !hasCheckpoint {
		trailers = append(trailers, fmt.Sprintf("Semantica-Checkpoint: %s", checkpointID))
	}
	if trailersOn && attrResult != nil && !hasAttribution {
		trailers = append(trailers, formatAttributionTrailers(attrResult.result, attrResult.totalLines)...)
	}
	if trailersOn {
		if attrResult != nil && !hasDiagnostics {
			trailers = append(trailers, formatDiagnosticsTrailer(attrResult))
		} else if attrResult == nil && checkpointID != "" && !hasDiagnostics {
			trailers = append(trailers, "Semantica-Diagnostics: attribution unavailable")
		}
	}
	if len(trailers) == 0 {
		return nil
	}

	// Ensure message ends with newline
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	// Ensure a blank line before trailers
	trimmed := strings.TrimRight(text, "\n")
	if trimmed != "" {
		text = trimmed + "\n\n"
	} else {
		text = ""
	}

	// Append trailers
	var w strings.Builder
	w.WriteString(text)
	for _, t := range trailers {
		w.WriteString(t)
		w.WriteString("\n")
	}

	f, err := os.OpenFile(msgFile, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	bw := bufio.NewWriter(f)
	if _, err := bw.WriteString(w.String()); err != nil {
		util.AppendActivityLog(semDir,
			"commit-msg warning: write failed: %v", err)
		return nil
	}
	if err := bw.Flush(); err != nil {
		util.AppendActivityLog(semDir,
			"commit-msg warning: flush failed: %v", err)
		return nil
	}

	return nil
}

// commitAttrResult wraps the attribution result with contextual information
// about why the result is what it is, enabling richer trailer messages.
type commitAttrResult struct {
	result     *AIPercentResult // nil only if computation completely failed
	totalLines int              // total modified lines from the diff
	noEvents   bool             // true if no AI events existed in the window
}

// computeAttribution attempts to compute AI attribution from staged changes.
// Returns nil if computation fails or times out. Never blocks the commit.
func (s *CommitMsgHookService) computeAttribution(
	ctx context.Context,
	h *sqlstore.Handle,
	repo *git.Repo,
	repoRoot, semDir, checkpointID string,
) *commitAttrResult {
	attrCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()

	repoRow, err := h.Queries.GetRepositoryByRootPath(attrCtx, repoRoot)
	if err != nil {
		util.AppendActivityLog(semDir,
			"commit-msg warning: get repository row failed: %v", err)
		return nil
	}

	cp, err := h.Queries.GetCheckpointByID(attrCtx, checkpointID)
	if err != nil {
		util.AppendActivityLog(semDir,
			"commit-msg warning: get checkpoint failed: %v", err)
		return nil
	}

	// Delta window: previous commit-linked checkpoint -> this checkpoint.
	var afterTs int64
	var prevCPPtr *sqldb.Checkpoint
	prev, err := h.Queries.GetPreviousCommitLinkedCheckpoint(attrCtx, sqldb.GetPreviousCommitLinkedCheckpointParams{
		RepositoryID: repoRow.RepositoryID,
		CreatedAt:    cp.CreatedAt,
	})
	if err == nil {
		afterTs = prev.CreatedAt
		prevCPPtr = &prev
	}

	objectsDir := filepath.Join(semDir, "objects")
	bs, err := blobs.NewStore(objectsDir)
	if err != nil {
		util.AppendActivityLog(semDir,
			"commit-msg warning: blob store init failed: %v", err)
		return nil
	}

	diffBytes, err := repo.DiffCached(attrCtx)
	if err != nil {
		util.AppendActivityLog(semDir,
			"commit-msg warning: diff cached failed: %v", err)
		return nil
	}
	if len(diffBytes) == 0 {
		util.AppendActivityLog(semDir,
			"commit-msg warning: diff cached returned empty")
		return nil
	}

	// Count total modified lines from the diff for use in trailers
	// even when no AI events are found.
	diffTotalLines := countDiffAddedLines(diffBytes)

	input := ComputeAIPercentInput{
		RepoRoot: repoRoot,
		RepoID:   repoRow.RepositoryID,
		AfterTs:  afterTs,
		UpToTs:   cp.CreatedAt,
	}

	// Fast path: compute with carry-forward from existing events in DB.
	cfr, err := attributeWithCarryForward(attrCtx, h, bs, diffBytes, input, prevCPPtr, semDir)
	if err == nil {
		return &commitAttrResult{result: &cfr.result, totalLines: diffTotalLines, noEvents: cfr.noEvents}
	}

	// If no events found, flush active capture sessions, then retry.
	if !errors.Is(err, ErrNoEventsInWindow) {
		util.AppendActivityLog(semDir,
			"commit-msg warning: attribution compute failed: %v", err)
		return nil
	}

	flushActiveSessions(attrCtx)

	cfr, err = attributeWithCarryForward(attrCtx, h, bs, diffBytes, input, prevCPPtr, semDir)
	if err == nil {
		return &commitAttrResult{result: &cfr.result, totalLines: diffTotalLines, noEvents: cfr.noEvents}
	}

	// Still no events after flush - return a zero-attribution result
	// so trailers are still appended with "no AI events" diagnostics.
	if errors.Is(err, ErrNoEventsInWindow) {
		return &commitAttrResult{totalLines: diffTotalLines, noEvents: true}
	}

	util.AppendActivityLog(semDir,
		"commit-msg warning: attribution compute failed after flush: %v", err)
	return nil
}

// countDiffAddedLines counts added (non-blank) lines in a unified diff,
// mirroring how the attribution engine counts TotalLines.
func countDiffAddedLines(diffBytes []byte) int {
	var count int
	for _, line := range strings.Split(string(diffBytes), "\n") {
		if len(line) > 0 && line[0] == '+' && !strings.HasPrefix(line, "+++") {
			trimmed := strings.TrimSpace(line[1:])
			if trimmed != "" {
				count++
			}
		}
	}
	return count
}

// formatAttributionTrailers builds one Semantica-Attribution trailer per
// provider. Format: "Semantica-Attribution: 40% claude_code (100/250 lines)"
// or with model: "Semantica-Attribution: 40% claude_code (opus 4.6) (100/250 lines)"
//
// When r is nil (no events or no AI match), emits a single zero-percent trailer
// using totalLines from the diff.
func formatAttributionTrailers(r *AIPercentResult, totalLines int) []string {
	// No events or zero AI lines: emit a single "0% AI detected" trailer.
	if r == nil || r.AILines == 0 {
		if totalLines == 0 {
			return nil
		}
		return []string{fmt.Sprintf("Semantica-Attribution: 0%% AI detected (0/%d lines)", totalLines)}
	}

	tl := r.TotalLines
	if tl == 0 {
		tl = totalLines
	}

	// If no per-provider breakdown, emit a single aggregate trailer.
	if len(r.Providers) == 0 {
		return []string{fmt.Sprintf("Semantica-Attribution: %.0f%% (%d/%d lines)",
			r.Percent, r.AILines, tl)}
	}

	var trailers []string
	for _, p := range r.Providers {
		pct := float64(p.AILines) / float64(tl) * 100
		if p.Model != "" {
			trailers = append(trailers, fmt.Sprintf(
				"Semantica-Attribution: %.0f%% %s (%s) (%d/%d lines)",
				pct, p.Provider, p.Model, p.AILines, tl))
		} else {
			trailers = append(trailers, fmt.Sprintf(
				"Semantica-Attribution: %.0f%% %s (%d/%d lines)",
				pct, p.Provider, p.AILines, tl))
		}
	}
	return trailers
}

// formatDiagnosticsTrailer builds the Semantica-Diagnostics trailer.
// Format varies by state:
//   - No events:   "no AI events found in the checkpoint window"
//   - Events but no match: "AI session events found, but no file-modifying changes matched this commit"
//   - Normal:      "15 files, lines: 120 exact, 20 modified, 10 formatted"
func formatDiagnosticsTrailer(cr *commitAttrResult) string {
	if cr.noEvents {
		return "Semantica-Diagnostics: no AI events found in the checkpoint window"
	}
	if cr.result == nil {
		return "Semantica-Diagnostics: AI session events found, but no file-modifying changes matched this commit"
	}
	r := cr.result
	if r.AILines == 0 {
		return "Semantica-Diagnostics: AI session events found, but no file-modifying changes matched this commit"
	}
	return fmt.Sprintf("Semantica-Diagnostics: %d files, lines: %d exact, %d modified, %d formatted",
		r.FilesTouched, r.ExactLines, r.ModifiedLines, r.FormattedLines)
}

// flushActiveSessions reads and routes pending transcript data from all
// active capture sessions. This is a targeted catch-up that only reads
// active sessions (typically 1), not all sources on disk.
// Uses the global blob store (same as hook capture) so that
// WriteEventsToRepo can copy blobs into per-repo stores.
func flushActiveSessions(ctx context.Context) {
	states, err := hooks.LoadActiveCaptureStates()
	if err != nil || len(states) == 0 {
		return
	}

	registryPath, err := broker.DefaultRegistryPath()
	if err != nil {
		return
	}
	bh, err := broker.Open(ctx, registryPath)
	if err != nil {
		return
	}
	defer func() { _ = broker.Close(bh) }()

	var blobStore *blobs.Store
	if objDir, err := broker.GlobalObjectsDir(); err == nil {
		if bs, err := blobs.NewStore(objDir); err != nil {
			fmt.Fprintf(os.Stderr, "semantica: commit-msg: global blob store: %v (attribution will degrade)\n", err)
		} else {
			blobStore = bs
		}
	}

	for _, state := range states {
		provider := hooks.GetProvider(state.Provider)
		if provider == nil {
			continue
		}
		event := &hooks.Event{
			SessionID:     state.SessionID,
			TranscriptRef: state.TranscriptRef,
			Timestamp:     time.Now().UnixMilli(),
		}
		if err := hooks.CaptureAndRoute(ctx, provider, event, bh, blobStore); err != nil {
			fmt.Fprintf(os.Stderr, "semantica: commit-msg: capture replay failed for %s: %v\n", state.Provider, err)
		}
	}
}

// scanForTrailers scans the commit message line-by-line for existing
// Semantica trailers, tolerating leading/trailing whitespace and \r\n.
func scanForTrailers(text string) (hasCheckpoint, hasAttribution, hasDiagnostics bool) {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "Semantica-Checkpoint:"):
			hasCheckpoint = true
		case strings.HasPrefix(trimmed, "Semantica-Attribution:"):
			hasAttribution = true
		case strings.HasPrefix(trimmed, "Semantica-Diagnostics:"):
			hasDiagnostics = true
		}
	}
	return
}

// writeAttributionSummary writes a one-line summary payload for post-commit to
// display. Best-effort - it never blocks the commit.
func writeAttributionSummary(semDir string, cr *commitAttrResult) {
	path := util.CommitAttributionSummaryPath(semDir)

	summary, ok := attributionSummaryFromResult(cr)
	if !ok {
		// No AI attribution - remove any stale summary.
		_ = os.Remove(path)
		return
	}

	if err := os.WriteFile(path, []byte(summary.serialize()), 0o644); err != nil {
		slog.Debug("commit-msg: write attribution summary failed", "path", path, "err", err)
	}
}
