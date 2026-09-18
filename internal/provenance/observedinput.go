package provenance

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/observedinput"
	"github.com/semanticash/cli/internal/platform"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

const (
	observedInputEvidenceKind = "observed_input"
	observedInputItemKind     = "observed_input_item"
	observedInputDeliveryKind = "observed_input_delivery"
	observedInputCallKind     = "observed_input_call"
	observedInputAncestryKind = "observed_input_ancestry"
	observedInputCallRegKind  = "observed_input_callreg"
	unresolvedTurn            = "unresolved"
)

// ErrObservedInputRetry indicates a missing production turn.
// Callers must retain the transcript offset and retry.
var ErrObservedInputRetry = errors.New("observed-input: evidence retained for retry")

// normalizeProvider matches the underscore form used for stored sessions.
func normalizeProvider(provider string) string { return strings.ReplaceAll(provider, "-", "_") }

// observedInputTurnEventID identifies the context event holding a turn's evidence links.
func observedInputTurnEventID(provider, sessionID, turnID string) string {
	sum := sha256.Sum256([]byte(normalizeProvider(provider) + "\x00" + sessionID + "\x00" + turnID))
	return fmt.Sprintf("observed-input:%x", sum)
}

func repoObjects(repoPath string) string  { return filepath.Join(repoPath, ".semantica", "objects") }
func repoLineageDB(repoPath string) string { return filepath.Join(repoPath, ".semantica", "lineage.db") }

// evidenceItem is an immutable request, observation with links, or set of gaps.
type evidenceItem struct {
	Kind        string                          `json:"kind"`
	Request     *observedinput.RequestEvent     `json:"request,omitempty"`
	Observation *observedinput.ObservedInput    `json:"observation,omitempty"`
	RequestLink *observedinput.RequestInputLink `json:"request_link,omitempty"`
	ToolLink    *observedinput.ToolCallLink     `json:"tool_link,omitempty"`
	Gaps        []observedinput.Gap             `json:"gaps,omitempty"`
}

// ObservedInputResult contains a turn's evidence or marks it unavailable.
// A zero value means no evidence was recorded.
type ObservedInputResult struct {
	Evidence    *observedinput.Evidence
	Hash        string
	Unavailable bool
}

