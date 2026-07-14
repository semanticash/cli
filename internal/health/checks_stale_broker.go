package health

import (
	"context"
	"fmt"

	"github.com/semanticash/cli/internal/broker"
)

// checkStaleBrokerEntries reports active broker entries whose local
// state can no longer receive routed events.
func checkStaleBrokerEntries(ctx context.Context) []Check {
	regPath, err := broker.DefaultRegistryPath()
	if err != nil {
		return nil
	}
	bh, err := broker.Open(ctx, regPath)
	if err != nil {
		return nil
	}
	repos, err := broker.ListActiveRepos(ctx, bh)
	if err != nil || len(repos) == 0 {
		return nil
	}

	var checks []Check
	for _, r := range repos {
		state := broker.CheckRepoState(ctx, r.Path)
		switch state.Verdict {
		case broker.RepoStateStale:
			checks = append(checks, Check{
				Category:    "diagnostics",
				ID:          "broker_stale:" + r.CanonicalPath,
				Status:      StatusWarn,
				Message:     fmt.Sprintf("stale broker registry entry: %s (%s)", r.Path, staleReasonMessage(state.Reason)),
				Remediation: "run `semantica tidy --apply`, or run `semantica enable` in that repo if you want capture there",
			})
		case broker.RepoStateUnknown:
			checks = append(checks, Check{
				Category:    "diagnostics",
				ID:          "broker_state_unknown:" + r.CanonicalPath,
				Status:      StatusWarn,
				Message:     fmt.Sprintf("broker entry %s: could not verify local state: %v", r.Path, state.Err),
				Remediation: "check filesystem permissions and lineage.db integrity",
			})
		}
	}
	return checks
}

// staleReasonMessage renders a stale reason for doctor output.
func staleReasonMessage(r broker.RepoStateReason) string {
	switch r {
	case broker.RepoStaleSemDirMissing:
		return ".semantica directory missing"
	case broker.RepoStaleLineageDBMissing:
		return "lineage.db missing"
	case broker.RepoStaleNoRepoRow:
		return "no local repository row"
	case broker.RepoStaleSettingsDisabled:
		return "disabled locally"
	default:
		return string(r)
	}
}
