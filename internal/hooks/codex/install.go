package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/platform"
)

// Codex hooks are repo-local and require user approval. Installation also
// enables Codex's global hooks feature without modifying global hook entries.

const (
	// semanticaMarker identifies entries managed by Semantica.
	semanticaMarker = "semantica capture codex"

	// hooksFileName is shared by project and user config layers.
	hooksFileName = "hooks.json"

	// configFileName contains the global hooks feature flag.
	configFileName = "config.toml"

	// codexRepoDirName is Codex's project config directory.
	codexRepoDirName = ".codex"

	// installLockName serializes Semantica updates to global Codex config.
	installLockName = ".semantica-install.lock"
)

// codexHookEvent describes one installed hook.
type codexHookEvent struct {
	pascalEvent string // event key in hooks.json (e.g. "PostToolUse")
	captureName string // subcommand passed to `semantica capture codex ...`
	matcher     string // optional regex; empty means "match every tool"
}

// hookEvents has stable order for deterministic output.
var hookEvents = []codexHookEvent{
	{"SessionStart", "session-start", ""},
	{"UserPromptSubmit", "user-prompt-submit", ""},
	// Bash needs a pre-execution snapshot; direct edit tools do not.
	{"PreToolUse", "pre-tool-use", "Bash"},
	{"PostToolUse", "post-tool-use", "apply_patch|Bash|Write|Edit"},
	{"Stop", "stop", ""},
}

// hookFileShape preserves fields Semantica does not own while merging.
type hookFileShape struct {
	Hooks map[string][]matcherGroup
	extra map[string]json.RawMessage
}

func (s *hookFileShape) UnmarshalJSON(b []byte) error {
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if v, ok := raw["hooks"]; ok {
		if err := json.Unmarshal(v, &s.Hooks); err != nil {
			return err
		}
		delete(raw, "hooks")
	}
	s.extra = raw
	return nil
}

func (s hookFileShape) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(s.extra)+1)
	for k, v := range s.extra {
		out[k] = v
	}
	hooks := s.Hooks
	if hooks == nil {
		hooks = map[string][]matcherGroup{}
	}
	hb, err := jsonNoEscape(hooks)
	if err != nil {
		return nil, err
	}
	out["hooks"] = hb
	return jsonNoEscape(out)
}

type matcherGroup struct {
	// matcherRaw preserves matcher tokens from existing files.
	Matcher    string
	Hooks      []commandEntry
	matcherRaw json.RawMessage
	extra      map[string]json.RawMessage
}

func (g *matcherGroup) UnmarshalJSON(b []byte) error {
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if v, ok := raw["matcher"]; ok {
		g.matcherRaw = v
		// Preserve values that do not decode as strings.
		_ = json.Unmarshal(v, &g.Matcher)
		delete(raw, "matcher")
	}
	if v, ok := raw["hooks"]; ok {
		if err := json.Unmarshal(v, &g.Hooks); err != nil {
			return err
		}
		delete(raw, "hooks")
	}
	g.extra = raw
	return nil
}

func (g matcherGroup) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(g.extra)+2)
	for k, v := range g.extra {
		out[k] = v
	}
	switch {
	case g.matcherRaw != nil:
		// Preserve the original matcher token.
		out["matcher"] = g.matcherRaw
	case g.Matcher != "":
		mb, err := jsonNoEscape(g.Matcher)
		if err != nil {
			return nil, err
		}
		out["matcher"] = mb
	}
	hb, err := jsonNoEscape(g.Hooks)
	if err != nil {
		return nil, err
	}
	out["hooks"] = hb
	return jsonNoEscape(out)
}

type commandEntry struct {
	Type    string
	Command string
	extra   map[string]json.RawMessage
}

func (c *commandEntry) UnmarshalJSON(b []byte) error {
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if v, ok := raw["type"]; ok {
		if err := json.Unmarshal(v, &c.Type); err != nil {
			return err
		}
		delete(raw, "type")
	}
	if v, ok := raw["command"]; ok {
		if err := json.Unmarshal(v, &c.Command); err != nil {
			return err
		}
		delete(raw, "command")
	}
	c.extra = raw
	return nil
}