// PersistObservedInputs stores immutable evidence and resolves provider identities
// to production turns. New items accumulate; conflicting deliveries are rejected.
func PersistObservedInputs(ctx context.Context, repoPath, provider, providerSessionID string, capturedAt int64, evs []observedinput.Evidence, contents map[string][]byte, callOwners, ancestry map[string]string) error {
	sessionID, turnByRequest, err := resolveIdentities(ctx, repoPath, provider, providerSessionID)
	if err != nil {
		return err
	}
	bs, err := blobs.NewStore(repoObjects(repoPath))
	if err != nil {
		return err
	}
	p := &oiWriter{ctx: ctx, repoPath: repoPath, provider: provider, providerSession: providerSessionID, sessionID: sessionID, capturedAt: capturedAt, bs: bs, contents: contents, seen: map[string]bool{}}
	for _, anchor := range []string{"@delivery", "@call", "@ancestry", "@callreg"} {
		if err := p.ensureEvent(anchor); err != nil {
			return err
		}
	}

	// Retain ancestry and calls even when their owning request is unknown.
	for child, parent := range ancestry {
		if err := p.writeAnchor(observedInputAncestryKind, child, parent, "@ancestry"); err != nil {
			return err
		}
	}
	for toolID, callUUID := range callOwners {
		if callUUID != "" {
			if err := p.writeAnchor(observedInputCallRegKind, toolID, callUUID, "@callreg"); err != nil {
				return err
			}
		}
	}

	// Resolve calls through ancestry retained across batches.
	durableAncestry, err := p.loadAnchors("@ancestry", observedInputAncestryKind)
	if err != nil {
		return err
	}
	callReg, err := p.loadAnchors("@callreg", observedInputCallRegKind)
	if err != nil {
		return err
	}
	durableCallTurn, err := p.loadAnchors("@call", observedInputCallKind)
	if err != nil {
		return err
	}
	for toolID, callUUID := range callReg {
		if durableCallTurn[toolID] != "" {
			continue
		}
		if t := resolveOwner(callUUID, durableAncestry, turnByRequest); t != "" {
			if err := p.writeAnchor(observedInputCallKind, toolID, t, "@call"); err != nil {
				return err
			}
			durableCallTurn[toolID] = t
		}
	}

	// Retry ownership resolution using the updated durable records.
	if err := p.reconcileUnresolved(turnByRequest, durableCallTurn); err != nil {
		return err
	}

	retry := false
	for _, ev := range evs {
		if ev.TurnID == unresolvedTurn {
			for i := range ev.Observations {
				if err := p.placeObservation(ev.Observations[i], turnByRequest, durableCallTurn); err != nil {
					return err
				}
			}
			if err := p.persistGaps(unresolvedTurn, ev.Gaps); err != nil {
				return err
			}
			continue
		}
		var reqUUID string
		if len(ev.Requests) > 0 {
			reqUUID = ev.Requests[0].ProviderEventID
		}
		turnID := turnByRequest[reqUUID]
		if turnID == "" {
			retry = true // Keep the offset until the production turn is recorded.
			continue
		}
		mapped := remapIdentities(ev, sessionID, turnID)
		if err := observedinput.Validate(mapped); err != nil {
			return fmt.Errorf("observed-input evidence for turn %s invalid: %w", turnID, err)
		}
		for i := range mapped.Requests {
			r := mapped.Requests[i]
			if err := p.putVerified(r.InstructionRef); err != nil {
				return err
			}
			if err := p.persistItem(turnID, r.ID, evidenceItem{Kind: "request", Request: &r}); err != nil {
				return err
			}
		}
		for i := range mapped.Observations {
			o := mapped.Observations[i]
			item := evidenceItem{Kind: "observation", Observation: &o,
				RequestLink: findRequestLink(mapped.RequestLinks, o.DeliveryID),
				ToolLink:    findToolLink(mapped.ToolCallLinks, o.DeliveryID)}
			if err := p.persistObservation(turnID, o, item); err != nil {
				return err
			}
		}
		if err := p.persistGaps(turnID, mapped.Gaps); err != nil {
			return err
		}
	}
	if retry {
		return ErrObservedInputRetry
	}
	return nil
}

// oiWriter holds storage and identity state for one persistence call.
type oiWriter struct {
	ctx             context.Context
	repoPath        string
	provider        string
	providerSession string
	sessionID       string
	capturedAt      int64
	bs              *blobs.Store
	contents        map[string][]byte
	seen            map[string]bool
}

func (p *oiWriter) putVerified(ref string) error {
	if ref == "" || p.bs.Exists(ref) {
		return nil // No reference or already stored.
	}
	body, ok := p.contents[ref]
	if !ok {
		return fmt.Errorf("observed-input content %s missing", ref)
	}
	h, _, err := p.bs.Put(p.ctx, body)
	if err != nil {
		return err
	}
	if h != ref {
		return fmt.Errorf("observed-input content hash mismatch: %s stored as %s", ref, h)
	}
	return nil
}

func (p *oiWriter) ensureEvent(turnID string) error {
	if p.seen[turnID] {
		return nil
	}
	record := broker.ObservationContext(broker.RawEvent{
		EventID: observedInputTurnEventID(p.provider, p.sessionID, turnID), SourceKey: "observed-input:" + p.sessionID,
		Provider: p.provider, ProviderSessionID: p.providerSession, TurnID: turnID,
		Timestamp: p.capturedAt, Kind: "context", Role: "system", EventSource: observedInputEvidenceKind,
		Summary: "Observed inputs supplied with the request.",
	})
	if _, err := broker.WriteEventsToRepo(p.ctx, p.repoPath, []broker.RawEvent{record}, nil); err != nil {
		return err
	}
	p.seen[turnID] = true
	return nil
}

