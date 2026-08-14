package events

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/semanticash/cli/internal/toolsnap"
)

// DeltaLink binds a tool-delta blob to an event and its session
// provider, with the event's recency for winner selection.
type DeltaLink struct {
	EventID      string
	EvidenceHash string
	GroupID      string
	Provider     string
	Ts           int64
	InsertSeq    int64
}

// DeltaBlobGetter loads a CAS blob by hash.
type DeltaBlobGetter interface {
	Get(ctx context.Context, hash string) ([]byte, error)
}

// DeltaClaimGroup contains one group's ordered claims for a file.
type DeltaClaimGroup struct {
	GroupID  string
	Provider string
	// Recency of the latest member event.
	Ts        int64
	InsertSeq int64
	EventID   string
	Lines     []string // ordered added lines, hunk order
	// Historical marks a carry-forward claim for a modified file.
	Historical bool
}

// Stable rejection reasons for excluded delta groups.
const (
	DeltaRejectBadHash         = "bad_hash"
	DeltaRejectDivergentHashes = "divergent_hashes"
	DeltaRejectBlobUnavailable = "blob_unavailable"
	DeltaRejectHashMismatch    = "hash_mismatch"
	DeltaRejectParseFailure    = "parse_failure"
	DeltaRejectEventBinding    = "event_binding"
	DeltaRejectIncompleteLinks = "incomplete_links"
	DeltaRejectProviderBinding = "provider_binding"
	DeltaRejectConcurrentGroup = "concurrent_group"
	DeltaRejectAmbiguousActors = "ambiguous_actors"
	DeltaRejectPartial         = "partial"
	DeltaRejectCrossGroupEvent = "cross_group_event"
)

// DeltaDiagnostics counts eligible and rejected groups.
type DeltaDiagnostics struct {
	GroupsSeen     int
	GroupsEligible int
	Rejected       map[string]int
}

func (d *DeltaDiagnostics) reject(reason string) {
	if d.Rejected == nil {
		d.Rejected = map[string]int{}
	}
	d.Rejected[reason]++
}

// DeltaCandidates holds verified evidence for one attribution window.
// Claims and providers retain first-seen order.
type DeltaCandidates struct {
	Claims map[string][]DeltaClaimGroup // file -> claim groups, capture order
	// Touched contains files without usable line evidence.
	Touched map[string][]string // file -> providers, first-seen order
	// Deleted contains files removed by complete deltas.
	Deleted map[string][]string // file -> providers, first-seen order
	Diags   DeltaDiagnostics
}

func addProvider(m map[string][]string, path, provider string) {
	for _, p := range m[path] {
		if p == provider {
			return
		}
	}
	m[path] = append(m[path], provider)
}

