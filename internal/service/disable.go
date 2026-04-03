package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/util"

	// Register hook providers via init().
	_ "github.com/semanticash/cli/internal/hooks/claude"
	_ "github.com/semanticash/cli/internal/hooks/copilot"
	_ "github.com/semanticash/cli/internal/hooks/cursor"
	_ "github.com/semanticash/cli/internal/hooks/gemini"
	_ "github.com/semanticash/cli/internal/hooks/kiroide"
	_ "github.com/semanticash/cli/internal/hooks/kirocli"
)

type DisableService struct{}

func NewDisableService() *DisableService { return &DisableService{} }

type DisableResult struct {
	RepoRoot     string `json:"repo_root"`
	SemanticaDir string `json:"semantica_dir"`
}

func (s *DisableService) Disable(ctx context.Context, repoPath string) (*DisableResult, error) {
	repo, err := git.OpenRepo(repoPath)
	if err != nil {
		return nil, err
	}
	repoRoot := repo.Root()

	semDir := filepath.Join(repoRoot, ".semantica")
	if _, err := os.Stat(semDir); err != nil {
		return nil, fmt.Errorf("semantica is not initialized in this repo. Run `semantica enable` first")
	}

	settings, err := util.ReadSettings(semDir)
	if err != nil {
		return nil, fmt.Errorf("read settings: %w", err)
	}
	if !settings.Enabled {
		return &DisableResult{RepoRoot: repoRoot, SemanticaDir: semDir}, nil
	}

	// Deactivate in broker FIRST - this is the gate that stops event
	// routing. capture.go checks the broker registry, not local settings,
	// so a stale broker entry means events keep flowing to a "disabled" repo.
	// If this fails, nothing has changed and the caller can retry cleanly.
	if err := deactivateInBroker(ctx, repoRoot); err != nil {
		return nil, fmt.Errorf("broker deactivation: %w", err)
	}

	// Flip local settings. The repo is already broker-inactive, so even
	// if this fails the repo won't receive new events. The next
	// `semantica disable` will retry the settings write.
	settings.Enabled = false
	settings.Providers = nil
	if err := util.WriteSettings(semDir, settings); err != nil {
		return nil, fmt.Errorf("write settings.json: %w", err)
	}

	// Uninstall repo-local provider hooks for this repo.
	for _, p := range hooks.ListProviders() {
		if err := p.UninstallHooks(ctx, repoRoot); err != nil {
			fmt.Fprintf(os.Stderr, "semantica: warning: uninstall %s hooks: %v\n", p.Name(), err)
		}
	}

	return &DisableResult{RepoRoot: repoRoot, SemanticaDir: semDir}, nil
}

func deactivateInBroker(ctx context.Context, repoRoot string) error {
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
	return broker.Deactivate(ctx, bh, canonical)
}