func (p *oiWriter) persistItem(turnID, group string, item evidenceItem) error {
	if err := p.ensureEvent(turnID); err != nil {
		return err
	}
	h, err := putItem(p.ctx, p.bs, item)
	if err != nil {
		return err
	}
	return broker.WriteEvidenceLinksToRepo(p.ctx, p.repoPath, []broker.EvidenceLink{
		evidenceItemLink(observedInputTurnEventID(p.provider, p.sessionID, turnID), group, h, p.capturedAt),
	})
}

// persistObservation atomically links an observation and its delivery identity.
// Conflicting content or provider links prevent publication.
func (p *oiWriter) persistObservation(turnID string, o observedinput.ObservedInput, item evidenceItem) error {
	if err := p.putVerified(o.Representation.ContentRef); err != nil {
		return err
	}
	if err := p.putVerified(o.Representation.SourceContentRef); err != nil {
		return err
	}
	if err := p.ensureEvent(turnID); err != nil {
		return err
	}
	h, err := putItem(p.ctx, p.bs, item)
	if err != nil {
		return err
	}
	return broker.WriteEvidenceLinksToRepo(p.ctx, p.repoPath, []broker.EvidenceLink{
		evidenceItemLink(observedInputTurnEventID(p.provider, p.sessionID, turnID), o.DeliveryID, h, p.capturedAt),
		{EventID: observedInputTurnEventID(p.provider, p.sessionID, "@delivery"), EvidenceKind: observedInputDeliveryKind,
			EvidenceHash: deliveryIdentityKey(o, item.RequestLink), GroupID: o.DeliveryID, CreatedAt: p.capturedAt},
	})
}

// persistGaps stores gaps independently so separate batches can add new gaps.
func (p *oiWriter) persistGaps(turnID string, gaps []observedinput.Gap) error {
	for _, g := range gaps {
		if err := p.persistItem(turnID, "gap:"+gapKey(g), evidenceItem{Kind: "gaps", Gaps: []observedinput.Gap{g}}); err != nil {
			return err
		}
	}
	return nil
}

func gapKey(g observedinput.Gap) string {
	sum := sha256.Sum256([]byte(string(g.Reason) + "\x00" + g.Subject + "\x00" + g.Detail))
	return hex.EncodeToString(sum[:])
}

// writeAnchor stores a session-wide mapping and rejects conflicting values.
func (p *oiWriter) writeAnchor(kind, group, value, anchorTurn string) error {
	return broker.WriteEvidenceLinksToRepo(p.ctx, p.repoPath, []broker.EvidenceLink{{
		EventID: observedInputTurnEventID(p.provider, p.sessionID, anchorTurn), EvidenceKind: kind,
		EvidenceHash: value, GroupID: group, CreatedAt: p.capturedAt,
	}})
}

func (p *oiWriter) loadAnchors(anchorTurn, kind string) (map[string]string, error) {
	h, err := sqlstore.Open(p.ctx, repoLineageDB(p.repoPath), sqlstore.DefaultOpenOptions())
	if err != nil {
		return nil, err
	}
	defer func() { _ = sqlstore.Close(h) }()
	rows, err := h.Queries.ListEvidenceLinksByEvent(p.ctx, observedInputTurnEventID(p.provider, p.sessionID, anchorTurn))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, r := range rows {
		if r.EvidenceKind == kind {
			out[r.GroupID] = r.EvidenceHash
		}
	}
	return out, nil
}

// resolveOwner walks provider ancestry from a record to the owning request turn.
func resolveOwner(uuid string, ancestry, turnByRequest map[string]string) string {
	seen := map[string]bool{}
	for uuid != "" && !seen[uuid] {
		if t := turnByRequest[uuid]; t != "" {
			return t
		}
		seen[uuid] = true
		uuid = ancestry[uuid]
	}
	return ""
}

