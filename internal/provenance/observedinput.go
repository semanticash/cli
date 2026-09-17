package provenance

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/observedinput"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

const observedInputEvidenceKind = "observed_input"

// observedInputEventID is a stable per-turn identity so replay is idempotent.
func observedInputEventID(provider, session, turn string) string {
	sum := sha256.Sum256([]byte(provider + "\x00" + session + "\x00" + turn))
	return fmt.Sprintf("observed-input:%x", sum)
}

func repoObjects(repoPath string) string { return filepath.Join(repoPath, ".semantica", "objects") }
func repoLineageDB(repoPath string) string {
	return filepath.Join(repoPath, ".semantica", "lineage.db")
}

// PersistObservedInputs stores turn documents and primary observation content in CAS.
// Evidence links support retrieval without transcripts. Identical replays are
// idempotent; differing documents for the same turn are rejected.
func PersistObservedInputs(ctx context.Context, repoPath, providerSessionID string, capturedAt int64, evs []observedinput.Evidence, contents map[string][]byte) error {
	bs, err := blobs.NewStore(repoObjects(repoPath))
	if err != nil {
		return err
	}
	for _, ev := range evs {
		if ev.TurnID == "" || ev.TurnID == "unresolved" {
			continue // only a provider-defined turn owns a bundle
		}
		if err := observedinput.Validate(ev); err != nil {
			return fmt.Errorf("observed-input evidence for turn %s invalid: %w", ev.TurnID, err)
		}
		for _, o := range ev.Observations {
			ref := o.Representation.ContentRef
			if ref == "" {
				continue
			}
			body, ok := contents[ref]
			if !ok {
				return fmt.Errorf("observed-input content %s missing for turn %s", ref, ev.TurnID)
			}
			if _, _, err := bs.Put(ctx, body); err != nil {
				return err
			}
		}
		doc, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		docHash, _, err := bs.Put(ctx, doc)
		if err != nil {
			return err
		}
		key := observedInputEventID(ev.Provider, ev.SessionID, ev.TurnID)
		record := broker.ObservationContext(broker.RawEvent{
			EventID: key, SourceKey: "observed-input:" + ev.SessionID,
			Provider: ev.Provider, ProviderSessionID: providerSessionID, TurnID: ev.TurnID,
			Timestamp: capturedAt, Kind: "context", Role: "system", EventSource: observedInputEvidenceKind,
			Summary: "Observed inputs supplied with the request.",
		})
		if _, err := broker.WriteEventsToRepo(ctx, repoPath, []broker.RawEvent{record}, nil); err != nil {
			return err
		}
		if err := broker.WriteEvidenceLinksToRepo(ctx, repoPath, []broker.EvidenceLink{{
			EventID: key, EvidenceKind: observedInputEvidenceKind, EvidenceHash: docHash,
			GroupID: key, CreatedAt: capturedAt,
		}}); err != nil {
			return err
		}
	}
	return nil
}

// CollectObservedInput loads a turn document from local storage and checks primary
// observation content references. It returns (nil, "", nil) if no evidence link exists.
func CollectObservedInput(ctx context.Context, repoPath, provider, session, turn string) (*observedinput.Evidence, string, error) {
	key := observedInputEventID(provider, session, turn)
	h, err := sqlstore.Open(ctx, repoLineageDB(repoPath), sqlstore.DefaultOpenOptions())
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = sqlstore.Close(h) }()
	row, err := h.Queries.GetEvidenceLink(ctx, sqldb.GetEvidenceLinkParams{
		EventID: key, EvidenceKind: observedInputEvidenceKind, GroupID: key,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	bs, err := blobs.NewStore(repoObjects(repoPath))
	if err != nil {
		return nil, "", err
	}
	doc, err := bs.Get(ctx, row.EvidenceHash)
	if err != nil {
		return nil, "", fmt.Errorf("observed-input document %s unresolved: %w", row.EvidenceHash, err)
	}
	var ev observedinput.Evidence
	if err := json.Unmarshal(doc, &ev); err != nil {
		return nil, "", err
	}
	if err := observedinput.Validate(ev); err != nil {
		return nil, "", err
	}
	if err := verifyObservedInputClosure(bs, ev); err != nil {
		return nil, "", err
	}
	return &ev, row.EvidenceHash, nil
}

// verifyObservedInputClosure checks that primary observation content objects exist.
func verifyObservedInputClosure(bs *blobs.Store, ev observedinput.Evidence) error {
	for _, o := range ev.Observations {
		ref := o.Representation.ContentRef
		if ref != "" && !bs.Exists(ref) {
			return fmt.Errorf("observed-input content %s (delivery %s) not resolvable locally", ref, o.DeliveryID)
		}
	}
	return nil
}
