package hooks

import (
	"sort"

	"github.com/semanticash/cli/internal/broker"
)

// defaultToolWindowCandidateCap limits selection when no positive cap is supplied.
var defaultToolWindowCandidateCap = 8

// toolWindowCandidateSet holds ordered observation candidates and cap omissions.
// Omitted repositories have unknown coverage.
type toolWindowCandidateSet struct {
	Repos        []broker.RegisteredRepo
	OmittedByCap int
	TotalActive  int
}

// selectToolWindowCandidates selects unique active repositories without I/O.
// It prioritizes the deepest command-directory match, then the deepest session
// match, followed by remaining repositories in canonical-path order.
//
// The cap applies after ordering. Unmatched directories add no priority.
func selectToolWindowCandidates(repos []broker.RegisteredRepo, cmdCWD, sessionCWD string, cap int) toolWindowCandidateSet {
	// Filter and deduplicate before computing priorities and totals.
	active := make([]broker.RegisteredRepo, 0, len(repos))
	activeSeen := make(map[string]bool, len(repos))
	for _, r := range repos {
		if !r.Active || activeSeen[r.CanonicalPath] {
			continue
		}
		activeSeen[r.CanonicalPath] = true
		active = append(active, r)
	}

	deepest := func(path string) *broker.RegisteredRepo {
		if path == "" {
			return nil
		}
		var best *broker.RegisteredRepo
		bestLen := 0
		for i := range active {
			if broker.PathBelongsToRepo(path, active[i].CanonicalPath) && len(active[i].CanonicalPath) > bestLen {
				best = &active[i]
				bestLen = len(active[i].CanonicalPath)
			}
		}
		return best
	}

	ordered := make([]broker.RegisteredRepo, 0, len(active))
	seen := make(map[string]bool, len(active))
	add := func(r *broker.RegisteredRepo) {
		if r == nil || seen[r.CanonicalPath] {
			return
		}
		seen[r.CanonicalPath] = true
		ordered = append(ordered, *r)
	}

	// Add directory matches before the remaining candidates.
	add(deepest(cmdCWD))
	add(deepest(sessionCWD))

	// Sort a copy to preserve the input order.
	rest := append([]broker.RegisteredRepo(nil), active...)
	sort.Slice(rest, func(i, j int) bool { return rest[i].CanonicalPath < rest[j].CanonicalPath })
	for i := range rest {
		add(&rest[i])
	}

	// Nonpositive limits use the default, with a minimum fallback of one.
	if cap <= 0 {
		cap = defaultToolWindowCandidateCap
	}
	if cap <= 0 {
		cap = 1
	}

	set := toolWindowCandidateSet{TotalActive: len(active)}
	if len(ordered) > cap {
		set.OmittedByCap = len(ordered) - cap
		ordered = ordered[:cap]
	}
	set.Repos = ordered
	return set
}
