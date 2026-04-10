package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"

	// Register hook providers via init().
	_ "github.com/semanticash/cli/internal/hooks/claude"
	_ "github.com/semanticash/cli/internal/hooks/copilot"
	_ "github.com/semanticash/cli/internal/hooks/cursor"
	_ "github.com/semanticash/cli/internal/hooks/gemini"
	_ "github.com/semanticash/cli/internal/hooks/kirocli"
	_ "github.com/semanticash/cli/internal/hooks/kiroide"
)

type EnableServiceOptions struct {
	RepoPath string
}

type EnableService struct {
	repoPath string
}

func NewEnableService(opts EnableServiceOptions) (*EnableService, error) {
	return &EnableService{repoPath: opts.RepoPath}, nil
}

type EnableOptions struct {
	Force     bool
	Providers []string // selected provider names; nil = install all registered
}

type EnableResult struct {
	RepoRoot           string   `json:"repo_root"`
	SemanticaDir       string   `json:"semantica_dir"`
	DBPath             string   `json:"db_path"`
	RepositoryID       string   `json:"repository_id,omitempty"`
	CheckpointID       string   `json:"checkpoint_id,omitempty"`
	WorkspaceTierTitle string   `json:"workspace_tier_title,omitempty"`
	UpdateAvailable    bool     `json:"update_available,omitempty"`
	LatestVersion      string   `json:"latest_version,omitempty"`
	UpdateDownloadURL  string   `json:"update_download_url,omitempty"`
	HooksInstalled     bool     `json:"hooks_installed"`
	Providers          []string `json:"providers,omitempty"`
}

func (s *EnableService) Enable(ctx context.Context, opts EnableOptions) (*EnableResult, error) {
	repo, err := git.OpenRepo(s.repoPath)
	if err != nil {
		return nil, err
	}

	repoRoot := repo.Root()

	semDir := filepath.Join(repoRoot, ".semantica")
	objectsDir := filepath.Join(semDir, "objects")
	dbPath := filepath.Join(semDir, "lineage.db")

	if _, err := os.Stat(dbPath); err == nil {
		if !opts.Force && util.IsEnabled(semDir) {
			return nil, fmt.Errorf("semantica already enabled in this repo (db exists at %s). Use --force to reinitialize", dbPath)
		}
		if !opts.Force {
			preExisting := snapshotProviderConfigs(repoRoot)
			installed, hookErr := installProviderHooks(ctx, repoRoot, opts.Providers)
			if hookErr != nil {
				return nil, fmt.Errorf("install provider hooks: %w", hookErr)
			}

			if err := ensureProviderGitignore(repoRoot, installed, preExisting); err != nil {
				fmt.Fprintf(os.Stderr, "semantica: warning: failed to update .gitignore for providers: %v\n", err)
			}

			result, localErr := reEnableLocal(ctx, repo, repoRoot, semDir, dbPath, installed)
			if localErr != nil {
				return nil, localErr
			}

			if err := registerInBroker(ctx, repoRoot); err != nil {
				return nil, fmt.Errorf("broker registration: %w", err)
			}

			if err := activateLocal(semDir); err != nil {
				return nil, errors.Join(
					fmt.Errorf("activate: %w", err),
					deactivateInBroker(ctx, repoRoot),
				)
			}

			return result, nil
		}
	}

	// Keep the repo invisible to routing until local state and hook config exist.
	result, localErr := s.initLocalState(ctx, repo, repoRoot, semDir, objectsDir, dbPath)
	if localErr != nil {
		return nil, localErr
	}

	preExisting := snapshotProviderConfigs(repoRoot)
	installed, hookErr := installProviderHooks(ctx, repoRoot, opts.Providers)
	if hookErr != nil {
		return nil, fmt.Errorf("install provider hooks: %w", hookErr)
	}
	settings, err := util.ReadSettings(semDir)
	if err != nil {
		return nil, fmt.Errorf("read settings: %w", err)
	}
	settings.Providers = installed
	if err := util.WriteSettings(semDir, settings); err != nil {
		return nil, fmt.Errorf("write settings: %w", err)
	}
	result.Providers = installed

	if err := ensureProviderGitignore(repoRoot, installed, preExisting); err != nil {
		fmt.Fprintf(os.Stderr, "semantica: warning: failed to update .gitignore for providers: %v\n", err)
	}

	if err := registerInBroker(ctx, repoRoot); err != nil {
		return nil, fmt.Errorf("broker registration: %w", err)
	}

	// Flip Enabled=true only after broker registration succeeds.
	if err := activateLocal(semDir); err != nil {
		return nil, errors.Join(
			fmt.Errorf("activate: %w", err),
			deactivateInBroker(ctx, repoRoot),
		)
	}

	return result, nil
}