// placeObservation stores an input under its owning turn when known.
// Otherwise, it retains the unresolved input and its provider links.
func (p *oiWriter) placeObservation(o observedinput.ObservedInput, turnByRequest, durableCallTurn map[string]string) error {
	if o.UnresolvedParentID != "" {
		if t := turnByRequest[o.UnresolvedParentID]; t != "" {
			po := o
			po.SessionID, po.TurnID, po.Scope = p.sessionID, t, observedinput.ScopeRequestEnvelope
			po.Gaps, po.UnresolvedParentID = dropGap(po.Gaps, observedinput.GapUnresolvedParent), ""
			link := observedinput.RequestInputLink{RequestID: "req:" + o.UnresolvedParentID, DeliveryID: po.DeliveryID,
				Relationship: observedinput.RelAttachedTo, Basis: observedinput.BasisParentLink, Source: po.Source}
			return p.persistObservation(t, po, evidenceItem{Kind: "observation", Observation: &po, RequestLink: &link})
		}
	}
	if o.ToolCallID != "" {
		if t := durableCallTurn[o.ToolCallID]; t != "" {
			po := o
			po.SessionID, po.TurnID, po.Scope = p.sessionID, t, observedinput.ScopeObservedContext
			po.Gaps = dropGap(po.Gaps, observedinput.GapUnresolvedParent)
			toolLink := observedinput.ToolCallLink{DeliveryID: po.DeliveryID, ToolCallID: po.ToolCallID, SessionID: p.sessionID, TurnID: t, Source: po.Source}
			return p.persistObservation(t, po, evidenceItem{Kind: "observation", Observation: &po, ToolLink: &toolLink})
		}
	}
	uo := o
	uo.SessionID = p.sessionID
	return p.persistObservation(unresolvedTurn, uo, evidenceItem{Kind: "observation", Observation: &uo})
}

// reconcileUnresolved assigns retained inputs to newly resolved owning turns.
func (p *oiWriter) reconcileUnresolved(turnByRequest, durableCallTurn map[string]string) error {
	h, err := sqlstore.Open(p.ctx, repoLineageDB(p.repoPath), sqlstore.DefaultOpenOptions())
	if err != nil {
		return err
	}
	rows, err := h.Queries.ListEvidenceLinksByEvent(p.ctx, observedInputTurnEventID(p.provider, p.sessionID, unresolvedTurn))
	_ = sqlstore.Close(h)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.EvidenceKind != observedInputItemKind {
			continue
		}
		raw, gerr := p.bs.Get(p.ctx, row.EvidenceHash)
		if gerr != nil {
			continue
		}
		var item evidenceItem
		if json.Unmarshal(raw, &item) != nil || item.Observation == nil {
			continue
		}
		o := *item.Observation
		resolvable := (o.UnresolvedParentID != "" && turnByRequest[o.UnresolvedParentID] != "") ||
			(o.ToolCallID != "" && durableCallTurn[o.ToolCallID] != "")
		if !resolvable {
			continue
		}
		o.Scope = observedinput.ScopeUnresolved // Reclassify through placeObservation.
		if err := p.placeObservation(o, turnByRequest, durableCallTurn); err != nil {
			return err
		}
	}
	return nil
}

// deliveryIdentityKey binds content to its provider parent or tool call.
// It excludes the resolved turn so ownership resolution preserves identity.
func deliveryIdentityKey(o observedinput.ObservedInput, reqLink *observedinput.RequestInputLink) string {
	parent := o.UnresolvedParentID
	if parent == "" && reqLink != nil {
		parent = strings.TrimPrefix(reqLink.RequestID, "req:")
	}
	structural := parent + "|" + o.ToolCallID
	sum := sha256.Sum256([]byte(o.Representation.ContentRef + "\x00" + o.Representation.SourceContentRef + "\x00" + structural))
	return hex.EncodeToString(sum[:])
}

// dropGap removes gaps with the given reason.
func dropGap(gaps []observedinput.Gap, reason observedinput.GapReason) []observedinput.Gap {
	var out []observedinput.Gap
	for _, g := range gaps {
		if g.Reason != reason {
			out = append(out, g)
		}
	}
	return out
}

