package provenance

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"

	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// TranscriptEnricher synthesizes a provenance blob from transcript evidence
// when a step event has tool_name but no provenance_hash.
type TranscriptEnricher interface {
	// CanEnrich returns true if this enricher can handle the given provider
	// and tool name combination.
	CanEnrich(provider, toolName string) bool

	// Enrich synthesizes a provenance blob from available transcript evidence.
	// Returns nil bytes if enrichment is not possible for this step.
	Enrich(ctx context.Context, input EnrichInput) ([]byte, error)
}

// EnrichInput holds the data available to an enricher for a single step.
type EnrichInput struct {
	Provider    string
	ToolName    string
	ToolUseID   string
	TurnID      string
	SessionID   string
	PayloadHash string              // CAS hash of the step event's payload
	Companions  []CompanionEvidence // matching tool_result events, ordered by ts
	BlobStore   *blobs.Store
}

// CompanionEvidence represents a nearby tool_result event that may complete a step.
type CompanionEvidence struct {
	PayloadHash string
	Summary     string
	Role        string // "user" (Claude) or "tool" (Copilot)
	Kind        string // "tool_result"
	Ts          int64
}

// enrichers is the registry of available enrichers.
var enrichers []TranscriptEnricher

// RegisterEnricher adds an enricher to the global registry.
func RegisterEnricher(e TranscriptEnricher) {
	enrichers = append(enrichers, e)
}

// findEnricher returns the first enricher that can handle the given provider/tool combination.
func findEnricher(provider, toolName string) TranscriptEnricher {
	for _, e := range enrichers {
		if e.CanEnrich(provider, toolName) {
			return e
		}
	}
	return nil
}

// enrichSteps attempts to synthesize provenance blobs for steps that have
// tool_name but no provenance_hash. Called during packaging before bundle building.
// companionQuerier abstracts the DB queries needed for companion lookup.
type companionQuerier interface {
	ListStepCompanionResults(ctx context.Context, arg sqldb.ListStepCompanionResultsParams) ([]sqldb.ListStepCompanionResultsRow, error)
	GetNextToolResultAfter(ctx context.Context, arg sqldb.GetNextToolResultAfterParams) (sqldb.GetNextToolResultAfterRow, error)
}

func enrichSteps(
	ctx context.Context,
	h companionQuerier,
	bs *blobs.Store,
	provider, sessionID, turnID string,
	steps []sqldb.ListStepEventsForTurnRow,
) []sqldb.ListStepEventsForTurnRow {
	if len(enrichers) == 0 {
		return steps
	}

	for i := range steps {
		s := &steps[i]

		// Skip steps that already have provenance.
		if s.ProvenanceHash.Valid && s.ProvenanceHash.String != "" {
			continue
		}
		// Skip steps without a tool name.
		if !s.ToolName.Valid || s.ToolName.String == "" {
			continue
		}
		// Skip steps without a payload (nothing to enrich from).
		if !s.PayloadHash.Valid || s.PayloadHash.String == "" {
			continue
		}

		enricher := findEnricher(provider, s.ToolName.String)
		if enricher == nil {
			continue
		}

		// Fetch companion tool_result evidence.
		// First try by tool_use_id (exact match). If that yields nothing
		// (e.g., Claude transcript tool_results lack tool_use_id), fall back
		// to temporal matching: the first tool_result after this step's ts.
		var companions []CompanionEvidence
		if s.ToolUseID.Valid && s.ToolUseID.String != "" {
			rows, err := h.ListStepCompanionResults(ctx, sqldb.ListStepCompanionResultsParams{
				SessionID: sessionID,
				TurnID:    sqlstore.NullStr(turnID),
				ToolUseID: s.ToolUseID,
			})
			if err == nil {
				for _, r := range rows {
					ce := CompanionEvidence{
						Summary: r.Summary.String,
						Role:    r.Role.String,
						Kind:    r.Kind,
						Ts:      r.Ts,
					}
					if r.PayloadHash.Valid {
						ce.PayloadHash = r.PayloadHash.String
					}
					companions = append(companions, ce)
				}
			} else {
				slog.Warn("provenance: companion lookup failed", "session", sessionID, "turn", turnID, "event", s.EventID, "tool", s.ToolName.String, "err", err)
			}
		}
		// Temporal fallback when tool_use_id matching yields nothing.
		if len(companions) == 0 {
			row, err := h.GetNextToolResultAfter(ctx, sqldb.GetNextToolResultAfterParams{
				SessionID: sessionID,
				TurnID:    sqlstore.NullStr(turnID),
				Ts:        s.Ts,
			})
			if err == nil {
				ce := CompanionEvidence{
					Summary: row.Summary.String,
					Role:    row.Role.String,
					Kind:    row.Kind,
					Ts:      row.Ts,
				}
				if row.PayloadHash.Valid {
					ce.PayloadHash = row.PayloadHash.String
				}
				companions = append(companions, ce)
			} else if !errors.Is(err, sql.ErrNoRows) {
				slog.Warn("provenance: temporal companion lookup failed", "session", sessionID, "turn", turnID, "event", s.EventID, "tool", s.ToolName.String, "err", err)
			}
		}

		blob, err := enricher.Enrich(ctx, EnrichInput{
			Provider:    provider,
			ToolName:    s.ToolName.String,
			ToolUseID:   s.ToolUseID.String,
			TurnID:      turnID,
			SessionID:   sessionID,
			PayloadHash: s.PayloadHash.String,
			Companions:  companions,
			BlobStore:   bs,
		})
		if err != nil || len(blob) == 0 {
			if err != nil {
				slog.Warn("provenance: step enrichment failed", "session", sessionID, "turn", turnID, "event", s.EventID, "tool", s.ToolName.String, "err", err)
			}
			continue
		}

		// Write synthesized blob to CAS and attach to the step.
		hash, _, err := bs.Put(ctx, blob)
		if err != nil {
			slog.Warn("provenance: store enriched step failed", "session", sessionID, "turn", turnID, "event", s.EventID, "tool", s.ToolName.String, "err", err)
			continue
		}
		s.ProvenanceHash.Valid = true
		s.ProvenanceHash.String = hash
	}

	return steps
}