// BuildDeltaCandidates verifies and classifies tool-delta links. Invalid,
// incomplete, partial, concurrent, or ambiguous groups contribute nothing.
// Linked events must exactly match the delta and its actor providers.
//
// Cancellation returns an error rather than partial candidates.
func BuildDeltaCandidates(ctx context.Context, links []DeltaLink, blobs DeltaBlobGetter) (*DeltaCandidates, error) {
	out := &DeltaCandidates{
		Claims:  map[string][]DeltaClaimGroup{},
		Touched: map[string][]string{},
		Deleted: map[string][]string{},
	}

	// Preserve event order while loading each group once.
	type groupLinks struct {
		hash      string
		providers map[string]string // event id -> session provider
		rejected  string
		// Latest member event by (ts, insert_seq, event_id).
		ts        int64
		insertSeq int64
		eventID   string
	}
	// Collect ownership first so every group sharing an event is rejected,
	// including groups connected through chained reuse.
	eventGroups := map[string][]string{}
	for _, l := range links {
		known := false
		for _, gid := range eventGroups[l.EventID] {
			if gid == l.GroupID {
				known = true
				break
			}
		}
		if !known {
			eventGroups[l.EventID] = append(eventGroups[l.EventID], l.GroupID)
		}
	}

	order := []string{}
	groups := map[string]*groupLinks{}
	for _, l := range links {
		g, ok := groups[l.GroupID]
		if !ok {
			g = &groupLinks{hash: l.EvidenceHash, providers: map[string]string{}}
			groups[l.GroupID] = g
			order = append(order, l.GroupID)
		}
		if g.rejected != "" {
			continue
		}
		switch {
		case len(eventGroups[l.EventID]) > 1:
			g.rejected = DeltaRejectCrossGroupEvent
		case !isHex64(l.EvidenceHash):
			g.rejected = DeltaRejectBadHash
		case l.EvidenceHash != g.hash:
			g.rejected = DeltaRejectDivergentHashes
		default:
			g.providers[l.EventID] = l.Provider
			if l.Ts > g.ts ||
				(l.Ts == g.ts && l.InsertSeq > g.insertSeq) ||
				(l.Ts == g.ts && l.InsertSeq == g.insertSeq && l.EventID > g.eventID) {
				g.ts, g.insertSeq, g.eventID = l.Ts, l.InsertSeq, l.EventID
			}
		}
	}

	for _, gid := range order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		g := groups[gid]
		out.Diags.GroupsSeen++
		if g.rejected != "" {
			out.Diags.reject(g.rejected)
			continue
		}
		raw, err := blobs.Get(ctx, g.hash)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			out.Diags.reject(DeltaRejectBlobUnavailable)
			continue
		}
		// A getter may return bytes after its context is cancelled.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != g.hash {
			out.Diags.reject(DeltaRejectHashMismatch)
			continue
		}
		delta, err := toolsnap.ParseDelta(raw)
		if err != nil {
			out.Diags.reject(DeltaRejectParseFailure)
			continue
		}
		// Hashing and parsing may outlive the caller's context.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if reason := bindGroup(delta, g.providers); reason != "" {
			out.Diags.reject(reason)
			continue
		}
		// Hook-time actors cannot prove ownership for concurrent groups.
		if delta.Scope == "concurrent_group" {
			out.Diags.reject(DeltaRejectConcurrentGroup)
			continue
		}
		if len(delta.Actors) != 1 {
			out.Diags.reject(DeltaRejectAmbiguousActors)
			continue
		}
		// Partial windows do not provide usable file evidence.
		if delta.Status != "complete" {
			out.Diags.reject(DeltaRejectPartial)
			continue
		}

		out.Diags.GroupsEligible++
		provider := delta.Actors[0].Provider
		for _, f := range delta.Files {
			switch {
			case f.Operation == "delete":
				addProvider(out.Deleted, f.Path, provider)
			case f.Binary || f.Truncated || !textualChange(f):
				addProvider(out.Touched, f.Path, provider)
			default:
				lines, nonBlank := fileLines(f)
				if !nonBlank {
					// Keep file-level evidence when no lines can align.
					addProvider(out.Touched, f.Path, provider)
					continue
				}
				out.Claims[f.Path] = append(out.Claims[f.Path], DeltaClaimGroup{
					GroupID: gid, Provider: provider,
					Ts: g.ts, InsertSeq: g.insertSeq, EventID: g.eventID,
					Lines: lines,
				})
			}
		}
	}
	return out, nil
}

// SuppressInferredDeletions removes command-inferred touches when a
// verified delta records the same deletion. Explicit touches remain.
func SuppressInferredDeletions(cands *Candidates, deltas *DeltaCandidates) {
	if cands == nil || deltas == nil {
		return
	}
	for path := range deltas.Deleted {
		prov, inferred := cands.InferredDeletions[path]
		if !inferred {
			continue
		}
		// Restore any explicit editor hidden by the inferred deletion.
		if cands.ProviderTouchedFiles[path] == prov {
			if editor, explicit := cands.ExplicitTouches[path]; explicit {
				cands.ProviderTouchedFiles[path] = editor.Provider
			} else {
				delete(cands.ProviderTouchedFiles, path)
			}
		}
		delete(cands.InferredDeletions, path)
	}
}

// bindGroup requires exact event membership and matching actor providers.
func bindGroup(delta *toolsnap.Delta, linked map[string]string) string {
	deltaEvents := map[string]int{} // event id -> actor index
	for _, tu := range delta.ToolUses {
		deltaEvents[tu.EventID] = tu.Actor
	}
	if len(linked) != len(deltaEvents) {
		return DeltaRejectIncompleteLinks
	}
	for ev, sessionProvider := range linked {
		actorIdx, ok := deltaEvents[ev]
		if !ok {
			return DeltaRejectEventBinding
		}
		if delta.Actors[actorIdx].Provider != sessionProvider {
			return DeltaRejectProviderBinding
		}
	}
	return ""
}

// fileLines returns ordered added lines and whether any are nonblank.
func fileLines(f toolsnap.FileDelta) ([]string, bool) {
	var lines []string
	nonBlank := false
	for _, h := range f.Hunks {
		for _, line := range h.NewLines {
			if strings.TrimSpace(line) != "" {
				nonBlank = true
			}
			lines = append(lines, line)
		}
	}
	return lines, nonBlank
}

// textualChange accepts creates and regular-file changes.
func textualChange(f toolsnap.FileDelta) bool {
	if !textualMode(f.AfterMode) {
		return false
	}
	return f.Operation == "create" || textualMode(f.BeforeMode)
}

// textualMode accepts regular-file modes only.
func textualMode(mode string) bool {
	return mode == "100644" || mode == "100755"
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
