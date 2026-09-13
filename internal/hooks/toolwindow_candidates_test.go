package hooks

import (
	"testing"

	"github.com/semanticash/cli/internal/broker"
)

func cand(path string) broker.RegisteredRepo {
	return broker.RegisteredRepo{RepoID: path, Path: path, CanonicalPath: path, Active: true}
}

func inactiveCand(path string) broker.RegisteredRepo {
	r := cand(path)
	r.Active = false
	return r
}

func paths(set toolWindowCandidateSet) []string {
	out := make([]string, len(set.Repos))
	for i, r := range set.Repos {
		out[i] = r.CanonicalPath
	}
	return out
}

func eq(a []string, b ...string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSelectToolWindowCandidates_PriorityThenStableOrder(t *testing.T) {
	repos := []broker.RegisteredRepo{cand("/r/aaa"), cand("/r/cli"), cand("/r/api"), cand("/r/zzz")}

	// Directory matches precede the remaining repositories in canonical order.
	set := selectToolWindowCandidates(repos, "/r/cli/sub", "/r/api", 0)
	if !eq(paths(set), "/r/cli", "/r/api", "/r/aaa", "/r/zzz") {
		t.Fatalf("order = %v", paths(set))
	}
	if set.TotalActive != 4 || set.OmittedByCap != 0 {
		t.Fatalf("set = %+v", set)
	}
}

func TestSelectToolWindowCandidates_DedupWhenCmdEqualsSession(t *testing.T) {
	repos := []broker.RegisteredRepo{cand("/r/api"), cand("/r/cli")}
	set := selectToolWindowCandidates(repos, "/r/api", "/r/api", 0)
	if !eq(paths(set), "/r/api", "/r/cli") {
		t.Fatalf("order = %v (api must appear once)", paths(set))
	}
}

func TestSelectToolWindowCandidates_NoCwdRepoStillYieldsCandidates(t *testing.T) {
	repos := []broker.RegisteredRepo{cand("/r/cli"), cand("/r/api")}
	// An unregistered cwd leaves all candidates in canonical order.
	set := selectToolWindowCandidates(repos, "/tmp/outside", "", 0)
	if !eq(paths(set), "/r/api", "/r/cli") {
		t.Fatalf("order = %v, want stable canonical order", paths(set))
	}
}

func TestSelectToolWindowCandidates_CapKeepsPrioritiesAndReportsOmitted(t *testing.T) {
	repos := []broker.RegisteredRepo{cand("/r/aaa"), cand("/r/bbb"), cand("/r/cli"), cand("/r/ddd")}
	// The command repository keeps priority when the cap excludes other candidates.
	set := selectToolWindowCandidates(repos, "/r/cli", "", 2)
	if !eq(paths(set), "/r/cli", "/r/aaa") {
		t.Fatalf("capped order = %v", paths(set))
	}
	if set.OmittedByCap != 2 {
		t.Fatalf("omitted = %d, want 2", set.OmittedByCap)
	}
	if set.TotalActive != 4 {
		t.Fatalf("total = %d, want 4", set.TotalActive)
	}
}

func TestSelectToolWindowCandidates_ExcludesInactive(t *testing.T) {
	repos := []broker.RegisteredRepo{
		cand("/r/api"),
		inactiveCand("/r/cli"), // must be excluded, even as the command-dir repo
		cand("/r/zzz"),
	}
	set := selectToolWindowCandidates(repos, "/r/cli", "", 0)
	if !eq(paths(set), "/r/api", "/r/zzz") {
		t.Fatalf("order = %v, want inactive /r/cli excluded", paths(set))
	}
	if set.TotalActive != 2 {
		t.Fatalf("TotalActive = %d, want 2 (active only)", set.TotalActive)
	}
}

func TestSelectToolWindowCandidates_NonPositiveCapUsesDefaultBound(t *testing.T) {
	orig := defaultToolWindowCandidateCap
	defaultToolWindowCandidateCap = 2
	t.Cleanup(func() { defaultToolWindowCandidateCap = orig })

	repos := []broker.RegisteredRepo{cand("/r/aaa"), cand("/r/bbb"), cand("/r/ccc"), cand("/r/ddd")}
	// A zero cap uses the configured default.
	set := selectToolWindowCandidates(repos, "", "", 0)
	if len(set.Repos) != 2 {
		t.Fatalf("selected = %d, want 2 (default bound applied to nonpositive cap)", len(set.Repos))
	}
	if set.OmittedByCap != 2 {
		t.Fatalf("omitted = %d, want 2", set.OmittedByCap)
	}
}
