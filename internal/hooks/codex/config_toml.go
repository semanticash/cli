package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// canonicalize a parsed JSON value by recursively sorting every object's
// keys. The result, when re-serialized, matches the byte stream Codex
// hashes for its trust fingerprint.
func canonicalJSON(v any) []byte {
	var b []byte
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b = append(b, '{')
		for i, k := range keys {
			if i > 0 {
				b = append(b, ',')
			}
			kb, _ := json.Marshal(k)
			b = append(b, kb...)
			b = append(b, ':')
			b = append(b, canonicalJSON(t[k])...)
		}
		b = append(b, '}')
	case []any:
		b = append(b, '[')
		for i, item := range t {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, canonicalJSON(item)...)
		}
		b = append(b, ']')
	default:
		out, _ := json.Marshal(t)
		b = append(b, out...)
	}
	return b
}

// commandHookHash returns Codex's content-addressed trust fingerprint for
// a single command-style hook. The algorithm mirrors version_for_toml +
// command_hook_hash in the upstream codex-rs source: assemble a
// NormalizedHookIdentity with flattened matcher/hooks fields, serialize
// via TOML semantics (None Option fields are omitted), canonicalize the
// resulting JSON (sorted object keys), and SHA-256 the bytes.
//
// matcher is the optional event matcher string; pass an empty value when
// the hook group has no matcher. command is the shell command Codex will
// invoke. timeoutSec mirrors the upstream default (600 when unset).
func commandHookHash(event, matcher, command string) string {
	identity := map[string]any{
		"event_name": event,
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": command,
				"timeout": uint64(hookTimeoutSec),
				"async":   false,
			},
		},
	}
	if matcher != "" {
		identity["matcher"] = matcher
	}
	sum := sha256.Sum256(canonicalJSON(identity))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// hookTimeoutSec is the default Codex applies when a hook entry omits
// the `timeout` field. Trust fingerprints are computed against the
// normalized value, so keeping this aligned with upstream guarantees our
// pre-stamped trust entries stay valid.
const hookTimeoutSec = 600

// configMutation describes one round of edits to config.toml.
//
// applyToTOML drives install: it guarantees [features] hooks = true,
// upserts trustHashes at the requested keys, and clears any
// pre-existing entries under hooksFilePath whose stored hash is in
// recognizedHashes but whose key is not in the new trustHashes (i.e.
// Semantica entries left at outdated positions by prior installs).
//
// removeFromTOML drives uninstall: it removes any entries under
// hooksFilePath whose hash is in recognizedHashes, regardless of
// position. [features] is left alone because the flag may be set for
// reasons unrelated to Semantica.
//
// Recognition is hash-based rather than key-based so position shifts
// (a user prepending a hook bumps Semantica from index 0 to index 1)
// do not leave orphaned trust state. User-modified entries (where the
// stored hash diverges from canonical) survive both paths.
type configMutation struct {
	// trustHashes maps each Semantica trust-state key (the literal
	// "<hooks-file>:<event>:<group>:<hook>" string Codex stores) to the
	// hash of the matching command. Install upserts each entry;
	// uninstall ignores this field on the upsert side and uses it only
	// as a hint of what hashes a fresh install would have produced.
	trustHashes map[string]string

	// hooksFilePath is the absolute path of the hooks.json this
	// mutation targets. The cleanup phase only inspects trust entries
	// whose key begins with this path so entries from other tools that
	// happen to share the [hooks.state] namespace are never touched.
	hooksFilePath string

	// recognizedHashes is the set of trust hash values produced by
	// commands the caller treats as Semantica's. Pre-existing entries
	// under hooksFilePath whose stored trusted_hash matches one of
	// these are eligible for removal during the cleanup phase. Install
	// computes this from the canonical hashes of the four events;
	// uninstall computes it from the actual command strings observed
	// in hooks.json before pruning.
	recognizedHashes map[string]struct{}
}

// applyToTOML merges the mutation into the parsed config map in place.
// Returns true when anything changed (so callers can skip writing back
// an unchanged file). Existing user values under [model],
// [plugins.*], [marketplaces.*], [projects.*], and arbitrary other
// tables survive the round-trip.
//
// Preservation is at the value level only: keys and their values are
// retained, but the underlying TOML library decodes into a generic map
// and re-emits with its own formatter, so comments and original key
// ordering are not retained. Users who hand-format config.toml with
// explanatory comments will lose those comments on first install. Tests
// assert value preservation; the file may otherwise be reformatted.
func (m configMutation) applyToTOML(doc map[string]any) bool {
	changed := false

	// Ensure [features] hooks = true.
	features, _ := doc["features"].(map[string]any)
	if features == nil {
		features = make(map[string]any)
		doc["features"] = features
		changed = true
	}
	if v, ok := features["hooks"].(bool); !ok || !v {
		features["hooks"] = true
		changed = true
	}

	// Clear pre-existing Semantica trust entries whose key is not part
	// of the new install (typically because hook positions shifted
	// between installs). Hash-based recognition keeps user-modified
	// or third-party entries intact, since their stored hash will not
	// be in recognizedHashes.
	if removed := m.removeRecognizedExcept(doc, m.trustHashes); removed {
		changed = true
	}

	// Upsert the new trust entries.
	if len(m.trustHashes) == 0 {
		return changed
	}
	state := ensureTrustState(doc)
	for key, hash := range m.trustHashes {
		existing, _ := state[key].(map[string]any)
		if existing == nil {
			existing = make(map[string]any)
			state[key] = existing
			changed = true
		}
		if existing["trusted_hash"] != hash {
			existing["trusted_hash"] = hash
			changed = true
		}
	}
	return changed
}

