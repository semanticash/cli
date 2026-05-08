// Package handoff assembles a redacted, provenance-rich markdown
// bundle from an active Semantica capture session so a fresh agent
// session can pick up where the previous one left off without
// re-reading the original transcript.
//
// The service is the source of truth for handoff content. Both the
// terminal-facing `semantica handoff --write` command and the hidden
// `semantica skills handoff` backing command call into this package
// so the bundle shape is identical across surfaces.
package handoff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/redact"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// HandoffFilename is the relative path inside the repo's `.semantica/`
// directory that the writer targets.
const HandoffFilename = "handoff.md"

// recentSessionWindow bounds how stale a capture state may be before
// the resolver refuses to use it. A session whose last activity is
// older than this is unlikely to be the one the user wants to hand
// off.
const recentSessionWindow = 24 * time.Hour

// supportedProvider gates the v1 resolver. The initial release scopes
// the writer to Claude Code; other providers can be added once their
// capture states cover the same fields the resolver relies on.
const supportedProvider = "claude-code"

// maxPromptChars is the redaction-friendly cap on prompt and
// assistant-message text included in the bundle.
const maxPromptChars = 500

// maxFilesInTouchSummary caps how many file-touch entries appear in
// the bundle so a noisy session doesn't blow the output budget.
const maxFilesInTouchSummary = 50

// ErrNoSession is returned when no usable Semantica capture session
// resolves for the current repo. Callers translate this into a
// non-zero exit with a clear user message.
var ErrNoSession = errors.New("no claude-code session found for this repo")

// ErrAmbiguousSession is returned when more than one parent capture
// state matches the resolver's filters. The user must close one of
// the sessions before retrying; the resolver never silently picks.
var ErrAmbiguousSession = errors.New("multiple claude-code sessions active for this repo")

// Input narrows the surface the caller has to provide. RepoPath is
// the only required field; the service derives everything else from
// the repo's lineage.db and the global capture-state directory.
type Input struct {
	// RepoPath is the working repository whose session is being
	// handed off. Defaults to the current working directory at the
	// command layer.
	RepoPath string

	// Now is the wall-clock used for "is this session recent enough"
	// checks. Tests inject a fixed time. Empty value means time.Now().
	Now time.Time
}

// Result describes what was written so the caller can render its
// two-line user instruction.
type Result struct {
	// Path is the absolute filesystem path of the written bundle.
	Path string

	// SessionID is the resolved session whose context the bundle
	// captures. Surfaced for diagnostics and tests.
	SessionID string

	// Provider is always "claude-code" in v1; reserved for when more
	// providers are supported.
	Provider string

	// Bytes is the raw markdown body that was written. Returned for
	// tests; do not echo this back into the originating session.
	Bytes []byte
}

// Service assembles handoff bundles. Construct via NewService.
type Service struct{}

// NewService returns a stateless service. All dependencies (lineage
// store, redactor, capture-state directory) are reached through
// existing internal packages.
func NewService() *Service { return &Service{} }

// Write resolves the active session for the repo, assembles a
// redacted markdown bundle, and writes it to
// `<repo>/.semantica/handoff.md`. Returns the resolved session and
// the bytes written.
func (s *Service) Write(ctx context.Context, in Input) (*Result, error) {
	// Normalize the repo root the same way enable/explain/status do.
	// Running from a subdirectory must resolve scope to the repo root,
	// or we'd miss the active session and write into a subdir's
	// .semantica/.
	repo, err := git.OpenRepo(in.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("open repo: %w", err)
	}
	repoRoot := repo.Root()

	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}

	state, err := resolveSession(repoRoot, now)
	if err != nil {
		return nil, err
	}

	body, err := assembleBundle(ctx, repoRoot, state, now)
	if err != nil {
		return nil, fmt.Errorf("assemble handoff: %w", err)
	}

	semDir := filepath.Join(repoRoot, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		return nil, fmt.Errorf("create .semantica dir: %w", err)
	}
	path := filepath.Join(semDir, HandoffFilename)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return nil, fmt.Errorf("write handoff: %w", err)
	}

	return &Result{
		Path:      path,
		SessionID: state.SessionID,
		Provider:  state.Provider,
		Bytes:     body,
	}, nil
}

// resolveSession picks the single Claude Code capture state for the
// current repo. Filters: provider, CWD under repo root, parent
// session (StateKey unset or equal to SessionID), recent timestamp.
// Errors when zero or more than one state matches; never silently
// picks among multiple actives.
func resolveSession(repoPath string, now time.Time) (*hooks.CaptureState, error) {
	all, err := hooks.LoadActiveCaptureStates()
	if err != nil {
		return nil, fmt.Errorf("load capture states: %w", err)
	}

	canonicalRepo, err := filepath.EvalSymlinks(repoPath)
	if err != nil {
		canonicalRepo = filepath.Clean(repoPath)
	}

	since := now.Add(-recentSessionWindow).UnixMilli()

	var matches []*hooks.CaptureState
	for _, st := range all {
		if st.Provider != supportedProvider {
			continue
		}
		if st.Timestamp <= 0 || st.Timestamp < since {
			continue
		}
		if st.StateKey != "" && st.StateKey != st.SessionID {
			// Subagent state files override the key. Skip them so we
			// only consider parent sessions.
			continue
		}
		if st.CWD == "" {
			continue
		}
		stateCanonical, err := filepath.EvalSymlinks(st.CWD)
		if err != nil {
			stateCanonical = filepath.Clean(st.CWD)
		}
		if !sameRepo(canonicalRepo, stateCanonical) {
			continue
		}
		matches = append(matches, st)
	}

	switch len(matches) {
	case 0:
		return nil, ErrNoSession
	case 1:
		return matches[0], nil
	default:
		return nil, ErrAmbiguousSession
	}
}

