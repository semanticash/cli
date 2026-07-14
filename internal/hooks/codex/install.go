package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/platform"
)

// Codex hooks are user-global: a single ~/.codex/hooks.json covers
// hook-capable Codex sessions on the machine. Per-session gating then
// runs at capture time through ShouldCapture, which rejects sessions
// whose cwd does not resolve to a registered repo. Other providers in
// this repo install per-repo hook configs; Codex differs because the
// hook configuration lives in the shared Codex state directory.

const (
	// semanticaMarker appears inside every hook command we install. It
	// lets install/uninstall recognize Semantica-owned entries when a
	// user has unrelated hooks under the same Codex events.
	semanticaMarker = "semantica capture codex"

	// hooksFileName is the user-global hooks file Codex reads. We keep
	// it as a constant rather than per-OS branching: Codex resolves
	// $CODEX_HOME (defaulting to ~/.codex) the same way across all
	// supported platforms.
	hooksFileName = "hooks.json"

	// configFileName is the TOML file that holds [features] and the
	// [hooks.state.*] trust namespace.
	configFileName = "config.toml"
)

// codexHookEvent describes one hook entry the installer writes.
//
// The pascalEvent / snakeEvent pair reflects a quirk of the Codex format:
// hooks.json uses PascalCase event names while the trust namespace
// ([hooks.state."<file>:<event>:..."]) uses snake_case. Both forms must
// agree with what Codex's runtime expects or the hook either fails to
// fire or appears as untrusted.
type codexHookEvent struct {
	pascalEvent string // event key in hooks.json (e.g. "PostToolUse")
	snakeEvent  string // event key in [hooks.state.*] (e.g. "post_tool_use")
	captureName string // subcommand passed to `semantica capture codex ...`
	matcher     string // optional regex; empty means "match every tool"
}

// hookEvents enumerates the Codex hooks Semantica installs. Order is
// stable for deterministic file output.
var hookEvents = []codexHookEvent{
	{"SessionStart", "session_start", "session-start", ""},
	{"UserPromptSubmit", "user_prompt_submit", "user-prompt-submit", ""},
	{"PostToolUse", "post_tool_use", "post-tool-use", "apply_patch|Bash|Write|Edit"},
	{"Stop", "stop", "stop", ""},
}

// hookFileShape mirrors the on-disk layout Codex expects for hooks.json:
// an outer object with a single "hooks" key whose value maps PascalCase
// event names to arrays of matcher groups.
type hookFileShape struct {
	Hooks map[string][]matcherGroup `json:"hooks"`
}

type matcherGroup struct {
	Matcher string         `json:"matcher,omitempty"`
	Hooks   []commandEntry `json:"hooks"`
}

type commandEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// installedHook captures the on-disk position and command string of one
// Semantica entry inside hooks.json. The trust namespace is keyed by
// (groupIndex, hookIndex) and the hash is computed over the exact
// command string Codex sees, so install and uninstall both need this
// per-entry information rather than synthesized canonical values.
type installedHook struct {
	pascalEvent string
	snakeEvent  string
	matcher     string
	groupIndex  int
	hookIndex   int
	command     string
}

// codexHomeDir returns the directory Codex stores its user-global state
// in. Defaults to ~/.codex and is overridable by $CODEX_HOME so test
// fixtures can run in isolation.
func codexHomeDir() (string, error) {
	if env := os.Getenv("CODEX_HOME"); env != "" {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".codex"), nil
}

