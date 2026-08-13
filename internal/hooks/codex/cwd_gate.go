package codex

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/git"
)

// ShouldCapture reports whether the hook belongs to an active repository.
func (p *Provider) ShouldCapture(ctx context.Context, payload []byte, activeRepos []broker.RegisteredRepo) (bool, error) {
	cwd := peekCwd(payload)
	if cwd == "" {
		return false, nil
	}

	root, err := git.FindRoot(cwd)
	if err != nil {
		return false, nil
	}

	canonical := canonicalize(root)
	for _, r := range activeRepos {
		if !r.Active {
			continue
		}
		if canonicalize(r.CanonicalPath) == canonical {
			return true, nil
		}
	}
	return false, nil
}

// canonicalize normalizes a path for repository comparison.
func canonicalize(p string) string {
	if p == "" {
		return ""
	}
	clean := filepath.Clean(p)
	clean = strings.TrimRight(clean, string(filepath.Separator))
	return clean
}
