package events

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/semanticash/cli/internal/toolsnap"
)

// DeltaLink binds a tool-delta blob to an event and its session provider.
type DeltaLink struct {
	EventID      string
	EvidenceHash string
	GroupID      string
	Provider     string
}

// DeltaBlobGetter loads a CAS blob by hash.
type DeltaBlobGetter interface {
	Get(ctx context.Context, hash string) ([]byte, error)
}

// DeltaClaim is one ordered occurrence of verified tool-produced text.
type DeltaClaim struct {
	Line     string // raw line content, occurrence-ordered
	Provider string
	GroupID  string
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
	Claims map[string][]DeltaClaim // file -> ordered claims
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
		Claims:  map[string][]DeltaClaim{},
		Touched: map[string][]string{},
		Deleted: map[string][]string{},
	}

	// Preserve event order while loading each group once.
	type groupLinks struct {
		hash      string
		providers map[string]string // event id -> session provider
		rejected  string
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
		case !isHex64(l.EvidenceHash):
			g.rejected = DeltaRejectBadHash
		case l.EvidenceHash != g.hash:
			g.rejected = DeltaRejectDivergentHashes
		default:
			g.providers[l.EventID] = l.Provider
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
				claims, nonBlank := fileClaims(f, provider, gid)
				if !nonBlank {
					// Keep file-level evidence when no lines can align.
					addProvider(out.Touched, f.Path, provider)
					continue
				}
				out.Claims[f.Path] = append(out.Claims[f.Path], claims...)
			}
		}
	}
	return out, nil
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

// fileClaims returns ordered added lines and whether any are nonblank.
func fileClaims(f toolsnap.FileDelta, provider, groupID string) ([]DeltaClaim, bool) {
	var claims []DeltaClaim
	nonBlank := false
	for _, h := range f.Hunks {
		for _, line := range h.NewLines {
			if strings.TrimSpace(line) != "" {
				nonBlank = true
			}
			claims = append(claims, DeltaClaim{Line: line, Provider: provider, GroupID: groupID})
		}
	}
	return claims, nonBlank
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
