package observedinput

import (
	"errors"
	"fmt"
)

func validOrigin(o Origin) bool {
	switch o {
	case OriginHuman, OriginDelegator, OriginUnknown:
		return true
	}
	return false
}

func validAcquisition(a Acquisition) bool {
	switch a {
	case AcquisitionAttachment, AcquisitionPromptExpansion, AcquisitionToolResult:
		return true
	}
	return false
}

func validScope(s DeliveryScope) bool {
	switch s {
	case ScopeRequestEnvelope, ScopeObservedContext, ScopeUnresolved:
		return true
	}
	return false
}

func validRelationship(r Relationship) bool {
	switch r {
	case RelAttachedTo, RelExpandedInto, RelInlineWith:
		return true
	}
	return false
}

func validBasis(b LinkBasis) bool {
	switch b {
	case BasisParentLink, BasisContentStructure, BasisExpansionStructure:
		return true
	}
	return false
}

func validRepState(s RepState) bool {
	switch s {
	case RepPresent, RepReferenceOnly, RepUnavailable:
		return true
	}
	return false
}

func validGapReason(r GapReason) bool {
	switch r {
	case GapMissingBody, GapTruncated, GapUnsupportedShape, GapUnresolvedParent,
		GapMalformed, GapReferenceOnly, GapSizeLimit, GapFailure:
		return true
	}
	return false
}