// InstallHooks writes the Semantica hook configuration into Codex's
// user-global config directory. The repoRoot argument is unused for the
// install itself (Codex hooks are not per-repo) but kept to satisfy the
// HookProvider contract; the cwd preflight gates per-repo behavior at
// capture time instead.
//
// On success, the user has:
//
//   - ~/.codex/hooks.json with four entries (SessionStart,
//     UserPromptSubmit, PostToolUse, Stop) pointing at the Semantica
//     binary. Existing user hook entries under the same events are
//     preserved; Semantica's entries are appended after them.
//   - ~/.codex/config.toml updated with [features] hooks = true and one
//     [hooks.state.*] trusted_hash per installed hook, so Codex does not
//     prompt for hook review on the next session. Trust keys reflect
//     the actual (groupIndex, hookIndex) where Semantica's entries
//     written, which can be non-zero when the file already contained
//     unrelated hooks for the same event.
//
// User configuration in config.toml (model pins, plugin blocks,
// marketplace declarations, project trust levels) is preserved across
// the round-trip. Comments and original key ordering are not retained
// because the TOML round-trip rewrites the file through a map; callers
// that need a comment-preserving editor should add one upstream and
// share it across providers.
//
// Re-running is safe: identical commands at identical positions produce
// identical content and identical trust hashes, so the file ends up
// byte-equivalent.
func (p *Provider) InstallHooks(ctx context.Context, repoRoot string, binaryPath string) (int, error) {
	bin := binaryPath
	if bin == "" {
		bin = "semantica"
	}

	home, err := codexHomeDir()
	if err != nil {
		return 0, err
	}
	hooksPath := filepath.Join(home, hooksFileName)
	configPath := filepath.Join(home, configFileName)

	commands := commandsForBinary(bin)

	// Capture the prior install's command strings before merging so
	// their hashes contribute to recognition. Without this, a binary
	// path change (or any future change to the guarded-command shape)
	// would leave stale trust entries at positions that have shifted
	// between runs: their hashes would not be in the new install's
	// recognizedHashes, so the cleanup phase would skip them.
	prior, err := scanInstalledSemanticaEntries(hooksPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}

	// Merge into existing hooks.json so unrelated user hooks survive.
	// installed carries the actual (groupIndex, hookIndex) each
	// Semantica entry occupies after the merge - non-zero when the
	// user already had hooks under one of our events.
	merged, installed, err := mergeHooksFile(hooksPath, commands)
	if err != nil {
		return 0, err
	}

	// Build the trust mutation. trustHashes drives the upsert;
	// recognizedHashes lets the cleanup phase remove any stale trust
	// entries under hooksFilePath whose stored hash matches a
	// Semantica command from this or a prior install.
	mutation := configMutation{
		trustHashes:      make(map[string]string, len(installed)),
		hooksFilePath:    hooksPath,
		recognizedHashes: make(map[string]struct{}, len(installed)+len(prior)),
	}
	for _, h := range installed {
		key := trustKey(hooksPath, h.snakeEvent, h.groupIndex, h.hookIndex)
		hash := commandHookHash(h.snakeEvent, h.matcher, h.command)
		mutation.trustHashes[key] = hash
		mutation.recognizedHashes[hash] = struct{}{}
	}
	for _, h := range prior {
		hash := commandHookHash(h.snakeEvent, h.matcher, h.command)
		mutation.recognizedHashes[hash] = struct{}{}
	}

	if err := updateConfigTOML(configPath, mutation.applyToTOML); err != nil {
		return 0, err
	}

	// Write hooks.json last: a partial install where hooks.json points
	// at the binary but the trust entries are missing would surface a
	// hook-review prompt the user did not consent to.
	if err := os.MkdirAll(home, 0o755); err != nil {
		return 0, fmt.Errorf("create %s: %w", home, err)
	}
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("marshal hooks.json: %w", err)
	}
	if err := writeFileAtomic(hooksPath, append(out, '\n'), 0o644); err != nil {
		return 0, err
	}

	return len(hookEvents), nil
}

