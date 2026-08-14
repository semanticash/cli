package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"
)

// ReadinessState describes the availability of one evidence component.
type ReadinessState string

const (
	ReadinessNotRequired ReadinessState = "not_required"
	ReadinessPending     ReadinessState = "pending"
	ReadinessReady       ReadinessState = "ready"
	ReadinessFailed      ReadinessState = "failed"
	ReadinessUnknown     ReadinessState = "unknown"
)

// ReadinessComponent is one evidence component's state with an
// optional human-readable reason.
type ReadinessComponent struct {
	State  ReadinessState `json:"state"`
	Reason string         `json:"reason,omitempty"`
}

// ReadinessPolicy identifies the evidence requirements used for a verdict.
type ReadinessPolicy string

const (
	// PolicyLocal evaluates local evidence only; sync is never
	// required.
	PolicyLocal ReadinessPolicy = "local"

	// PolicyHosted additionally requires hosted sync evidence.
	PolicyHosted ReadinessPolicy = "hosted"
)

// AuditReadiness is the audit-readiness verdict for one checkpoint.
// Checkpoint completion means the immutable core snapshot exists;
// audit readiness means the required derived evidence is available.
type AuditReadiness struct {
	Policy      string             `json:"policy"`
	Manifest    ReadinessComponent `json:"manifest"`
	Attribution ReadinessComponent `json:"attribution"`
	Provenance  ReadinessComponent `json:"provenance"`
	Sync        ReadinessComponent `json:"sync"`
	AuditReady  bool               `json:"audit_ready"`
	// AttributionVersion identifies the algorithm used for the stored result.
	AttributionVersion string `json:"attribution_version,omitempty"`
}

// EvaluateAuditReadiness derives a checkpoint verdict from stored evidence.
func EvaluateAuditReadiness(ctx context.Context, h *sqlstore.Handle, semDir string, cp sqldb.Checkpoint, policy ReadinessPolicy) AuditReadiness {
	ar := AuditReadiness{Policy: string(policy)}

	// Unknown policies fail closed instead of treating evidence as optional.
	if policy != PolicyLocal && policy != PolicyHosted {
		unknown := ReadinessComponent{State: ReadinessUnknown, Reason: fmt.Sprintf("unknown policy %q", policy)}
		ar.Manifest, ar.Attribution, ar.Provenance, ar.Sync = unknown, unknown, unknown, unknown
		return ar
	}

	links, linksErr := h.Queries.GetCommitLinksByCheckpoint(ctx, cp.CheckpointID)
	commitLinked := len(links) > 0

	stats, statsErr := h.Queries.GetCheckpointStats(ctx, cp.CheckpointID)
	hasStats := statsErr == nil
	if hasStats && stats.AttributionVersion.Valid {
		ar.AttributionVersion = stats.AttributionVersion.String
	}

	ar.Manifest = manifestReadiness(cp)
	if linksErr != nil {
		ar.Attribution = ReadinessComponent{State: ReadinessUnknown, Reason: fmt.Sprintf("commit links unavailable: %v", linksErr)}
	} else {
		ar.Attribution = attributionReadiness(cp, commitLinked, hasStats, stats)
	}
	ar.Provenance, ar.Sync = provenanceAndSyncReadiness(ctx, h, semDir, cp, hasStats, stats)

	required := []ReadinessComponent{ar.Manifest, ar.Attribution, ar.Provenance}
	if policy == PolicyHosted {
		required = append(required, ar.Sync)
	}
	ar.AuditReady = true
	for _, c := range required {
		if c.State != ReadinessReady && c.State != ReadinessNotRequired {
			ar.AuditReady = false
			break
		}
	}
	return ar
}

func manifestReadiness(cp sqldb.Checkpoint) ReadinessComponent {
	switch {
	case cp.ManifestHash.Valid && cp.ManifestHash.String != "":
		return ReadinessComponent{State: ReadinessReady}
	case cp.Status == "pending":
		return ReadinessComponent{State: ReadinessPending, Reason: "checkpoint not yet processed"}
	case cp.Status == "failed":
		reason := "checkpoint failed"
		if cp.LastError.Valid && cp.LastError.String != "" {
			reason += ": " + cp.LastError.String
		}
		return ReadinessComponent{State: ReadinessFailed, Reason: reason}
	default:
		return ReadinessComponent{State: ReadinessUnknown, Reason: "complete checkpoint without manifest hash"}
	}
}

func attributionReadiness(cp sqldb.Checkpoint, commitLinked, hasStats bool, stats sqldb.CheckpointStat) ReadinessComponent {
	if !commitLinked {
		return ReadinessComponent{State: ReadinessNotRequired, Reason: "no commit link"}
	}
	if cp.Status == "pending" {
		return ReadinessComponent{State: ReadinessPending, Reason: "checkpoint not yet processed"}
	}
	if cp.Status == "failed" {
		return ReadinessComponent{State: ReadinessFailed, Reason: "checkpoint failed before attribution"}
	}
	if !hasStats {
		return ReadinessComponent{State: ReadinessUnknown, Reason: "no checkpoint stats recorded"}
	}
	if stats.AttributionComputedAt.Valid {
		if stats.AiPercentage < 0 {
			// Ran to completion with nothing to attribute: a valid
			// zero-AI or no-diff checkpoint, not a failure.
			return ReadinessComponent{State: ReadinessReady, Reason: "computed; no attributable changes"}
		}
		return ReadinessComponent{State: ReadinessReady}
	}
	if stats.AiPercentage >= 0 {
		// Percentage exists from before the completion marker.
		return ReadinessComponent{State: ReadinessReady, Reason: "computed before completion markers existed"}
	}
	return ReadinessComponent{State: ReadinessUnknown, Reason: "attribution completion not recorded"}
}

