package hooks

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/turncapture"
)

func turnRecorder() (turncapture.Recorder, error) {
	base, err := broker.GlobalBase()
	return turncapture.Recorder{Root: filepath.Join(base, "turn-observations")}, err
}

func turnCaptureSession(provider string, event *Event) string {
	if provider == "kiro-cli" {
		// A workspace key cannot distinguish Kiro conversations.
		return event.ProviderSessionID
	}
	return event.SessionID
}

// beginTurnCapture captures repository baselines for supported providers.
func beginTurnCapture(ctx context.Context, provider string, event *Event, bh *broker.Handle, offset int) {
	if provider != "codex" && provider != "claude-code" && provider != "gemini-cli" && provider != "copilot" && provider != "cursor" && provider != "kiro-cli" {
		return
	}
	// Cursor baselines require a generation ID from the prompt boundary.
	if provider == "cursor" && event.ProviderTurnID == "" {
		return
	}
	if turnCaptureSession(provider, event) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := turnRecorder()
	if err == nil {
		err = startTurnCapture(ctx, r, provider, event, bh, offset)
	}
	if err != nil {
		slog.Warn("turn observation start failed", "err", err)
	}
}

func startTurnCapture(ctx context.Context, r turncapture.Recorder, provider string, event *Event, bh *broker.Handle, offset int) error {
	if bh == nil {
		return fmt.Errorf("turn repository registry unavailable")
	}
	repos, err := broker.ListActiveRepos(ctx, bh)
	if err != nil {
		return err
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].CanonicalPath < repos[j].CanonicalPath })
	subjects := make([]turncapture.Subject, len(repos))
	var wg sync.WaitGroup
	workers := min(len(repos), 8)
	for n := 0; n < workers; n++ {
		wg.Go(func() {
			for i := n; i < len(repos); i += workers {
				repo := repos[i]
				path := repo.CanonicalPath
				if path == "" {
					path = repo.Path
				}
				s := turncapture.Subject{RegistrationID: repo.RepoID, Path: path}
				id, resolveErr := broker.RepositoryIDForPath(ctx, path)
				s.RepositoryID = id
				if resolveErr != nil {
					s.Gap = "repository_identity_unavailable"
				}
				subjects[i] = s
			}
		})
	}
	wg.Wait()
	key := turnObservationKey(event, offset)
	return r.Begin(ctx, provider, turnCaptureSession(provider, event), event.TurnID, event.ProviderTurnID, key, subjects)
}

func turnObservationKey(event *Event, offset int) string {
	key := "provider:" + event.ProviderTurnID
	if event.ProviderTurnID == "" {
		// Without a provider turn ID, use the transcript position and prompt as identity.
		sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%s", event.TranscriptRef, offset, event.Prompt))
		key = fmt.Sprintf("source:%x", sum)
	}
	return key
}

func observeTurnCapture(ctx context.Context, provider string, event *Event) {
	if provider != "codex" && provider != "claude-code" && provider != "gemini-cli" && provider != "copilot" && provider != "cursor" && provider != "kiro-cli" {
		return
	}
	if turnCaptureSession(provider, event) == "" {
		return
	}
	evidence, stop := turnEvidence(provider, event)
	if len(evidence) == 0 && !stop {
		return
	}
	r, err := turnRecorder()
	if err != nil {
		return
	}
	if _, err := os.Stat(r.Root); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := r.Observe(ctx, provider, turnCaptureSession(provider, event), event.ProviderTurnID, evidence, stop); err != nil {
		slog.Warn("turn observation evidence failed", "err", err)
	}
}

func turnEvidence(provider string, event *Event) ([]turncapture.Evidence, bool) {
	base := turncapture.Evidence{ExecutionID: event.ToolUseID, ProviderAt: event.Timestamp, ReceivedAt: time.Now().UTC()}
	switch event.Type {
	case SubagentPromptSubmitted:
		base.Kind = "execution_started"
		return []turncapture.Evidence{base}, false
	case SubagentCompleted:
		base.Kind = "execution_terminal"
		return []turncapture.Evidence{base}, false
	case ToolStepStarted:
		base.Kind = "execution_started"
		return []turncapture.Evidence{base}, false
	case ToolStepCompleted:
		if event.ToolName != "Bash" {
			return nil, false
		}
		base.Kind = "execution_terminal"
		result := []turncapture.Evidence{base}
		if provider == "claude-code" {
			var response *struct {
				BackgroundTaskID string `json:"backgroundTaskId"`
			}
			if err := json.Unmarshal(event.ToolResponse, &response); err != nil || response == nil {
				base.Kind, base.Status = "gap", "task_metadata_unavailable"
				result = append(result, base)
			} else if response.BackgroundTaskID != "" {
				base.Kind, base.TaskID = "managed_task", response.BackgroundTaskID
				result = append(result, base)
			}
		}
		return result, false
	case AgentCompleted:
		base.Kind = "stop"
		result := []turncapture.Evidence{base}
		if provider == "claude-code" {
			var tasks []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			}
			if len(event.BackgroundTasks) == 0 || string(event.BackgroundTasks) == "null" || json.Unmarshal(event.BackgroundTasks, &tasks) != nil {
				base.Kind, base.Status = "gap", "background_inventory_unavailable"
				result = append(result, base)
			} else {
				for _, task := range tasks {
					base.TaskID, base.Status = task.ID, task.Status
					base.Kind = "gap"
					if task.Status == "running" {
						base.Kind = "inventory_running"
					}
					result = append(result, base)
				}
			}
		}
		return result, true
	case SessionClosed:
		base.Kind = "session_end"
		return []turncapture.Evidence{base}, false
	}
	return nil, false
}