// sameRepo reports whether candidate is repoRoot or a descendant of
// it. Mirrors the helper used in the health package; duplicated to
// avoid coupling unrelated packages.
func sameRepo(repoRoot, candidate string) bool {
	if repoRoot == candidate {
		return true
	}
	rel, err := filepath.Rel(repoRoot, candidate)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..") && rel != ""
}

// Generic notes used when assembly degrades. Detailed errors stay
// in the caller's logs (and tests); the bundle text never carries
// raw `err.Error()` content because handoff bundles are paste-ready
// markdown that travels into a fresh agent session, and absolute
// local paths or SQLite internals would leak there.
const (
	noteLineageMissing = "lineage data not available for this repo; bundle contains only session-resolution metadata"
	noteLineageUnavail = "lineage data could not be loaded; bundle contains only session-resolution metadata"
	noteSessionUnknown = "session not yet registered in lineage.db; bundle contains only session-resolution metadata"
	noteEventsUnavail  = "session events could not be loaded"
)

// assembleBundle queries lineage.db for the resolved session's
// content, redacts every prose field, and renders the markdown
// template. Degraded paths surface a stable generic note (never the
// raw error string) so a partial bundle is still safe to share.
func assembleBundle(ctx context.Context, repoPath string, state *hooks.CaptureState, now time.Time) ([]byte, error) {
	semDir := filepath.Join(repoPath, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")

	branch := readGitBranch(ctx, repoPath)

	view := bundleView{
		Repo:        filepath.Base(repoPath),
		Branch:      branch,
		Provider:    state.Provider,
		SessionID:   state.SessionID,
		GeneratedAt: now.UTC().Format(time.RFC3339),
	}

	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		view.Note = noteLineageMissing
		return renderBundle(view), nil
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.OpenOptions{
		BusyTimeout: 100 * time.Millisecond,
		Synchronous: "NORMAL",
	})
	if err != nil {
		view.Note = noteLineageUnavail
		return renderBundle(view), nil
	}
	defer func() { _ = sqlstore.Close(h) }()

	// Capture state stores the provider's session ID, but
	// agent_events.session_id is Semantica's local UUID. Resolve the
	// local ID by chaining repo lookup + provider-session lookup
	// before listing events. Without this step the event list is
	// always empty.
	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, repoPath)
	if err != nil {
		view.Note = noteSessionUnknown
		return renderBundle(view), nil
	}
	sessionRow, err := h.Queries.GetAgentSessionByProviderID(ctx, sqldb.GetAgentSessionByProviderIDParams{
		RepositoryID:      repoRow.RepositoryID,
		Provider:          storageProviderName(state.Provider),
		ProviderSessionID: state.SessionID,
	})
	if err != nil {
		view.Note = noteSessionUnknown
		return renderBundle(view), nil
	}

	events, err := h.Queries.ListAgentEventsBySession(ctx, sqldb.ListAgentEventsBySessionParams{
		SessionID: sessionRow.SessionID,
		Limit:     500,
	})
	if err != nil {
		view.Note = noteEventsUnavail
		return renderBundle(view), nil
	}

	view.LastPrompt = redactString(extractLastPrompt(events))
	view.LastAssistant = redactString(extractLastAssistant(events))
	view.FileTouches = aggregateFileTouches(events)

	return renderBundle(view), nil
}

// storageProviderName translates a hook-registry provider name
// (with dashes, e.g. "claude-code") into the form
// agent_sessions.provider stores (with underscores or "_cli"
// suffixes). v1 only resolves Claude Code; the table-driven shape
// is here so adding more providers is mechanical.
func storageProviderName(hookName string) string {
	switch hookName {
	case "claude-code":
		return "claude_code"
	case "gemini":
		return "gemini_cli"
	default:
		return strings.ReplaceAll(hookName, "-", "_")
	}
}

// readGitBranch returns the current branch name for the repo, or
// "(detached)" if HEAD isn't on a branch. Errors are swallowed; the
// bundle proceeds without branch metadata rather than blocking on a
// git read.
func readGitBranch(ctx context.Context, repoPath string) string {
	r, err := git.OpenRepo(repoPath)
	if err != nil {
		return ""
	}
	branch, err := r.CurrentBranch(ctx)
	if err != nil || branch == "" {
		return "(detached)"
	}
	return branch
}

