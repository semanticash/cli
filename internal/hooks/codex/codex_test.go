package codex

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pelletier/go-toml/v2"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/toolsnap"
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

// PreToolUse is installed for Bash only.
func TestHookEvents_PreToolUseIsBashOnly(t *testing.T) {
	var pre *codexHookEvent
	for i := range hookEvents {
		if hookEvents[i].pascalEvent == "PreToolUse" {
			pre = &hookEvents[i]
		}
	}
	if pre == nil {
		t.Fatal("hookEvents missing PreToolUse")
	}
	if pre.matcher != "Bash" {
		t.Errorf("PreToolUse matcher = %q, want Bash", pre.matcher)
	}
	if pre.snakeEvent != "pre_tool_use" || pre.captureName != "pre-tool-use" {
		t.Errorf("PreToolUse names = %q/%q, want pre_tool_use/pre-tool-use", pre.snakeEvent, pre.captureName)
	}
}

func TestInstallHooks_WritesAllHooksWithExpectedShape(t *testing.T) {
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

// Codex's New() constructor is the explicit-injection entry point
// used by providers.NewHookRegistry(); a Registry built around it
// must surface the provider by its canonical name. This test
// guards the constructor and its Name() pairing for explicit
// registry composition.
func TestProvider_RegistersUnderCanonicalName(t *testing.T) {
	r := hooks.NewRegistry(New())
	if r.Get(providerName) == nil {
		t.Fatalf("Codex provider not retrievable from registry under %q", providerName)
	}
}

// Codex post-tool-use payloads include a provider turn_id, but Semantica
// packages provenance by the capture-state turn created for the prompt.
// ParseHookEvent leaves Event.TurnID empty so lifecycle can attach that
// active turn before direct emission.
//
// Two tests together lock the fix:
//
//   - TestParseHookEvent_PostToolUseDropsProviderTurnID covers the parse
//     layer: payload turn_id never reaches hooks.Event.TurnID.
//   - TestParseAndDispatch_CodexToolStep_InheritsCaptureStateTurnID
//     covers the end-to-end chain: parse drops it, Dispatch fills from
//     capture state, the event downstream code sees carries the prompt
//     turn id.
//
// The test also asserts that supplied ToolUseID values round-trip for
// all capturable tools. Missing ToolUseID behavior is intentionally
// not covered here; dropping or synthesizing ids would be a separate
// behavior choice.
func TestParseHookEvent_PostToolUseDropsProviderTurnID(t *testing.T) {
	cases := []struct {
		toolName string
		// The direct emitter may split apply_patch into per-file events
		// later. This test only checks that the supplied invocation id
		// survives parsing.
		toolUseID string
	}{
		{"apply_patch", "call_apply_patch_1"},
		{"Bash", "call_bash_1"},
		{"Write", "call_write_1"},
		{"Edit", "call_edit_1"},
	}
	p := &Provider{}
	for _, tc := range cases {
		t.Run(tc.toolName, func(t *testing.T) {
			payload := map[string]any{
				"session_id":  "sess-codex-1",
				"turn_id":     "provider-turn",
				"tool_name":   tc.toolName,
				"tool_use_id": tc.toolUseID,
				"tool_input":  json.RawMessage(`{}`),
			}
			data, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal payload: %v", err)
			}
			event, err := p.ParseHookEvent(context.Background(), "post-tool-use", strings.NewReader(string(data)))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if event == nil {
				t.Fatalf("event was nil for tool %q (expected ToolStepCompleted)", tc.toolName)
			}
			if event.Type != hooks.ToolStepCompleted {
				t.Errorf("type: got %v, want ToolStepCompleted", event.Type)
			}
			if event.TurnID != "" {
				t.Errorf("TurnID: got %q, want empty (provider turn id must be dropped)", event.TurnID)
			}
			if event.ToolUseID != tc.toolUseID {
				t.Errorf("ToolUseID: got %q, want %q (supplied id must round-trip)", event.ToolUseID, tc.toolUseID)
			}
			if event.ToolName != tc.toolName {
				t.Errorf("ToolName: got %q, want %q", event.ToolName, tc.toolName)
			}
		})
	}
}

