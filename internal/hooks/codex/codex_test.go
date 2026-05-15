package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
)

// withCodexHome redirects CODEX_HOME to a temporary directory for the
// duration of a test. Returns the directory path; the original env value
// is restored on test cleanup.
func withCodexHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// t.Setenv handles save/restore around the test on its own.
	t.Setenv("CODEX_HOME", dir)
	return dir
}

// readHooksJSON returns the parsed hooks.json file under the test's
// CODEX_HOME. Helper because every install-side test needs it.
func readHooksJSON(t *testing.T, home string) hookFileShape {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, hooksFileName))
	if err != nil {
		t.Fatalf("read hooks.json: %v", err)
	}
	var shape hookFileShape
	if err := json.Unmarshal(data, &shape); err != nil {
		t.Fatalf("parse hooks.json: %v", err)
	}
	return shape
}

// readConfigDoc returns the parsed config.toml file as a generic map.
func readConfigDoc(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, configFileName))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse config.toml: %v", err)
	}
	return doc
}

// realisticConfig mirrors the layout of a typical ~/.codex/config.toml:
// a model pin, several plugin enablements, marketplace declarations, an
// existing project trust entry, and a TUI key. The install must leave
// every one of these intact (value-equivalent; key order and comments
// are not guaranteed).
const realisticConfig = `model = "gpt-5.4"
model_reasoning_effort = "xhigh"

[plugins."github@openai-curated"]
enabled = true

[plugins."browser-use@openai-bundled"]
enabled = true

[marketplaces.openai-bundled]
last_updated = "2026-05-13T15:17:46Z"
source_type = "local"
source = "/tmp/codex-fixture/bundled-marketplaces/openai-bundled"

[projects."/tmp/codex-fixture/example-project"]
trust_level = "trusted"

[tui.model_availability_nux]
"gpt-5.5" = 1
`

func TestInstallHooks_WritesFourHooksWithExpectedShape(t *testing.T) {
	home := withCodexHome(t)
	p := &Provider{}

	n, err := p.InstallHooks(context.Background(), "/anywhere/repo", "/usr/local/bin/semantica")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if n != len(hookEvents) {
		t.Fatalf("install reported %d hooks, want %d", n, len(hookEvents))
	}

	shape := readHooksJSON(t, home)
	for _, ev := range hookEvents {
		groups, ok := shape.Hooks[ev.pascalEvent]
		if !ok {
			t.Errorf("hooks.json missing event %q", ev.pascalEvent)
			continue
		}
		if len(groups) != 1 {
			t.Errorf("event %q has %d matcher groups, want 1", ev.pascalEvent, len(groups))
			continue
		}
		group := groups[0]
		if group.Matcher != ev.matcher {
			t.Errorf("event %q matcher=%q, want %q", ev.pascalEvent, group.Matcher, ev.matcher)
		}
		if len(group.Hooks) != 1 {
			t.Errorf("event %q has %d command entries, want 1", ev.pascalEvent, len(group.Hooks))
			continue
		}
		cmd := group.Hooks[0].Command
		if !strings.Contains(cmd, semanticaMarker) {
			t.Errorf("event %q command missing semantica marker: %q", ev.pascalEvent, cmd)
		}
		if !strings.Contains(cmd, ev.captureName) {
			t.Errorf("event %q command missing capture name %q: %q", ev.pascalEvent, ev.captureName, cmd)
		}
	}
}

func TestInstallHooks_TrustHashesMatchUpstreamFormat(t *testing.T) {
	home := withCodexHome(t)
	p := &Provider{}

	if _, err := p.InstallHooks(context.Background(), "/anywhere", "/usr/local/bin/semantica"); err != nil {
		t.Fatalf("install: %v", err)
	}

	doc := readConfigDoc(t, home)
	hooksSection, _ := doc["hooks"].(map[string]any)
	if hooksSection == nil {
		t.Fatal("config.toml missing [hooks] section after install")
	}
	state, _ := hooksSection["state"].(map[string]any)
	if state == nil {
		t.Fatal("config.toml missing [hooks.state] section after install")
	}

	hooksPath := filepath.Join(home, hooksFileName)
	for _, ev := range hookEvents {
		key := trustKey(hooksPath, ev.snakeEvent, 0, 0)
		entry, ok := state[key].(map[string]any)
		if !ok {
			t.Errorf("missing trust entry for key %q", key)
			continue
		}
		got, _ := entry["trusted_hash"].(string)
		expectedCommand := commandsForBinary("/usr/local/bin/semantica")[indexOfEvent(ev)]
		want := commandHookHash(ev.snakeEvent, ev.matcher, expectedCommand)
		if got != want {
			t.Errorf("trust hash for %q = %q, want %q", ev.snakeEvent, got, want)
		}
		if !strings.HasPrefix(got, "sha256:") {
			t.Errorf("trust hash for %q lacks sha256: prefix: %q", ev.snakeEvent, got)
		}
	}
}