// extractLastPrompt returns the most recent user-role event's summary
// text, truncated to maxPromptChars. The events slice is in
// descending ts order from ListAgentEventsBySession.
func extractLastPrompt(events []sqldb.AgentEvent) string {
	for _, e := range events {
		if e.Role.Valid && e.Role.String == "user" && e.Summary.Valid {
			return truncateRunes(e.Summary.String, maxPromptChars)
		}
	}
	return ""
}

// extractLastAssistant returns the most recent assistant-role event's
// summary text, similarly truncated.
func extractLastAssistant(events []sqldb.AgentEvent) string {
	for _, e := range events {
		if e.Role.Valid && e.Role.String == "assistant" && e.Summary.Valid {
			return truncateRunes(e.Summary.String, maxPromptChars)
		}
	}
	return ""
}

// aggregateFileTouches walks the session's events and tallies
// per-path tool occurrences (Write x3, Edit, Bash, etc.). The output
// is sorted by frequency (desc) then path so the bundle is
// deterministic.
func aggregateFileTouches(events []sqldb.AgentEvent) []fileTouch {
	type key struct{ path, tool string }
	counts := map[key]int{}
	for _, e := range events {
		if !e.ToolUses.Valid || e.ToolUses.String == "" {
			continue
		}
		var tu struct {
			Tools []struct {
				Name     string `json:"name"`
				FilePath string `json:"file_path"`
			} `json:"tools"`
		}
		if err := json.Unmarshal([]byte(e.ToolUses.String), &tu); err != nil {
			continue
		}
		for _, t := range tu.Tools {
			if t.FilePath == "" || t.Name == "" {
				continue
			}
			counts[key{path: t.FilePath, tool: t.Name}]++
		}
	}

	pathTotals := map[string]map[string]int{}
	for k, n := range counts {
		if pathTotals[k.path] == nil {
			pathTotals[k.path] = map[string]int{}
		}
		pathTotals[k.path][k.tool] += n
	}

	out := make([]fileTouch, 0, len(pathTotals))
	for path, tools := range pathTotals {
		var total int
		var parts []string
		toolNames := make([]string, 0, len(tools))
		for tn := range tools {
			toolNames = append(toolNames, tn)
		}
		sort.Strings(toolNames)
		for _, tn := range toolNames {
			n := tools[tn]
			total += n
			if n > 1 {
				parts = append(parts, fmt.Sprintf("%s x%d", tn, n))
			} else {
				parts = append(parts, tn)
			}
		}
		out = append(out, fileTouch{
			Path:    path,
			Summary: strings.Join(parts, ", "),
			Total:   total,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].Path < out[j].Path
	})
	if len(out) > maxFilesInTouchSummary {
		out = out[:maxFilesInTouchSummary]
	}
	return out
}

// redactString runs prose through the existing redactor and returns
// the redacted result. On redactor failure the result is the empty
// string (fail-closed for outbound content).
func redactString(s string) string {
	if s == "" {
		return s
	}
	r, err := redact.String(s)
	if err != nil {
		return ""
	}
	return r
}

// truncateRunes truncates a string to at most n runes, appending a
// trailing ellipsis when truncation actually fired.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

// fileTouch is one row in the "Files touched this session" section.
type fileTouch struct {
	Path    string
	Summary string // e.g. "Edit x3, Write"
	Total   int
}

// bundleView holds everything renderBundle needs. Filling fields is
// best-effort; renderBundle handles missing values gracefully.
type bundleView struct {
	Repo          string
	Branch        string
	Provider      string
	SessionID     string
	GeneratedAt   string
	LastPrompt    string
	LastAssistant string
	FileTouches   []fileTouch
	Note          string // optional caveat shown when assembly degraded
}

func renderBundle(v bundleView) []byte {
	var b strings.Builder

	header := fmt.Sprintf("# Session continuation: %s", v.Repo)
	if v.Branch != "" {
		header += " on " + v.Branch
	}
	b.WriteString(header)
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "Generated by Semantica at %s. Original session: %s (%s).\n",
		v.GeneratedAt, v.SessionID, v.Provider)
	if v.Note != "" {
		fmt.Fprintf(&b, "\n_Note: %s._\n", v.Note)
	}
	b.WriteString("\n")

	b.WriteString("## Files touched this session\n\n")
	if len(v.FileTouches) == 0 {
		b.WriteString("_No file-touching tool events recorded for this session yet._\n")
	} else {
		for _, ft := range v.FileTouches {
			fmt.Fprintf(&b, "- %s (%s)\n", ft.Path, ft.Summary)
		}
	}
	b.WriteString("\n")

	b.WriteString("## Where I left off\n\n")
	if v.LastPrompt != "" {
		b.WriteString("**Last user prompt (truncated, redacted):**\n\n")
		b.WriteString(v.LastPrompt)
		b.WriteString("\n\n")
	}
	if v.LastAssistant != "" {
		b.WriteString("**Last assistant message (truncated, redacted):**\n\n")
		b.WriteString(v.LastAssistant)
		b.WriteString("\n\n")
	}
	if v.LastPrompt == "" && v.LastAssistant == "" {
		b.WriteString("_No prompt or assistant message available for this session yet._\n\n")
	}

	return []byte(b.String())
}
