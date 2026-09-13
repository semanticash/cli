package health

import (
	"testing"

	"github.com/semanticash/cli/internal/util"
)

func TestSummarizeRoutingDecisions_DedupAndBuckets(t *testing.T) {
	entries := []util.RoutingDecisionEntry{
		// The same event has different write outcomes at each destination.
		{EventID: "e1", Signal: "structured_path", Repo: "/repos/api", SessionRepo: "/repos/x", SignalDiff: true, Persisted: true},
		{EventID: "e1", Signal: "structured_path", Repo: "/repos/cli", SessionRepo: "/repos/x", SignalDiff: true, Persisted: false},
		// A fallback selection with a successful write.
		{EventID: "e2", Signal: "launch_fallback", Repo: "/repos/cli", Persisted: true},
		// An unresolved event has no destination.
		{EventID: "e3", Signal: "unresolved"},
		// A successful retry replaces the earlier outcome without adding a pair.
		{EventID: "e1", Signal: "structured_path", Repo: "/repos/cli", SessionRepo: "/repos/x", SignalDiff: true, Persisted: true},
	}

	s := summarizeRoutingDecisions(entries)

	if s.pairs != 4 { // (e1,api),(e1,cli),(e2,cli),(e3,"")
		t.Fatalf("pairs = %d, want 4 (deduped)", s.pairs)
	}
	if s.structuredPath != 2 || s.launchFallback != 1 || s.unresolved != 1 {
		t.Fatalf("buckets = sp:%d lf:%d unresolved:%d, want 2/1/1", s.structuredPath, s.launchFallback, s.unresolved)
	}
	if s.signalDiffs != 2 {
		t.Fatalf("signalDiffs = %d, want 2", s.signalDiffs)
	}
	if s.selectedNotPersisted != 0 {
		t.Fatalf("selectedNotPersisted = %d, want 0 (retry marked (e1,cli) persisted)", s.selectedNotPersisted)
	}

	// Destination totals exclude unresolved events.
	if len(s.byRepo) != 2 {
		t.Fatalf("byRepo has %d repos, want 2 (unresolved excluded)", len(s.byRepo))
	}
	api := s.byRepo["/repos/api"]
	if api == nil || api.pairs != 1 || api.structuredPath != 1 || api.signalDiffs != 1 || api.selectedNotPersisted != 0 {
		t.Fatalf("api counts = %+v", api)
	}
	cli := s.byRepo["/repos/cli"]
	if cli == nil || cli.pairs != 2 || cli.structuredPath != 1 || cli.launchFallback != 1 || cli.signalDiffs != 1 {
		t.Fatalf("cli counts = %+v", cli)
	}
}

func TestSummarizeRoutingDecisions_SelectedNotPersistedExcludesUnresolved(t *testing.T) {
	entries := []util.RoutingDecisionEntry{
		{EventID: "e1", Signal: "structured_path", Repo: "/repos/api", Persisted: false},
		{EventID: "e2", Signal: "unresolved"}, // empty Repo, not persisted, but no destination
	}
	s := summarizeRoutingDecisions(entries)
	if s.selectedNotPersisted != 1 {
		t.Fatalf("selectedNotPersisted = %d, want 1 (unresolved must not count)", s.selectedNotPersisted)
	}
	if s.unresolved != 1 {
		t.Fatalf("unresolved = %d, want 1", s.unresolved)
	}
}
