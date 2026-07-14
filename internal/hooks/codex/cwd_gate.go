package codex

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/git"
)

// ShouldCapture decides whether a Codex hook invocation belongs to a
// registered repo.
//
// User-global Codex hooks can fire outside registered repos. Require the
// hook cwd to resolve to an active broker repo before any capture side
// effects run.
func (p *Provider) ShouldCapture(ctx context.Context, payload []byte, activeRepos []broker.RegisteredRepo) (bool, error) {
	cwd := peekCwd(payload)
	if cwd == "" {
		return false, nil
	}

	root, err := git.FindRoot(cwd)
	if err != nil {
		// Cwd is not inside any git repo, or the path could not be
		// resolved. Either way: nothing to attribute, no work to do.
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

// canonicalize cleans and normalizes a path for repo-equality comparison.
// Symlink resolution is already handled by git.FindRoot for the cwd side
// and by the broker's registration code for the registry side; this helper
// just strips trailing separators and handles the macOS /tmp -> /private/tmp
// case the platform itself injects.
func canonicalize(p string) string {
	if p == "" {
		return ""
	}
	clean := filepath.Clean(p)
	clean = strings.TrimRight(clean, string(filepath.Separator))
	return clean
}
