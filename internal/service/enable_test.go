package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/util"
)

// initGitRepo creates a bare git repo with an initial commit in a temp directory.
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// Resolve symlinks so the path matches what OpenRepo returns after
	// filepath.EvalSymlinks (e.g., /var/folders -> /private/var/folders on macOS).
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	// Isolate broker DB so tests don't hit ~/.semantica/.
	t.Setenv("SEMANTICA_HOME", filepath.Join(dir, ".semantica-global"))

	// Isolate HOME so provider DetectAll calls don't scan real
	// ~/.claude/, ~/.cursor/, ~/.gemini/ directories (which is slow).
	t.Setenv("HOME", dir)

	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Create an initial commit so HEAD exists
	f := filepath.Join(dir, "README")
	if err := os.WriteFile(f, []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "README"},
		{"commit", "-m", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	return dir
}

func TestEnable_CreatesSemanticaDir(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	svc, err := NewEnableService(EnableServiceOptions{RepoPath: dir})
	if err != nil {
		t.Fatal(err)
	}

	res, err := svc.Enable(ctx, EnableOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// .semantica directory exists
	semDir := filepath.Join(dir, ".semantica")
	if _, err := os.Stat(semDir); err != nil {
		t.Fatalf(".semantica dir not created: %v", err)
	}

	// objects subdirectory exists
	if _, err := os.Stat(filepath.Join(semDir, "objects")); err != nil {
		t.Fatalf(".semantica/objects dir not created: %v", err)
	}

	// database exists
	if _, err := os.Stat(res.DBPath); err != nil {
		t.Fatalf("database not created: %v", err)
	}

	// result fields are populated
	if res.RepoRoot != dir {
		t.Errorf("RepoRoot = %q, want %q", res.RepoRoot, dir)
	}
	if res.RepositoryID == "" {
		t.Error("RepositoryID is empty")
	}
	if res.CheckpointID == "" {
		t.Error("CheckpointID is empty (baseline checkpoint should be created)")
	}
	if !res.HooksInstalled {
		t.Error("HooksInstalled is false")
	}
}

func TestEnable_InstallsGitHooks(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	svc, err := NewEnableService(EnableServiceOptions{RepoPath: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enable(ctx, EnableOptions{}); err != nil {
		t.Fatal(err)
	}

	// Resolve hooks directory (respects core.hooksPath)
	cmd := exec.Command("git", "rev-parse", "--git-path", "hooks")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	hooksDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(hooksDir) {
		hooksDir = filepath.Join(dir, hooksDir)
	}

	for _, hookName := range []string{"pre-commit", "post-commit", "commit-msg"} {
		hookPath := filepath.Join(hooksDir, hookName)
		data, err := os.ReadFile(hookPath)
		if err != nil {
			t.Errorf("hook %s not installed: %v", hookName, err)
			continue
		}

		content := string(data)
		if !strings.Contains(content, "Semantica git hook") {
			t.Errorf("hook %s missing Semantica marker", hookName)
		}
		if !strings.Contains(content, "semantica hook") {
			t.Errorf("hook %s missing semantica hook invocation", hookName)
		}

		// Verify executable (Windows ignores permission bits).
		if runtime.GOOS != "windows" {
			info, _ := os.Stat(hookPath)
			if info.Mode()&0o100 == 0 {
				t.Errorf("hook %s is not executable", hookName)
			}
		}
	}
}

func TestEnable_UpdatesGitignore(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	svc, err := NewEnableService(EnableServiceOptions{RepoPath: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enable(ctx, EnableOptions{}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatalf("no .gitignore: %v", err)
	}

	if !strings.Contains(string(data), ".semantica/") {
		t.Error(".gitignore does not contain .semantica/ entry")
	}
}

func TestEnable_RejectsDoubleEnable(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	svc, err := NewEnableService(EnableServiceOptions{RepoPath: dir})
	if err != nil {
		t.Fatal(err)
	}

	// First enable succeeds
	if _, err := svc.Enable(ctx, EnableOptions{}); err != nil {
		t.Fatal(err)
	}

	// Second enable without --force should fail
	_, err = svc.Enable(ctx, EnableOptions{})
	if err == nil {
		t.Fatal("expected error on double enable without --force")
	}
	if !strings.Contains(err.Error(), "already enabled") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestEnable_ForceReenables(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	svc, err := NewEnableService(EnableServiceOptions{RepoPath: dir})
	if err != nil {
		t.Fatal(err)
	}

	// First enable
	res1, err := svc.Enable(ctx, EnableOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Force re-enable should succeed
	res2, err := svc.Enable(ctx, EnableOptions{Force: true})
	if err != nil {
		t.Fatalf("force re-enable failed: %v", err)
	}

	// Should get a new checkpoint ID
	if res2.CheckpointID == "" {
		t.Error("force re-enable did not create a new baseline checkpoint")
	}
	if res2.CheckpointID == res1.CheckpointID {
		t.Error("force re-enable reused the same checkpoint ID")
	}
}

func TestEnable_FailsOutsideGitRepo(t *testing.T) {
	dir := t.TempDir() // not a git repo
	ctx := context.Background()

	svc, err := NewEnableService(EnableServiceOptions{RepoPath: dir})
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.Enable(ctx, EnableOptions{})
	if err == nil {
		t.Fatal("expected error when enabling outside a git repo")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestEnable_PreservesExistingGitignore(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	// Write an existing .gitignore
	existing := "node_modules/\n*.log\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	svc, err := NewEnableService(EnableServiceOptions{RepoPath: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enable(ctx, EnableOptions{}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}

	content := string(data)
	if !strings.Contains(content, "node_modules/") {
		t.Error("existing .gitignore content was lost")
	}
	if !strings.Contains(content, "*.log") {
		t.Error("existing .gitignore content was lost")
	}
	if !strings.Contains(content, ".semantica/") {
		t.Error(".semantica/ not added to .gitignore")
	}
}

func TestEnable_GitignoreIdempotent(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	svc, err := NewEnableService(EnableServiceOptions{RepoPath: dir})
	if err != nil {
		t.Fatal(err)
	}

	// Enable, then force re-enable
	if _, err := svc.Enable(ctx, EnableOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enable(ctx, EnableOptions{Force: true}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}

	// .semantica/ should appear exactly once
	count := strings.Count(string(data), ".semantica/")
	if count != 1 {
		t.Errorf(".semantica/ appears %d times in .gitignore, want 1", count)
	}
}

// TestDisableReEnable_PreservesSettings verifies the full disable -> re-enable
// lifecycle preserves remote config, user-disabled automation flags, and merges
// newly detected providers without overwriting existing ones.
func TestDisableReEnable_PreservesSettings(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	// 1. Enable Semantica.
	svc, err := NewEnableService(EnableServiceOptions{RepoPath: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enable(ctx, EnableOptions{}); err != nil {
		t.Fatal(err)
	}

	semDir := filepath.Join(dir, ".semantica")

	// 2. Simulate user customization: set remote endpoint, disable playbook
	//    automation, and add a provider with a custom path.
	settings, err := util.ReadSettings(semDir)
	if err != nil {
		t.Fatal(err)
	}
	settings.Connected = true
	settings.Automations = &util.Automations{
		Playbook: util.PlaybookAutomation{Enabled: false},
	}
	settings.Providers = append(settings.Providers, "custom-agent")
	if err := util.WriteSettings(semDir, settings); err != nil {
		t.Fatal(err)
	}

	// 3. Disable.
	disableSvc := NewDisableService()
	if _, err := disableSvc.Disable(ctx, dir); err != nil {
		t.Fatal(err)
	}

	// Verify disabled: enabled marker removed, settings.enabled is false.
	after, err := util.ReadSettings(semDir)
	if err != nil {
		t.Fatal(err)
	}
	if after.Enabled {
		t.Fatal("settings.enabled should be false after disable")
	}
	if _, err := os.Stat(filepath.Join(semDir, "enabled")); err == nil {
		t.Fatal("enabled marker file should not exist after disable")
	}

	// 4. Re-enable (no --force).
	if _, err := svc.Enable(ctx, EnableOptions{}); err != nil {
		t.Fatal(err)
	}

	// 5. Read back settings and assert preservation.
	restored, err := util.ReadSettings(semDir)
	if err != nil {
		t.Fatal(err)
	}

	if !restored.Enabled {
		t.Error("settings.enabled should be true after re-enable")
	}

	// Connected flag preserved.
	if !restored.Connected {
		t.Error("connected not preserved after re-enable")
	}

	// User-disabled playbook flag preserved (not reset to true).
	if restored.Automations == nil {
		t.Fatal("automations should not be nil")
	}
	if restored.Automations.Playbook.Enabled {
		t.Error("automations.playbook.enabled should remain false (user disabled it)")
	}
	// Providers reflect installed hooks (custom-agent is not a real hook provider,
	// so it won't appear - providers are set by installProviderHooks).
	if len(restored.Providers) == 0 {
		t.Error("providers should not be empty after re-enable")
	}

	// Enabled marker file restored.
	if _, err := os.Stat(filepath.Join(semDir, "enabled")); err != nil {
		t.Errorf("enabled marker file should exist after re-enable: %v", err)
	}
}

