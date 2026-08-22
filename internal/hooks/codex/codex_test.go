package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pelletier/go-toml/v2"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/hooks/builder"
	"github.com/semanticash/cli/internal/platform"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/toolsnap"
)

// jsonQuote returns s as a JSON string literal.
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// withCodexHome redirects CODEX_HOME for a test.
func withCodexHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	return dir
}

// readHooksJSON parses the test hooks file.
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

// realisticConfig covers unrelated values that installation must preserve.
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
	if pre.captureName != "pre-tool-use" {
		t.Errorf("PreToolUse captureName = %q, want pre-tool-use", pre.captureName)
	}
}

func TestInstallHooks_WritesAllHooksWithExpectedShape(t *testing.T) {
	repoRoot, _ := codexRepo(t)
	p := &Provider{}

	n, err := p.InstallHooks(context.Background(), repoRoot, "/usr/local/bin/semantica")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if n != len(hookEvents) {
		t.Fatalf("install reported %d hooks, want %d", n, len(hookEvents))
	}

	shape := readHooksJSON(t, repoHooksDir(repoRoot))
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

// codexRepo returns an isolated repository and CODEX_HOME.
func codexRepo(t *testing.T) (repoRoot, home string) {
	t.Helper()
	return t.TempDir(), withCodexHome(t)
}

// repoHooksDir is the .codex directory inside a repo root.
func repoHooksDir(repoRoot string) string {
	return filepath.Join(repoRoot, codexRepoDirName)
}

// mustReadFile reads a file or fails the test.
func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// hooksFileHasSemantica reports whether path contains a Semantica hook.
func hooksFileHasSemantica(t *testing.T, path string) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var shape hookFileShape
	if err := json.Unmarshal(data, &shape); err != nil {
		t.Fatalf("parse %s: %v", path, err)
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

// seedGlobalSemanticaHook writes a legacy user-global hook.
func seedGlobalSemanticaHook(t *testing.T, home string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          { "type": "command", "command": "semantica capture codex post-tool-use" }
        ]
      }
    ]
  }
}
`
	if err := os.WriteFile(filepath.Join(home, hooksFileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Install enables hooks globally without preapproving project hooks.
func TestInstallHooks_EnablesFeatureWithoutPreStampingTrust(t *testing.T) {
	repoRoot, home := codexRepo(t)
	p := &Provider{}
	if _, err := p.InstallHooks(context.Background(), repoRoot, "/usr/local/bin/semantica"); err != nil {
		t.Fatalf("install: %v", err)
	}

	doc := readConfigDoc(t, home)
	if features, _ := doc["features"].(map[string]any); features["hooks"] != true {
		t.Errorf("[features] hooks not enabled globally: %+v", doc["features"])
	}
	if hooksSection, ok := doc["hooks"].(map[string]any); ok {
		if state, ok := hooksSection["state"].(map[string]any); ok && len(state) > 0 {
			t.Errorf("install pre-stamped trust state, want none: %+v", state)
		}
	}
	if _, err := os.Stat(filepath.Join(repoHooksDir(repoRoot), configFileName)); !os.IsNotExist(err) {
		t.Errorf("repo-local config.toml should not be created; stat err = %v", err)
	}
}

func TestInstallHooks_PreservesExistingGlobalUserConfig(t *testing.T) {
	repoRoot, home := codexRepo(t)
	configPath := filepath.Join(home, configFileName)
	if err := os.WriteFile(configPath, []byte(realisticConfig), 0o600); err != nil {
		t.Fatalf("seed config.toml: %v", err)
	}

	p := &Provider{}
	if _, err := p.InstallHooks(context.Background(), repoRoot, "/usr/local/bin/semantica"); err != nil {
		t.Fatalf("install: %v", err)
	}

	doc := readConfigDoc(t, home)
	if features, _ := doc["features"].(map[string]any); features["hooks"] != true {
		t.Errorf("[features] hooks not enabled after install: %+v", doc["features"])
	}
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
	repoRoot, home := codexRepo(t)
	p := &Provider{}
	repoHooks := filepath.Join(repoHooksDir(repoRoot), hooksFileName)
	globalConfig := filepath.Join(home, configFileName)

	if _, err := p.InstallHooks(context.Background(), repoRoot, ""); err != nil {
		t.Fatalf("install 1: %v", err)
	}
	hooksBefore := mustReadFile(t, repoHooks)
	configBefore := mustReadFile(t, globalConfig)

	if _, err := p.InstallHooks(context.Background(), repoRoot, ""); err != nil {
		t.Fatalf("install 2: %v", err)
	}
	hooksAfter := mustReadFile(t, repoHooks)
	configAfter := mustReadFile(t, globalConfig)

	if hooksBefore != hooksAfter {
		t.Errorf("hooks.json changed across identical re-install\nbefore:\n%s\nafter:\n%s", hooksBefore, hooksAfter)
	}
	if configBefore != configAfter {
		t.Errorf("config.toml changed across identical re-install\nbefore:\n%s\nafter:\n%s", configBefore, configAfter)
	}
}

func TestInstallHooks_PreservesExistingRepoHooks(t *testing.T) {
	repoRoot, _ := codexRepo(t)
	dir := repoHooksDir(repoRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Preserve a tracked project hook.
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
	if err := os.WriteFile(filepath.Join(dir, hooksFileName), []byte(existing), 0o644); err != nil {
		t.Fatalf("seed hooks.json: %v", err)
	}

	p := &Provider{}
	if _, err := p.InstallHooks(context.Background(), repoRoot, ""); err != nil {
		t.Fatalf("install: %v", err)
	}

	shape := readHooksJSON(t, dir)
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

// failRepoPublish replaces project publication with a test failure.
func failRepoPublish(t *testing.T, before func()) {
	t.Helper()
	orig := publishRepoHooks
	t.Cleanup(func() { publishRepoHooks = orig })
	publishRepoHooks = func(_, _ string, _ hookFileShape) error {
		if before != nil {
			before()
		}
		return errors.New("boom: repo publish failed")
	}
}

// A failed project publish removes a newly created global config.
func TestInstallHooks_RollsBackGlobalFeatureOnRepoFailure(t *testing.T) {
	repoRoot, home := codexRepo(t)
	globalConfig := filepath.Join(home, configFileName)
	if _, err := os.Stat(globalConfig); !os.IsNotExist(err) {
		t.Fatalf("expected no pre-existing global config, stat err = %v", err)
	}
	failRepoPublish(t, nil)

	p := &Provider{}
	_, err := p.InstallHooks(context.Background(), repoRoot, "")
	if err == nil {
		t.Fatal("expected install to fail when the repo publish fails")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should carry the publish failure: %v", err)
	}
	if _, err := os.Stat(globalConfig); !os.IsNotExist(err) {
		t.Errorf("global config survived a failed install; want rollback. stat err = %v", err)
	}
}

// Rollback restores the content and mode of an existing config.
func TestInstallHooks_RollbackRestoresPriorConfig(t *testing.T) {
	repoRoot, home := codexRepo(t)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	globalConfig := filepath.Join(home, configFileName)
	// Use a distinct mode so restoration is observable.
	prior := "[features]\nhooks = false\n"
	if err := os.WriteFile(globalConfig, []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}
	const priorMode = os.FileMode(0o640)
	if err := os.Chmod(globalConfig, priorMode); err != nil {
		t.Fatal(err)
	}
	failRepoPublish(t, nil)

	p := &Provider{}
	if _, err := p.InstallHooks(context.Background(), repoRoot, ""); err == nil {
		t.Fatal("expected install to fail")
	}
	if got := mustReadFile(t, globalConfig); got != prior {
		t.Errorf("config not restored to prior bytes:\nwant:\n%s\ngot:\n%s", prior, got)
	}
	fi, err := os.Stat(globalConfig)
	if err != nil {
		t.Fatal(err)
	}
	// Unix permission bits are not meaningful on Windows.
	if runtime.GOOS != "windows" && fi.Mode().Perm() != priorMode {
		t.Errorf("config mode after rollback = %o, want %o", fi.Mode().Perm(), priorMode)
	}
}

// A cancelled install writes neither global nor project config.
func TestInstallHooks_PreCancelledContextInstallsNothing(t *testing.T) {
	repoRoot, home := codexRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := (&Provider{}).InstallHooks(ctx, repoRoot, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, configFileName)); !os.IsNotExist(err) {
		t.Errorf("global config was written despite a cancelled context; stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoHooksDir(repoRoot), hooksFileName)); !os.IsNotExist(err) {
		t.Errorf("repo hooks were written despite a cancelled context; stat err = %v", err)
	}
}

// Rollback preserves a config changed by another writer.
func TestInstallHooks_RollbackSkipsWhenConfigChangedConcurrently(t *testing.T) {
	repoRoot, home := codexRepo(t)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	globalConfig := filepath.Join(home, configFileName)
	external := "[features]\nhooks = true\n\n[unrelated]\nkey = \"user edit\"\n"
	// Replace the config after the feature write but before rollback.
	failRepoPublish(t, func() {
		if err := os.WriteFile(globalConfig, []byte(external), 0o600); err != nil {
			t.Fatal(err)
		}
	})

	p := &Provider{}
	if _, err := p.InstallHooks(context.Background(), repoRoot, ""); err == nil {
		t.Fatal("expected install to fail")
	}
	if got := mustReadFile(t, globalConfig); got != external {
		t.Errorf("rollback clobbered a concurrent external edit:\nwant:\n%s\ngot:\n%s", external, got)
	}
}

// An enabled gate is not rewritten when project publication fails.
func TestInstallHooks_NoRewriteWhenGateAlreadyEnabled(t *testing.T) {
	repoRoot, home := codexRepo(t)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	globalConfig := filepath.Join(home, configFileName)
	prior := "[features]\nhooks = true\n"
	if err := os.WriteFile(globalConfig, []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}
	fi0, err := os.Stat(globalConfig)
	if err != nil {
		t.Fatal(err)
	}
	failRepoPublish(t, nil)

	p := &Provider{}
	if _, err := p.InstallHooks(context.Background(), repoRoot, ""); err == nil {
		t.Fatal("expected install to fail")
	}
	fi1, err := os.Stat(globalConfig)
	if err != nil {
		t.Fatalf("config removed though the gate was unchanged: %v", err)
	}
	if got := mustReadFile(t, globalConfig); got != prior {
		t.Errorf("config content changed: %q", got)
	}
	if !fi1.ModTime().Equal(fi0.ModTime()) {
		t.Errorf("config was rewritten (mtime changed) though the gate was already enabled")
	}
}

// Lock contention honors the install deadline.
func TestInstallHooks_HonorsContextWhileConfigLocked(t *testing.T) {
	repoRoot, home := codexRepo(t)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	// Hold the install lock from a separate open file description.
	lf, err := os.OpenFile(filepath.Join(home, installLockName), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lf.Close() })
	if err := platform.LockFile(lf); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = platform.UnlockFile(lf) })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := (&Provider{}).InstallHooks(ctx, repoRoot, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context deadline error while the lock is held, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("install did not honor the deadline promptly: %v", elapsed)
	}
}

// Project installation leaves legacy global hooks unchanged.
func TestInstallHooks_LeavesGlobalHooksIntact(t *testing.T) {
	repoRoot, home := codexRepo(t)
	seedGlobalSemanticaHook(t, home)
	globalHooksPath := filepath.Join(home, hooksFileName)
	globalBefore := mustReadFile(t, globalHooksPath)

	p := &Provider{}
	if _, err := p.InstallHooks(context.Background(), repoRoot, "/usr/local/bin/semantica"); err != nil {
		t.Fatalf("install: %v", err)
	}

	// Global hooks remain byte-for-byte unchanged.
	if got := mustReadFile(t, globalHooksPath); got != globalBefore {
		t.Errorf("global hooks.json was modified by a repo-local install:\nbefore:\n%s\nafter:\n%s", globalBefore, got)
	}
	// Project hooks and the global feature gate are installed.
	if !hooksFileHasSemantica(t, filepath.Join(repoHooksDir(repoRoot), hooksFileName)) {
		t.Error("repo-local hooks missing after install")
	}
	gdoc := readConfigDoc(t, home)
	if features, _ := gdoc["features"].(map[string]any); features["hooks"] != true {
		t.Errorf("[features] hooks not enabled after install: %+v", gdoc["features"])
	}
}

// Project installation preserves fields on entries it does not own.
func TestInstallHooks_PreservesUnmodeledHookFields(t *testing.T) {
	repoRoot, _ := codexRepo(t)
	dir := repoHooksDir(repoRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Preserve fields Semantica does not model.
	existing := `{
  "description": "Entire hooks for this repo",
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "apply_patch",
        "hooks": [
          {
            "type": "command",
            "command": "/usr/local/bin/entire log",
            "timeout": 30,
            "async": true,
            "statusMessage": "logging",
            "commandWindows": ["entire.exe", "log"]
          }
        ]
      }
    ]
  }
}
`
	hooksPath := filepath.Join(dir, hooksFileName)
	if err := os.WriteFile(hooksPath, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &Provider{}
	if _, err := p.InstallHooks(context.Background(), repoRoot, ""); err != nil {
		t.Fatalf("install: %v", err)
	}

	// Inspect preserved fields without the installer's typed shape.
	var raw map[string]any
	if err := json.Unmarshal([]byte(mustReadFile(t, hooksPath)), &raw); err != nil {
		t.Fatalf("parse merged hooks.json: %v", err)
	}
	if raw["description"] != "Entire hooks for this repo" {
		t.Errorf("top-level description lost: %v", raw["description"])
	}
	groups := raw["hooks"].(map[string]any)["PostToolUse"].([]any)
	var team map[string]any
	for _, g := range groups {
		gm := g.(map[string]any)
		hooksArr := gm["hooks"].([]any)
		h0 := hooksArr[0].(map[string]any)
		if cmd, _ := h0["command"].(string); cmd == "/usr/local/bin/entire log" {
			team = h0
		}
	}
	if team == nil {
		t.Fatalf("team hook not found after merge: %+v", groups)
	}
	if team["timeout"] != float64(30) {
		t.Errorf("timeout lost/changed: %v", team["timeout"])
	}
	if team["async"] != true {
		t.Errorf("async lost/changed: %v", team["async"])
	}
	if team["statusMessage"] != "logging" {
		t.Errorf("statusMessage lost: %v", team["statusMessage"])
	}
	if cw, ok := team["commandWindows"].([]any); !ok || len(cw) != 2 {
		t.Errorf("commandWindows lost: %v", team["commandWindows"])
	}
	// Semantica entries are added alongside the existing hook.
	if !hooksFileHasSemantica(t, hooksPath) {
		t.Error("Semantica hooks missing after merge")
	}
}

// Existing matcher tokens retain null, empty, and string forms.
func TestInstallHooks_PreservesMatcherToken(t *testing.T) {
	for _, tc := range []struct {
		name    string
		matcher string // raw JSON token for the "matcher" field
		want    any    // expected decoded value after the round-trip
	}{
		{"null", "null", nil},
		{"empty", `""`, ""},
		{"string", `"apply_patch"`, "apply_patch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repoRoot, _ := codexRepo(t)
			dir := repoHooksDir(repoRoot)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			existing := `{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": ` + tc.matcher + `,
        "hooks": [
          { "type": "command", "command": "/usr/local/bin/other-tool log" }
        ]
      }
    ]
  }
}
`
			hooksPath := filepath.Join(dir, hooksFileName)
			if err := os.WriteFile(hooksPath, []byte(existing), 0o644); err != nil {
				t.Fatal(err)
			}

			p := &Provider{}
			if _, err := p.InstallHooks(context.Background(), repoRoot, ""); err != nil {
				t.Fatalf("install: %v", err)
			}

			var raw map[string]any
			if err := json.Unmarshal([]byte(mustReadFile(t, hooksPath)), &raw); err != nil {
				t.Fatalf("parse merged hooks.json: %v", err)
			}
			// Find the non-Semantica group.
			var team map[string]any
			for _, g := range raw["hooks"].(map[string]any)["PostToolUse"].([]any) {
				gm := g.(map[string]any)
				h0 := gm["hooks"].([]any)[0].(map[string]any)
				if cmd, _ := h0["command"].(string); cmd == "/usr/local/bin/other-tool log" {
					team = gm
				}
			}
			if team == nil {
				t.Fatal("passthrough group not found after merge")
			}
			got, present := team["matcher"]
			if !present {
				t.Fatalf("matcher field was dropped; want token %s", tc.matcher)
			}
			if got != tc.want {
				t.Errorf("matcher = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestUninstallHooks_RemovesRepoHooksOnly(t *testing.T) {
	// Uninstall removes only project hooks.
	repoRoot, home := codexRepo(t)
	p := &Provider{}

	if _, err := p.InstallHooks(context.Background(), repoRoot, "/opt/special/semantica"); err != nil {
		t.Fatalf("install: %v", err)
	}
	// A legacy global hook may still serve another repository.
	seedGlobalSemanticaHook(t, home)
	globalHooksPath := filepath.Join(home, hooksFileName)
	globalBefore := mustReadFile(t, globalHooksPath)

	if err := p.UninstallHooks(context.Background(), repoRoot); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	// An empty project hook file is deleted.
	if _, err := os.Stat(filepath.Join(repoHooksDir(repoRoot), hooksFileName)); !os.IsNotExist(err) {
		t.Errorf("repo hooks.json should be removed; stat err = %v", err)
	}
	// Global hooks remain byte-for-byte unchanged.
	if got := mustReadFile(t, globalHooksPath); got != globalBefore {
		t.Errorf("uninstall modified the shared global hooks.json:\nbefore:\n%s\nafter:\n%s", globalBefore, got)
	}
}

func TestUninstallHooks_PreservesUnrelatedRepoHooks(t *testing.T) {
	repoRoot, _ := codexRepo(t)
	dir := repoHooksDir(repoRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Preserve a team-owned project hook.
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
	if err := os.WriteFile(filepath.Join(dir, hooksFileName), []byte(existing), 0o644); err != nil {
		t.Fatalf("seed hooks.json: %v", err)
	}

	p := &Provider{}
	if _, err := p.InstallHooks(context.Background(), repoRoot, ""); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := p.UninstallHooks(context.Background(), repoRoot); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	// Other tool's hook must survive.
	shape := readHooksJSON(t, dir)
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

func TestAreHooksInstalled_TrueAfterInstallFalseAfterUninstall(t *testing.T) {
	repoRoot, _ := codexRepo(t)
	p := &Provider{}
	ctx := context.Background()

	if p.AreHooksInstalled(ctx, repoRoot) {
		t.Error("clean state should report no hooks installed")
	}
	if _, err := p.InstallHooks(ctx, repoRoot, ""); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !p.AreHooksInstalled(ctx, repoRoot) {
		t.Error("install should report hooks installed")
	}
	if err := p.UninstallHooks(ctx, repoRoot); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if p.AreHooksInstalled(ctx, repoRoot) {
		t.Error("uninstall should leave no hooks")
	}
}

func TestHookBinary_ReturnsInstalledBinary(t *testing.T) {
	repoRoot, _ := codexRepo(t)
	p := &Provider{}
	ctx := context.Background()

	if _, err := p.InstallHooks(ctx, repoRoot, "/opt/special/semantica"); err != nil {
		t.Fatalf("install: %v", err)
	}
	got, err := p.HookBinary(ctx, repoRoot)
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

	// This repository is intentionally not registered.
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

// mkGitDir creates the marker required by repository discovery.
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

func TestIsAvailable_RespectsCODEXHOME(t *testing.T) {
	// Availability may still resolve the binary from the host PATH.
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "missing"))
	p := &Provider{}
	_ = p.IsAvailable()
}

// New registers the provider under its canonical name.
func TestProvider_RegistersUnderCanonicalName(t *testing.T) {
	r := hooks.NewRegistry(New())
	if r.Get(providerName) == nil {
		t.Fatalf("Codex provider not retrievable from registry under %q", providerName)
	}
}

// Parsing drops the provider turn ID and preserves the tool-use ID.
func TestParseHookEvent_PostToolUseDropsProviderTurnID(t *testing.T) {
	cases := []struct {
		toolName string
		// Direct emission may later split one invocation by file.
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

// Dispatch attaches the active capture-state turn to tool events.
func TestParseAndDispatch_CodexToolStep_InheritsCaptureStateTurnID(t *testing.T) {
	// Isolate capture state from the user's home.
	t.Setenv("SEMANTICA_HOME", t.TempDir())

	// Seed the turn created for the active prompt.
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

	// The provider turn differs from the active capture turn.
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

	// Route through the normal dispatcher path.
	registryPath := filepath.Join(t.TempDir(), "repos.json")
	bh, err := broker.Open(context.Background(), registryPath)
	if err != nil {
		t.Fatalf("open broker: %v", err)
	}
	t.Cleanup(func() { _ = broker.Close(bh) })

	// Downstream code must see the capture-state turn.
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

// Numeric hook IDs retain their decimal representation.
func TestParseHookEvent_NumericIdsParse(t *testing.T) {
	for _, hook := range []string{"pre-tool-use", "post-tool-use"} {
		t.Run(hook, func(t *testing.T) {
			payload := `{"session_id":12345,"turn_id":67,"cwd":"/repo",` +
				`"tool_name":"Bash","tool_use_id":890,"tool_input":{"command":"gofmt -w ."}}`
			ev, err := (&Provider{}).ParseHookEvent(context.Background(), hook, strings.NewReader(payload))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if ev == nil {
				t.Fatal("numeric-id payload dropped, want an event")
			}
			if ev.SessionID != "12345" {
				t.Errorf("SessionID = %q, want %q", ev.SessionID, "12345")
			}
			if ev.ToolUseID != "890" {
				t.Errorf("ToolUseID = %q, want %q", ev.ToolUseID, "890")
			}
		})
	}
}

// Stop responses preserve absent, null, empty, and non-empty values.
func TestParseHookEvent_CodexStopCarriesResponse(t *testing.T) {
	p := &Provider{}
	parse := func(payload string) *hooks.Event {
		t.Helper()
		ev, err := p.ParseHookEvent(context.Background(), "stop", strings.NewReader(payload))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		return ev
	}

	// A non-empty response is present.
	ev := parse(`{"session_id":"s","transcript_path":"/t","last_assistant_message":"the answer"}`)
	if ev == nil || ev.Type != hooks.AgentCompleted {
		t.Fatalf("event = %+v, want AgentCompleted", ev)
	}
	if ev.Response == nil || *ev.Response != "the answer" {
		t.Errorf("Response = %v, want \"the answer\"", ev.Response)
	}

	// An empty string is present but empty.
	if ev := parse(`{"session_id":"s","transcript_path":"/t","last_assistant_message":""}`); ev == nil || ev.Response == nil || *ev.Response != "" {
		t.Errorf(`empty string: Response = %v, want &""`, ev.Response)
	}

	// Absent and null values do not produce a candidate.
	for _, payload := range []string{
		`{"session_id":"s","transcript_path":"/t"}`,
		`{"session_id":"s","transcript_path":"/t","last_assistant_message":null}`,
	} {
		if ev := parse(payload); ev == nil || ev.Response != nil {
			t.Errorf("payload %s: Response = %v, want nil", payload, ev.Response)
		}
	}
}

// Codex hook fields apply different conversion rules based on their role.
func TestParseHookEvent_FieldTolerancePolicies(t *testing.T) {
	// override adds one field to an otherwise valid pre-tool-use payload.
	override := func(fieldJSON string) string {
		return `{"session_id":"s","cwd":"/repo","tool_name":"Bash","tool_use_id":"c1",` +
			`"tool_input":{"command":"x"},` + fieldJSON + `}`
	}
	parse := func(fieldJSON string) (*hooks.Event, error) {
		return (&Provider{}).ParseHookEvent(context.Background(), "pre-tool-use",
			strings.NewReader(override(fieldJSON)))
	}

	// Optional metadata accepts scalars and ignores other values.
	t.Run("metadata_tolerates_all", func(t *testing.T) {
		for _, field := range []string{"model", "source", "last_assistant_message"} {
			for _, tok := range []string{`5`, `true`, `"x"`, `{}`, `[1]`, `null`} {
				ev, err := parse(`"` + field + `":` + tok)
				if err != nil {
					t.Errorf("%s=%s: unexpected error %v", field, tok, err)
				}
				if ev == nil {
					t.Errorf("%s=%s: dropped, want an event", field, tok)
				}
			}
		}
		// Model is the optional metadata field exposed on the event.
		if ev, _ := parse(`"model":7`); ev == nil || ev.Model != "7" {
			t.Errorf("model=7 -> %+v, want Model %q", ev, "7")
		}
		if ev, _ := parse(`"model":{}`); ev == nil || ev.Model != "" {
			t.Errorf("model={} -> %+v, want empty Model", ev)
		}
	})

	// Prompt accepts scalars and rejects composite values.
	t.Run("prompt_coerces_scalar", func(t *testing.T) {
		ev, err := parse(`"prompt":42`)
		if err != nil {
			t.Fatalf("scalar prompt: %v", err)
		}
		if ev.Prompt != "42" {
			t.Errorf("Prompt = %q, want %q", ev.Prompt, "42")
		}
	})
	t.Run("prompt_rejects_composite", func(t *testing.T) {
		for _, tok := range []string{`{}`, `[1,2]`} {
			if ev, err := parse(`"prompt":` + tok); err == nil {
				t.Errorf("prompt=%s: want a parse error (fail closed), got ev=%v", tok, ev)
			}
		}
	})

	// A non-string transcript path is treated as absent.
	t.Run("transcript_path_string_or_empty", func(t *testing.T) {
		if ev, err := parse(`"transcript_path":"/t/x.jsonl"`); err != nil || ev.TranscriptRef != "/t/x.jsonl" {
			t.Errorf(`transcript_path string -> ev=%+v err=%v, want "/t/x.jsonl"`, ev, err)
		}
		for _, tok := range []string{`7`, `true`, `{}`, `[1]`, `null`} {
			ev, err := parse(`"transcript_path":` + tok)
			if err != nil {
				t.Errorf("transcript_path=%s: unexpected error %v", tok, err)
				continue
			}
			if ev == nil || ev.TranscriptRef != "" {
				t.Errorf("transcript_path=%s -> %+v, want empty TranscriptRef", tok, ev)
			}
		}
	})
}

// Missing transcript paths use the session identifier as the source key.
func TestCodexSourceKey_SessionScopedWhenTranscriptEmpty(t *testing.T) {
	mk := func(session string) *hooks.Event {
		return &hooks.Event{
			Type: hooks.ToolStepCompleted, SessionID: session,
			ToolName: "Bash", ToolUseID: "call_1", Timestamp: 1,
		}
	}
	a := builder.ComputeEventID(codexSourceKey(mk("sess-A")), mk("sess-A"))
	b := builder.ComputeEventID(codexSourceKey(mk("sess-B")), mk("sess-B"))
	if a == b {
		t.Errorf("event ids collided across sessions with a reused tool-use id: %s", a)
	}
	// The same session and tool-use identifier remain stable.
	if a2 := builder.ComputeEventID(codexSourceKey(mk("sess-A")), mk("sess-A")); a != a2 {
		t.Errorf("event id not stable within a session: %s vs %s", a, a2)
	}
	// A transcript path takes precedence when present.
	withPath := &hooks.Event{Type: hooks.ToolStepCompleted, SessionID: "s", TranscriptRef: "/t/x.jsonl", ToolUseID: "call_1"}
	if got := codexSourceKey(withPath); got != "/t/x.jsonl" {
		t.Errorf("source key = %q, want the transcript path", got)
	}
}

// Unsupported session identifier values reject the payload.
func TestParseHookEvent_MalformedIdFailsClosed(t *testing.T) {
	for _, tok := range []string{`{}`, `true`, `[1,2]`, `1.5`, `-3`} {
		t.Run(tok, func(t *testing.T) {
			payload := `{"session_id":` + tok + `,"cwd":"/repo","tool_name":"Bash","tool_use_id":"c1","tool_input":{}}`
			ev, err := (&Provider{}).ParseHookEvent(context.Background(), "pre-tool-use", strings.NewReader(payload))
			if err == nil {
				t.Errorf("session_id=%s parsed, want a fail-closed error (ev=%v)", tok, ev)
			}
			if ev != nil {
				t.Errorf("session_id=%s produced an event, want nil", tok)
			}
			// Composite identifiers are named in diagnostics.
			if (tok == `{}` || tok == `[1,2]`) && err != nil && !strings.Contains(err.Error(), "session_id") {
				t.Errorf("composite session_id error did not name the field: %v", err)
			}
		})
	}
}

// Diagnostics name strict fields without blaming the ignored turn_id.
func TestParseHookEvent_HintNamesStrictFieldNotTurnID(t *testing.T) {
	payload := `{"session_id":{},"turn_id":{},"cwd":"/repo","tool_name":"Bash","tool_use_id":"c1","tool_input":{}}`
	_, err := (&Provider{}).ParseHookEvent(context.Background(), "pre-tool-use", strings.NewReader(payload))
	if err == nil {
		t.Fatal("want a parse error from the composite session_id")
	}
	if !strings.Contains(err.Error(), "session_id") {
		t.Errorf("hint did not name the failing session_id: %v", err)
	}
	if strings.Contains(err.Error(), "turn_id") {
		t.Errorf("hint blamed tolerant turn_id: %v", err)
	}
}

// The ignored turn_id accepts any JSON shape.
func TestParseHookEvent_UnusedTurnIDTolerated(t *testing.T) {
	for _, tok := range []string{`{}`, `[1]`, `true`, `5`, `"t"`, `null`} {
		t.Run(tok, func(t *testing.T) {
			payload := `{"session_id":"s","turn_id":` + tok +
				`,"cwd":"/repo","tool_name":"Bash","tool_use_id":"c1","tool_input":{"command":"x"}}`
			ev, err := (&Provider{}).ParseHookEvent(context.Background(), "pre-tool-use", strings.NewReader(payload))
			if err != nil {
				t.Errorf("turn_id=%s: dropped the hook (%v); it is unused and must be tolerated", tok, err)
			}
			if ev == nil {
				t.Errorf("turn_id=%s: no event, want one", tok)
			}
		})
	}
}

// Numeric pre-hook IDs pair with equivalent string post-hook IDs.
func TestParseAndDispatch_CodexNumericPrePairsWithStringPost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	repoPath, semDir, bh := newCodexRepoWorld(t, home)
	t.Cleanup(func() { _ = broker.Close(bh) })
	ctx := context.Background()

	const sessionStr = "55501"
	if err := hooks.SaveCaptureState(&hooks.CaptureState{
		SessionID: sessionStr, Provider: "codex",
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

	// The pre-hook uses numeric IDs.
	pre := `{"session_id":55501,"cwd":` + jsonQuote(repoPath) +
		`,"tool_name":"Bash","tool_use_id":88802,"tool_input":{"command":"make generate"}}`
	dispatch("pre-tool-use", pre)

	if err := os.WriteFile(filepath.Join(repoPath, "gen.txt"), []byte("generated line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The post-hook uses equivalent string IDs.
	post := `{"session_id":"55501","cwd":` + jsonQuote(repoPath) +
		`,"tool_name":"Bash","tool_use_id":"88802","tool_input":{"command":"make generate"},"tool_response":{"output":"ok"}}`
	dispatch("post-tool-use", post)

	deltas := codexDeltasIn(t, semDir)
	if len(deltas) != 1 || deltas[0].Status != "complete" {
		t.Fatalf("deltas = %+v, want one complete (numeric pre must pair with string post)", deltas)
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
}

// Installed commands keep shell metacharacters literal.
func TestInstallHooks_DoesNotHTMLEscapeCommands(t *testing.T) {
	repoRoot, _ := codexRepo(t)
	p := &Provider{}
	if _, err := p.InstallHooks(context.Background(), repoRoot, ""); err != nil {
		t.Fatalf("install: %v", err)
	}
	raw := mustReadFile(t, filepath.Join(repoHooksDir(repoRoot), hooksFileName))
	// Build the escape prefix from bytes to avoid a source-level escape.
	if strings.Contains(raw, string([]byte{'\\', 'u'})) {
		t.Errorf("hooks.json contains a unicode escape; want literal characters:\n%s", raw)
	}
	if !strings.Contains(raw, ">/dev/null 2>&1") {
		t.Errorf("hooks.json missing the literal guarded redirection:\n%s", raw)
	}
}

// Existing repo installations gain PreToolUse on the next enable.
func TestInstallHooks_UpgradeAddsPreToolUseToLegacyInstall(t *testing.T) {
	repoRoot, _ := codexRepo(t)
	dir := repoHooksDir(repoRoot)
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
	if _, err := p.InstallHooks(ctx, repoRoot, "/usr/local/bin/semantica"); err != nil {
		t.Fatalf("legacy install: %v", err)
	}
	if _, ok := readHooksJSON(t, dir).Hooks["PreToolUse"]; ok {
		t.Fatal("legacy install already had PreToolUse")
	}
	hookEvents = orig

	n, err := p.InstallHooks(ctx, repoRoot, "/usr/local/bin/semantica")
	if err != nil {
		t.Fatalf("upgrade install: %v", err)
	}
	if n != len(hookEvents) {
		t.Fatalf("upgrade reported %d hooks, want %d", n, len(hookEvents))
	}
	shape := readHooksJSON(t, dir)
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
	if _, err := p.InstallHooks(ctx, repoRoot, "/usr/local/bin/semantica"); err != nil {
		t.Fatalf("second install: %v", err)
	}
	if got := len(readHooksJSON(t, dir).Hooks["PreToolUse"]); got != 1 {
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

	pre := `{"session_id":"` + sessionID + `","cwd":` + jsonQuote(repoPath) +
		`,"tool_name":"Bash","tool_use_id":"call_window_1","tool_input":{"command":"make generate"}}`
	dispatch("pre-tool-use", pre)

	if err := os.WriteFile(filepath.Join(repoPath, "gen.txt"), []byte("generated line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	post := `{"session_id":"` + sessionID + `","cwd":` + jsonQuote(repoPath) +
		`,"tool_name":"Bash","tool_use_id":"call_window_1","tool_input":{"command":"make generate"},"tool_response":{"output":"ok"}}`
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
	// Real Git and SQLite work can exceed the production capture deadline on
	// slower CI runners.
	t.Cleanup(hooks.SetToolWindowDeadlineForTest(30 * time.Second))
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
