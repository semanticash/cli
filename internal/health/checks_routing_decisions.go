package health

import (
	"context"
	"fmt"
	"sort"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/util"
)

// routingDecisionTailLimit limits the record count, not the time range.
const routingDecisionTailLimit = 5000

// repoRoutingCounts holds destination totals. Unresolved decisions are global-only.
type repoRoutingCounts struct {
	pairs                int
	structuredPath       int
	launchFallback       int
	signalDiffs          int
	selectedNotPersisted int
}

// routingDecisionSummary counts deduplicated selections and write outcomes.
// Selection signals do not establish attribution correctness.
type routingDecisionSummary struct {
	pairs                int // deduplicated (event, destination) decisions
	structuredPath       int
	launchFallback       int
	unresolved           int
	signalDiffs          int // selected repo differs from session dir (neutral)
	selectedNotPersisted int // a repo was selected but its write did not persist
	byRepo               map[string]*repoRoutingCounts
}

// summarizeRoutingDecisions totals the last entry for each (EventID, Repo)
// globally and per repository. Entries must be in append order.
func summarizeRoutingDecisions(entries []util.RoutingDecisionEntry) routingDecisionSummary {
	type key struct{ event, repo string }
	latest := make(map[key]util.RoutingDecisionEntry, len(entries))
	for _, e := range entries {
		latest[key{e.EventID, e.Repo}] = e // append-order tail: last wins
	}

	s := routingDecisionSummary{byRepo: make(map[string]*repoRoutingCounts)}
	for _, e := range latest {
		s.pairs++
		if e.SignalDiff {
			s.signalDiffs++
		}
		// Unresolved decisions are not failed destination writes.
		selectedNotPersisted := e.Repo != "" && !e.Persisted
		if selectedNotPersisted {
			s.selectedNotPersisted++
		}

		switch e.Signal {
		case string(broker.SignalStructuredPath):
			s.structuredPath++
		case string(broker.SignalLaunchFallback):
			s.launchFallback++
		case string(broker.SignalUnresolved):
			s.unresolved++
		}

		if e.Repo == "" {
			continue // unresolved: no per-repository attribution
		}
		rc := s.byRepo[e.Repo]
		if rc == nil {
			rc = &repoRoutingCounts{}
			s.byRepo[e.Repo] = rc
		}
		rc.pairs++
		if e.SignalDiff {
			rc.signalDiffs++
		}
		if selectedNotPersisted {
			rc.selectedNotPersisted++
		}
		switch e.Signal {
		case string(broker.SignalStructuredPath):
			rc.structuredPath++
		case string(broker.SignalLaunchFallback):
			rc.launchFallback++
		}
	}
	return s
}

// checkRoutingDecisions reports retained broker routing records globally and
// per repository. Fallbacks and signal differences are informational.
func checkRoutingDecisions(_ context.Context, _ Options) []Check {
	entries, err := util.ReadRoutingDecisionTail(routingDecisionTailLimit)
	if err != nil {
		return []Check{{
			Category: "attribution", ID: "routing_decisions", Status: StatusWarn,
			Message: "could not read routing-decisions log: " + err.Error(),
		}}
	}
	if len(entries) == 0 {
		return []Check{{
			Category: "attribution", ID: "routing_decisions", Status: StatusOK,
			Message: "no broker routing decisions recorded yet (broker routing only; provider tool-window target routing not covered)",
		}}
	}
	s := summarizeRoutingDecisions(entries)
	checks := []Check{{
		Category: "attribution", ID: "routing_decisions", Status: StatusOK,
		Message: fmt.Sprintf(
			"global (all repositories, most recent %d records; broker routing only, tool-window target routing not covered): "+
				"%d event/destination pairs — %d structured_path, %d launch_fallback, %d unresolved; "+
				"%d cross-repo signal differences (neutral); %d selected-but-not-persisted",
			routingDecisionTailLimit, s.pairs, s.structuredPath, s.launchFallback, s.unresolved,
			s.signalDiffs, s.selectedNotPersisted,
		),
	}}

	repos := make([]string, 0, len(s.byRepo))
	for repo := range s.byRepo {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		rc := s.byRepo[repo]
		checks = append(checks, Check{
			Category: "attribution", ID: "routing_decisions:" + repo, Status: StatusOK,
			Message: fmt.Sprintf(
				"%s: %d pairs — %d structured_path, %d launch_fallback; %d signal differences (neutral); %d selected-but-not-persisted",
				repo, rc.pairs, rc.structuredPath, rc.launchFallback, rc.signalDiffs, rc.selectedNotPersisted,
			),
		})
	}
	return checks
}
