// Package observedinput defines provider-neutral request and input evidence.
package observedinput

// EvidenceVersion is the schema version of a persisted evidence document.
const EvidenceVersion = 1

// Origin records who issued a request.
type Origin string

const (
	OriginHuman     Origin = "human"
	OriginDelegator Origin = "delegator"
	OriginUnknown   Origin = "unknown"
)

// Acquisition is how an observed input reached the agent.
type Acquisition string

const (
	AcquisitionAttachment      Acquisition = "attachment"
	AcquisitionPromptExpansion Acquisition = "prompt_expansion"
	AcquisitionToolResult      Acquisition = "tool_result"
)

// DeliveryScope identifies whether input belongs to a request envelope or context.
type DeliveryScope string

const (
	ScopeRequestEnvelope DeliveryScope = "request_envelope"
	ScopeObservedContext DeliveryScope = "observed_context"
	ScopeUnresolved      DeliveryScope = "unresolved"
)

// Relationship names how an envelope input joins its request.
type Relationship string

const (
	RelAttachedTo   Relationship = "attached_to_request"
	RelExpandedInto Relationship = "expanded_into_request"
	RelInlineWith   Relationship = "inline_with_request"
)

// LinkBasis identifies the provider structure establishing envelope membership.
// Timing, source mentions, and inferred relationships do not establish membership.
type LinkBasis string

const (
	BasisParentLink         LinkBasis = "provider_parent_link"
	BasisContentStructure   LinkBasis = "provider_content_structure"
	BasisExpansionStructure LinkBasis = "provider_expansion_structure"
)

// RepState is whether an observed representation is present or explicitly absent.
type RepState string

const (
	RepPresent       RepState = "present"
	RepReferenceOnly RepState = "reference_only"
	RepUnavailable   RepState = "unavailable"
)

// GapReason names why coverage is incomplete.
type GapReason string

const (
	GapMissingBody      GapReason = "missing_body"
	GapTruncated        GapReason = "truncated"
	GapUnsupportedShape GapReason = "unsupported_shape"
	GapUnresolvedParent GapReason = "unresolved_parent"
	GapMalformed        GapReason = "malformed"
	GapReferenceOnly    GapReason = "reference_only"
	GapSizeLimit        GapReason = "size_limit"
	GapFailure          GapReason = "failure"
)

// SourceRef identifies a captured provider record, not a source to fetch again.
type SourceRef struct {
	Locator  string `json:"locator"`            // e.g. transcript path
	Position int64  `json:"position,omitempty"` // line or offset within the source
	Native   string `json:"native,omitempty"`   // provider block/record identity
}

// Representation describes captured content or its absence.
// ContentRef identifies observed bytes, not the original external source.
type Representation struct {
	State       RepState `json:"state"`
	ContentRef  string   `json:"content_ref,omitempty"`
	ContentSize int64    `json:"content_size,omitempty"`
	MediaType   string   `json:"media_type,omitempty"`
	LineStart   int      `json:"line_start,omitempty"`
	LineCount   int      `json:"line_count,omitempty"`
	TotalLines  int      `json:"total_lines,omitempty"`
	// SourceContentRef identifies source content that differs from the delivered
	// representation, such as raw file text behind formatted tool output.
	SourceContentRef  string `json:"source_content_ref,omitempty"`
	SourceContentSize int64  `json:"source_content_size,omitempty"`
	SourceDigest      string `json:"source_digest,omitempty"`
	Truncated         bool   `json:"truncated,omitempty"`
}

// Gap is an explicit coverage gap. Subject is a request or delivery identity when
// the gap is scoped to one; it is empty for session-level omissions.
type Gap struct {
	Subject string    `json:"subject,omitempty"`
	Reason  GapReason `json:"reason"`
	Detail  string    `json:"detail,omitempty"`
}

// RequestEvent is a provider-defined request with its own envelope.
type RequestEvent struct {
	ID              string    `json:"id"`
	ProviderEventID string    `json:"provider_event_id,omitempty"`
	Provider        string    `json:"provider"`
	SessionID       string    `json:"session_id,omitempty"`
	TurnID          string    `json:"turn_id,omitempty"`
	Origin          Origin    `json:"origin"`
	OriginEvidence  string    `json:"origin_evidence,omitempty"`
	InstructionRef  string    `json:"instruction_ref,omitempty"`
	ParentEventID   string    `json:"parent_event_id,omitempty"`
	Ordinal         int64     `json:"ordinal"`
	Timestamp       int64     `json:"timestamp,omitempty"`
	Source          SourceRef `json:"source"`
}

// ObservedInput records one delivery. Identical bytes delivered twice retain
// separate delivery identities.
type ObservedInput struct {
	DeliveryID     string         `json:"delivery_id"`
	Provider       string         `json:"provider"`
	SessionID      string         `json:"session_id,omitempty"`
	TurnID         string         `json:"turn_id,omitempty"`
	Acquisition Acquisition   `json:"acquisition"`
	Scope       DeliveryScope `json:"scope"`
	ToolCallID  string        `json:"tool_call_id,omitempty"`
	// UnresolvedParentID preserves the provider parent ID for later resolution.
	UnresolvedParentID string         `json:"unresolved_parent_id,omitempty"`
	Ordinal            int64          `json:"ordinal"`
	Representation     Representation  `json:"representation"`
	Source             SourceRef       `json:"source"`
	Gaps               []Gap           `json:"gaps,omitempty"`
}

// RequestInputLink joins an envelope input to its request by provider structure.
type RequestInputLink struct {
	RequestID    string       `json:"request_id"`
	DeliveryID   string       `json:"delivery_id"`
	Relationship Relationship `json:"relationship"`
	Basis        LinkBasis    `json:"basis"`
	Source       SourceRef    `json:"source"`
}

// ToolCallLink joins an observed input to the tool call that returned it. It is
// never a basis for request-envelope membership.
type ToolCallLink struct {
	DeliveryID string    `json:"delivery_id"`
	ToolCallID string    `json:"tool_call_id"`
	SessionID  string    `json:"session_id,omitempty"`
	TurnID     string    `json:"turn_id,omitempty"`
	Source     SourceRef `json:"source"`
}

// Evidence is the per-turn observed-input document persisted in CAS.
type Evidence struct {
	Version       int                `json:"version"`
	Provider      string             `json:"provider"`
	SessionID     string             `json:"session_id"`
	TurnID        string             `json:"turn_id"`
	Requests      []RequestEvent     `json:"requests"`
	Observations  []ObservedInput    `json:"observations"`
	RequestLinks  []RequestInputLink `json:"request_links"`
	ToolCallLinks []ToolCallLink     `json:"tool_call_links"`
	Gaps          []Gap              `json:"gaps,omitempty"`
}
