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

// kiroSessionsCLISubdir is the conventional location for Kiro CLI
// session files, both parent and child. AgentCrew stage children land
// here alongside user-driven parent sessions; the discoverer is the
// component that tells them apart.
const kiroSessionsCLISubdir = ".kiro/sessions/cli"

// kiroSessionHeader is the minimal projection of a Kiro CLI session's
// .json companion that the discoverer needs. We deliberately do not
// model the full conversation state to keep the coupling narrow:
// future schema additions in unrelated fields will not break parsing.
type kiroSessionHeader struct {
	SessionID    string             `json:"session_id"`
	CWD          string             `json:"cwd"`
	SessionState kiroSessionStateLn `json:"session_state"`
}

type kiroSessionStateLn struct {
	ConversationMetadata kiroSessionConvMeta `json:"conversation_metadata"`
}

type kiroSessionConvMeta struct {
	UserTurnMetadatas []kiroUserTurnMeta `json:"user_turn_metadatas"`
}

type kiroUserTurnMeta struct {
	LoopID kiroLoopID `json:"loop_id"`
}

type kiroLoopID struct {
	AgentID kiroAgentID `json:"agent_id"`
}

type kiroAgentID struct {
	Name string `json:"name"`
}

// resolveSessionsDir returns the directory holding Kiro CLI session
// files. Tests inject a temp dir via Provider.sessionsDir; production
// resolves it under the user home.
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

// DiscoverSubagentTranscripts walks the Kiro CLI sessions directory
// and returns the .jsonl paths of session files that look like
// AgentCrew stage children for the parent described by dctx.
//
// AgentCrew dispatches stages programmatically rather than through a
// user prompt cycle. Observed session files have two relevant traits:
//
//  1. Stage children write to ~/.kiro/sessions/cli/ alongside
//     user-driven parent sessions, with no in-file pointer back to the
//     parent. There is no parent_id field anywhere on either side.
//  2. Stage children carry no populated agent_id.name in any
//     user_turn_metadatas entry, while user-driven parent sessions
//     always do (typically "kiro_default"). This is the discriminator
//     that separates child stages from concurrent unrelated parents
//     in the same repo.
//
// The cwd guard isolates same-repo activity. The mtime window limits
// matches to the parent's prompt-to-stop interval. Missing cwd or
// PromptTime returns no matches instead of scanning broadly.
//
// Discovery requires exactly one parent-shaped session (cwd match,
// mtime in window, populated agent_id.name) to anchor the children
// it is about to return. Child files have no parent pointer, so any
// other count breaks the anchor:
//
//   - Zero parents: candidate children might belong to a parent whose
//     header is not yet flushed, lives outside the time window, or is
//     in a Kiro CLI shape we do not parse. Without a positive anchor
//     we cannot claim them.
//   - More than one parent: two same-repo same-window parents are
//     overlapping, and there is no way to tell whose children are
//     whose.
//
// Both cases fail closed and log a warning when candidate children
// are dropped.
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
		// Keep the lower-bound filter even if no stop time is available.
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
		if hasPopulatedAgentName(header) {
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
		// Zero parents with no candidate children is the common
		// no-subagent-activity path; do not warn.
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

// SubagentStateKey returns a stable per-child capture-state key. The
// .jsonl filename is the Kiro session UUID and is unique across the
// sessions directory.
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

// hasPopulatedAgentName reports whether the session's
// user_turn_metadatas carries any entry with a non-empty agent name.
// User-driven parent sessions always do; AgentCrew stage children
// never do.
func hasPopulatedAgentName(hdr kiroSessionHeader) bool {
	for _, t := range hdr.SessionState.ConversationMetadata.UserTurnMetadatas {
		if t.LoopID.AgentID.Name != "" {
			return true
		}
	}
	return false
}