// Validate checks identities, membership links, and representation states.
// It returns all detected violations as a joined error.
func Validate(e Evidence) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if e.Version != EvidenceVersion {
		add("evidence: unexpected version %d", e.Version)
	}
	if e.Provider == "" || e.SessionID == "" || e.TurnID == "" {
		add("evidence: missing provider/session/turn identity")
	}

	requests := map[string]bool{}
	for _, r := range e.Requests {
		if r.ID == "" {
			add("request: empty identity")
			continue
		}
		if requests[r.ID] {
			add("request %q: duplicate identity", r.ID)
		}
		requests[r.ID] = true
		if !validOrigin(r.Origin) {
			add("request %q: invalid origin %q", r.ID, r.Origin)
		}
		if r.Source.Locator == "" {
			add("request %q: missing source locator", r.ID)
		}
		if r.SessionID != "" && r.SessionID != e.SessionID {
			add("request %q: foreign session identity %q", r.ID, r.SessionID)
		}
		if r.TurnID != "" && r.TurnID != e.TurnID {
			add("request %q: foreign turn identity %q", r.ID, r.TurnID)
		}
	}

	deliveries := map[string]ObservedInput{}
	for _, o := range e.Observations {
		if o.DeliveryID == "" {
			add("observation: empty delivery identity")
			continue
		}
		if _, dup := deliveries[o.DeliveryID]; dup {
			add("observation %q: duplicate delivery identity", o.DeliveryID)
		}
		deliveries[o.DeliveryID] = o
		switch o.InputSource.Kind {
		case "", "file", "url", "unknown":
		default:
			add("observation %q: invalid input source kind", o.DeliveryID)
		}
		switch o.Representation.Transformation {
		case "", "none", "summarized", "extracted", "unknown":
		default:
			add("observation %q: invalid transformation", o.DeliveryID)
		}
		switch o.Representation.Extent {
		case "", "complete", "partial", "unknown":
		default:
			add("observation %q: invalid extent", o.DeliveryID)
		}
		if n := o.Representation.ReportedSourceBytes; n != nil && *n < 0 {
			add("observation %q: negative reported source bytes", o.DeliveryID)
		}
		if !validAcquisition(o.Acquisition) {
			add("observation %q: invalid acquisition %q", o.DeliveryID, o.Acquisition)
		}
		if !validScope(o.Scope) {
			add("observation %q: invalid scope %q", o.DeliveryID, o.Scope)
		}
		if !validRepState(o.Representation.State) {
			add("observation %q: invalid representation state %q", o.DeliveryID, o.Representation.State)
		}
		if o.SessionID != "" && o.SessionID != e.SessionID {
			add("observation %q: foreign session identity %q", o.DeliveryID, o.SessionID)
		}
		if o.TurnID != "" && o.TurnID != e.TurnID {
			add("observation %q: foreign turn identity %q", o.DeliveryID, o.TurnID)
		}
		// Tool results cannot establish request-envelope membership.
		if o.Acquisition == AcquisitionToolResult && o.Scope == ScopeRequestEnvelope {
			add("observation %q: tool result cannot be request-envelope evidence", o.DeliveryID)
		}
		// Present content carries a reference; absence carries a gap and no reference.
		switch o.Representation.State {
		case RepPresent:
			if o.Representation.ContentRef == "" {
				add("observation %q: present representation without content reference", o.DeliveryID)
			}
		case RepReferenceOnly, RepUnavailable:
			if o.Representation.ContentRef != "" {
				add("observation %q: absent representation must not carry a content reference", o.DeliveryID)
			}
			if !hasGap(o.Gaps) {
				add("observation %q: absent representation must record an explicit gap", o.DeliveryID)
			}
		}
		// Unresolved membership requires an explicit gap.
		if o.Scope == ScopeUnresolved && !hasGap(o.Gaps) {
			add("observation %q: unresolved membership must record an explicit gap", o.DeliveryID)
		}
		for _, g := range o.Gaps {
			if !validGapReason(g.Reason) {
				add("observation %q: invalid gap reason %q", o.DeliveryID, g.Reason)
			}
		}
	}

	// Request links: provider structure only, endpoints resolvable, envelope scope.
	linkCount := map[string]int{}
	for _, l := range e.RequestLinks {
		if !requests[l.RequestID] {
			add("request link: unknown request %q", l.RequestID)
		}
		o, ok := deliveries[l.DeliveryID]
		if !ok {
			add("request link: unknown observation %q", l.DeliveryID)
			continue
		}
		if !validRelationship(l.Relationship) {
			add("request link %q: invalid relationship %q", l.DeliveryID, l.Relationship)
		}
		if !validBasis(l.Basis) {
			add("request link %q: invalid basis %q", l.DeliveryID, l.Basis)
		}
		if l.Source.Locator == "" {
			add("request link %q: missing source establishing the join", l.DeliveryID)
		}
		if o.Scope != ScopeRequestEnvelope {
			add("request link %q: observation scope %q is not a request envelope", l.DeliveryID, o.Scope)
		}
		linkCount[l.DeliveryID]++
	}

	// Tool-call links describe observed context and never promote to an envelope.
	for _, l := range e.ToolCallLinks {
		o, ok := deliveries[l.DeliveryID]
		if !ok {
			add("tool-call link: unknown observation %q", l.DeliveryID)
			continue
		}
		if l.ToolCallID == "" {
			add("tool-call link %q: missing tool call identity", l.DeliveryID)
		}
		if l.ToolCallID != o.ToolCallID {
			add("tool-call link %q: call id %q disagrees with observation %q", l.DeliveryID, l.ToolCallID, o.ToolCallID)
		}
		if l.SessionID != "" && l.SessionID != e.SessionID {
			add("tool-call link %q: foreign session identity %q", l.DeliveryID, l.SessionID)
		}
		if l.TurnID != "" && l.TurnID != e.TurnID {
			add("tool-call link %q: foreign turn identity %q", l.DeliveryID, l.TurnID)
		}
		if o.Scope == ScopeRequestEnvelope {
			add("tool-call link %q: tool results cannot be request-envelope evidence", l.DeliveryID)
		}
	}

	// Every envelope observation needs exactly one structural membership link.
	for id, o := range deliveries {
		if o.Scope != ScopeRequestEnvelope {
			continue
		}
		switch linkCount[id] {
		case 0:
			add("observation %q: request-envelope scope without a membership link", id)
		case 1:
		default:
			add("observation %q: %d membership links, want exactly one", id, linkCount[id])
		}
	}

	for _, g := range e.Gaps {
		if !validGapReason(g.Reason) {
			add("evidence gap: invalid reason %q", g.Reason)
		}
	}
	return errors.Join(errs...)
}

func hasGap(gaps []Gap) bool { return len(gaps) > 0 }