// existingTrustState returns the [hooks.state] table when present, or
// nil when the document does not yet contain one. Read-only: never
// materializes intermediate tables that would dirty the document.
func existingTrustState(doc map[string]any) map[string]any {
	hooksSection, _ := doc["hooks"].(map[string]any)
	if hooksSection == nil {
		return nil
	}
	state, _ := hooksSection["state"].(map[string]any)
	return state
}

// ensureTrustState returns the [hooks.state] table, creating it (and
// the [hooks] parent) when missing so callers can insert into it.
func ensureTrustState(doc map[string]any) map[string]any {
	hooksSection, _ := doc["hooks"].(map[string]any)
	if hooksSection == nil {
		hooksSection = make(map[string]any)
		doc["hooks"] = hooksSection
	}
	state, _ := hooksSection["state"].(map[string]any)
	if state == nil {
		state = make(map[string]any)
		hooksSection["state"] = state
	}
	return state
}

// removeFromTOML removes every Semantica-recognized trust entry under
// hooksFilePath. Recognition is hash-based: an entry is removed when
// its stored trusted_hash appears in recognizedHashes. Entries whose
// hash diverges (user-modified, or written by another tool) are left
// untouched.
//
// [features] hooks = true is left in place. The flag may be enabled
// across the system for reasons unrelated to Semantica; flipping it
// off on uninstall would surprise other tools that depend on it.
func (m configMutation) removeFromTOML(doc map[string]any) bool {
	return m.removeRecognizedExcept(doc, nil)
}

// removeRecognizedExcept removes trust entries under hooksFilePath
// whose stored hash is in recognizedHashes and whose key is not in
// keep. Returns true if any entry (or surrounding empty table) was
// removed. Used by both install (to clear stale positions while
// preserving the keys it is about to upsert) and uninstall (with a
// nil keep map, removing everything we recognize).
func (m configMutation) removeRecognizedExcept(doc map[string]any, keep map[string]string) bool {
	if m.hooksFilePath == "" || len(m.recognizedHashes) == 0 {
		return false
	}
	state := existingTrustState(doc)
	if state == nil {
		return false
	}
	prefix := m.hooksFilePath + ":"
	changed := false
	for key, raw := range state {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if _, kept := keep[key]; kept {
			continue
		}
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		hash, _ := entry["trusted_hash"].(string)
		if _, recognized := m.recognizedHashes[hash]; !recognized {
			continue
		}
		delete(state, key)
		changed = true
	}
	if changed {
		pruneEmptyHookState(doc)
	}
	return changed
}

// pruneEmptyHookState removes empty [hooks.state] and [hooks] tables
// after a deletion pass so the rewritten file does not carry empty
// table headers the user did not author.
func pruneEmptyHookState(doc map[string]any) {
	hooksSection, _ := doc["hooks"].(map[string]any)
	if hooksSection == nil {
		return
	}
	if state, ok := hooksSection["state"].(map[string]any); ok && len(state) == 0 {
		delete(hooksSection, "state")
	}
	if len(hooksSection) == 0 {
		delete(doc, "hooks")
	}
}

// readConfigTOML loads a config.toml into a generic map. Returns an empty
// map (no error) when the file is missing - callers create one on first
// install. Returns an error only for unreadable or malformed files, since
// silently overwriting user content on a parse failure would be worse
// than refusing to install.
func readConfigTOML(data []byte) (map[string]any, error) {
	if len(data) == 0 {
		return make(map[string]any), nil
	}
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse codex config.toml: %w", err)
	}
	if doc == nil {
		doc = make(map[string]any)
	}
	return doc, nil
}

// writeConfigTOML serializes the document back to bytes. The output is
// stable for identical inputs (pelletier/go-toml/v2 emits tables in a
// deterministic order, so repeated install/uninstall cycles produce
// identical files), but it is NOT byte-equivalent to the user's
// original input: comments and key ordering are not preserved across
// the unmarshal/marshal pair.
func writeConfigTOML(doc map[string]any) ([]byte, error) {
	out, err := toml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshal codex config.toml: %w", err)
	}
	return out, nil
}