// indexOfEvent finds the position of ev in hookEvents so a test can
// pair the event with the command it produces. Avoids hardcoding the
// numeric indices in two places.
func indexOfEvent(ev codexHookEvent) int {
	for i, e := range hookEvents {
		if e.pascalEvent == ev.pascalEvent {
			return i
		}
	}
	return -1
}

func TestInstallHooks_PreservesExistingUserConfig(t *testing.T) {
	home := withCodexHome(t)
	configPath := filepath.Join(home, configFileName)
	if err := os.WriteFile(configPath, []byte(realisticConfig), 0o600); err != nil {
		t.Fatalf("seed config.toml: %v", err)
	}

	p := &Provider{}
	if _, err := p.InstallHooks(context.Background(), "/anywhere", "/usr/local/bin/semantica"); err != nil {
		t.Fatalf("install: %v", err)
	}

	doc := readConfigDoc(t, home)
	if v, _ := doc["model"].(string); v != "gpt-5.4" {
		t.Errorf("model lost: got %q", v)
	}
	if v, _ := doc["model_reasoning_effort"].(string); v != "xhigh" {
		t.Errorf("reasoning effort lost: got %q", v)
	}
	plugins, _ := doc["plugins"].(map[string]any)
	if plugins["github@openai-curated"] == nil {
		t.Error("plugin entry github@openai-curated lost")
	}
	if plugins["browser-use@openai-bundled"] == nil {
		t.Error("plugin entry browser-use@openai-bundled lost")
	}
	if mps, _ := doc["marketplaces"].(map[string]any); mps["openai-bundled"] == nil {
		t.Error("marketplace entry openai-bundled lost")
	}
	if projs, _ := doc["projects"].(map[string]any); projs["/tmp/codex-fixture/example-project"] == nil {
		t.Error("project trust entry lost")
	}
	if tui, _ := doc["tui"].(map[string]any); tui["model_availability_nux"] == nil {
		t.Error("[tui.model_availability_nux] lost")
	}
}

func TestInstallHooks_IsIdempotent(t *testing.T) {
	home := withCodexHome(t)
	p := &Provider{}

	if _, err := p.InstallHooks(context.Background(), "/anywhere", ""); err != nil {
		t.Fatalf("install 1: %v", err)
	}
	hooksBefore, err := os.ReadFile(filepath.Join(home, hooksFileName))
	if err != nil {
		t.Fatalf("read hooks.json after first install: %v", err)
	}
	configBefore, err := os.ReadFile(filepath.Join(home, configFileName))
	if err != nil {
		t.Fatalf("read config.toml after first install: %v", err)
	}

	if _, err := p.InstallHooks(context.Background(), "/anywhere", ""); err != nil {
		t.Fatalf("install 2: %v", err)
	}
	hooksAfter, err := os.ReadFile(filepath.Join(home, hooksFileName))
	if err != nil {
		t.Fatalf("read hooks.json after second install: %v", err)
	}
	configAfter, err := os.ReadFile(filepath.Join(home, configFileName))
	if err != nil {
		t.Fatalf("read config.toml after second install: %v", err)
	}

	if string(hooksBefore) != string(hooksAfter) {
		t.Errorf("hooks.json changed across identical re-install\nbefore:\n%s\nafter:\n%s", hooksBefore, hooksAfter)
	}
	if string(configBefore) != string(configAfter) {
		t.Errorf("config.toml changed across identical re-install\nbefore:\n%s\nafter:\n%s", configBefore, configAfter)
	}
}

