package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"strings"

	attrscoring "github.com/semanticash/cli/internal/attribution/scoring"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/toolsnap"
	"github.com/semanticash/cli/internal/turncapture"
)

type turnAttribution struct {
	claims map[string][]attrscoring.DeltaClaimGroup
	models map[string]string
}

// loadTurnAttribution derives inferred line claims from stored turn observations.
func loadTurnAttribution(ctx context.Context, h *sqlstore.Handle, bs *blobs.Store, cpID, repoID, commit string, win eventWindow) (turnAttribution, error) {
	out := turnAttribution{claims: map[string][]attrscoring.DeltaClaimGroup{}, models: map[string]string{}}
	if cpID == "" || commit == "" {
		return out, nil
	}
	rows, err := h.Queries.ListTurnObservationsForCheckpoint(ctx, sqldb.ListTurnObservationsForCheckpointParams{
		RepositoryID: repoID, CheckpointID: cpID, UseCursor: win.cursorFlag(),
		AfterCursor: win.cursorAfter(), UpToCursor: win.cursorUpTo(), AfterTs: win.afterTs, UpToTs: win.upToTs,
	})
	if err != nil {
		return out, err
	}
	selected := map[string]sqldb.ListTurnObservationsForCheckpointRow{}
	var earliest int64
	for _, row := range rows {
		rec := readAttributionTurn(ctx, bs, row.EvidenceHash.String, row.TurnID.String, repoID)
		if rec == nil {
			continue
		}
		selected[row.EventID] = row
		if ts := rec.StartedAt.UnixMilli(); earliest == 0 || ts < earliest {
			earliest = ts
		}
	}
	if len(selected) == 0 {
		return out, ctx.Err()
	}
	all, err := h.Queries.ListTurnObservationEvidence(ctx, sqldb.ListTurnObservationEvidenceParams{RepositoryID: repoID, Ts: earliest})
	if err != nil {
		return out, err
	}
	records := map[string]*turncapture.Record{}
	for _, row := range all {
		rec := readAttributionTurn(ctx, bs, row.EvidenceHash.String, row.TurnID.String, repoID)
		if rec == nil || strings.ReplaceAll(rec.Provider, "-", "_") != strings.ReplaceAll(row.Provider, "-", "_") {
			// Reject inference when overlapping turns cannot be checked.
			return out, ctx.Err()
		}
		if previous := records[row.EventID]; previous != nil {
			return out, ctx.Err()
		}
		records[row.EventID] = rec
	}
	for _, row := range all {
		selectedRow, ok := selected[row.EventID]
		if !ok {
			continue
		}
		rec := records[row.EventID]
		if !eligibleAttributionTurn(rec) {
			continue
		}
		overlaps := false
		for id, other := range records {
			if id != row.EventID && rec.StartedAt.Before(other.End.FinishedAt) && other.StartedAt.Before(rec.End.FinishedAt) {
				overlaps = true
				break
			}
		}
		if overlaps {
			continue
		}
		inWindow := selectedRow.Ts > win.afterTs && selectedRow.Ts <= win.upToTs
		if win.useCursor {
			inWindow = (selectedRow.Ts > win.afterTs || (selectedRow.Ts == win.afterTs && selectedRow.InsertSeq.Int64 > win.afterCursor)) &&
				(selectedRow.Ts < win.upToTs || (selectedRow.Ts == win.upToTs && selectedRow.InsertSeq.Int64 <= win.upToCursor))
		}
		change := attributionTurnChange(rec, commit, inWindow)
		if change == nil {
			continue
		}
		for _, f := range change.Files {
			if f.Binary || f.Truncated || (f.Operation != "create" && f.Operation != "edit") ||
				(f.AfterMode != "100644" && f.AfterMode != "100755") ||
				(f.Operation == "edit" && f.BeforeMode != "100644" && f.BeforeMode != "100755") ||
				f.Path == "" || path.IsAbs(f.Path) || path.Clean(f.Path) != f.Path || f.Path == ".." || strings.HasPrefix(f.Path, "../") {
				continue
			}
			var lines []string
			for _, hunk := range f.Hunks {
				lines = append(lines, hunk.NewLines...)
			}
			if len(lines) > 0 {
				out.claims[f.Path] = append(out.claims[f.Path], attrscoring.DeltaClaimGroup{
					Provider: row.Provider, EventID: row.EventID, Lines: lines,
				})
			}
		}
		out.models[row.Provider] = row.Model.String
	}
	return out, ctx.Err()
}

func readAttributionTurn(ctx context.Context, bs *blobs.Store, hash, turn, repo string) *turncapture.Record {
	raw, err := bs.Get(ctx, hash)
	sum := sha256.Sum256(raw)
	var rec turncapture.Record
	if err != nil || hex.EncodeToString(sum[:]) != hash || json.Unmarshal(raw, &rec) != nil ||
		rec.Version != 1 || rec.Provider == "" || rec.SessionID == "" || rec.TurnID != turn ||
		rec.StartedAt.IsZero() || rec.End == nil || rec.End.FinishedAt.IsZero() ||
		len(rec.Repositories) != 1 || len(rec.End.Repositories) != 1 || rec.Repositories[0].Subject.RepositoryID != repo {
		return nil
	}
	return &rec
}

func eligibleAttributionTurn(rec *turncapture.Record) bool {
	before, after := rec.Repositories[0], rec.End.Repositories[0]
	completion, _ := turncapture.Completion(rec.End.Evidence)
	return before.Gap == "" && before.Subject.Gap == "" && before.Baseline.Snapshot.TreeHash != "" &&
		!before.Baseline.FinishedAt.IsZero() && !rec.BaselineFinishedAt.IsZero() &&
		!rec.BaselineFinishedAt.After(rec.End.BoundaryAt) && !after.StartedAt.Before(rec.BaselineFinishedAt) &&
		!after.FinishedAt.Before(after.StartedAt) && !rec.End.FinishedAt.Before(after.FinishedAt) &&
		after.State == "changed" && after.Reason == "" && completion != "unsettled" &&
		(rec.End.TrackedCompletion == "settled" || rec.End.TrackedCompletion == "unknown")
}

func attributionTurnChange(rec *turncapture.Record, commit string, inWindow bool) *toolsnap.TurnChange {
	before := rec.Repositories[0].Baseline
	changes := rec.End.Repositories[0].Changes
	for i := range changes {
		if changes[i].Commit == commit {
			// HEAD-relative deltas can include changes made before the turn.
			if before.Snapshot.TreeHash != before.Repository.HeadTree {
				return nil
			}
			return &changes[i]
		}
	}
	if !inWindow || len(changes) != 1 || changes[0].Commit != "" || changes[0].BeforeTree != before.Snapshot.TreeHash {
		return nil
	}
	return &changes[0]
}
