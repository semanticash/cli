package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/semanticash/cli/internal/broker"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
)

// Disabled repos should render as plain "not enabled".
func TestStatusService_DisabledIsPlainNotEnabled(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()
	enableSemantica(t, ctx, dir)

	// Simulate `semantica disable` by removing only the enabled marker.
	if err := os.Remove(filepath.Join(dir, ".semantica", "enabled")); err != nil {
		t.Fatalf("remove enabled marker: %v", err)
	}

	res, err := NewStatusService().Status(ctx, StatusInput{RepoPath: dir})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if res.Enabled {
		t.Errorf("Enabled = true, want false after disable")
	}
	if res.StaleReason != "" {
		t.Errorf("StaleReason = %q, want empty (disable is intentional, not stale)", res.StaleReason)
	}
}

// A repo with lineage.db but no repository row should surface a stale reason.
func TestStatusService_NoRepoRowSurfacesStale(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	// Set up .semantica + enabled marker + migrated DB, but no repository row.
	semDir := filepath.Join(dir, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sqlstore.MigratePath(ctx, filepath.Join(semDir, "lineage.db")); err != nil {
		t.Fatal(err)
	}

	res, err := NewStatusService().Status(ctx, StatusInput{RepoPath: dir})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if res.Enabled {
		t.Errorf("Enabled = true, want false for stale drift")
	}
	if res.StaleReason != string(broker.RepoStaleNoRepoRow) {
		t.Errorf("StaleReason = %q, want %q", res.StaleReason, broker.RepoStaleNoRepoRow)
	}
}