// reEnableLocal performs local state updates for a re-enable: reads existing
// settings, merges providers, writes settings, and reinstalls hooks. Enabled
// stays false until activateLocal runs.
func reEnableLocal(ctx context.Context, repo *git.Repo, repoRoot, semDir, dbPath string, installedProviders []string) (*EnableResult, error) {
	settings, err := util.ReadSettings(semDir)
	if err != nil {
		return nil, fmt.Errorf("read settings: %w", err)
	}

	settings.Providers = installedProviders

	if err := util.WriteSettings(semDir, settings); err != nil {
		return nil, fmt.Errorf("write settings.json: %w", err)
	}

	// Reinstall hooks to ensure on-disk scripts are up to date.
	hooksInstalled := true
	for _, hi := range []git.HookInstallOptions{
		{Name: "pre-commit", Subcommand: "pre-commit"},
		{Name: "post-commit", Subcommand: "post-commit"},
		{Name: "commit-msg", Subcommand: "commit-msg", PassArgs: true},
	} {
		if err := repo.InstallSemanticaHook(ctx, hi); err != nil {
			fmt.Fprintf(os.Stderr, "semantica: warning: reinstall %s hook: %v\n", hi.Name, err)
			hooksInstalled = false
		}
	}

	return &EnableResult{
		RepoRoot:       repoRoot,
		SemanticaDir:   semDir,
		DBPath:         dbPath,
		HooksInstalled: hooksInstalled,
		Providers:      installedProviders,
	}, nil
}

// initLocalState sets up the .semantica directory, database, baseline
// checkpoint, gitignore, and hooks while the repo is still absent from routing.
func (s *EnableService) initLocalState(
	ctx context.Context,
	repo *git.Repo,
	repoRoot, semDir, objectsDir, dbPath string,
) (*EnableResult, error) {
	if err := os.MkdirAll(objectsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create .semantica dirs: %w", err)
	}

	// Hooks are installed before activation, so keep the repo disabled here.
	if err := util.WriteSettings(semDir, util.Settings{
		Enabled: false,
		Version: 1,
		Automations: &util.Automations{
			Playbook:              util.PlaybookAutomation{Enabled: true},
			ImplementationSummary: util.ImplementationSummaryAutomation{Enabled: true},
		},
	}); err != nil {
		return nil, fmt.Errorf("write settings.json: %w", err)
	}

	handle, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		return nil, err
	}
	defer func() { _ = sqlstore.Close(handle) }()

	repoID, err := sqlstore.EnsureRepository(ctx, handle.Queries, repoRoot)
	if err != nil {
		return nil, err
	}

	now := time.Now().UnixMilli()
	repoRow, err := handle.Queries.GetRepositoryByRootPath(ctx, repoRoot)
	if err != nil {
		return nil, fmt.Errorf("get repo row: %w", err)
	}
	if repoRow.EnabledAt == 0 {
		if err := handle.Queries.UpdateRepositoryEnabledAt(ctx, sqldb.UpdateRepositoryEnabledAtParams{
			EnabledAt:    now,
			RepositoryID: repoID,
		}); err != nil {
			return nil, fmt.Errorf("set enabled_at: %w", err)
		}
	}

	var checkpointID string
	paths, listErr := repo.ListFilesFromGit(ctx)
	if listErr != nil {
		fmt.Fprintf(os.Stderr, "semantica: warning: list files for baseline: %v\n", listErr)
	} else {
		blobStore, blobErr := blobs.NewStore(objectsDir)
		if blobErr != nil {
			fmt.Fprintf(os.Stderr, "semantica: warning: init blob store for baseline: %v\n", blobErr)
		} else {
			mr, manErr := blobs.BuildManifest(ctx, blobStore, repoRoot, paths, repo.ReadFile, nil)
			if manErr != nil {
				fmt.Fprintf(os.Stderr, "semantica: warning: build baseline manifest: %v\n", manErr)
			} else {
				checkpointID = uuid.NewString()
				if err := handle.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
					CheckpointID: checkpointID,
					RepositoryID: repoID,
					CreatedAt:    now,
					Kind:         string(CheckpointBaseline),
					Trigger:      sqlstore.NullStr("enable"),
					Message:      sqlstore.NullStr("Baseline snapshot at enable time"),
					ManifestHash: sqlstore.NullStr(mr.ManifestHash),
					SizeBytes:    sql.NullInt64{Int64: mr.TotalBytes, Valid: true},
					Status:       "complete",
					CompletedAt:  sql.NullInt64{Int64: now, Valid: true},
				}); err != nil {
					fmt.Fprintf(os.Stderr, "semantica: warning: insert baseline checkpoint: %v\n", err)
					checkpointID = ""
				}
			}
		}
	}

	if err := ensureSemanticaGitignore(repoRoot); err != nil {
		fmt.Fprintf(os.Stderr, "semantica: warning: failed to update .gitignore: %v\n", err)
	}

	if err := repo.InstallSemanticaHook(ctx, git.HookInstallOptions{
		Name:       "pre-commit",
		Subcommand: "pre-commit",
	}); err != nil {
		return nil, fmt.Errorf("install pre-commit hook: %w", err)
	}

	if err := repo.InstallSemanticaHook(ctx, git.HookInstallOptions{
		Name:       "post-commit",
		Subcommand: "post-commit",
	}); err != nil {
		return nil, fmt.Errorf("install post-commit hook: %w", err)
	}

	if err := repo.InstallSemanticaHook(ctx, git.HookInstallOptions{
		Name:       "commit-msg",
		Subcommand: "commit-msg",
		PassArgs:   true,
	}); err != nil {
		return nil, fmt.Errorf("install commit-msg hook: %w", err)
	}

	return &EnableResult{
		RepoRoot:       repoRoot,
		SemanticaDir:   semDir,
		DBPath:         dbPath,
		RepositoryID:   repoID,
		CheckpointID:   checkpointID,
		HooksInstalled: true,
	}, nil
}

