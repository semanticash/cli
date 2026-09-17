package observedinput

import (
	"strings"
	"testing"
)

// validEvidence contains a request, a linked attachment, and a tool result.
func validEvidence() Evidence {
	return Evidence{
		Version:   EvidenceVersion,
		Provider:  "claude_code",
		SessionID: "sess",
		TurnID:    "turn",
		Requests: []RequestEvent{{
			ID: "req-1", Provider: "claude_code", Origin: OriginHuman,
			Source: SourceRef{Locator: "t.jsonl", Position: 1, Native: "uuid-req"},
		}},
		Observations: []ObservedInput{
			{
				DeliveryID: "att-1", Provider: "claude_code", Acquisition: AcquisitionAttachment,
				Scope: ScopeRequestEnvelope, Representation: Representation{State: RepPresent, ContentRef: "hashA"},
				Source: SourceRef{Locator: "t.jsonl", Position: 2, Native: "uuid-att"},
			},
			{
				DeliveryID: "res-1", Provider: "claude_code", Acquisition: AcquisitionToolResult,
				Scope: ScopeObservedContext, ToolCallID: "toolu_1",
				Representation: Representation{State: RepPresent, ContentRef: "hashB"},
				Source:         SourceRef{Locator: "t.jsonl", Position: 4, Native: "uuid-res"},
			},
		},
		RequestLinks: []RequestInputLink{{
			RequestID: "req-1", DeliveryID: "att-1", Relationship: RelAttachedTo,
			Basis: BasisParentLink, Source: SourceRef{Locator: "t.jsonl", Native: "uuid-att"},
		}},
		ToolCallLinks: []ToolCallLink{{
			DeliveryID: "res-1", ToolCallID: "toolu_1", Source: SourceRef{Locator: "t.jsonl", Native: "uuid-res"},
		}},
	}
}

func TestValidate_Accepts(t *testing.T) {
	if err := Validate(validEvidence()); err != nil {
		t.Fatalf("valid evidence rejected: %v", err)
	}
}

func TestValidate_Rejections(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Evidence)
		want   string
	}{
		{"tool-call link as envelope evidence", func(e *Evidence) {
			// A tool result scoped as an envelope input must be rejected.
			e.Observations[1].Scope = ScopeRequestEnvelope
		}, "tool results cannot be request-envelope evidence"},
		{"invalid basis", func(e *Evidence) {
			e.RequestLinks[0].Basis = "provider_timestamp"
		}, "invalid basis"},
		{"invalid relationship", func(e *Evidence) {
			e.RequestLinks[0].Relationship = "near_in_time"
		}, "invalid relationship"},
		{"envelope without membership link", func(e *Evidence) {
			e.RequestLinks = nil
		}, "without a membership link"},
		{"unknown request endpoint", func(e *Evidence) {
			e.RequestLinks[0].RequestID = "ghost"
		}, "unknown request"},
		{"unresolved without gap", func(e *Evidence) {
			e.Observations[1].Scope = ScopeUnresolved
			e.ToolCallLinks = nil
		}, "unresolved membership must record an explicit gap"},
		{"absent representation with content ref", func(e *Evidence) {
			e.Observations[0].Representation = Representation{State: RepUnavailable, ContentRef: "x"}
		}, "must not carry a content reference"},
		{"present representation without content ref", func(e *Evidence) {
			e.Observations[0].Representation = Representation{State: RepPresent}
		}, "without content reference"},
		{"absent representation without gap", func(e *Evidence) {
			e.Observations[0].Representation = Representation{State: RepUnavailable}
		}, "must record an explicit gap"},
		{"duplicate delivery identity", func(e *Evidence) {
			e.Observations[1].DeliveryID = "att-1"
		}, "duplicate delivery identity"},
		{"link to unknown observation", func(e *Evidence) {
			e.RequestLinks[0].DeliveryID = "ghost"
		}, "unknown observation"},
		{"duplicate membership link", func(e *Evidence) {
			e.RequestLinks = append(e.RequestLinks, e.RequestLinks[0])
		}, "membership links, want exactly one"},
		{"foreign request session", func(e *Evidence) {
			e.Requests[0].SessionID = "other-session"
		}, "foreign session identity"},
		{"foreign observation turn", func(e *Evidence) {
			e.Observations[0].TurnID = "other-turn"
		}, "foreign turn identity"},
		{"tool-call link with mismatched call id", func(e *Evidence) {
			e.ToolCallLinks[0].ToolCallID = "toolu_other"
		}, "disagrees with observation"},
		{"tool-call link with foreign session", func(e *Evidence) {
			e.ToolCallLinks[0].SessionID = "other-session"
		}, "foreign session identity"},
		{"tool-call link with foreign turn", func(e *Evidence) {
			e.ToolCallLinks[0].TurnID = "other-turn"
		}, "foreign turn identity"},
		{"tool result scoped as envelope without a link", func(e *Evidence) {
			// Acquisition alone must disqualify envelope scope, even with no tool link.
			e.Observations[1].Scope = ScopeRequestEnvelope
			e.ToolCallLinks = nil
			e.RequestLinks = append(e.RequestLinks, RequestInputLink{
				RequestID: "req-1", DeliveryID: "res-1", Relationship: RelAttachedTo,
				Basis: BasisParentLink, Source: SourceRef{Locator: "t.jsonl", Native: "uuid-res"},
			})
		}, "tool result cannot be request-envelope evidence"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := validEvidence()
			tc.mutate(&e)
			err := Validate(e)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// Shared content does not make separate deliveries duplicates.
func TestValidate_SharedContentDistinctDeliveries(t *testing.T) {
	e := validEvidence()
	e.Requests = append(e.Requests, RequestEvent{
		ID: "req-2", Provider: "claude_code", Origin: OriginHuman,
		Source: SourceRef{Locator: "t.jsonl", Position: 5, Native: "uuid-req2"},
	})
	e.Observations = append(e.Observations, ObservedInput{
		DeliveryID: "att-2", Provider: "claude_code", Acquisition: AcquisitionAttachment,
		Scope: ScopeRequestEnvelope, Representation: Representation{State: RepPresent, ContentRef: "hashA"},
		Source: SourceRef{Locator: "t.jsonl", Position: 6, Native: "uuid-att2"},
	})
	e.RequestLinks = append(e.RequestLinks, RequestInputLink{
		RequestID: "req-2", DeliveryID: "att-2", Relationship: RelAttachedTo,
		Basis: BasisParentLink, Source: SourceRef{Locator: "t.jsonl", Native: "uuid-att2"},
	})
	if err := Validate(e); err != nil {
		t.Fatalf("shared content across deliveries rejected: %v", err)
	}
}