func (c commandEntry) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(c.extra)+2)
	for k, v := range c.extra {
		out[k] = v
	}
	tb, err := jsonNoEscape(c.Type)
	if err != nil {
		return nil, err
	}
	out["type"] = tb
	cb, err := jsonNoEscape(c.Command)
	if err != nil {
		return nil, err
	}
	out["command"] = cb
	return jsonNoEscape(out)
}

// jsonNoEscape marshals compact JSON without escaping HTML metacharacters.
// Nested hook marshalers use it so shell commands remain readable.
func jsonNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// marshalHooksIndent renders readable hooks.json output.
func marshalHooksIndent(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// codexHomeDir returns $CODEX_HOME or ~/.codex.
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

// codexRepoDir returns <repoRoot>/.codex.
func codexRepoDir(repoRoot string) (string, error) {
	if repoRoot == "" {
		return "", errors.New("codex hooks require a repository path")
	}
	return filepath.Join(repoRoot, codexRepoDirName), nil
}

// InstallHooks merges repo-local hooks and enables Codex's global hook gate.
// Existing hooks are preserved. If repository publication fails after the
// gate changes, the installer attempts to restore the prior global config.
func (p *Provider) InstallHooks(ctx context.Context, repoRoot string, binaryPath string) (int, error) {
	bin := binaryPath
	if bin == "" {
		bin = "semantica"
	}

	repoDir, err := codexRepoDir(repoRoot)
	if err != nil {
		return 0, err
	}
	repoHooksPath := filepath.Join(repoDir, hooksFileName)

	commands := commandsForBinary(bin)

	// Validate the project config before changing global state.
	merged, err := mergeHooksFile(repoHooksPath, commands)
	if err != nil {
		return 0, err
	}

	// Serialize global config changes across Semantica installs.
	home, err := codexHomeDir()
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return 0, fmt.Errorf("create %s: %w", home, err)
	}
	unlock, err := lockCodexConfig(ctx, home)
	if err != nil {
		return 0, err
	}
	defer unlock()

	// Snapshot the global config for rollback.
	globalConfigPath := filepath.Join(home, configFileName)
	priorGlobal, priorGlobalErr := os.ReadFile(globalConfigPath)
	if priorGlobalErr != nil && !errors.Is(priorGlobalErr, os.ErrNotExist) {
		return 0, fmt.Errorf("read %s: %w", globalConfigPath, priorGlobalErr)
	}
	priorMode := os.FileMode(0o600)
	if priorGlobalErr == nil {
		fi, statErr := os.Stat(globalConfigPath)
		if statErr != nil {
			return 0, fmt.Errorf("stat %s: %w", globalConfigPath, statErr)
		}
		priorMode = fi.Mode().Perm()
	}

	changed, written, err := ensureGlobalHooksFeature(home)
	if err != nil {
		return 0, err
	}

	// Publish repo hooks last so global changes can be rolled back on failure.
	if err := publishRepoHooks(repoDir, repoHooksPath, merged); err != nil {
		var rbErr error
		if changed {
			rbErr = restoreGlobalConfig(globalConfigPath, priorGlobal, priorGlobalErr, written, priorMode)
		}
		return 0, errors.Join(err, rbErr)
	}

	return len(hookEvents), nil
}

// publishRepoHooks is a test seam for publication failures.
var publishRepoHooks = func(repoDir, repoHooksPath string, merged hookFileShape) error {
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", repoDir, err)
	}
	out, err := marshalHooksIndent(merged)
	if err != nil {
		return fmt.Errorf("marshal hooks.json: %w", err)
	}
	return writeFileAtomic(repoHooksPath, append(out, '\n'), 0o644)
}

// codexLockPollInterval bounds lock cancellation latency.
const codexLockPollInterval = 25 * time.Millisecond

// lockCodexConfig acquires the global config lock while honoring ctx.
func lockCodexConfig(ctx context.Context, home string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(home, installLockName), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open codex install lock: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		ok, err := platform.TryLockFile(f)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("lock codex config: %w", err)
		}
		if ok {
			break
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(codexLockPollInterval):
		}
	}
	// Catch cancellation that races with lock acquisition.
	if err := ctx.Err(); err != nil {
		_ = platform.UnlockFile(f)
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = platform.UnlockFile(f)
		_ = f.Close()
	}, nil
}