func ensureSemanticaGitignore(repoRoot string) error {
	return util.EnsureGitignoreEntries(repoRoot, []string{".semantica/"})
}

// providerGitignorePaths maps hook provider names to repo-local config files.
var providerGitignorePaths = map[string]string{
	"claude-code": ".claude/settings.json",
	"cursor":      ".cursor/hooks.json",
	"gemini":      ".gemini/settings.json",
	"copilot":     ".github/hooks/semantica.json",
	"kiro-ide":    ".kiro/hooks/",
	"kiro-cli":    ".kiro/agents/semantica.json",
}

// snapshotProviderConfigs returns provider config files that already exist.
func snapshotProviderConfigs(repoRoot string) map[string]bool {
	existing := make(map[string]bool)
	for _, rel := range providerGitignorePaths {
		if _, err := os.Stat(filepath.Join(repoRoot, rel)); err == nil {
			existing[rel] = true
		}
	}
	return existing
}

// ensureProviderGitignore ignores only provider config files created by enable.
func ensureProviderGitignore(repoRoot string, installedProviders []string, preExisting map[string]bool) error {
	var entries []string
	for _, name := range installedProviders {
		if rel, ok := providerGitignorePaths[name]; ok && !preExisting[rel] {
			entries = append(entries, rel)
		}
	}
	if len(entries) == 0 {
		return nil
	}
	return util.EnsureGitignoreEntries(repoRoot, entries)
}

// activateLocal flips settings.json to Enabled=true. Called only after broker
// registration succeeds so hooks become live only when the repo is already
// visible in ListActiveRepos.
func activateLocal(semDir string) error {
	settings, err := util.ReadSettings(semDir)
	if err != nil {
		return fmt.Errorf("read settings: %w", err)
	}
	settings.Enabled = true
	return util.WriteSettings(semDir, settings)
}

// registerInBroker registers the repo in the global broker registry.
func registerInBroker(ctx context.Context, repoRoot string) error {
	registryPath, err := broker.DefaultRegistryPath()
	if err != nil {
		return fmt.Errorf("broker registry path: %w", err)
	}

	bh, err := broker.Open(ctx, registryPath)
	if err != nil {
		return fmt.Errorf("open broker: %w", err)
	}
	defer func() { _ = broker.Close(bh) }()

	canonical := broker.CanonicalRepoPath(repoRoot)
	if err := broker.Register(ctx, bh, repoRoot, canonical); err != nil {
		return fmt.Errorf("broker register: %w", err)
	}

	return nil
}

// installProviderHooks installs capture hooks into each selected provider's
// repo-local config and returns the providers that succeeded.
func installProviderHooks(ctx context.Context, repoRoot string, selectedNames []string) ([]string, error) {
	if selectedNames != nil && len(selectedNames) == 0 {
		return nil, nil
	}

	// Use the shared bare command so provider hooks survive install-path changes.
	binPath := hooks.ManagedCommand

	var toInstall []hooks.HookProvider
	if len(selectedNames) > 0 {
		selected := make(map[string]bool, len(selectedNames))
		for _, n := range selectedNames {
			selected[n] = true
		}
		for _, p := range hooks.ListProviders() {
			if selected[p.Name()] {
				toInstall = append(toInstall, p)
			}
		}
	} else {
		toInstall = hooks.ListProviders()
	}

	var installed []string
	var lastErr error
	for _, p := range toInstall {
		if _, err := p.InstallHooks(ctx, repoRoot, binPath); err != nil {
			fmt.Fprintf(os.Stderr, "semantica: warning: install %s hooks: %v\n", p.Name(), err)
			lastErr = err
			continue
		}
		installed = append(installed, p.Name())
	}

	if len(installed) == 0 && len(toInstall) > 0 {
		return nil, fmt.Errorf("no provider hooks could be installed (last error: %w)", lastErr)
	}
	return installed, nil
}
