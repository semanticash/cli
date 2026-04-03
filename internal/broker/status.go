package broker

import (
	"context"
	"log/slog"
	"os"
)

// RepoInfo holds registration info for a single repository.
type RepoInfo struct {
	Path          string `json:"path"`
	CanonicalPath string `json:"canonical_path"`
	EnabledAt     int64  `json:"enabled_at"`
	Active        bool   `json:"active"`
}

// StatusResult holds the output of GetStatus.
type StatusResult struct {
	Repos       []RepoInfo `json:"repos"`
	ActiveCount int        `json:"active_count"`
}

// GetStatus collects broker-level diagnostics from the default registry.
// Returns empty status (not an error) if the registry does not exist yet.
func GetStatus(ctx context.Context) (*StatusResult, error) {
	registryPath, err := DefaultRegistryPath()
	if err != nil {
		return nil, err
	}
	return getStatusFromPath(ctx, registryPath)
}

func getStatusFromPath(ctx context.Context, registryPath string) (*StatusResult, error) {
	if _, err := os.Stat(registryPath); os.IsNotExist(err) {
		return &StatusResult{}, nil
	}

	h, err := Open(ctx, registryPath)
	if err != nil {
		return nil, err
	}

	// Clean up stale entries (e.g. manually deleted .semantica dirs).
	if err := Prune(ctx, h); err != nil {
		slog.Warn("broker: prune registry failed", "err", err)
	}

	// Re-read after prune since mutate reloads from disk.
	h, err = Open(ctx, registryPath)
	if err != nil {
		return nil, err
	}

	result := &StatusResult{}
	for _, r := range h.registry.Repos {
		ri := RepoInfo{
			Path:          r.Path,
			CanonicalPath: r.CanonicalPath,
			EnabledAt:     r.EnabledAt,
			Active:        r.Active,
		}
		if ri.Active {
			result.ActiveCount++
		}
		result.Repos = append(result.Repos, ri)
	}
	return result, nil
}