// UninstallHooks removes Semantica's entries from the user-global
// configuration. Other tools' hooks - including any non-Semantica
// entries the user may have added by hand - are preserved.
//
// The function reads the current hooks.json to recover the exact command
// strings and positions of Semantica's entries before pruning the file,
// then removes only the trust entries whose hash matches what those
// commands produced. Trust entries whose hash differs (e.g. modified by
// the user or written by a different tool) are left untouched.
//
// Codex hooks are user-global, so a disable on any one repo removes the
// hooks for all repos. Re-running enable on another registered repo
// restores them. This is a deliberate trade for keeping the install
// surface symmetric across CLI and desktop sessions; users with multiple
// active repos who only want to disable Codex on one should rely on the
// cwd preflight (a deregistered repo will not produce capture events)
// rather than running `disable --providers codex`.
func (p *Provider) UninstallHooks(ctx context.Context, repoRoot string) error {
	home, err := codexHomeDir()
	if err != nil {
		return err
	}
	hooksPath := filepath.Join(home, hooksFileName)
	configPath := filepath.Join(home, configFileName)

	// Recover the actual installed state before pruning. If the file is
	// missing there is nothing to do for either side.
	installed, err := scanInstalledSemanticaEntries(hooksPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	// Prune hooks.json first. If this step fails the file is left
	// intact (writeFileAtomic guarantees all-or-nothing) and trust
	// entries are still valid, so the user is not left with hooks that
	// fire but appear unapproved to Codex.
	if err := pruneHooksFile(hooksPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	// Compute trust entries from the actual command strings on disk so
	// installs with non-default binary paths (e.g. `--binary
	// /opt/special/semantica`) round-trip cleanly. recognizedHashes
	// drives the removal pass, which sweeps every entry under
	// hooksFilePath whose stored hash matches one of those commands -
	// including entries at outdated positions that a prior shift
	// stranded.
	mutation := configMutation{
		hooksFilePath:    hooksPath,
		recognizedHashes: make(map[string]struct{}, len(installed)),
	}
	for _, h := range installed {
		hash := commandHookHash(h.snakeEvent, h.matcher, h.command)
		mutation.recognizedHashes[hash] = struct{}{}
	}
	return updateConfigTOML(configPath, mutation.removeFromTOML)
}

// AreHooksInstalled reports whether ~/.codex/hooks.json contains at
// least one Semantica-owned entry. Detection runs on the file alone -
// trust state is best-effort and a missing trust entry only means the
// user has not yet acknowledged the hooks, not that they are absent.
func (p *Provider) AreHooksInstalled(ctx context.Context, repoRoot string) bool {
	home, err := codexHomeDir()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(home, hooksFileName))
	if err != nil {
		return false
	}
	var shape hookFileShape
	if err := json.Unmarshal(data, &shape); err != nil {
		return false
	}
	for _, groups := range shape.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if strings.Contains(h.Command, semanticaMarker) {
					return true
				}
			}
		}
	}
	return false
}

// HookBinary returns the binary path Codex would execute for any one of
// our installed hooks. Health checks use this to verify `semantica` is
// still reachable via exec.LookPath on the user's machine.
func (p *Provider) HookBinary(ctx context.Context, repoRoot string) (string, error) {
	home, err := codexHomeDir()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(home, hooksFileName))
	if err != nil {
		return "", err
	}
	var shape hookFileShape
	if err := json.Unmarshal(data, &shape); err != nil {
		return "", fmt.Errorf("parse %s: %w", hooksFileName, err)
	}
	for _, groups := range shape.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if strings.Contains(h.Command, semanticaMarker) {
					return hooks.ExtractBinary(h.Command), nil
				}
			}
		}
	}
	return "", fmt.Errorf("no semantica hook found in %s", hooksFileName)
}

// commandsForBinary returns the shell command strings, in the same order
// as hookEvents, that the installer writes for the given binary.
func commandsForBinary(bin string) []string {
	out := make([]string, len(hookEvents))
	for i, ev := range hookEvents {
		out[i] = hooks.GuardedCommand(bin, "capture codex "+ev.captureName)
	}
	return out
}

// trustKey reproduces the literal key Codex stores under [hooks.state.*].
// Format: "<hooks-file-path>:<snake_event>:<group_index>:<hook_index>".
func trustKey(hooksPath, snakeEvent string, groupIndex, hookIndex int) string {
	return fmt.Sprintf("%s:%s:%d:%d", hooksPath, snakeEvent, groupIndex, hookIndex)
}

// mergeHooksFile reads an existing hooks.json (if present), removes
// any previously-installed Semantica entries, and appends the freshly
// computed ones. Non-Semantica entries belonging to other tools
// survive the round-trip. The returned slice records the position
// each Semantica entry occupies in the merged shape so callers can
// compute trust fingerprints keyed against the actual (groupIndex,
// hookIndex).
func mergeHooksFile(hooksPath string, commands []string) (hookFileShape, []installedHook, error) {
	shape := hookFileShape{Hooks: make(map[string][]matcherGroup)}
	if data, err := os.ReadFile(hooksPath); err == nil {
		if err := json.Unmarshal(data, &shape); err != nil {
			return shape, nil, fmt.Errorf("parse %s: %w", hooksPath, err)
		}
		if shape.Hooks == nil {
			shape.Hooks = make(map[string][]matcherGroup)
		}
		stripSemanticaEntries(&shape)
	} else if !errors.Is(err, os.ErrNotExist) {
		return shape, nil, fmt.Errorf("read %s: %w", hooksPath, err)
	}

	installed := make([]installedHook, 0, len(hookEvents))
	for i, ev := range hookEvents {
		entry := matcherGroup{
			Matcher: ev.matcher,
			Hooks: []commandEntry{{
				Type:    "command",
				Command: commands[i],
			}},
		}
		shape.Hooks[ev.pascalEvent] = append(shape.Hooks[ev.pascalEvent], entry)
		installed = append(installed, installedHook{
			pascalEvent: ev.pascalEvent,
			snakeEvent:  ev.snakeEvent,
			matcher:     ev.matcher,
			groupIndex:  len(shape.Hooks[ev.pascalEvent]) - 1,
			hookIndex:   0,
			command:     commands[i],
		})
	}
	return shape, installed, nil
}

