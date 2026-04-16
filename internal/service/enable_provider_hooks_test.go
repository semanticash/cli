package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnable_InstallsProviderHooksWithManagedCommand(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	svc, err := NewEnableService(EnableServiceOptions{RepoPath: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enable(ctx, EnableOptions{Providers: []string{"claude-code"}}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatal(err)
	}

	content := string(data)
	if !strings.Contains(content, "\"command\": \"semantica capture claude-code stop\"") {
		t.Fatalf("expected Claude hooks to use bare semantica command, got: %s", content)
	}
	if strings.Contains(content, "/usr/local/bin/semantica") || strings.Contains(content, "/opt/homebrew/bin/semantica") {
		t.Fatalf("expected no absolute semantica path in provider hooks, got: %s", content)
	}
}
