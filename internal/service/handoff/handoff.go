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
	"github.com/semanticash/cli/internal/store/blobs"
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

// maxPromptChars caps each prompt and assistant-message rendered
// in the bundle. The cap keeps bundles bounded while preserving
// substantive prompts; redaction runs first, so the cap operates
// on already-sanitized text.
const maxPromptChars = 1500

// maxFilesInTouchSummary caps how many file-touch entries appear in
// the bundle so a noisy session doesn't blow the output budget.
const maxFilesInTouchSummary = 50

// ErrNoSession is returned when no usable Semantica capture session
// resolves for the current repo. Callers translate this into a
// non-zero exit with a clear user message.
var ErrNoSession = errors.New("no agent session found for this repo")

// ErrAmbiguousSession is returned when more than one distinct
// provider has active capture state in the repo. Multiple sessions
// for the same provider auto-resolve through that provider's latest
// lineage row; distinct providers require a caller choice. Callers
// can use errors.As to unwrap an AmbiguousActiveSessionError.
var ErrAmbiguousSession = errors.New("multiple agent sessions active for this repo")

// ActiveProvider describes one distinct provider with active
// capture states in the repo. Used by the ambiguity resolution
// flow to populate the picker (interactive) or the error message
// (non-interactive).
type ActiveProvider struct {
	// Provider is the hook-form provider name (claude-code,
	// gemini-cli, etc.) the command layer surfaces to the user and
	// passes back as --from.
	Provider string

	// Count is the number of active capture states for this
	// provider. Surfaced in the picker label so the user can spot
	// stale-orphan clusters at a glance.
	Count int

	// LatestTimestamp is the most-recent capture-state timestamp
	// among this provider's active states. Used to sort the picker
	// (most-recent first) and to render the "latest Xm ago" hint.
	LatestTimestamp time.Time
}

// AmbiguousActiveSessionError carries the candidate provider list
// alongside the ErrAmbiguousSession sentinel so the command layer
// can either show a picker (TTY) or print the list in a clear
// non-interactive error. Implements Is(target) so existing
// errors.Is(err, ErrAmbiguousSession) checks keep working.
type AmbiguousActiveSessionError struct {
	Providers []ActiveProvider
}

func (e *AmbiguousActiveSessionError) Error() string {
	names := make([]string, len(e.Providers))
	for i, p := range e.Providers {
		names[i] = p.Provider
	}
	return fmt.Sprintf("multiple active providers: %s", strings.Join(names, ", "))
}

// Is wires errors.Is(err, ErrAmbiguousSession) back to true so
// existing call sites that test the sentinel keep working without
// knowing about the typed wrapper.
func (e *AmbiguousActiveSessionError) Is(target error) bool {
	return target == ErrAmbiguousSession
}

// ErrNoFromMatch is returned when an explicit --from source cannot
// be resolved. The wrapped error includes the specific reason, such
// as missing lineage data or no recent session for that provider.
//
// Only used when the user typed --from. When the service auto-routes
// through the from path (same-provider collapse), failures wrap
// ErrAutoSelectFailed instead so the command layer does not advise
// the user to "drop --from" they never typed.
var ErrNoFromMatch = errors.New("could not resolve --from source")

// ErrAutoSelectFailed is the auto-collapse sibling of ErrNoFromMatch.
// When multiple capture states for one provider are active and the
// service silently routes through the from path for that provider,
// a downstream lineage miss surfaces as this error, not
// ErrNoFromMatch. Wraps the same specific reasons ("no recent X
// session", "lineage.db not found", etc.); only the surface
// shaping differs at the command layer.
var ErrAutoSelectFailed = errors.New("could not resolve auto-selected provider")

// fromFailureSentinel picks between ErrNoFromMatch and
// ErrAutoSelectFailed based on whether the from path was entered
// because the user typed --from (explicit) or because the
// same-provider-collapse path set it internally (auto-selected).
// Centralizing the choice keeps every from-path wrap site in
// sync; adding a third path in the future is one branch instead
// of four edits.
func fromFailureSentinel(autoSelected bool) error {
	if autoSelected {
		return ErrAutoSelectFailed
	}
	return ErrNoFromMatch
}

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

	// From, when non-empty, sources the bundle from the named
	// provider's most-recent session in this repo. The value is the
	// hook-form provider name (claude-code, cursor, gemini-cli,
	// copilot, kiro-cli, kiro-ide). Empty uses the default resolution
	// chain: active capture state, then lineage fallback.
	From string
}