// CollectObservedInput assembles evidence from Semantica storage and verifies
// content hashes. Missing or invalid items produce an unavailable result;
// no recorded items produce an empty result.
func CollectObservedInput(ctx context.Context, repoPath, provider, sessionID, turnID string) (ObservedInputResult, error) {
	h, err := sqlstore.Open(ctx, repoLineageDB(repoPath), sqlstore.DefaultOpenOptions())
	if err != nil {
		return ObservedInputResult{}, err
	}
	defer func() { _ = sqlstore.Close(h) }()
	ctxEventID := observedInputTurnEventID(provider, sessionID, turnID)
	rows, err := h.Queries.ListEvidenceLinksByEvent(ctx, ctxEventID)
	if err != nil {
		return ObservedInputResult{}, err
	}
	var itemHashes []string
	for _, r := range rows {
		if r.EvidenceKind == observedInputItemKind {
			itemHashes = append(itemHashes, r.EvidenceHash)
		}
	}
	if len(itemHashes) == 0 {
		return ObservedInputResult{}, nil
	}
	sort.Strings(itemHashes)

	bs, err := blobs.NewStore(repoObjects(repoPath))
	if err != nil {
		return ObservedInputResult{}, err
	}
	ev := observedinput.Evidence{Version: observedinput.EvidenceVersion, Provider: provider, SessionID: sessionID, TurnID: turnID}
	for _, hash := range itemHashes {
		raw, gerr := bs.Get(ctx, hash)
		if gerr != nil {
			return ObservedInputResult{Unavailable: true}, nil
		}
		var item evidenceItem
		if json.Unmarshal(raw, &item) != nil {
			return ObservedInputResult{Unavailable: true}, nil
		}
		switch {
		case item.Request != nil:
			ev.Requests = append(ev.Requests, *item.Request)
		case item.Observation != nil:
			ev.Observations = append(ev.Observations, *item.Observation)
			if item.RequestLink != nil {
				ev.RequestLinks = append(ev.RequestLinks, *item.RequestLink)
			}
			if item.ToolLink != nil {
				ev.ToolCallLinks = append(ev.ToolCallLinks, *item.ToolLink)
			}
		case len(item.Gaps) > 0:
			ev.Gaps = append(ev.Gaps, item.Gaps...)
		}
	}
	sortEvidence(&ev)
	// Invalid evidence remains distinct from absent evidence.
	if err := observedinput.Validate(ev); err != nil {
		return ObservedInputResult{Unavailable: true}, nil
	}
	if !closureResolves(ctx, bs, ev) {
		return ObservedInputResult{Unavailable: true}, nil
	}
	doc, err := json.Marshal(ev)
	if err != nil {
		return ObservedInputResult{}, err
	}
	stored, _, err := bs.Put(ctx, doc)
	if err != nil {
		return ObservedInputResult{}, err
	}
	return ObservedInputResult{Evidence: &ev, Hash: stored}, nil
}

// PropagateObservedInput copies retained evidence and referenced content from
// originRepoPath to the destination store, using the destination session identity.
//
// originRepoPath must be a resolved repository root. Missing sessions or evidence
// return an empty result; unreadable origin evidence returns Unavailable.
func PropagateObservedInput(ctx context.Context, destBlobs *blobs.Store, originRepoPath, provider, providerSessionID, destSessionID, turnID string) (ObservedInputResult, error) {
	originSessionID, err := resolveSessionID(ctx, originRepoPath, provider, providerSessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ObservedInputResult{}, nil // No matching session in a valid origin.
		}
		return ObservedInputResult{Unavailable: true}, nil // lookup/storage failure
	}
	res, err := CollectObservedInput(ctx, originRepoPath, provider, originSessionID, turnID)
	if err != nil {
		return ObservedInputResult{Unavailable: true}, nil // origin storage failure
	}
	if res.Evidence == nil {
		return res, nil // confirmed absence, or already-unavailable evidence
	}
	originBlobs, err := blobs.NewStore(repoObjects(originRepoPath))
	if err != nil {
		return ObservedInputResult{Unavailable: true}, nil
	}
	// Copy the content closure into the destination store so its references resolve.
	for _, ref := range closureRefs(res.Evidence) {
		if ref == "" || destBlobs.Exists(ref) {
			continue
		}
		body, gerr := originBlobs.Get(ctx, ref)
		if gerr != nil {
			return ObservedInputResult{Unavailable: true}, nil
		}
		if _, _, perr := destBlobs.Put(ctx, body); perr != nil {
			return ObservedInputResult{}, perr
		}
	}
	mapped := remapIdentities(*res.Evidence, destSessionID, turnID)
	if !closureResolves(ctx, destBlobs, mapped) {
		return ObservedInputResult{Unavailable: true}, nil
	}
	doc, err := json.Marshal(mapped)
	if err != nil {
		return ObservedInputResult{}, err
	}
	stored, _, err := destBlobs.Put(ctx, doc)
	if err != nil {
		return ObservedInputResult{}, err
	}
	return ObservedInputResult{Evidence: &mapped, Hash: stored}, nil
}

