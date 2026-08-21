package service

import (
	"context"
	"time"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/doctor"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/util"
)

// sweepRepoTimeout bounds recovery work for one repository.
const sweepRepoTimeout = 30 * time.Second

// SweepToolWindows recovers tool-window evidence for active repositories.
// Repository failures are logged without stopping the remaining sweep.
func SweepToolWindows(ctx context.Context) {
	registryPath, err := broker.DefaultRegistryPath()
	if err != nil {
		wlog("worker: toolwindow sweep: resolve registry path: %v\n", err)
		return
	}
	bh, err := broker.Open(ctx, registryPath)
	if err != nil {
		wlog("worker: toolwindow sweep: open broker: %v\n", err)
		return
	}
	defer func() { _ = broker.Close(bh) }()

	repos, err := broker.ListActiveRepos(ctx, bh)
	if err != nil {
		wlog("worker: toolwindow sweep: list active repos: %v\n", err)
		return
	}
	for _, r := range repos {
		if ctx.Err() != nil {
			return
		}
		if !util.IsEnabledAt(r.Path) {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, sweepRepoTimeout)
		report, err := hooks.SweepToolWindows(rctx, r.Path)
		cancel()
		if err != nil {
			wlog("worker: toolwindow sweep %s: reclaimed=%d tombstoned=%d err=%v\n",
				r.Path, report.GroupsReclaimed, report.MembersTombstoned, err)
			// Preserve recovery results recorded before the terminal failure.
			doctor.EmitBenchRecord(r.Path, doctor.BenchRecord{
				Kind: "toolwindow_sweep", Outcome: "error", SweepErrors: report.Errors + 1,
				GroupsReclaimed:   report.GroupsReclaimed,
				MembersTombstoned: report.MembersTombstoned,
			})
			continue
		}
		if report.PartialsReplayed+report.GroupsResumed+report.GroupsTerminal+report.GroupsReclaimed+report.LinksSkipped+report.Errors > 0 {
			wlog("worker: toolwindow sweep %s: replayed=%d resumed=%d terminal=%d reclaimed=%d tombstoned=%d links_skipped=%d errors=%d\n",
				r.Path, report.PartialsReplayed, report.GroupsResumed, report.GroupsTerminal, report.GroupsReclaimed, report.MembersTombstoned, report.LinksSkipped, report.Errors)
		}
		// Record recovery progress, snapshot-store size, and maintenance outcome.
		doctor.EmitBenchRecord(r.Path, doctor.BenchRecord{
			Kind:                "toolwindow_sweep",
			PartialsReplayed:    report.PartialsReplayed,
			GroupsResumed:       report.GroupsResumed,
			GroupsTerminal:      report.GroupsTerminal,
			GroupsReclaimed:     report.GroupsReclaimed,
			MembersTombstoned:   report.MembersTombstoned,
			LinksSkipped:        report.LinksSkipped,
			SweepErrors:         report.Errors,
			StoreBytes:          report.Maintenance.StoreBytes,
			PruneRan:            report.Maintenance.PruneRan,
			MaintenanceDeferred: report.Maintenance.Deferred,
			MaintenanceSkipped:  report.MaintenanceSkipped,
			RefsDeleted:         report.Maintenance.RefsDeleted,
		})
	}
}