func TestInstallHooks_PreservesUnrelatedHookEntries(t *testing.T) {
	home := withCodexHome(t)
	hooksPath := filepath.Join(home, hooksFileName)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A non-Semantica hook that the user (or another tool) installed.
	existing := `{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "apply_patch",
        "hooks": [
          { "type": "command", "command": "/usr/local/bin/other-tool log" }
        ]
      }
    ]
  }
}
`
	if err := os.WriteFile(hooksPath, []byte(existing), 0o644); err != nil {
		t.Fatalf("seed hooks.json: %v", err)
	}

	p := &Provider{}
	if _, err := p.InstallHooks(context.Background(), "/anywhere", ""); err != nil {
		t.Fatalf("install: %v", err)
	}

	shape := readHooksJSON(t, home)
	postGroups := shape.Hooks["PostToolUse"]

	var foundOther, foundSemantica bool
	for _, g := range postGroups {
		for _, h := range g.Hooks {
			if h.Command == "/usr/local/bin/other-tool log" {
				foundOther = true
			}
			if strings.Contains(h.Command, semanticaMarker) {
				foundSemantica = true
			}
		}
	}
	if !foundOther {
		t.Error("unrelated PostToolUse hook was dropped on install")
	}
	if !foundSemantica {
		t.Error("Semantica PostToolUse hook missing after install")
	}
}

func TestInstallHooks_TrustKeyTracksPositionWhenUserHookExists(t *testing.T) {
	// Trust keys must follow Semantica's actual hook position, not assume
	// group_index=0, hook_index=0. Existing user hooks keep their position,
	// and Semantica receives trust state at the index where it lands.
	home := withCodexHome(t)
	hooksPath := filepath.Join(home, hooksFileName)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	preExisting := `{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "apply_patch",
        "hooks": [
          { "type": "command", "command": "/usr/local/bin/other-tool log" }
        ]
      }
    ]
  }
}
`
	if err := os.WriteFile(hooksPath, []byte(preExisting), 0o644); err != nil {
		t.Fatalf("seed hooks.json: %v", err)
	}

	p := &Provider{}
	if _, err := p.InstallHooks(context.Background(), "/anywhere", "/usr/local/bin/semantica"); err != nil {
		t.Fatalf("install: %v", err)
	}

	// Semantica's PostToolUse entry must land at index 1 because the
	// user's hook still sits at index 0.
	shape := readHooksJSON(t, home)
	postGroups := shape.Hooks["PostToolUse"]
	if len(postGroups) != 2 {
		t.Fatalf("PostToolUse groups = %d, want 2 (user + semantica)", len(postGroups))
	}
	semanticaCmd := postGroups[1].Hooks[0].Command
	if !strings.Contains(semanticaCmd, semanticaMarker) {
		t.Fatalf("Semantica entry not at group 1; got: %+v", postGroups)
	}

	// Trust entry for PostToolUse must be keyed at group 1, hook 0.
	doc := readConfigDoc(t, home)
	state := doc["hooks"].(map[string]any)["state"].(map[string]any)

	wrongKey := trustKey(hooksPath, "post_tool_use", 0, 0)
	if _, exists := state[wrongKey]; exists {
		t.Errorf("trust entry written at user's position (group 0, hook 0); state=%+v", state)
	}
	correctKey := trustKey(hooksPath, "post_tool_use", 1, 0)
	entry, ok := state[correctKey].(map[string]any)
	if !ok {
		t.Fatalf("trust entry missing at Semantica's actual position (group 1, hook 0); state=%+v", state)
	}
	gotHash, _ := entry["trusted_hash"].(string)
	wantHash := commandHookHash("post_tool_use", "apply_patch|Bash|Write|Edit", semanticaCmd)
	if gotHash != wantHash {
		t.Errorf("trust hash at correct position = %q, want %q", gotHash, wantHash)
	}
}

