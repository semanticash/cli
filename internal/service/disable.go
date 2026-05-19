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
)

type DisableService struct {
	registry *hooks.Registry
}

// NewDisableService constructs the disable-service with the given
// hook registry. The registry drives which providers get their
// repo-local hooks uninstalled during teardown. Production
// callers must pass providers.NewHookRegistry() (the disable
// cobra command does so); a nil registry causes the uninstall
// loop to be a no-op, leaving repo-local hook files in place.
// Treat nil as test-only.
func NewDisableService(registry *hooks.Registry) *DisableService {
	return &DisableService{registry: registry}
}

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
	for _, p := range s.registry.List() {
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
