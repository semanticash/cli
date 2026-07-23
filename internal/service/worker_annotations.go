package service

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/attribution/annotations"
	attrevents "github.com/semanticash/cli/internal/attribution/events"
	attrscoring "github.com/semanticash/cli/internal/attribution/scoring"
	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// detectCommitAnnotations derives conservative timeline annotations for a
// commit from its attribution-window agent events and diff. It reuses the same
// previous-commit-linked-checkpoint window as attribution so the event set
// matches. Best-effort: any load failure yields no annotations rather than
// blocking the push.
func detectCommitAnnotations(ctx context.Context, repo *git.Repo, h *sqlstore.Handle, commitHash string) []annotations.Annotation {
	link, err := h.Queries.GetCommitLinkByCommitHash(ctx, commitHash)
	if err != nil {
		return nil
	}
	cp, err := h.Queries.GetCheckpointByID(ctx, link.CheckpointID)
	if err != nil {
		return nil
	}

	var afterTs int64
	if prev, prevErr := h.Queries.GetPreviousCommitLinkedCheckpoint(ctx, sqldb.GetPreviousCommitLinkedCheckpointParams{
		RepositoryID: cp.RepositoryID,
		CreatedAt:    cp.CreatedAt,
	}); prevErr == nil {
		afterTs = prev.CreatedAt
	}

	rows, err := h.Queries.ListEventsInWindowForAnnotations(ctx, sqldb.ListEventsInWindowForAnnotationsParams{
		RepositoryID: cp.RepositoryID,
		AfterTs:      afterTs,
		UpToTs:       cp.CreatedAt,
	})
	if err != nil || len(rows) == 0 {
		return nil
	}

	repoRoot := repo.Root()
	bs, err := blobs.NewStore(filepath.Join(repoRoot, ".semantica", "objects"))
	if err != nil {
		return nil
	}

	diffBytes, err := repo.DiffForCommit(ctx, commitHash)
	if err != nil || len(diffBytes) == 0 {
		return nil
	}

	return annotations.Detect(annotations.DetectInput{
		CommitSHA: commitHash,
		RepoRoot:  repoRoot,
		Events:    toAnnotationEvents(ctx, bs, rows),
		Commit:    toCommitDiff(diffBytes),
	})
}

// toAnnotationEvents maps window rows into detector events, loading payloads
// only for assistant events that carry a file-modifying tool call. Provider
// file-touch events are recognized from tool_uses and need no payload.
func toAnnotationEvents(ctx context.Context, bs *blobs.Store, rows []sqldb.ListEventsInWindowForAnnotationsRow) []annotations.Event {
	out := make([]annotations.Event, 0, len(rows))
	for _, r := range rows {
		ev := annotations.Event{
			EventID:        r.EventID,
			TurnID:         r.TurnID.String,
			Provider:       r.Provider,
			TS:             r.Ts,
			Role:           r.Role.String,
			ToolUses:       r.ToolUses.String,
			ProvenanceHash: r.ProvenanceHash.String,
		}

		if r.Role.String == "assistant" && r.PayloadHash.Valid && r.PayloadHash.String != "" && bs != nil {
			hasBash := r.ToolUses.Valid && strings.Contains(r.ToolUses.String, `"Bash"`)
			if attrevents.HasEditOrWrite(r.ToolUses.String) || hasBash {
				if raw, err := bs.Get(ctx, r.PayloadHash.String); err == nil {
					ev.Payload = raw
				}
			}
		}

		out = append(out, ev)
	}
	return out
}

// annotationPayload is the JSON shape for CLI-derived timeline annotations on
// the attribution push.
type annotationPayload struct {
	AnnotationID       string              `json:"annotation_id"`
	Kind               string              `json:"kind"`
	Source             string              `json:"source"`
	FilePath           string              `json:"file_path,omitempty"`
	LineRangeStart     int                 `json:"line_range_start,omitempty"`
	LineRangeEnd       int                 `json:"line_range_end,omitempty"`
	TurnIDs            []string            `json:"turn_ids,omitempty"`
	Anchors            []annotationAnchor  `json:"anchors,omitempty"`
	SupportingStepRefs []annotationStepRef `json:"supporting_step_refs,omitempty"`
	StartedAt          int64               `json:"started_at,omitempty"`
	EndedAt            int64               `json:"ended_at,omitempty"`
	Summary            string              `json:"summary,omitempty"`
	Confidence         float64             `json:"confidence,omitempty"`
	Status             string              `json:"status"`
	AlgorithmVersion   string              `json:"algorithm_version"`
}

type annotationAnchor struct {
	EventID   string `json:"event_id,omitempty"`
	TurnID    string `json:"turn_id,omitempty"`
	CommitSHA string `json:"commit_sha,omitempty"`
	FilePath  string `json:"file_path,omitempty"`
	LineRange []int  `json:"line_range,omitempty"`
}

type annotationStepRef struct {
	TurnID         string `json:"turn_id,omitempty"`
	EventID        string `json:"event_id,omitempty"`
	ProvenanceHash string `json:"provenance_hash,omitempty"`
}

// toAnnotationPayloads maps detector output into the push payload shape.
// Returns nil (omitted from JSON) when there are no annotations.
func toAnnotationPayloads(anns []annotations.Annotation) []annotationPayload {
	if len(anns) == 0 {
		return nil
	}
	out := make([]annotationPayload, 0, len(anns))
	for _, a := range anns {
		p := annotationPayload{
			AnnotationID:     a.ID,
			Kind:             string(a.Kind),
			Source:           annotations.SourceCLI,
			FilePath:         a.FilePath,
			LineRangeStart:   a.LineStart,
			LineRangeEnd:     a.LineEnd,
			TurnIDs:          a.TurnIDs,
			StartedAt:        a.StartedAt,
			EndedAt:          a.EndedAt,
			Summary:          a.Summary,
			Confidence:       a.Confidence,
			Status:           string(a.Status),
			AlgorithmVersion: a.AlgorithmVersion,
		}
		for _, an := range a.Anchors {
			anchor := annotationAnchor{
				EventID:   an.EventID,
				TurnID:    an.TurnID,
				CommitSHA: an.CommitSHA,
				FilePath:  an.FilePath,
			}
			if an.LineStart != 0 || an.LineEnd != 0 {
				anchor.LineRange = []int{an.LineStart, an.LineEnd}
			}
			p.Anchors = append(p.Anchors, anchor)
		}
		for _, sr := range a.SupportingStepRefs {
			p.SupportingStepRefs = append(p.SupportingStepRefs, annotationStepRef{
				TurnID:         sr.TurnID,
				EventID:        sr.EventID,
				ProvenanceHash: sr.ProvenanceHash,
			})
		}
		out = append(out, p)
	}
	return out
}

// toCommitDiff projects a parsed diff into the file sets the detector needs:
// every path the commit changed, and the subset it deleted.
func toCommitDiff(diffBytes []byte) annotations.CommitDiff {
	dr := attrscoring.ParseDiff(diffBytes)

	files := make(map[string]bool)
	for _, f := range dr.Files {
		files[f.Path] = true
	}
	for _, p := range dr.FilesCreated {
		files[p] = true
	}
	deleted := make(map[string]bool, len(dr.FilesDeleted))
	for _, p := range dr.FilesDeleted {
		deleted[p] = true
		files[p] = true
	}

	return annotations.CommitDiff{Files: files, FilesDeleted: deleted}
}
