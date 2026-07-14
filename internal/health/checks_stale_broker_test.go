package health

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/broker"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

func insertRepositoryRow(t *testing.T, ctx context.Context, dbPath, rootPath string) {
	t.Helper()
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: uuid.NewString(),
		RootPath:     rootPath,
		CreatedAt:    1000,
		EnabledAt:    1000,
	}); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
}

// A registry entry whose lineage.db lacks a repository row should
// surface with a specific remediation.
func TestCheckStaleBrokerEntries_WarnsOnNoRepoRow(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)

	repo := t.TempDir()
	semDir := filepath.Join(repo, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sqlstore.MigratePath(ctx, filepath.Join(semDir, "lineage.db")); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	reg := struct {
		Repos []broker.RegisteredRepo `json:"repos"`
	}{
		Repos: []broker.RegisteredRepo{{
			RepoID:        "no-repo-row",
			Path:          repo,
			CanonicalPath: repo,
			EnabledAt:     time.Now().UnixMilli(),
			Active:        true,
		}},
	}
	data, _ := json.Marshal(reg)
	if err := os.WriteFile(filepath.Join(home, "repos.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	checks := checkStaleBrokerEntries(ctx)
	if len(checks) != 1 {
		t.Fatalf("got %d checks, want 1: %+v", len(checks), checks)
	}
	c := checks[0]
	if c.Status != StatusWarn {
		t.Errorf("Status = %v, want Warn", c.Status)
	}
	if !strings.Contains(c.Message, "no local repository row") {
		t.Errorf("Message = %q, want to contain 'no local repository row'", c.Message)
	}
	if !strings.Contains(c.Remediation, "semantica tidy --apply") {
		t.Errorf("Remediation = %q, want to reference `semantica tidy --apply`", c.Remediation)
	}
	if !strings.Contains(c.Remediation, "semantica enable") {
		t.Errorf("Remediation = %q, want to reference `semantica enable`", c.Remediation)
	}
}

// Disabled repos leave inactive broker entries. Doctor should ignore
// them because hooks will not route events there.
func TestCheckStaleBrokerEntries_SkipsInactiveEntries(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)

	// This would be sem-dir-missing if the entry were active.
	staleRepo := filepath.Join(t.TempDir(), "gone-repo")

	// Register then deactivate to mirror `semantica disable`.
	regPath := filepath.Join(home, "repos.json")
	bh, err := broker.Open(ctx, regPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := broker.Register(ctx, bh, staleRepo, staleRepo); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := broker.Deactivate(ctx, bh, staleRepo); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	checks := checkStaleBrokerEntries(ctx)
	if len(checks) != 0 {
		t.Errorf("got %d checks for an inactive entry, want 0: %+v", len(checks), checks)
	}
}

// A healthy registry entry must not surface a warn.
func TestCheckStaleBrokerEntries_QuietWhenHealthy(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)

	// Mimic the enable flow so CheckRepoState returns OK.
	repo := t.TempDir()
	semDir := filepath.Join(repo, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sqlstore.MigratePath(ctx, filepath.Join(semDir, "lineage.db")); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	insertRepositoryRow(t, ctx, filepath.Join(semDir, "lineage.db"), repo)

	reg := struct {
		Repos []broker.RegisteredRepo `json:"repos"`
	}{
		Repos: []broker.RegisteredRepo{{
			RepoID:        "healthy",
			Path:          repo,
			CanonicalPath: repo,
			EnabledAt:     time.Now().UnixMilli(),
			Active:        true,
		}},
	}
	data, _ := json.Marshal(reg)
	if err := os.WriteFile(filepath.Join(home, "repos.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	checks := checkStaleBrokerEntries(ctx)
	if len(checks) != 0 {
		t.Errorf("got %d checks for a healthy entry, want 0: %+v", len(checks), checks)
	}
}