func TestInstallHooks_RemovesStaleTrustEntriesFromShiftedPositions(t *testing.T) {
	// Reinstalling after a hook position shift must clear stale trust
	// state from the old position. Scenario:
	//   1. Install with no user hooks    -> Semantica at PostToolUse (0,0); trust at (0,0)
	//   2. User adds their own PostToolUse hook (bumps Semantica out)
	//   3. Reinstall                     -> Semantica at PostToolUse (1,0); trust at (1,0)
	//   The stale (0,0) entry must not survive step 3.
	home := withCodexHome(t)
	hooksPath := filepath.Join(home, hooksFileName)
	configPath := filepath.Join(home, configFileName)
	p := &Provider{}
	ctx := context.Background()

	if _, err := p.InstallHooks(ctx, "/anywhere", ""); err != nil {
		t.Fatalf("install 1: %v", err)
	}

	// Confirm the first install put PostToolUse trust at (0,0).
	oldKey := trustKey(hooksPath, "post_tool_use", 0, 0)
	doc := readConfigDoc(t, home)
	state := doc["hooks"].(map[string]any)["state"].(map[string]any)
	if _, ok := state[oldKey]; !ok {
		t.Fatalf("first install missing trust entry at (0,0); state=%+v", state)
	}

	// Inject a user hook ahead of Semantica's entry. This is what the
	// user would experience if a separate tool wrote into hooks.json
	// or they hand-edited the file to add their own command.
	shape := readHooksJSON(t, home)
	prepended := append([]matcherGroup{{
		Matcher: "apply_patch",
		Hooks: []commandEntry{{
			Type:    "command",
			Command: "/usr/local/bin/other-tool log",
		}},
	}}, shape.Hooks["PostToolUse"]...)
	shape.Hooks["PostToolUse"] = prepended
	out, err := json.MarshalIndent(shape, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(hooksPath, append(out, '\n'), 0o644); err != nil {
		t.Fatalf("write hooks.json: %v", err)
	}

	if _, err := p.InstallHooks(ctx, "/anywhere", ""); err != nil {
		t.Fatalf("install 2: %v", err)
	}

	// Reread state after the second install. Stale entry at (0,0) must
	// be gone (its hash matched a recognized Semantica command), new
	// entry at (1,0) must exist.
	doc = readConfigDoc(t, home)
	state = doc["hooks"].(map[string]any)["state"].(map[string]any)
	if _, ok := state[oldKey]; ok {
		t.Errorf("stale trust entry at (0,0) survived reinstall; state=%+v", state)
	}
	newKey := trustKey(hooksPath, "post_tool_use", 1, 0)
	if _, ok := state[newKey]; !ok {
		t.Errorf("new trust entry at (1,0) missing after reinstall; state=%+v", state)
	}

	// Also confirm we did not strand a config.toml without [hooks.state].
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config.toml missing after reinstall: %v", err)
	}
}

