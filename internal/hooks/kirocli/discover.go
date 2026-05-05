package kirocli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/hooks"
)

// kiroSessionsCLISubdir holds Kiro CLI session files. Parent sessions and
// AgentCrew stage children share this directory.
const kiroSessionsCLISubdir = ".kiro/sessions/cli"

// kiroSessionHeader is the minimal projection of a Kiro CLI session's
// .json companion that the discoverer needs. The narrow shape keeps
// unrelated schema changes from breaking parsing.
type kiroSessionHeader struct {
	SessionID    string             `json:"session_id"`
	CWD          string             `json:"cwd"`
	SessionState kiroSessionStateLn `json:"session_state"`
}

type kiroSessionStateLn struct {
	AgentName string `json:"agent_name"`
}

// resolveSessionsDir returns the Kiro CLI sessions directory, honoring a
// test-injected override when set.
func (p *Provider) resolveSessionsDir() (string, error) {
	if p.sessionsDir != "" {
		return p.sessionsDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, kiroSessionsCLISubdir), nil
}

// DiscoverSubagentTranscripts returns AgentCrew child .jsonl paths for dctx.
//
// Filtering rules:
//
//   - cwd must match dctx.Cwd
//   - .jsonl mtime must fall in [dctx.PromptTime, dctx.StopTime]
//   - session_state.agent_name must be empty for child candidates
//
// Discovery requires exactly one parent-shaped session (cwd match, mtime in
// window, non-empty agent_name). With no parent anchor or multiple overlapping
// parents, discovery returns nil to avoid misattribution.
func (p *Provider) DiscoverSubagentTranscripts(_ context.Context, _ string, dctx hooks.DiscoveryContext) ([]string, error) {
	if dctx.Cwd == "" || dctx.PromptTime <= 0 {
		return nil, nil
	}

	dir, err := p.resolveSessionsDir()
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read kiro sessions dir: %w", err)
	}

	stopMS := dctx.StopTime
	if stopMS <= 0 {
		stopMS = math.MaxInt64
	}

	var matches []string
	parentMatchCount := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		jsonPath := filepath.Join(dir, entry.Name())
		jsonlPath := strings.TrimSuffix(jsonPath, ".json") + ".jsonl"

		info, err := os.Stat(jsonlPath)
		if err != nil {
			continue
		}
		mtimeMS := info.ModTime().UnixMilli()
		if mtimeMS < dctx.PromptTime || mtimeMS > stopMS {
			continue
		}

		header, err := readKiroSessionHeader(jsonPath)
		if err != nil {
			continue
		}
		if header.CWD != dctx.Cwd {
			continue
		}
		if header.SessionState.AgentName != "" {
			parentMatchCount++
			continue
		}

		matches = append(matches, jsonlPath)
	}

	switch {
	case parentMatchCount > 1:
		slog.Warn(
			"kiro discovery: concurrent parent sessions in same cwd/window, skipping subagent attribution",
			"cwd", dctx.Cwd,
			"parent_count", parentMatchCount,
			"candidate_children", len(matches),
		)
		return nil, nil
	case parentMatchCount == 0:
		// Stay silent when there is also no candidate child; that is
		// the common no-subagent-activity path.
		if len(matches) > 0 {
			slog.Warn(
				"kiro discovery: no parent-shaped session matched in cwd/window, skipping subagent attribution",
				"cwd", dctx.Cwd,
				"candidate_children", len(matches),
			)
		}
		return nil, nil
	}

	return matches, nil
}

// SubagentStateKey returns a stable capture-state key for a child
// transcript, derived from its Kiro session UUID.
func (p *Provider) SubagentStateKey(subagentTranscriptRef string) string {
	base := filepath.Base(subagentTranscriptRef)
	return "kirocli-subagent-" + strings.TrimSuffix(base, ".jsonl")
}

func readKiroSessionHeader(path string) (kiroSessionHeader, error) {
	var hdr kiroSessionHeader
	data, err := os.ReadFile(path)
	if err != nil {
		return hdr, fmt.Errorf("read kiro session header: %w", err)
	}
	if err := json.Unmarshal(data, &hdr); err != nil {
		return hdr, fmt.Errorf("parse kiro session header: %w", err)
	}
	return hdr, nil
}