// sameRepoPath reports whether two repository paths are equal under normalization.
func sameRepoPath(a, b string) bool {
	return platform.NormalizePathForCompare(a) == platform.NormalizePathForCompare(b)
}

// closureRefs returns the content references an evidence document depends on.
func closureRefs(ev *observedinput.Evidence) []string {
	refs := make([]string, 0, len(ev.Requests)+2*len(ev.Observations))
	for _, r := range ev.Requests {
		refs = append(refs, r.InstructionRef)
	}
	for _, o := range ev.Observations {
		refs = append(refs, o.Representation.ContentRef, o.Representation.SourceContentRef)
	}
	return refs
}

// errOriginUnavailable indicates unreadable origin storage or repository identity.
var errOriginUnavailable = errors.New("observed-input: origin unavailable")

// resolveSessionID resolves a repo's internal session ID for a provider session.
// Only a missing session in a valid repository returns sql.ErrNoRows.
func resolveSessionID(ctx context.Context, repoPath, provider, providerSessionID string) (string, error) {
	h, err := sqlstore.OpenExisting(ctx, repoLineageDB(repoPath), sqlstore.DefaultOpenOptions())
	if err != nil {
		return "", errOriginUnavailable
	}
	defer func() { _ = sqlstore.Close(h) }()
	repo, err := h.Queries.GetRepositoryByRootPath(ctx, repoPath)
	if err != nil {
		return "", errOriginUnavailable // missing/unreadable repository identity
	}
	sess, err := h.Queries.GetAgentSessionByProviderID(ctx, sqldb.GetAgentSessionByProviderIDParams{
		RepositoryID: repo.RepositoryID, Provider: normalizeProvider(provider), ProviderSessionID: providerSessionID,
	})
	if err != nil {
		return "", err // sql.ErrNoRows here means the session is absent in a valid origin
	}
	return sess.SessionID, nil
}

// resolveIdentities resolves the internal session and maps request IDs to turns.
func resolveIdentities(ctx context.Context, repoPath, provider, providerSessionID string) (string, map[string]string, error) {
	h, err := sqlstore.Open(ctx, repoLineageDB(repoPath), sqlstore.DefaultOpenOptions())
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = sqlstore.Close(h) }()
	repo, err := h.Queries.GetRepositoryByRootPath(ctx, repoPath)
	if err != nil {
		return "", nil, fmt.Errorf("resolve repository: %w", err)
	}
	sess, err := h.Queries.GetAgentSessionByProviderID(ctx, sqldb.GetAgentSessionByProviderIDParams{
		RepositoryID: repo.RepositoryID, Provider: normalizeProvider(provider), ProviderSessionID: providerSessionID,
	})
	if err != nil {
		return "", nil, fmt.Errorf("resolve session %s: %w", providerSessionID, err)
	}
	events, err := h.Queries.ListAgentEventsBySession(ctx, sqldb.ListAgentEventsBySessionParams{
		SessionID: sess.SessionID, Limit: 1_000_000,
	})
	if err != nil {
		return "", nil, err
	}
	// Only requests anchor ownership; other rows may have time-based turn assignments.
	turnByRequest := map[string]string{}
	for _, e := range events {
		if e.Kind != "user" || !e.Role.Valid || e.Role.String != "user" {
			continue
		}
		if e.ProviderEventID.Valid && e.TurnID.Valid && e.ProviderEventID.String != "" && e.TurnID.String != "" {
			if _, seen := turnByRequest[e.ProviderEventID.String]; !seen {
				turnByRequest[e.ProviderEventID.String] = e.TurnID.String
			}
		}
	}
	return sess.SessionID, turnByRequest, nil
}