// scanInstalledSemanticaEntries reads hooks.json and returns one record
// per Semantica entry currently in the file. The records carry the
// exact command string Codex hashed, so the caller can reproduce trust
// fingerprints for arbitrary installed binaries.
//
// Only entries under canonical Semantica events are scanned. Entries
// the user may have moved manually to other events are not handled here
// - they will surface as untrusted at next install instead.
func scanInstalledSemanticaEntries(hooksPath string) ([]installedHook, error) {
	data, err := os.ReadFile(hooksPath)
	if err != nil {
		return nil, err
	}
	var shape hookFileShape
	if err := json.Unmarshal(data, &shape); err != nil {
		return nil, fmt.Errorf("parse %s: %w", hooksPath, err)
	}
	var result []installedHook
	for _, ev := range hookEvents {
		groups := shape.Hooks[ev.pascalEvent]
		for gIdx, g := range groups {
			for hIdx, h := range g.Hooks {
				if !strings.Contains(h.Command, semanticaMarker) {
					continue
				}
				result = append(result, installedHook{
					pascalEvent: ev.pascalEvent,
					snakeEvent:  ev.snakeEvent,
					matcher:     g.Matcher,
					groupIndex:  gIdx,
					hookIndex:   hIdx,
					command:     h.Command,
				})
			}
		}
	}
	return result, nil
}

// pruneHooksFile removes Semantica entries from hooks.json in place. If
// the file ends up empty, the whole file is deleted so a future enable
// starts from a clean state.
func pruneHooksFile(hooksPath string) error {
	data, err := os.ReadFile(hooksPath)
	if err != nil {
		return err
	}
	var shape hookFileShape
	if err := json.Unmarshal(data, &shape); err != nil {
		return fmt.Errorf("parse %s: %w", hooksPath, err)
	}
	if shape.Hooks == nil {
		return nil
	}
	stripSemanticaEntries(&shape)

	if len(shape.Hooks) == 0 {
		return os.Remove(hooksPath)
	}
	out, err := json.MarshalIndent(shape, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal hooks.json: %w", err)
	}
	return writeFileAtomic(hooksPath, append(out, '\n'), 0o644)
}

// stripSemanticaEntries removes Semantica-owned commands from the shape
// in place. A matcher group becomes empty after stripping is dropped; an
// event that ends up with no remaining groups is removed from the map.
func stripSemanticaEntries(shape *hookFileShape) {
	for event, groups := range shape.Hooks {
		kept := groups[:0]
		for _, g := range groups {
			keptHooks := g.Hooks[:0]
			for _, h := range g.Hooks {
				if strings.Contains(h.Command, semanticaMarker) {
					continue
				}
				keptHooks = append(keptHooks, h)
			}
			if len(keptHooks) == 0 {
				continue
			}
			g.Hooks = keptHooks
			kept = append(kept, g)
		}
		if len(kept) == 0 {
			delete(shape.Hooks, event)
			continue
		}
		shape.Hooks[event] = kept
	}
}

// updateConfigTOML loads ~/.codex/config.toml, applies the given mutation
// callback, and writes it back only when something changed. Returns nil
// on a missing file (callers create one) and never overwrites a file
// that failed to parse.
func updateConfigTOML(path string, apply func(map[string]any) bool) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	doc, err := readConfigTOML(data)
	if err != nil {
		return err
	}
	if !apply(doc) {
		return nil
	}
	out, err := writeConfigTOML(doc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	return writeFileAtomic(path, out, 0o600)
}

// writeFileAtomic writes data to path through a sibling temp file plus
// a platform-safe rename, so a crash mid-write never leaves a truncated
// or partially-overwritten config visible to Codex. Any failure along
// the way returns an error; the destination file is either fully
// updated to the new content or left at its previous content. There is
// no non-atomic fallback path.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp file for %s: %w", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp file for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp file for %s: %w", path, err)
	}
	if err := platform.SafeRename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("rename temp file to %s: %w", path, err)
	}
	return nil
}
