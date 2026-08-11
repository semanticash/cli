package health

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/semanticash/cli/internal/hooks"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	"github.com/semanticash/cli/internal/toolsnap"
	"github.com/semanticash/cli/internal/util"
)

// checkToolWindows reports tool-window health without modifying state.
func checkToolWindows(ctx context.Context, opts Options) []Check {
	if opts.RepoPath == "" {
		return nil
	}
	semDir := filepath.Join(opts.RepoPath, ".semantica")
	if !util.IsEnabled(semDir) {
		return nil
	}
	if _, err := os.Stat(filepath.Join(semDir, "tool-windows")); errors.Is(err, os.ErrNotExist) {
		return append([]Check{{
			Category: "toolwindow",
			ID:       "registry",
			Status:   StatusOK,
			Message:  "tool windows: none recorded yet",
		}}, checkToolWindowDrift(ctx, opts)...)
	}

	status, err := hooks.InspectToolWindows(ctx, opts.RepoPath)
	if err != nil {
		if errors.Is(err, toolsnap.ErrRegistryCorrupt) {
			return []Check{{
				Category:    "toolwindow",
				ID:          "registry",
				Status:      StatusFail,
				Message:     "tool-window registry corrupt: " + err.Error(),
				Remediation: "inspect .semantica/tool-windows; corrupt state blocks capture and recovery",
			}}
		}
		return []Check{{
			Category: "toolwindow",
			ID:       "registry",
			Status:   StatusWarn,
			Message:  "tool-window registry unreadable: " + err.Error(),
		}}
	}

	var checks []Check
	backlog := status.PendingPartials + status.PendingFinalizations
	switch {
	case backlog > 0:
		checks = append(checks, Check{
			Category: "toolwindow",
			ID:       "recovery",
			Status:   StatusWarn,
			Message: fmt.Sprintf("tool windows: %d recoverable item(s) (%d partial records, %d stranded groups)",
				backlog, status.PendingPartials, status.PendingFinalizations),
			Remediation: "the worker sweep recovers these on its next pass; `semantica tidy --apply` recovers them now",
		})
	default:
		checks = append(checks, Check{
			Category: "toolwindow",
			ID:       "recovery",
			Status:   StatusOK,
			Message:  fmt.Sprintf("tool windows: %d active, no recovery backlog", status.ActiveWindows),
		})
	}
	if status.StaleWindows > 0 {
		checks = append(checks, Check{
			Category:    "toolwindow",
			ID:          "stale",
			Status:      StatusWarn,
			Message:     fmt.Sprintf("tool windows: %d stale window(s) older than %s", status.StaleWindows, toolsnap.DefaultStaleWindowAge),
			Remediation: "`semantica tidy --apply` removes abandoned windows",
		})
	}
	if status.MalformedTombstones > 0 {
		checks = append(checks, Check{
			Category:    "toolwindow",
			ID:          "tombstones",
			Status:      StatusFail,
			Message:     fmt.Sprintf("tool windows: %d malformed tombstone(s)", status.MalformedTombstones),
			Remediation: "inspect .semantica/tool-windows/tombstones; malformed entries cannot gate late posts",
		})
	}
	if size, err := dirSize(filepath.Join(semDir, "tool-snapshots.git", "objects")); err == nil {
		checks = append(checks, Check{
			Category: "toolwindow",
			ID:       "store",
			Status:   StatusOK,
			Message:  fmt.Sprintf("snapshot store: %s", formatBytes(size)),
		})
	}
	// Wait for pending work before evaluating hook drift.
	if status.ActiveWindows > 0 || status.PendingFinalizations > 0 || status.PendingPartials > 0 {
		return checks
	}
	return append(checks, checkToolWindowDrift(ctx, opts)...)
}

// checkToolWindowDrift reports Bash events without captured tool deltas.
func checkToolWindowDrift(ctx context.Context, opts Options) []Check {
	dbPath := filepath.Join(opts.RepoPath, ".semantica", "lineage.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil
	}
	h, err := openLineage(ctx, dbPath)
	if err != nil {
		return nil
	}
	defer func() { _ = sqlstore.Close(h) }()

	since := time.Now().Add(-24 * time.Hour).UnixMilli()
	var unmatched int
	// Drift applies only to providers with a pre-tool hook.
	if err := h.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agent_events e
		 JOIN agent_sessions s ON s.session_id = e.session_id
		 WHERE e.tool_name = 'Bash' AND e.event_source = 'hook' AND e.ts > ?
		 AND s.provider = 'claude_code'
		 AND NOT EXISTS (
		   SELECT 1 FROM agent_event_evidence_links l
		   WHERE l.event_id = e.event_id AND l.evidence_kind = 'tool_delta'
		     AND l.group_id NOT LIKE 'pre_snapshot_missing:%')`,
		since).Scan(&unmatched); err != nil {
		return nil
	}
	if unmatched == 0 {
		return nil
	}
	return []Check{{
		Category: "toolwindow",
		ID:       "drift",
		Status:   StatusWarn,
		Message: fmt.Sprintf("tool windows: %d Bash hook event(s) in 24h without captured tool-window evidence",
			unmatched),
		Remediation: "the pre-tool hook may be missing; re-run `semantica enable` to reinstall hooks",
	}}
}

// dirSize sums regular file sizes under root.
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