func remapIdentities(ev observedinput.Evidence, sessionID, turnID string) observedinput.Evidence {
	out := ev
	out.SessionID, out.TurnID = sessionID, turnID
	out.Requests = append([]observedinput.RequestEvent(nil), ev.Requests...)
	for i := range out.Requests {
		out.Requests[i].SessionID = sessionID
		out.Requests[i].TurnID = turnID
	}
	out.Observations = append([]observedinput.ObservedInput(nil), ev.Observations...)
	for i := range out.Observations {
		out.Observations[i].SessionID = sessionID
		if turnID != unresolvedTurn {
			out.Observations[i].TurnID = turnID
		}
	}
	out.ToolCallLinks = append([]observedinput.ToolCallLink(nil), ev.ToolCallLinks...)
	for i := range out.ToolCallLinks {
		out.ToolCallLinks[i].SessionID = sessionID
		if turnID != unresolvedTurn {
			out.ToolCallLinks[i].TurnID = turnID
		}
	}
	return out
}

func putItem(ctx context.Context, bs *blobs.Store, item evidenceItem) (string, error) {
	b, err := json.Marshal(item)
	if err != nil {
		return "", err
	}
	h, _, err := bs.Put(ctx, b)
	return h, err
}

func evidenceItemLink(ctxEventID, group, hash string, at int64) broker.EvidenceLink {
	return broker.EvidenceLink{EventID: ctxEventID, EvidenceKind: observedInputItemKind, EvidenceHash: hash, GroupID: group, CreatedAt: at}
}

func findRequestLink(links []observedinput.RequestInputLink, deliveryID string) *observedinput.RequestInputLink {
	for i := range links {
		if links[i].DeliveryID == deliveryID {
			return &links[i]
		}
	}
	return nil
}

func findToolLink(links []observedinput.ToolCallLink, deliveryID string) *observedinput.ToolCallLink {
	for i := range links {
		if links[i].DeliveryID == deliveryID {
			return &links[i]
		}
	}
	return nil
}

func sortEvidence(ev *observedinput.Evidence) {
	sort.SliceStable(ev.Requests, func(i, j int) bool { return ev.Requests[i].ID < ev.Requests[j].ID })
	sort.SliceStable(ev.Observations, func(i, j int) bool { return ev.Observations[i].DeliveryID < ev.Observations[j].DeliveryID })
	sort.SliceStable(ev.RequestLinks, func(i, j int) bool { return ev.RequestLinks[i].DeliveryID < ev.RequestLinks[j].DeliveryID })
	sort.SliceStable(ev.ToolCallLinks, func(i, j int) bool { return ev.ToolCallLinks[i].DeliveryID < ev.ToolCallLinks[j].DeliveryID })
}

// stripObservedInput removes references to local-only evidence before upload.
func stripObservedInput(rawBundle []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(rawBundle, &m) != nil {
		return rawBundle
	}
	if _, ok := m["observed_input"]; !ok {
		return rawBundle
	}
	delete(m, "observed_input")
	out, err := json.Marshal(m)
	if err != nil {
		return rawBundle
	}
	return out
}

// closureResolves verifies that every referenced content object matches its hash.
func closureResolves(ctx context.Context, bs *blobs.Store, ev observedinput.Evidence) bool {
	verify := func(ref string) bool {
		if ref == "" {
			return true
		}
		b, err := bs.Get(ctx, ref)
		if err != nil {
			return false
		}
		sum := sha256.Sum256(b)
		return hex.EncodeToString(sum[:]) == ref
	}
	for _, r := range ev.Requests {
		if !verify(r.InstructionRef) {
			return false
		}
	}
	for _, o := range ev.Observations {
		if !verify(o.Representation.ContentRef) || !verify(o.Representation.SourceContentRef) {
			return false
		}
	}
	return true
}