// Result describes what was written so the caller can render its
// two-line user instruction.
type Result struct {
	// Path is the absolute filesystem path of the written bundle.
	Path string

	// SessionID is the resolved session whose context the bundle
	// captures. Surfaced for diagnostics and tests.
	SessionID string

	// Provider is the capture provider for the resolved session.
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
//
// Session resolution has three layers, in priority order:
//
//  1. Explicit --from: when Input.From names a provider, source
//     the bundle from that provider's most-recent session in the
//     repo regardless of which agent currently holds the active
//     capture state.
//  2. Active capture state, written by the agent's prompt-submit
//     hook and deleted by the stop hook at end of turn. Works
//     in-turn (e.g., from the skill body's bash invocation while
//     the agent is still mid-response).
//  3. Lineage fallback: when no active capture state matches, look
//     up the most-recent parent session for the repo in
//     agent_sessions (within the same 24h recency window) that has
//     at least one event. This is what makes `handoff --write`
//     work between turns, when capture state has been cleaned up
//     but durable lineage data still exists.
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

	body, providerSessionID, providerHookName, err := assembleBundleForRepo(ctx, repoRoot, now, strings.TrimSpace(in.From))
	if err != nil {
		return nil, err
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
		SessionID: providerSessionID,
		Provider:  providerHookName,
		Bytes:     body,
	}, nil
}

// resolvedSession is the triple every downstream bundle step
// needs, regardless of which resolver produced it.
type resolvedSession struct {
	LocalSessionID    string // agent_sessions.session_id, used for event lookup
	ProviderSessionID string // shown in the bundle header
	Provider          string // hook-form name (claude-code, gemini-cli, etc.)
}

// hookProviderName canonicalizes a DB-form provider value back to
// the hook-registry name that bundle headers and `handoff continue`
// expect. The lineage fallback reads from `agent_sessions.provider`
// which stores `claude_code` / `gemini_cli` for those two agents;
// `cursor`, `copilot`, `kiro-cli`, and `kiro-ide` are stored in the
// same form the hook layer uses, so they pass through unchanged.
func hookProviderName(dbName string) string {
	switch dbName {
	case "claude_code":
		return "claude-code"
	case "gemini_cli":
		return "gemini-cli"
	default:
		return dbName
	}
}

// dbProviderName is the inverse of hookProviderName: hook-form
// names (claude-code, gemini-cli) are translated back to DB-form
// (claude_code, gemini_cli) for use in SQL filters. The other
// providers (cursor, copilot, kiro-cli, kiro-ide) use the same
// shape in both layers and pass through unchanged. Used by the
// --from override to feed a user-supplied provider name into the
// agent_sessions.provider column.
func dbProviderName(hookName string) string {
	switch hookName {
	case "claude-code":
		return "claude_code"
	case "gemini-cli":
		return "gemini_cli"
	default:
		return hookName
	}
}

