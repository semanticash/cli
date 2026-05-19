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
)

type CommitMsgHookService struct {
	RepoPath string
	// Registry is the hook-provider registry used by the
	// flushActiveSessions sweep that runs before commit-msg
	// attribution. Production callers must pass
	// providers.NewHookRegistry(); the commit-msg cobra command
	// does so. A nil Registry makes flushActiveSessions a no-op
	// (every per-session lookup returns nil and is skipped),
	// which is useful only for tests that intentionally exercise
	// the non-flush paths.
	Registry *hooks.Registry
}

func NewCommitMsgHookService(repoPath string, registry *hooks.Registry) *CommitMsgHookService {
	return &CommitMsgHookService{RepoPath: repoPath, Registry: registry}
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

	flushActiveSessions(attrCtx, s.Registry)

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
// provider. Format depends on the evidence shape:
//
//   - Single-provider headline:   "40% (100/250 lines)"
//   - Per-provider involvement:   "claude_code 40% involvement (100/250 lines)"
//   - With model:                 "claude_code 40% involvement (100/250 lines, opus 4.6)"
//   - Provider-touch only:        "cursor provider-touched 5 lines"
//
// The per-provider percentage is an involvement metric, not exclusive
// ownership: a line emitted by two providers credits both, so the
// individual per-provider percentages can sum above the headline
// AI%. The "involvement" qualifier in the rendered line makes the
// semantics explicit so readers don't read shared-line credit as
// double-counting.
//
// When r is nil and there are no diff lines either, no trailer is
// emitted. When the commit only has provider-touch evidence, the
// trailer reflects that; emitting a "0% AI detected" line when
// Semantica has provider-touch data would misrepresent the state.
func formatAttributionTrailers(r *AIPercentResult, totalLines int) []string {
	if r == nil || (r.AILines == 0 && r.ProviderOnlyLines == 0) {
		if totalLines == 0 {
			return nil
		}
		return []string{fmt.Sprintf("Semantica-Attribution: 0%% AI detected (0/%d lines)", totalLines)}
	}

	tl := r.TotalLines
	if tl == 0 {
		tl = totalLines
	}

	// No per-provider breakdown: emit a single aggregate trailer
	// covering whichever evidence shape exists.
	if len(r.Providers) == 0 {
		if r.AILines > 0 {
			line := fmt.Sprintf("Semantica-Attribution: %.0f%% (%d/%d lines)",
				r.Percent, r.AILines, tl)
			if r.ProviderOnlyLines > 0 {
				line += fmt.Sprintf(", +%d provider-touched", r.ProviderOnlyLines)
			}
			return []string{line}
		}
		return []string{fmt.Sprintf("Semantica-Attribution: provider-touched %d lines (no line-level evidence)",
			r.ProviderOnlyLines)}
	}

	var trailers []string
	for _, p := range r.Providers {
		// Build the per-provider line-level segment, then append
		// the provider-only sidecar to the same trailer when both
		// exist. One provider = one trailer, regardless of which
		// evidence shapes it carries.
		var line string
		if p.AILines > 0 {
			pct := float64(p.AILines) / float64(tl) * 100
			// Shared lines credit every provider that emitted them,
			// so per-provider percentages can sum above the headline
			// AI%. The qualifier makes that involvement model clear.
			if p.Model != "" {
				line = fmt.Sprintf("Semantica-Attribution: %s %.0f%% involvement (%d/%d lines, %s)",
					p.Provider, pct, p.AILines, tl, p.Model)
			} else {
				line = fmt.Sprintf("Semantica-Attribution: %s %.0f%% involvement (%d/%d lines)",
					p.Provider, pct, p.AILines, tl)
			}
			if p.ProviderOnlyLines > 0 {
				// Same-provider mixed evidence keeps the
				// provider-touch count on this provider's trailer.
				line += fmt.Sprintf(", +%d provider-touched", p.ProviderOnlyLines)
			}
		} else if p.ProviderOnlyLines > 0 {
			line = fmt.Sprintf("Semantica-Attribution: %s provider-touched %d lines",
				p.Provider, p.ProviderOnlyLines)
		} else {
			continue
		}
		trailers = append(trailers, line)
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
	if r.AILines == 0 && r.ProviderOnlyLines == 0 {
		return "Semantica-Diagnostics: AI session events found, but no file-modifying changes matched this commit"
	}
	// Surface provider-only lines in the diagnostics so the user
	// can see them when the line-level counts are zero. Without
	// this, a Cursor-only commit shows "0 exact, 0 modified, 0
	// formatted" with no hint that provider-touch evidence was
	// recorded.
	if r.AILines == 0 {
		return fmt.Sprintf("Semantica-Diagnostics: %d files, %d provider-touched lines (no line-level evidence)",
			r.FilesTouched, r.ProviderOnlyLines)
	}
	if r.ProviderOnlyLines > 0 {
		return fmt.Sprintf("Semantica-Diagnostics: %d files, lines: %d exact, %d modified, %d formatted, %d provider-touched",
			r.FilesTouched, r.ExactLines, r.ModifiedLines, r.FormattedLines, r.ProviderOnlyLines)
	}
	return fmt.Sprintf("Semantica-Diagnostics: %d files, lines: %d exact, %d modified, %d formatted",
		r.FilesTouched, r.ExactLines, r.ModifiedLines, r.FormattedLines)
}

// flushActiveSessions reads and routes pending transcript data from all
// active capture sessions. This is a targeted catch-up that only reads
// active sessions (typically 1), not all sources on disk.
// Uses the global blob store (same as hook capture) so that
// WriteEventsToRepo can copy blobs into per-repo stores.
func flushActiveSessions(ctx context.Context, registry *hooks.Registry) {
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
		provider := registry.Get(state.Provider)
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