// provenanceAndSyncReadiness derives turn-bundle coverage for the
// checkpoint's event window (provenance) and hosted evidence state
// (sync). Provenance is local packaging; uploads and attribution
// pushes belong to sync.
func provenanceAndSyncReadiness(ctx context.Context, h *sqlstore.Handle, semDir string, cp sqldb.Checkpoint, hasStats bool, stats sqldb.CheckpointStat) (ReadinessComponent, ReadinessComponent) {
	// Unreadable settings make sync unknown, not optional.
	connected := false
	sync := ReadinessComponent{State: ReadinessNotRequired, Reason: "repository not connected"}
	if settings, err := util.ReadSettings(semDir); err != nil {
		sync = ReadinessComponent{State: ReadinessUnknown, Reason: fmt.Sprintf("settings unreadable: %v", err)}
	} else {
		connected = settings.Connected
	}

	win, err := checkpointWindow(ctx, h, cp)
	if err != nil {
		unknown := ReadinessComponent{State: ReadinessUnknown, Reason: fmt.Sprintf("event window unavailable: %v", err)}
		if connected {
			sync = unknown
		}
		return unknown, sync
	}
	cov, err := h.Queries.CountWindowTurnProvenance(ctx, sqldb.CountWindowTurnProvenanceParams{
		RepositoryID: cp.RepositoryID,
		UseCursor:    win.cursorFlag(),
		AfterCursor:  win.cursorAfter(),
		UpToCursor:   win.cursorUpTo(),
		AfterTs:      win.afterTs,
		UpToTs:       win.upToTs,
	})
	if err != nil {
		unknown := ReadinessComponent{State: ReadinessUnknown, Reason: fmt.Sprintf("turn coverage unavailable: %v", err)}
		if connected {
			sync = unknown
		}
		return unknown, sync
	}

	unpackaged := cov.TotalTurns - cov.PackagedTurns
	var provenance ReadinessComponent
	switch {
	case cov.TotalTurns == 0:
		provenance = ReadinessComponent{State: ReadinessNotRequired, Reason: "no agent turns in window"}
	case unpackaged > 0:
		// Missing bundles require repair before provenance is ready.
		provenance = ReadinessComponent{
			State:  ReadinessFailed,
			Reason: fmt.Sprintf("%d of %d turn(s) without a packaged provenance bundle", unpackaged, cov.TotalTurns),
		}
	default:
		provenance = ReadinessComponent{State: ReadinessReady}
	}

	if connected {
		sync = syncReadiness(cp, hasStats, stats, cov)
	}
	return provenance, sync
}

func syncReadiness(cp sqldb.Checkpoint, hasStats bool, stats sqldb.CheckpointStat, cov sqldb.CountWindowTurnProvenanceRow) ReadinessComponent {
	if cp.Status == "pending" {
		return ReadinessComponent{State: ReadinessPending, Reason: "checkpoint not yet processed"}
	}
	pushed := hasStats && stats.AttributionPushedAt.Valid
	turnsUploaded := cov.TotalTurns == 0 ||
		(cov.PackagedTurns == cov.TotalTurns && cov.UploadedTurns == cov.TotalTurns)
	switch {
	case cov.FailedUploadTurns > 0:
		// Failed uploads require repair and cannot converge while pending.
		return ReadinessComponent{
			State:  ReadinessFailed,
			Reason: fmt.Sprintf("%d turn bundle upload(s) failed", cov.FailedUploadTurns),
		}
	case pushed && turnsUploaded:
		return ReadinessComponent{State: ReadinessReady}
	case !pushed && hasStats && !stats.AttributionComputedAt.Valid && stats.AiPercentage < 0:
		return ReadinessComponent{State: ReadinessUnknown, Reason: "no recorded attribution push (predates push markers or attribution never ran)"}
	case !pushed:
		return ReadinessComponent{State: ReadinessPending, Reason: "attribution push not recorded"}
	default:
		return ReadinessComponent{
			State:  ReadinessPending,
			Reason: fmt.Sprintf("%d of %d turn bundle(s) not uploaded", cov.TotalTurns-cov.UploadedTurns, cov.TotalTurns),
		}
	}
}

// queueBlockage returns the failed checkpoint that gates later commit-linked
// work. Failures followed by a completed successor are historical and do not block.
func queueBlockage(ctx context.Context, h *sqlstore.Handle, repoID string) *BlockedCheckpointInfo {
	rows, err := h.Queries.ListPendingCommitLinkedCheckpoints(ctx, repoID)
	if err != nil {
		return nil
	}
	var maxCompleteSeq int64
	if latest, err := h.Queries.GetMostRecentCommitLinkedCheckpoint(ctx, repoID); err == nil {
		maxCompleteSeq = latest.RepositorySequence
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if seen[r.CheckpointID] {
			continue
		}
		seen[r.CheckpointID] = true
		if r.Status == "failed" && r.RepositorySequence > maxCompleteSeq {
			return &BlockedCheckpointInfo{
				CheckpointID: r.CheckpointID,
				Status:       r.Status,
				LastError:    r.LastError.String,
			}
		}
	}
	return nil
}

// checkpointWindow rebuilds the checkpoint's event window against its
// commit-linked predecessor, matching the enrichment windows.
func checkpointWindow(ctx context.Context, h *sqlstore.Handle, cp sqldb.Checkpoint) (eventWindow, error) {
	prev, err := h.Queries.GetPreviousCommitLinkedCheckpoint(ctx, sqldb.GetPreviousCommitLinkedCheckpointParams{
		RepositoryID:       cp.RepositoryID,
		RepositorySequence: cp.RepositorySequence,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return windowBetween(nil, cp), nil
	}
	if err != nil {
		return eventWindow{}, err
	}
	return windowBetween(&prev, cp), nil
}
