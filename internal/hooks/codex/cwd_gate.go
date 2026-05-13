package codex

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/git"
)

// ShouldCapture decides whether a Codex hook invocation should run any
// downstream side effects, based on the session's working directory.
//
// User-global hooks at ~/.codex/hooks.json fire for every Codex session on
// the machine, including sessions in repositories the user has not
// registered with Semantica. The capture entrypoint's default broker-wide
// gate ("any active repo?") is too permissive for that surface: a Codex
// session in /tmp would otherwise leak prompts, file edits, and shell
// commands into capture state belonging to an unrelated registered repo.
//
// We require the session's cwd to resolve to a git repository root that
// matches one of the active broker entries by canonical path. If either
// resolution fails - no enclosing repo, no matching canonical entry -
// ShouldCapture returns false and the caller exits before parsing stdin,
// opening the blob store, writing to the broker, or appending to the
// hook-error log.
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