func TestInstallHooks_RemovesStaleTrustWhenBinaryPathChanges(t *testing.T) {
	// Stale trust cleanup must recognize both previous and current command
	// hashes. This keeps old entries removable when the installed binary
	// path changes between runs.
	home := withCodexHome(t)
	hooksPath := filepath.Join(home, hooksFileName)
	p := &Provider{}
	ctx := context.Background()

	// First install with an absolute binary path. Records trust at
	// PostToolUse position (0,0) with the hash of the absolute-path
	// guarded command.
	if _, err := p.InstallHooks(ctx, "/anywhere", "/opt/special/semantica"); err != nil {
		t.Fatalf("install 1: %v", err)
	}
	oldKey := trustKey(hooksPath, "post_tool_use", 0, 0)

	// User adds their own PostToolUse hook ahead of Semantica's so
	// the next install will shift Semantica to (1,0).
	shape := readHooksJSON(t, home)
	prepended := append([]matcherGroup{{
		Matcher: "apply_patch",
		Hooks: []commandEntry{{
			Type:    "command",
			Command: "/usr/local/bin/other-tool log",
		}},
	}}, shape.Hooks["PostToolUse"]...)
	shape.Hooks["PostToolUse"] = prepended
	out, err := json.MarshalIndent(shape, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(hooksPath, append(out, '\n'), 0o644); err != nil {
		t.Fatalf("write hooks.json: %v", err)
	}

	// Second install with the DEFAULT binary path. The PostToolUse
	// command string changes (different binary inside the guard), so
	// the new install's recognizedHashes alone would not match the
	// trust entry the first install wrote at (0,0).
	if _, err := p.InstallHooks(ctx, "/anywhere", ""); err != nil {
		t.Fatalf("install 2: %v", err)
	}

	doc := readConfigDoc(t, home)
	state := doc["hooks"].(map[string]any)["state"].(map[string]any)
	if _, ok := state[oldKey]; ok {
		t.Errorf("stale trust entry at (0,0) from prior absolute-binary install survived reinstall; state=%+v", state)
	}
	newKey := trustKey(hooksPath, "post_tool_use", 1, 0)
	if _, ok := state[newKey]; !ok {
		t.Errorf("new trust entry at (1,0) missing after reinstall; state=%+v", state)
	}
}

func TestUninstallHooks_RemovesTrustEntryForAbsoluteBinaryInstall(t *testing.T) {
	// Uninstall must remove trust entries for the exact command installed
	// on disk, including installs that used an absolute binary path.
	home := withCodexHome(t)
	p := &Provider{}

	if _, err := p.InstallHooks(context.Background(), "/anywhere", "/opt/special/semantica"); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := p.UninstallHooks(context.Background(), "/anywhere"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	// config.toml must no longer carry the Semantica trust entries.
	data, err := os.ReadFile(filepath.Join(home, configFileName))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	if strings.Contains(string(data), semanticaMarker) {
		t.Errorf("config.toml still references semantica after uninstall:\n%s", data)
	}
	if strings.Contains(string(data), "trusted_hash") {
		t.Errorf("config.toml still has trust hashes after absolute-binary uninstall:\n%s", data)
	}
}

func TestUninstallHooks_RemovesSemanticaContentOnly(t *testing.T) {
	home := withCodexHome(t)
	// Seed an unrelated hook so we can verify it survives.
	existing := `{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "apply_patch",
        "hooks": [
          { "type": "command", "command": "/usr/local/bin/other-tool log" }
        ]
      }
    ]
  }
}
`
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, hooksFileName), []byte(existing), 0o644); err != nil {
		t.Fatalf("seed hooks.json: %v", err)
	}

	p := &Provider{}
	if _, err := p.InstallHooks(context.Background(), "/anywhere", ""); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := p.UninstallHooks(context.Background(), "/anywhere"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	// Other tool's hook must survive.
	shape := readHooksJSON(t, home)
	postGroups := shape.Hooks["PostToolUse"]
	if len(postGroups) != 1 || len(postGroups[0].Hooks) != 1 ||
		postGroups[0].Hooks[0].Command != "/usr/local/bin/other-tool log" {
		t.Errorf("uninstall did not preserve unrelated hook; PostToolUse groups=%+v", postGroups)
	}
	// Semantica events with no remaining entries are pruned.
	if _, ok := shape.Hooks["SessionStart"]; ok {
		t.Error("SessionStart event left behind after uninstall")
	}
	if _, ok := shape.Hooks["Stop"]; ok {
		t.Error("Stop event left behind after uninstall")
	}
}

func TestUninstallHooks_LeavesUnknownTrustEntriesIntact(t *testing.T) {
	home := withCodexHome(t)
	p := &Provider{}

	if _, err := p.InstallHooks(context.Background(), "/anywhere", ""); err != nil {
		t.Fatalf("install: %v", err)
	}

	// Simulate a third-party trust entry under [hooks.state.*] with a
	// command hash unrelated to Semantica. Uninstall must not touch it.
	configPath := filepath.Join(home, configFileName)
	doc := readConfigDoc(t, home)
	state := doc["hooks"].(map[string]any)["state"].(map[string]any)
	state["/tmp/other-tool/hooks.json:post_tool_use:0:0"] = map[string]any{
		"trusted_hash": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
	}
	out, err := toml.Marshal(doc)
	if err != nil {
		t.Fatalf("re-marshal config: %v", err)
	}
	if err := os.WriteFile(configPath, out, 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}

	if err := p.UninstallHooks(context.Background(), "/anywhere"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	doc = readConfigDoc(t, home)
	hooksSection, _ := doc["hooks"].(map[string]any)
	if hooksSection == nil {
		t.Fatal("uninstall stripped the [hooks] table even though a third-party trust entry remained")
	}
	state, _ = hooksSection["state"].(map[string]any)
	if state == nil {
		t.Fatal("uninstall stripped [hooks.state] even though a third-party entry remained")
	}
	if _, ok := state["/tmp/other-tool/hooks.json:post_tool_use:0:0"]; !ok {
		t.Error("third-party trust entry was removed by uninstall")
	}
}

func TestUninstallHooks_LeavesModifiedSemanticaTrustEntry(t *testing.T) {
	home := withCodexHome(t)
	p := &Provider{}

	if _, err := p.InstallHooks(context.Background(), "/anywhere", ""); err != nil {
		t.Fatalf("install: %v", err)
	}

	// Tamper with one trust hash to simulate a manual edit. Uninstall
	// only removes entries whose hash matches what we computed, so the
	// tampered entry must survive.
	configPath := filepath.Join(home, configFileName)
	hooksPath := filepath.Join(home, hooksFileName)
	doc := readConfigDoc(t, home)
	state := doc["hooks"].(map[string]any)["state"].(map[string]any)
	tamperedKey := trustKey(hooksPath, "post_tool_use", 0, 0)
	state[tamperedKey] = map[string]any{
		"trusted_hash": "sha256:deadbeef",
	}
	out, err := toml.Marshal(doc)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if err := os.WriteFile(configPath, out, 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	if err := p.UninstallHooks(context.Background(), "/anywhere"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	doc = readConfigDoc(t, home)
	hooksSection, _ := doc["hooks"].(map[string]any)
	state, _ = hooksSection["state"].(map[string]any)
	entry, ok := state[tamperedKey].(map[string]any)
	if !ok {
		t.Fatalf("tampered entry was removed even though its hash differed; state=%+v", state)
	}
	if entry["trusted_hash"] != "sha256:deadbeef" {
		t.Errorf("tampered entry rewritten: got %v", entry["trusted_hash"])
	}
}

func TestAreHooksInstalled_TrueAfterInstallFalseAfterUninstall(t *testing.T) {
	home := withCodexHome(t)
	_ = home
	p := &Provider{}
	ctx := context.Background()

	if p.AreHooksInstalled(ctx, "/anywhere") {
		t.Error("clean state should report no hooks installed")
	}
	if _, err := p.InstallHooks(ctx, "/anywhere", ""); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !p.AreHooksInstalled(ctx, "/anywhere") {
		t.Error("install should report hooks installed")
	}
	if err := p.UninstallHooks(ctx, "/anywhere"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if p.AreHooksInstalled(ctx, "/anywhere") {
		t.Error("uninstall should leave no hooks")
	}
}

func TestHookBinary_ReturnsInstalledBinary(t *testing.T) {
	withCodexHome(t)
	p := &Provider{}
	ctx := context.Background()

	if _, err := p.InstallHooks(ctx, "/anywhere", "/opt/special/semantica"); err != nil {
		t.Fatalf("install: %v", err)
	}
	got, err := p.HookBinary(ctx, "/anywhere")
	if err != nil {
		t.Fatalf("hook binary: %v", err)
	}
	if got != "/opt/special/semantica" {
		t.Errorf("hook binary = %q, want %q", got, "/opt/special/semantica")
	}
}

func TestShouldCapture_GatesByActiveRepoMembership(t *testing.T) {
	// A git repo to use as the session's cwd.
	repo := t.TempDir()
	mkGitDir(t, repo)

	subdir := filepath.Join(repo, "pkg", "scoring")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}

	// A second repo we never register, to verify off-repo cwds gate out.
	otherRepo := t.TempDir()
	mkGitDir(t, otherRepo)

	canonRepo, _ := filepath.EvalSymlinks(repo)

	cases := []struct {
		name     string
		payload  string
		active   []broker.RegisteredRepo
		expected bool
	}{
		{
			name:     "cwd inside active repo subdir",
			payload:  jsonWithCwd(subdir),
			active:   []broker.RegisteredRepo{{CanonicalPath: canonRepo, Active: true}},
			expected: true,
		},
		{
			name:     "cwd at active repo root",
			payload:  jsonWithCwd(repo),
			active:   []broker.RegisteredRepo{{CanonicalPath: canonRepo, Active: true}},
			expected: true,
		},
		{
			name:     "cwd in a different unregistered repo",
			payload:  jsonWithCwd(otherRepo),
			active:   []broker.RegisteredRepo{{CanonicalPath: canonRepo, Active: true}},
			expected: false,
		},
		{
			name:     "cwd in registered but inactive repo",
			payload:  jsonWithCwd(repo),
			active:   []broker.RegisteredRepo{{CanonicalPath: canonRepo, Active: false}},
			expected: false,
		},
		{
			name:     "cwd outside any git repo",
			payload:  jsonWithCwd(t.TempDir()),
			active:   []broker.RegisteredRepo{{CanonicalPath: canonRepo, Active: true}},
			expected: false,
		},
		{
			name:     "payload without cwd field",
			payload:  `{"session_id":"abc"}`,
			active:   []broker.RegisteredRepo{{CanonicalPath: canonRepo, Active: true}},
			expected: false,
		},
		{
			name:     "empty payload",
			payload:  ``,
			active:   []broker.RegisteredRepo{{CanonicalPath: canonRepo, Active: true}},
			expected: false,
		},
		{
			name:     "malformed payload",
			payload:  `{not json`,
			active:   []broker.RegisteredRepo{{CanonicalPath: canonRepo, Active: true}},
			expected: false,
		},
	}

	p := &Provider{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.ShouldCapture(context.Background(), []byte(tc.payload), tc.active)
			if err != nil {
				t.Fatalf("ShouldCapture err: %v", err)
			}
			if got != tc.expected {
				t.Errorf("ShouldCapture = %v, want %v", got, tc.expected)
			}
		})
	}
}

// mkGitDir creates a minimal .git marker so git.FindRoot recognizes the
// directory as a repo root. We do not need a fully initialized
// repository for the cwd-gate logic, only the on-disk signal.
func mkGitDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
}

// jsonWithCwd builds the minimal payload subset the cwd preflight reads.
func jsonWithCwd(cwd string) string {
	b, _ := json.Marshal(map[string]string{"cwd": cwd})
	return string(b)
}

func TestCommandHookHash_MatchesUpstreamFixture(t *testing.T) {
	// Test vectors captured from a live Codex 0.130.0 install after an
	// in-session /hooks approval. The probe hooks.json lived at
	// /private/tmp/codex-hook-probe/repo/.codex/hooks.json (canonical
	// path), and each hook command logged stdin to a per-event file.
	matcher := "apply_patch|Bash|Write|Edit"
	cases := []struct {
		event   string
		matcher string
		command string
		want    string
	}{
		{
			event:   "session_start",
			matcher: "",
			command: "/tmp/codex-hook-probe/log.sh SessionStart",
			want:    "sha256:535bdcc7eb7968fea940e8aa467cfd2c02d96d088425104334a085e39ce9105c",
		},
		{
			event:   "user_prompt_submit",
			matcher: "",
			command: "/tmp/codex-hook-probe/log.sh UserPromptSubmit",
			want:    "sha256:067417d6c6435d1cf039fb578965de8fc04082dbf0214a9f6cd9cbd88cca73a9",
		},
		{
			event:   "post_tool_use",
			matcher: matcher,
			command: "/tmp/codex-hook-probe/log.sh PostToolUse",
			want:    "sha256:c2b89791a5f1223ee3a2eab54da538f52f2a1d3a89153bf1094d909a2e3ac46b",
		},
		{
			event:   "stop",
			matcher: "",
			command: "/tmp/codex-hook-probe/log.sh Stop",
			want:    "sha256:a314d867cf6d56273f0ab136ab416dbfd45f3962779972ae7061d5e14dd1a1c5",
		},
	}
	for _, tc := range cases {
		t.Run(tc.event, func(t *testing.T) {
			got := commandHookHash(tc.event, tc.matcher, tc.command)
			if got != tc.want {
				t.Errorf("commandHookHash(%q, %q, %q)\n  got  = %s\n  want = %s",
					tc.event, tc.matcher, tc.command, got, tc.want)
			}
		})
	}
}

func TestIsAvailable_RespectsCODEXHOME(t *testing.T) {
	// Force CODEX_HOME to a path that does not exist and confirm
	// IsAvailable falls through to the binary lookup. The
	// ResolveExecutable result depends on the runner's environment so
	// we only assert that the function returns a bool without panic.
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "missing"))
	p := &Provider{}
	_ = p.IsAvailable()
}

// Registry integration (canonical order, ListProviders discovery) lives
// in internal/hooks/registry_test.go; the codex package only needs to
// confirm that its init() side effect is reachable.
func TestProvider_RegistersUnderCanonicalName(t *testing.T) {
	// init() runs at package load via the codex package's own import
	// chain; this test asserts the provider can be retrieved by name
	// without going through the blank-import side-channel.
	if hooks.GetProvider(providerName) == nil {
		t.Fatalf("provider %q not registered after package init", providerName)
	}
}