// ParseHookEvent plus Dispatch should attach the active capture-state
// turn to Codex tool events. That is the turn packaging uses for the
// prompt manifest.
func TestParseAndDispatch_CodexToolStep_InheritsCaptureStateTurnID(t *testing.T) {
	// Isolate SEMANTICA_HOME so SaveCaptureState writes into a tempdir.
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	// Seed capture state with a Semantica-generated turn id, mimicking
	// what lifecycle.go does on PromptSubmitted (turnID := uuid.NewString()).
	const sessionID = "sess-codex-dispatch"
	const captureTurnID = "semantica-prompt-turn-1"
	if err := hooks.SaveCaptureState(&hooks.CaptureState{
		SessionID:        sessionID,
		Provider:         "codex",
		TranscriptRef:    "",
		TranscriptOffset: 0,
		Timestamp:        1,
		TurnID:           captureTurnID,
	}); err != nil {
		t.Fatalf("save capture state: %v", err)
	}

	// Parse a post-tool-use payload with a different provider turn_id.
	// Dispatch should replace the empty parsed TurnID with the active
	// capture-state turn.
	const providerTurnID = "codex-provider-turn"
	payload := map[string]any{
		"session_id":  sessionID,
		"turn_id":     providerTurnID,
		"tool_name":   "Bash",
		"tool_use_id": "call_dispatch_1",
		"tool_input":  json.RawMessage(`{"command":"echo hello"}`),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	p := &Provider{}
	event, err := p.ParseHookEvent(context.Background(), "post-tool-use", strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event == nil {
		t.Fatal("parse returned nil for post-tool-use Bash")
	}
	if event.TurnID != "" {
		t.Fatalf("parsed event TurnID = %q, want empty (parse layer must drop provider turn id)", event.TurnID)
	}

	// Open a broker handle so Dispatch can run through the normal
	// routing path. The assertion is on the inherited turn id.
	registryPath := filepath.Join(t.TempDir(), "repos.json")
	bh, err := broker.Open(context.Background(), registryPath)
	if err != nil {
		t.Fatalf("open broker: %v", err)
	}
	t.Cleanup(func() { _ = broker.Close(bh) })

	// Dispatch mutates the event before routing so downstream code sees
	// the capture-state turn id.
	if err := hooks.Dispatch(context.Background(), p, event, bh, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if event.TurnID != captureTurnID {
		t.Errorf("event TurnID after Dispatch = %q, want %q (capture-state turn id)", event.TurnID, captureTurnID)
	}
	if event.TurnID == providerTurnID {
		t.Errorf("event TurnID = provider turn id %q; fix did not take effect", providerTurnID)
	}
}

// Bash PreToolUse carries the identity required for window pairing.
func TestParseHookEvent_PreToolUseBashMapsToToolStepStarted(t *testing.T) {
	payload := map[string]any{
		"session_id":  "sess-pre-1",
		"turn_id":     "provider-turn",
		"cwd":         "/repo/work",
		"tool_name":   "Bash",
		"tool_use_id": "call_pre_bash_1",
		"tool_input":  json.RawMessage(`{"command":"gofmt -w ."}`),
	}
	data, _ := json.Marshal(payload)
	event, err := (&Provider{}).ParseHookEvent(context.Background(), "pre-tool-use", strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if event == nil {
		t.Fatal("pre-tool-use Bash parsed to nil")
	}
	if event.Type != hooks.ToolStepStarted {
		t.Errorf("type = %v, want ToolStepStarted", event.Type)
	}
	if event.SessionID != "sess-pre-1" || event.ToolUseID != "call_pre_bash_1" ||
		event.ToolName != "Bash" || event.CWD != "/repo/work" {
		t.Errorf("identity fields lost: %+v", event)
	}
	if !strings.Contains(string(event.ToolInput), "gofmt -w .") {
		t.Errorf("tool_input command lost: %s", event.ToolInput)
	}
	if len(event.ToolResponse) != 0 {
		t.Errorf("pre half carried a tool_response: %s", event.ToolResponse)
	}
	if event.TurnID != "" {
		t.Errorf("TurnID = %q, want empty (provider turn dropped)", event.TurnID)
	}
}

// Non-Bash pre-tool hooks do not open snapshot windows.
func TestParseHookEvent_PreToolUseNonBashIgnored(t *testing.T) {
	for _, tool := range []string{"apply_patch", "Write", "Edit", "read_file", ""} {
		t.Run("tool="+tool, func(t *testing.T) {
			payload := map[string]any{
				"session_id": "s", "tool_name": tool,
				"tool_use_id": "c1", "tool_input": json.RawMessage(`{}`),
			}
			data, _ := json.Marshal(payload)
			event, err := (&Provider{}).ParseHookEvent(context.Background(), "pre-tool-use", strings.NewReader(string(data)))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if event != nil {
				t.Fatalf("pre-tool-use %q produced an event, want nil (Bash-only)", tool)
			}
		})
	}
}

// Pre and post hooks inherit one capture-state turn.
func TestParseAndDispatch_CodexPrePostSharePairingIdentity(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	const sessionID = "sess-pair"
	const captureTurnID = "semantica-turn-1"
	if err := hooks.SaveCaptureState(&hooks.CaptureState{
		SessionID: sessionID, Provider: "codex", TurnID: captureTurnID, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	bh, err := broker.Open(context.Background(), filepath.Join(t.TempDir(), "repos.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close(bh) })

	dispatch := func(hookName string) *hooks.Event {
		payload := map[string]any{
			"session_id": sessionID, "turn_id": "codex-provider-turn",
			"tool_name": "Bash", "tool_use_id": "call_pair_1",
			"tool_input": json.RawMessage(`{"command":"echo hi"}`),
		}
		data, _ := json.Marshal(payload)
		ev, err := (&Provider{}).ParseHookEvent(context.Background(), hookName, strings.NewReader(string(data)))
		if err != nil || ev == nil {
			t.Fatalf("parse %s: ev=%v err=%v", hookName, ev, err)
		}
		if err := hooks.Dispatch(context.Background(), &Provider{}, ev, bh, nil); err != nil {
			t.Fatalf("dispatch %s: %v", hookName, err)
		}
		return ev
	}

	pre := dispatch("pre-tool-use")
	post := dispatch("post-tool-use")

	if pre.Type != hooks.ToolStepStarted || post.Type != hooks.ToolStepCompleted {
		t.Fatalf("types = %v/%v", pre.Type, post.Type)
	}
	if pre.SessionID != post.SessionID || pre.ToolUseID != post.ToolUseID ||
		pre.TurnID != post.TurnID || pre.ToolName != post.ToolName {
		t.Fatalf("pre/post identity diverged: pre=%+v post=%+v", pre, post)
	}
	if pre.ToolUseID != "call_pair_1" {
		t.Errorf("ToolUseID = %q, want the shared tool_use_id", pre.ToolUseID)
	}
	// Both hooks use the capture-state turn.
	if pre.TurnID != captureTurnID || post.TurnID != captureTurnID {
		t.Errorf("turns = %q/%q, want the capture-state turn %q", pre.TurnID, post.TurnID, captureTurnID)
	}
}

// Existing installations gain PreToolUse on the next enable.
func TestInstallHooks_UpgradeAddsPreToolUseToLegacyInstall(t *testing.T) {
	home := withCodexHome(t)
	p := &Provider{}
	ctx := context.Background()

	orig := hookEvents
	// Restore shared test state on every exit.
	t.Cleanup(func() { hookEvents = orig })
	legacy := make([]codexHookEvent, 0, len(orig))
	for _, e := range orig {
		if e.pascalEvent != "PreToolUse" {
			legacy = append(legacy, e)
		}
	}
	hookEvents = legacy
	if _, err := p.InstallHooks(ctx, "/anywhere", "/usr/local/bin/semantica"); err != nil {
		t.Fatalf("legacy install: %v", err)
	}
	if _, ok := readHooksJSON(t, home).Hooks["PreToolUse"]; ok {
		t.Fatal("legacy install already had PreToolUse")
	}
	hookEvents = orig

	n, err := p.InstallHooks(ctx, "/anywhere", "/usr/local/bin/semantica")
	if err != nil {
		t.Fatalf("upgrade install: %v", err)
	}
	if n != len(hookEvents) {
		t.Fatalf("upgrade reported %d hooks, want %d", n, len(hookEvents))
	}
	shape := readHooksJSON(t, home)
	if len(shape.Hooks) != len(legacy)+1 {
		t.Errorf("upgrade left %d event groups, want %d (legacy + PreToolUse)", len(shape.Hooks), len(legacy)+1)
	}
	pre, ok := shape.Hooks["PreToolUse"]
	if !ok || len(pre) != 1 || pre[0].Matcher != "Bash" {
		t.Fatalf("PreToolUse not added on upgrade: %+v", shape.Hooks["PreToolUse"])
	}
	if !strings.Contains(pre[0].Hooks[0].Command, "pre-tool-use") {
		t.Errorf("PreToolUse command missing capture name: %q", pre[0].Hooks[0].Command)
	}
	for _, ev := range legacy {
		if _, ok := shape.Hooks[ev.pascalEvent]; !ok {
			t.Errorf("upgrade dropped existing event %q", ev.pascalEvent)
		}
	}
	if _, err := p.InstallHooks(ctx, "/anywhere", "/usr/local/bin/semantica"); err != nil {
		t.Fatalf("second install: %v", err)
	}
	if got := len(readHooksJSON(t, home).Hooks["PreToolUse"]); got != 1 {
		t.Errorf("PreToolUse groups after re-install = %d, want 1", got)
	}
}

// Parsed Codex Bash hooks produce a complete linked delta.
func TestParseAndDispatch_CodexBashWindowProducesCompleteDelta(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	repoPath, semDir, bh := newCodexRepoWorld(t, home)
	t.Cleanup(func() { _ = broker.Close(bh) })
	ctx := context.Background()

	const sessionID = "sess-window"
	if err := hooks.SaveCaptureState(&hooks.CaptureState{
		SessionID: sessionID, Provider: "codex",
		TurnID: "turn-1", CWD: repoPath, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}

	objDir, err := broker.GlobalObjectsDir()
	if err != nil {
		t.Fatal(err)
	}
	blobStore, err := blobs.NewStore(objDir)
	if err != nil {
		t.Fatal(err)
	}

	p := &Provider{}
	dispatch := func(hookName, payload string) {
		ev, err := p.ParseHookEvent(ctx, hookName, strings.NewReader(payload))
		if err != nil || ev == nil {
			t.Fatalf("parse %s: ev=%v err=%v", hookName, ev, err)
		}
		if err := hooks.Dispatch(ctx, p, ev, bh, blobStore); err != nil {
			t.Fatalf("dispatch %s: %v", hookName, err)
		}
	}

	pre := `{"session_id":"` + sessionID + `","cwd":"` + repoPath +
		`","tool_name":"Bash","tool_use_id":"call_window_1","tool_input":{"command":"make generate"}}`
	dispatch("pre-tool-use", pre)

	if err := os.WriteFile(filepath.Join(repoPath, "gen.txt"), []byte("generated line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	post := `{"session_id":"` + sessionID + `","cwd":"` + repoPath +
		`","tool_name":"Bash","tool_use_id":"call_window_1","tool_input":{"command":"make generate"},"tool_response":{"output":"ok"}}`
	dispatch("post-tool-use", post)

	deltas := codexDeltasIn(t, semDir)
	if len(deltas) != 1 || deltas[0].Status != "complete" {
		t.Fatalf("deltas = %+v, want one complete", deltas)
	}
	if len(deltas[0].Actors) != 1 || deltas[0].Actors[0].Provider != "codex" {
		t.Fatalf("actors = %+v, want codex", deltas[0].Actors)
	}
	found := false
	for _, f := range deltas[0].Files {
		if f.Path == "gen.txt" && f.Operation == "create" &&
			len(f.Hunks) == 1 && f.Hunks[0].NewLines[0] == "generated line" {
			found = true
		}
	}
	if !found {
		t.Fatalf("files = %+v, want gen.txt creation", deltas[0].Files)
	}
	links := codexLinksIn(t, semDir)
	if len(links) != 1 || links[0].kind != "tool_delta" ||
		links[0].groupID == "" || strings.Contains(links[0].groupID, ":") {
		t.Fatalf("links = %+v, want one complete (unprefixed) tool_delta link", links)
	}
	if links[0].eventID == "" || links[0].hash == "" {
		t.Fatalf("link missing event/hash: %+v", links[0])
	}
}

// newCodexRepoWorld creates and registers an enabled test repository.
func newCodexRepoWorld(t *testing.T, home string) (repoPath, semDir string, bh *broker.Handle) {
	t.Helper()
	ctx := context.Background()

	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repoPath = filepath.Join(base, "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repoPath, "init", "-q", "-b", "main")
	gitRun(t, repoPath, "config", "user.email", "t@example.com")
	gitRun(t, repoPath, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repoPath, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repoPath, "add", ".")
	gitRun(t, repoPath, "commit", "-q", "-m", "init")

	semDir = filepath.Join(repoPath, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(semDir, "lineage.db")
	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: uuid.NewString(), RootPath: repoPath, CreatedAt: 1000, EnabledAt: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := sqlstore.Close(h); err != nil {
		t.Fatal(err)
	}

	bh, err = broker.Open(ctx, filepath.Join(home, "repos.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Register(ctx, bh, repoPath, repoPath); err != nil {
		t.Fatal(err)
	}
	return repoPath, semDir, bh
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// codexDeltasIn scans the repo CAS for canonical tool deltas.
func codexDeltasIn(t *testing.T, semDir string) []*toolsnap.Delta {
	t.Helper()
	objects := filepath.Join(semDir, "objects")
	bs, err := blobs.NewStore(objects)
	if err != nil {
		t.Fatal(err)
	}
	var deltas []*toolsnap.Delta
	_ = filepath.WalkDir(objects, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		raw, err := bs.Get(context.Background(), filepath.Base(path))
		if err != nil {
			return nil
		}
		if delta, err := toolsnap.ParseDelta(raw); err == nil {
			deltas = append(deltas, delta)
		}
		return nil
	})
	return deltas
}

type codexLinkRow struct{ eventID, kind, hash, groupID string }

func codexLinksIn(t *testing.T, semDir string) []codexLinkRow {
	t.Helper()
	ctx := context.Background()
	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	rows, err := h.DB.QueryContext(ctx,
		"SELECT event_id, evidence_kind, evidence_hash, group_id FROM agent_event_evidence_links ORDER BY event_id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []codexLinkRow
	for rows.Next() {
		var r codexLinkRow
		if err := rows.Scan(&r.eventID, &r.kind, &r.hash, &r.groupID); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