// listActiveProviders returns the distinct providers with active
// capture states in the repo, sorted by most-recent timestamp
// descending. Same filters as resolveSession (CWD under repo
// root, parent session, recent timestamp). Used by the ambiguity
// resolution flow:
//
//   - 0 providers: no active sessions; caller falls through to
//     lineage or ErrNoSession.
//   - 1 provider: caller auto-routes to that provider via the
//     --from path (multiple same-provider capture states are
//     treated as one logical "active provider").
//   - 2+ providers: caller surfaces AmbiguousActiveSessionError
//     with this list attached so the command layer can pick or
//     enumerate.
//
// Returning the empty list (not an error) for "no matches" keeps
// the call sites simple; an actual capture-state directory error
// is surfaced.
func listActiveProviders(repoPath string, now time.Time) ([]ActiveProvider, error) {
	all, err := hooks.LoadActiveCaptureStates()
	if err != nil {
		return nil, fmt.Errorf("load capture states: %w", err)
	}

	canonicalRepo, err := filepath.EvalSymlinks(repoPath)
	if err != nil {
		canonicalRepo = filepath.Clean(repoPath)
	}
	since := now.Add(-recentSessionWindow).UnixMilli()

	// Group by provider as we scan, tracking count and latest ts.
	// Using a map keeps the dedup linear; we sort at the end.
	type agg struct {
		count  int
		latest int64
	}
	groups := map[string]*agg{}
	for _, st := range all {
		if st.Timestamp <= 0 || st.Timestamp < since {
			continue
		}
		if st.StateKey != "" && st.StateKey != st.SessionID {
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
		g, ok := groups[st.Provider]
		if !ok {
			g = &agg{}
			groups[st.Provider] = g
		}
		g.count++
		if st.Timestamp > g.latest {
			g.latest = st.Timestamp
		}
	}

	out := make([]ActiveProvider, 0, len(groups))
	for provider, g := range groups {
		out = append(out, ActiveProvider{
			Provider:        provider,
			Count:           g.count,
			LatestTimestamp: time.UnixMilli(g.latest),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		// Most-recent first; stable secondary sort by provider
		// name to keep test output deterministic.
		if !out[i].LatestTimestamp.Equal(out[j].LatestTimestamp) {
			return out[i].LatestTimestamp.After(out[j].LatestTimestamp)
		}
		return out[i].Provider < out[j].Provider
	})
	return out, nil
}

// resolveSession picks the single capture state to hand off from.
// Filters: CWD under repo root, parent session (StateKey unset or
// equal to SessionID), recent timestamp. The resolver is provider-
// agnostic: any agent that runs through Semantica's capture
// pipeline (claude-code, cursor, gemini-cli, kiro-cli, etc.) is
// eligible. Errors when zero or more than one state matches;
// never silently picks among multiple actives.
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

// assembleBundleForRepo is the top-level bundle builder. It tries
// the capture-state resolver first (works in-turn), falls back to
// lineage data when no active capture state matches (works
// between turns), and emits a degraded metadata-only bundle when
// even lineage data is unavailable but a capture state was
// recovered. Returns ErrNoSession when neither path produces any
// session at all; ErrAmbiguousSession bubbles up unchanged so a
// user with two genuinely concurrent agents in one repo still
// sees the safety prompt.
//
// Returned triple: body bytes, the provider-session-id for the
// Result, and the hook-form provider name for the Result.
func assembleBundleForRepo(ctx context.Context, repoPath string, now time.Time, from string) ([]byte, string, string, error) {
	branch := readGitBranch(ctx, repoPath)
	view := bundleView{
		Repo:        filepath.Base(repoPath),
		Branch:      branch,
		GeneratedAt: now.UTC().Format(time.RFC3339),
	}

	// --from intentionally ignores active capture state and resolves
	// only through lineage for the named provider.
	//
	// autoSelected flips to true when same-provider capture states set
	// `from` internally. Downstream failures then wrap ErrAutoSelectFailed
	// instead of ErrNoFromMatch so the command layer can avoid --from
	// advice the user did not ask for.
	autoSelected := false
	captureState, captureErr := resolveSession(repoPath, now)
	if from == "" && captureErr != nil && errors.Is(captureErr, ErrAmbiguousSession) {
		// Multiple active capture states are grouped by provider:
		//   - 1 distinct provider: silently route through the
		//     --from path for that provider (no picker needed).
		//   - 2+ distinct providers: bubble an
		//     AmbiguousActiveSessionError with the candidate list
		//     so the command layer can show a picker (TTY) or
		//     enumerate them in the error message (CI/skill).
		providers, listErr := listActiveProviders(repoPath, now)
		if listErr != nil {
			return nil, "", "", listErr
		}
		switch len(providers) {
		case 0:
			// Race: ErrAmbiguousSession said >1 match, but the
			// states are now gone (Stop hook fired between calls).
			// Fall through to the no-session path.
			captureState = nil
			captureErr = ErrNoSession
		case 1:
			// Same-provider duplicates collapse to one active
			// provider. Mark the route as auto-selected so
			// downstream errors avoid --from advice.
			from = providers[0].Provider
			autoSelected = true
			captureState = nil
			captureErr = nil
		default:
			return nil, "", "", &AmbiguousActiveSessionError{Providers: providers}
		}
	}
	if from == "" && captureErr != nil && !errors.Is(captureErr, ErrNoSession) {
		// Defensive: any other capture-state error bubbles
		// unchanged. None defined today, but the explicit guard
		// keeps a future error class from being swallowed.
		return nil, "", "", captureErr
	}

	semDir := filepath.Join(repoPath, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")

	// Without --from, capture-state metadata can still produce a
	// degraded bundle when lineage data is missing. With --from,
	// lineage is required so the bundle cannot be written under a
	// different active provider's identity.
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		if from != "" {
			return nil, "", "", fmt.Errorf("%w: lineage.db not found at %s", fromFailureSentinel(autoSelected), dbPath)
		}
		if captureState == nil {
			return nil, "", "", ErrNoSession
		}
		view.Provider = captureState.Provider
		view.SessionID = captureState.SessionID
		view.Note = noteLineageMissing
		return renderBundle(view), captureState.SessionID, captureState.Provider, nil
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.OpenOptions{
		BusyTimeout: 100 * time.Millisecond,
		Synchronous: "NORMAL",
	})
	if err != nil {
		if from != "" {
			return nil, "", "", fmt.Errorf("%w: lineage.db could not be opened: %v", fromFailureSentinel(autoSelected), err)
		}
		if captureState == nil {
			return nil, "", "", ErrNoSession
		}
		view.Provider = captureState.Provider
		view.SessionID = captureState.SessionID
		view.Note = noteLineageUnavail
		return renderBundle(view), captureState.SessionID, captureState.Provider, nil
	}
	defer func() { _ = sqlstore.Close(h) }()

	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, repoPath)
	if err != nil {
		if from != "" {
			return nil, "", "", fmt.Errorf("%w: repo not registered in lineage.db", fromFailureSentinel(autoSelected))
		}
		if captureState == nil {
			return nil, "", "", ErrNoSession
		}
		view.Provider = captureState.Provider
		view.SessionID = captureState.SessionID
		view.Note = noteSessionUnknown
		return renderBundle(view), captureState.SessionID, captureState.Provider, nil
	}

	// Resolution order:
	//
	//   - Explicit --from override (cross-agent handoff): pick the
	//     named provider's most-recent session with events in this
	//     repo. Bypasses both the capture state and the lineage
	//     fallback so the user gets exactly the source they asked
	//     for, not whatever happens to be active.
	//   - Active capture state (in-turn): try resolveFromCaptureState
	//     only. If the session is in-flight but hasn't been
	//     registered in agent_sessions yet (race between the
	//     prompt-submit hook writing capture state and the worker
	//     writing the lineage row), fall through to the header-only
	//     degraded bundle below. Do not consult the lineage
	//     fallback here. It would pick the previous session and
	//     bundle that session's prompts and events under the
	//     in-flight session's identity, which is the wrong content.
	//   - No capture state (between turns): the lineage fallback
	//     is the right answer. The most-recent parent session with
	//     events is the user's last actual conversation, which is
	//     what they want to hand off.
	var resolved *resolvedSession
	switch {
	case from != "":
		resolved = resolveFromProvider(ctx, h, repoRow.RepositoryID, from, now)
		if resolved == nil {
			return nil, "", "", fmt.Errorf("%w: no recent %s session in this repo", fromFailureSentinel(autoSelected), from)
		}
	case captureState != nil:
		resolved = resolveFromCaptureState(ctx, h, repoRow.RepositoryID, captureState)
	default:
		resolved = resolveFromLineage(ctx, h, repoRow.RepositoryID, now)
	}
	if resolved == nil {
		if captureState == nil {
			return nil, "", "", ErrNoSession
		}
		// Capture state existed but nothing matched in lineage
		// (e.g. a session that hasn't written any events yet).
		// Render the header-only bundle so the user at least gets
		// some output. Picking an older lineage session here would
		// silently swap content for a session the user did not
		// intend to hand off.
		view.Provider = captureState.Provider
		view.SessionID = captureState.SessionID
		view.Note = noteSessionUnknown
		return renderBundle(view), captureState.SessionID, captureState.Provider, nil
	}

	view.Provider = resolved.Provider
	view.SessionID = resolved.ProviderSessionID

	events, err := h.Queries.ListAgentEventsBySession(ctx, sqldb.ListAgentEventsBySessionParams{
		SessionID: resolved.LocalSessionID,
		Limit:     500,
	})
	if err != nil {
		view.Note = noteEventsUnavail
		return renderBundle(view), resolved.ProviderSessionID, resolved.Provider, nil
	}

	// Prefer raw prompt blobs over the broker-stored summary for
	// the handoff bundle. The summary is intentionally short
	// (~200 chars) because it feeds compact UI surfaces; handoff
	// benefits from the fuller prompt when the blob is available.
	// If blob-store setup fails, the extractor falls back to the
	// stored summary.
	bs, _ := blobs.NewStore(filepath.Join(semDir, "objects"))
	view.RecentPrompts = extractRecentUserPromptsRich(ctx, events, bs, recentPromptsLimit)
	view.LastAssistant = redactString(extractLastAssistant(events))
	view.FileTouches = aggregateFileTouches(events)

	// Working-tree state and recent-commit context: best-effort. A
	// failure on either (broken git, detached state, no HEAD) leaves
	// the section empty rather than blocking the bundle.
	if commits, gErr := readRecentCommits(ctx, repoPath, sessionStartTime(events)); gErr == nil {
		view.RecentCommits = commits
	}
	statusList, diffText := readUncommittedWork(ctx, repoPath)
	view.UncommittedList = statusList
	view.UncommittedDiff = diffText

	return renderBundle(view), resolved.ProviderSessionID, resolved.Provider, nil
}

// resolveFromCaptureState chains the capture state's
// provider_session_id through agent_sessions to the local
// session_id used as the event lookup key. Returns nil when the
// chain doesn't find a matching session (capture state names a
// provider session lineage.db has never seen).
func resolveFromCaptureState(ctx context.Context, h *sqlstore.Handle, repoID string, state *hooks.CaptureState) *resolvedSession {
	rows, err := h.Queries.ListAgentSessionsByProviderSessionID(ctx,
		sqldb.ListAgentSessionsByProviderSessionIDParams{
			RepositoryID:      repoID,
			ProviderSessionID: state.SessionID,
		})
	if err != nil || len(rows) == 0 {
		return nil
	}
	matched, ok := matchSessionByProvider(rows, state.Provider)
	if !ok {
		return nil
	}
	return &resolvedSession{
		LocalSessionID:    matched.SessionID,
		ProviderSessionID: state.SessionID,
		Provider:          state.Provider,
	}
}

// resolveFromProvider picks the most-recent parent session for
// the named provider in the repo, within the same recency window
// the other resolvers use. The bundle's "Original session" line
// then names this provider, so `handoff continue` defaults to
// spawning the source agent. The user can still override with
// --agent if they want to continue in a different one.
//
// hookProvider is the hook-form name (claude-code, gemini-cli,
// etc.). It's translated to DB-form for the agent_sessions
// filter before the query runs.
func resolveFromProvider(ctx context.Context, h *sqlstore.Handle, repoID, hookProvider string, now time.Time) *resolvedSession {
	since := now.Add(-recentSessionWindow).UnixMilli()
	row, err := h.Queries.GetMostRecentSessionByProviderWithEvents(ctx,
		sqldb.GetMostRecentSessionByProviderWithEventsParams{
			RepositoryID: repoID,
			Provider:     dbProviderName(hookProvider),
			SinceTs:      since,
		})
	if err != nil {
		return nil
	}
	return &resolvedSession{
		LocalSessionID:    row.SessionID,
		ProviderSessionID: row.ProviderSessionID,
		Provider:          hookProviderName(row.Provider),
	}
}

// resolveFromLineage picks the most-recent parent session with
// events from agent_sessions, within the same recency window the
// capture-state resolver uses. The session's stored provider name
// is canonicalized back to hook form so the bundle header and
// `handoff continue` see the same provider shape regardless of
// which resolver produced the bundle.
func resolveFromLineage(ctx context.Context, h *sqlstore.Handle, repoID string, now time.Time) *resolvedSession {
	since := now.Add(-recentSessionWindow).UnixMilli()
	row, err := h.Queries.GetMostRecentParentSessionWithEvents(ctx,
		sqldb.GetMostRecentParentSessionWithEventsParams{
			RepositoryID: repoID,
			SinceTs:      since,
		})
	if err != nil {
		return nil
	}
	return &resolvedSession{
		LocalSessionID:    row.SessionID,
		ProviderSessionID: row.ProviderSessionID,
		Provider:          hookProviderName(row.Provider),
	}
}

// recentPromptsLimit caps how many of the session's most-recent
// user prompts the bundle surfaces. Five fits in a typical bundle
// without bloating the response, while giving the next session
// enough context to reconstruct the arc of the work.
const recentPromptsLimit = 5

// recentCommitsLimit caps the commit list shown in the bundle.
// Keeps the section bounded for noisy sessions.
const recentCommitsLimit = 10

// readRecentCommits returns commits landed since the session
// started, capped at recentCommitsLimit. Returns nil on any error
// (broken git, no HEAD, no commits in window) so the bundle still
// renders without the section. Semantica-* trailers are stripped
// from commit bodies before rendering.
func readRecentCommits(ctx context.Context, repoPath string, sessionStart time.Time) ([]git.Commit, error) {
	if sessionStart.IsZero() {
		return nil, nil
	}
	repo, err := git.OpenRepo(repoPath)
	if err != nil {
		return nil, err
	}
	commits, err := repo.LogSince(ctx, sessionStart, recentCommitsLimit)
	if err != nil {
		return nil, err
	}
	for i := range commits {
		commits[i].Body = stripSemanticaTrailers(commits[i].Body)
	}
	return commits, nil
}

// stripSemanticaTrailers removes any trailing Semantica-* trailer
// lines (e.g. "Semantica-Checkpoint: <uuid>" written by the
// post-commit hook) from a commit body. Filters contiguous
// Semantica-trailer lines at the bottom only, plus any blank
// separator lines they leave behind; non-Semantica trailers
// (Co-Authored-By, Signed-off-by, etc.) are preserved. The first
// non-Semantica, non-blank line stops the walk, so prose that
// happens to mention "Semantica-Checkpoint" mid-body is
// untouched.
func stripSemanticaTrailers(body string) string {
	if body == "" {
		return body
	}
	lines := strings.Split(body, "\n")
	end := len(lines)
	for end > 0 {
		line := strings.TrimRight(lines[end-1], " \t")
		if line == "" {
			end--
			continue
		}
		if !isSemanticaTrailer(line) {
			break
		}
		end--
	}
	return strings.TrimRight(strings.Join(lines[:end], "\n"), "\n")
}

// isSemanticaTrailer reports whether a line matches the
// "Semantica-<Token>: <value>" trailer shape. The token must be
// non-empty and contain only alnum or hyphen; the colon must
// appear immediately after the token. Defensive about prefix
// collisions (e.g. "Semantica-suggested-prose:" would match by
// design, but free-form "Semantica is great" would not because
// there is no token-then-colon).
func isSemanticaTrailer(line string) bool {
	const prefix = "Semantica-"
	if !strings.HasPrefix(line, prefix) {
		return false
	}
	rest := line[len(prefix):]
	colon := strings.IndexByte(rest, ':')
	if colon <= 0 {
		return false
	}
	token := rest[:colon]
	for _, r := range token {
		alnumOrHyphen := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '-'
		if !alnumOrHyphen {
			return false
		}
	}
	return true
}

// readUncommittedWork returns the `git status --short` file list and
// a bounded, redacted `git diff HEAD` excerpt. Either may be empty
// (clean tree or git error). Redaction failure clears the diff but
// preserves the path-only file list, which is useful local repo
// context for the next agent session.
func readUncommittedWork(ctx context.Context, repoPath string) (statusList, diffText string) {
	repo, err := git.OpenRepo(repoPath)
	if err != nil {
		return "", ""
	}
	statusList, _ = repo.StatusShort(ctx)
	if statusList == "" {
		return "", ""
	}
	rawDiff, err := repo.DiffWorkingTree(ctx)
	if err != nil {
		return statusList, ""
	}
	redacted, err := redact.Bytes(rawDiff)
	if err != nil {
		// Fail-closed on the diff: drop it, keep the file list.
		return statusList, ""
	}
	if len(redacted) > maxUncommittedDiffBytes {
		return statusList, string(redacted[:maxUncommittedDiffBytes]) + "\n... (truncated)"
	}
	return statusList, string(redacted)
}

// maxUncommittedDiffBytes caps the redacted working-tree diff so a
// session with a huge uncommitted refactor still produces a bundle
// the next agent can read without choking on a 10MB diff. Same
// rationale and same value as explain's diff bound.
const maxUncommittedDiffBytes = 12_000

// matchSessionByProvider picks the agent_sessions row whose provider
// matches the capture state's provider via the alias set. Returns
// (row, true) on a single unambiguous match. Multiple matches
// shouldn't occur under the (repository_id, provider,
// provider_session_id) unique index; if they ever do, refuse rather
// than silently picking. Zero matches means the capture state's
// provider doesn't align with anything stored in this repo, which
// is treated as session-unknown so the bundle degrades cleanly
// instead of pulling another provider's events.
func matchSessionByProvider(rows []sqldb.AgentSession, captureProvider string) (sqldb.AgentSession, bool) {
	aliases := providerAliases(captureProvider)
	aliasSet := make(map[string]struct{}, len(aliases))
	for _, a := range aliases {
		aliasSet[a] = struct{}{}
	}
	var matches []sqldb.AgentSession
	for _, row := range rows {
		if _, ok := aliasSet[row.Provider]; ok {
			matches = append(matches, row)
		}
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	return sqldb.AgentSession{}, false
}

// providerAliases returns the agent_sessions.provider values that
// could correspond to a hook-form provider name. The hook registry
// uses dashes ("claude-code", "gemini-cli") while
// agent_sessions.provider varies by agent: claude_code, gemini_cli,
// cursor, copilot, kiro-cli, kiro-ide. Each branch enumerates the
// known mappings; the default falls back to the hook name itself
// plus its dash-to-underscore variant so a previously-unknown
// provider has a reasonable chance of matching without a code
// change.
func providerAliases(hookName string) []string {
	switch hookName {
	case "claude-code":
		return []string{"claude_code", "claude-code"}
	case "gemini-cli", "gemini":
		return []string{"gemini_cli", "gemini-cli", "gemini"}
	case "cursor":
		return []string{"cursor"}
	case "copilot":
		return []string{"copilot"}
	case "kiro-cli":
		return []string{"kiro-cli", "kiro_cli"}
	case "kiro-ide":
		return []string{"kiro-ide", "kiro_ide"}
	default:
		under := strings.ReplaceAll(hookName, "-", "_")
		if under == hookName {
			return []string{hookName}
		}
		return []string{hookName, under}
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

// extractRecentUserPromptsRich is the handoff-bundle entry point
// for prompt extraction. It prefers the raw prompt payload stored
// in the blob store over agent_events.summary, falling back to
// summary when the blob is missing/unreadable or the event has no
// payload_hash. This is a handoff-specific enrichment: the
// broker keeps summaries short for compact UI surfaces, but the
// bundle wants the full prompt where it's available.
//
// Redaction runs on whichever source was chosen before truncation
// so a secret that straddles the maxPromptChars boundary cannot
// slip past the redactor.
//
// Tool-result events also carry role="user" because the Claude
// transcript model treats them that way; they are filtered out so
// the bundle surfaces what the human typed, not the bash/edit
// outputs the agent received back.
func extractRecentUserPromptsRich(ctx context.Context, events []sqldb.AgentEvent, bs *blobs.Store, n int) []string {
	if n <= 0 {
		return nil
	}
	out := make([]string, 0, n)
	for _, e := range events {
		if !e.Role.Valid || e.Role.String != "user" || e.Kind == "tool_result" {
			continue
		}
		text := loadPromptText(ctx, e, bs)
		if text == "" {
			continue
		}
		// Redact-then-truncate: a secret straddling the cap
		// boundary must not slip past the redactor. When the
		// redactor fail-closes (returns "" on error), skip the
		// event entirely rather than emit an empty fenced block
		// that would still consume one of the prompt slots.
		redacted := redactString(text)
		if redacted == "" {
			continue
		}
		out = append(out, truncateRunes(redacted, maxPromptChars))
		if len(out) >= n {
			break
		}
	}
	// Reverse so the rendered list reads chronologically.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// loadPromptText returns the best available text for a prompt
// event. Tries the blob store first (raw, full-length payload),
// falls back to the event summary (already-trimmed). Returns an
// empty string when neither source has usable text.
func loadPromptText(ctx context.Context, e sqldb.AgentEvent, bs *blobs.Store) string {
	if bs != nil && e.PayloadHash.Valid && e.PayloadHash.String != "" {
		if raw, err := bs.Get(ctx, e.PayloadHash.String); err == nil && len(raw) > 0 {
			return string(raw)
		}
	}
	if e.Summary.Valid {
		return e.Summary.String
	}
	return ""
}

// extractRecentUserPrompts walks the (descending-ts) events slice
// for up to n user-role prompts, returning them oldest-first so the
// rendered list reads as a natural session arc. The single-prompt
// view that the bundle used to surface only shows the latest meta
// turn (often "handoff please"); a fresh agent reading the bundle
// gets much more useful context from "the last 5 things you asked"
// than from "the last 1 thing you asked."
//
// Tool-result events also carry role="user" because the Claude Code
// transcript model treats them as user-side responses to assistant
// tool calls. Those are explicitly filtered out here: what we want
// in the bundle is what the human typed, not the bash/edit/read
// outputs the agent received back.
func extractRecentUserPrompts(events []sqldb.AgentEvent, n int) []string {
	if n <= 0 {
		return nil
	}
	var out []string
	for _, e := range events {
		if !e.Role.Valid || e.Role.String != "user" {
			continue
		}
		if e.Kind == "tool_result" {
			continue
		}
		if !e.Summary.Valid || e.Summary.String == "" {
			continue
		}
		out = append(out, truncateRunes(e.Summary.String, maxPromptChars))
		if len(out) >= n {
			break
		}
	}
	// Reverse so the rendered list reads chronologically (oldest first).
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// sessionStartTime returns the timestamp of the earliest event in
// the slice with a positive Ts, or time.Time{} when no event has a
// usable timestamp. Used as the `--since` anchor for `git log` so
// the "recent commits in this session" section only surfaces
// commits actually related to the session being handed off.
//
// Events with Ts == 0 (unset, or stripped during capture) are
// skipped entirely. Seeding from any element's Ts up front would
// risk anchoring on a zero, after which `e.Ts < earliest` never
// fires (Ts is non-negative) and the function would return zero
// even when later entries carry valid positive timestamps.
func sessionStartTime(events []sqldb.AgentEvent) time.Time {
	var earliest int64
	found := false
	for _, e := range events {
		if e.Ts <= 0 {
			continue
		}
		if !found || e.Ts < earliest {
			earliest = e.Ts
			found = true
		}
	}
	if !found {
		return time.Time{}
	}
	return time.UnixMilli(earliest)
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
//
// Indirected through redactStringFn so tests can exercise the
// fail-closed redaction branch.
var redactStringFn = defaultRedactString

func redactString(s string) string { return redactStringFn(s) }

func defaultRedactString(s string) string {
	if s == "" {
		return s
	}
	r, err := redact.String(s)
	if err != nil {
		return ""
	}
	return r
}

// truncateRunes truncates a string to at most n runes, appending
// an explicit " [truncated]" marker when truncation fired. The
// marker is preferred over an ellipsis because the bundle is
// rendered in fenced code blocks where a trailing "..." is
// ambiguous (was that the user's text or the truncation?). The
// marker is added outside the n-rune budget so callers asking
// for a 1500-rune cap still get 1500 runes of content.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + " [truncated]"
}

// writeFenced renders body inside a markdown code fence and writes
// it to b. Chooses a fence length one greater than the longest
// run of backticks anywhere in the body so prompts that themselves
// contain ``` (e.g. a user pasting markdown) cannot terminate the
// outer fence early.
func writeFenced(b *strings.Builder, body string) {
	fence := strings.Repeat("`", longestBacktickRun(body)+1)
	if len(fence) < 3 {
		fence = "```"
	}
	b.WriteString(fence)
	b.WriteString("\n")
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\n")
	}
	b.WriteString(fence)
	b.WriteString("\n\n")
}

// longestBacktickRun returns the length of the longest consecutive
// backtick run in s. Used by writeFenced to pick a fence that
// cannot be terminated by anything in the body.
func longestBacktickRun(s string) int {
	longest, current := 0, 0
	for _, r := range s {
		if r == '`' {
			current++
			if current > longest {
				longest = current
			}
		} else {
			current = 0
		}
	}
	return longest
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
	Repo            string
	Branch          string
	Provider        string
	SessionID       string
	GeneratedAt     string
	RecentPrompts   []string // oldest-first list of user prompts
	LastAssistant   string
	FileTouches     []fileTouch
	RecentCommits   []git.Commit // hash + subject + (optional) body
	UncommittedList string       // `git status --short` output
	UncommittedDiff string       // bounded, redacted `git diff HEAD` output
	Note            string       // optional caveat shown when assembly degraded
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
	if len(v.RecentPrompts) > 0 {
		b.WriteString("**Recent user prompts (oldest first, redacted):**\n\n")
		for _, p := range v.RecentPrompts {
			writeFenced(&b, p)
		}
	}
	if v.LastAssistant != "" {
		b.WriteString("**Last assistant message (redacted):**\n\n")
		writeFenced(&b, v.LastAssistant)
	}
	if len(v.RecentPrompts) == 0 && v.LastAssistant == "" {
		b.WriteString("_No prompt or assistant message available for this session yet._\n\n")
	}

	if len(v.RecentCommits) > 0 {
		b.WriteString("## Recent commits during this session\n\n")
		for _, c := range v.RecentCommits {
			fmt.Fprintf(&b, "- **%s** %s\n", c.ShortHash, c.Subject)
			if c.Body != "" {
				// Indent body lines so markdown renders them as
				// part of the same list item.
				for _, line := range strings.Split(c.Body, "\n") {
					if line == "" {
						b.WriteString("\n")
						continue
					}
					b.WriteString("  ")
					b.WriteString(line)
					b.WriteString("\n")
				}
			}
		}
		b.WriteString("\n")
	}

	if v.UncommittedList != "" {
		b.WriteString("## Working tree changes (uncommitted)\n\n")
		b.WriteString("Files:\n\n")
		b.WriteString("```\n")
		b.WriteString(v.UncommittedList)
		if !strings.HasSuffix(v.UncommittedList, "\n") {
			b.WriteByte('\n')
		}
		b.WriteString("```\n\n")
		if v.UncommittedDiff != "" {
			b.WriteString("Diff (redacted, bounded):\n\n")
			b.WriteString("```diff\n")
			b.WriteString(v.UncommittedDiff)
			if !strings.HasSuffix(v.UncommittedDiff, "\n") {
				b.WriteByte('\n')
			}
			b.WriteString("```\n\n")
		}
	}

	return []byte(b.String())
}
