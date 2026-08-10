package hooks

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/toolsnap"
	"github.com/semanticash/cli/internal/util"
)

// toolWindowDeadline bounds one pre-tool capture.
const toolWindowDeadline = 2 * time.Second

// toolWindowTarget identifies the repository that owns a tool window.
type toolWindowTarget struct {
	repoPath     string
	semDir       string
	repositoryID string
}

// resolveToolWindowTarget returns the deepest enabled repository containing cwd.
func resolveToolWindowTarget(ctx context.Context, bh *broker.Handle, cwd string) (*toolWindowTarget, error) {
	if cwd == "" {
		return nil, nil
	}
	repos, err := broker.ListActiveRepos(ctx, bh)
	if err != nil {
		return nil, err
	}
	var best *broker.RegisteredRepo
	bestLen := 0
	for i := range repos {
		if broker.PathBelongsToRepo(cwd, repos[i].CanonicalPath) && len(repos[i].CanonicalPath) > bestLen {
			best = &repos[i]
			bestLen = len(repos[i].CanonicalPath)
		}
	}
	if best == nil {
		return nil, nil
	}
	repoID, err := broker.RepositoryIDForPath(ctx, best.Path)
	if err != nil {
		return nil, err
	}
	return &toolWindowTarget{
		repoPath:     best.Path,
		semDir:       filepath.Join(best.Path, ".semantica"),
		repositoryID: repoID,
	}, nil
}

// handleToolStepStarted captures a pre-tool snapshot without failing the provider command.
func handleToolStepStarted(ctx context.Context, providerName string, event *Event, bh *broker.Handle) error {
	// Include repository and database lookup in the capture deadline.
	wctx, cancel := context.WithTimeout(ctx, toolWindowDeadline)
	defer cancel()

	state, err := LoadCaptureState(event.SessionID)
	if errors.Is(err, ErrNoCaptureState) {
		return nil
	}
	if err != nil {
		slog.Warn("tool window: load capture state", "err", err)
		return nil
	}
	if event.TurnID == "" {
		event.TurnID = state.TurnID
	}
	if event.ToolUseID == "" || event.TurnID == "" {
		return nil
	}

	cwd := event.CWD
	if cwd == "" {
		cwd = state.CWD
	}
	target, err := resolveToolWindowTarget(wctx, bh, cwd)
	if err != nil || target == nil {
		if err != nil {
			slog.Warn("tool window: resolve target", "err", err)
		}
		return nil
	}

	rc, err := toolsnap.ResolveRepoContext(wctx, target.repoPath)
	if err != nil {
		slog.Warn("tool window: resolve repository", "err", err)
		return nil
	}
	store, err := toolsnap.OpenStore(wctx, rc, target.semDir)
	if err != nil {
		slog.Warn("tool window: open snapshot store", "err", err)
		return nil
	}
	reg, err := toolsnap.OpenRegistry(target.semDir)
	if err != nil {
		slog.Warn("tool window: open registry", "err", err)
		return nil
	}

	// Registry keys use the same underscore-normalized provider names as events.
	key := toolsnap.ToolKey{
		RepositoryID: target.repositoryID,
		Provider:     strings.ReplaceAll(providerName, "-", "_"),
		SessionID:    event.SessionID,
		TurnID:       event.TurnID,
		ToolUseID:    event.ToolUseID,
	}
	if _, err := reg.CaptureAndBegin(wctx, store, key, event.ToolName, event.Timestamp); err != nil {
		var pe *toolsnap.PartialError
		if errors.As(err, &pe) && pe.Reason == toolsnap.ReasonLockTimeout {
			// The registry is unavailable, so use the independent activity log.
			util.AppendActivityLog(target.semDir,
				"tool-window pre capture skipped (%s): tool_use=%s", pe.Reason, event.ToolUseID)
			return nil
		}
		slog.Warn("tool window: pre capture", "tool_use", event.ToolUseID, "err", err)
		return nil
	}
	return nil
}