// ensureGlobalHooksFeature enables [features] hooks while the config is locked.
// It returns the persisted bytes when the file changes.
func ensureGlobalHooksFeature(home string) (changed bool, written []byte, err error) {
	path := filepath.Join(home, configFileName)
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, nil, fmt.Errorf("read %s: %w", path, err)
	}
	doc, err := readConfigTOML(data)
	if err != nil {
		return false, nil, err
	}
	features, _ := doc["features"].(map[string]any)
	if features == nil {
		features = make(map[string]any)
		doc["features"] = features
	}
	if v, ok := features["hooks"].(bool); ok && v {
		return false, nil, nil
	}
	features["hooks"] = true
	out, err := writeConfigTOML(doc)
	if err != nil {
		return false, nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, nil, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := writeFileAtomic(path, out, 0o600); err != nil {
		return false, nil, err
	}
	return true, out, nil
}

// restoreGlobalConfig rolls back only when the file still matches expected.
// Writers that ignore the config lock may race this check.
func restoreGlobalConfig(path string, prior []byte, priorErr error, expected []byte, priorMode os.FileMode) error {
	current, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s for rollback: %w", path, err)
	}
	if !bytes.Equal(current, expected) {
		return nil
	}
	if errors.Is(priorErr, os.ErrNotExist) {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("rollback remove %s: %w", path, err)
		}
		return nil
	}
	if err := writeFileAtomic(path, prior, priorMode); err != nil {
		return fmt.Errorf("rollback restore %s: %w", path, err)
	}
	return nil
}

// UninstallHooks removes Semantica entries from the project hook file.
// Global hooks, the feature flag, trust state, and unrelated entries remain.
func (p *Provider) UninstallHooks(ctx context.Context, repoRoot string) error {
	repoDir, err := codexRepoDir(repoRoot)
	if err != nil {
		return err
	}
	repoHooksPath := filepath.Join(repoDir, hooksFileName)
	if err := pruneHooksFile(repoHooksPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// AreHooksInstalled reports whether the project file contains Semantica hooks.
func (p *Provider) AreHooksInstalled(ctx context.Context, repoRoot string) bool {
	repoDir, err := codexRepoDir(repoRoot)
	if err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(repoDir, hooksFileName))
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

// HookBinary returns the binary used by an installed Semantica hook.
func (p *Provider) HookBinary(ctx context.Context, repoRoot string) (string, error) {
	repoDir, err := codexRepoDir(repoRoot)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(repoDir, hooksFileName))
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

// commandsForBinary returns commands in hookEvents order.
func commandsForBinary(bin string) []string {
	out := make([]string, len(hookEvents))
	for i, ev := range hookEvents {
		out[i] = hooks.GuardedCommand(bin, "capture codex "+ev.captureName)
	}
	return out
}

// mergeHooksFile replaces Semantica entries and preserves all others.
func mergeHooksFile(hooksPath string, commands []string) (hookFileShape, error) {
	shape := hookFileShape{Hooks: make(map[string][]matcherGroup)}
	if data, err := os.ReadFile(hooksPath); err == nil {
		if err := json.Unmarshal(data, &shape); err != nil {
			return shape, fmt.Errorf("parse %s: %w", hooksPath, err)
		}
		if shape.Hooks == nil {
			shape.Hooks = make(map[string][]matcherGroup)
		}
		stripSemanticaEntries(&shape)
	} else if !errors.Is(err, os.ErrNotExist) {
		return shape, fmt.Errorf("read %s: %w", hooksPath, err)
	}

	for i, ev := range hookEvents {
		shape.Hooks[ev.pascalEvent] = append(shape.Hooks[ev.pascalEvent], matcherGroup{
			Matcher: ev.matcher,
			Hooks: []commandEntry{{
				Type:    "command",
				Command: commands[i],
			}},
		})
	}
	return shape, nil
}

// pruneHooksFile removes Semantica entries and deletes an empty file.
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
	out, err := marshalHooksIndent(shape)
	if err != nil {
		return fmt.Errorf("marshal hooks.json: %w", err)
	}
	return writeFileAtomic(hooksPath, append(out, '\n'), 0o644)
}

// stripSemanticaEntries removes owned commands and empty containers.
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

// writeFileAtomic replaces path through a sibling temporary file.
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
