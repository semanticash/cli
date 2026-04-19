// Package builder provides shared scaffolding for the per-provider
// direct-emit hook handlers under internal/hooks/<provider>. Every
// provider translates incoming hook payloads into broker.RawEvent
// values, and most of that translation is identical across providers:
// computing a content-addressed event ID, assembling the session
// envelope, storing blobs with SHA-based hashes, synthesizing the
// assistant payload shape consumed by the attribution scorer.
//
// This package owns those shared operations so each provider's
// direct_emit.go can focus on its provider-specific logic
// (tool-input parsing, payload fields, summary formatting rules).
//
// Failure semantics. Hash-returning helpers never return an error.
// On any failure (nil blob putter, blob-store error, marshal error)
// they return an empty string, and the caller continues to assemble
// a well-formed broker.RawEvent with the missing hash field empty.
// This matches the direct-emit write-path contract that has been in
// place since the per-provider helpers were introduced: a local
// serialization failure is a data-quality issue for one field, not a
// runtime degradation the event stream should surface, and adding
// logging at this layer would produce noise without actionable
// signal. The silent-degradation rule is enforced by the signatures
// in this package; helpers that might grow an error return in the
// future should live in a different package.
package builder

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
)

// BaseInput carries the provider-specific fields that vary per
// provider when constructing the base broker.RawEvent envelope.
// Everything else (event ID computation, timestamp wiring) is
// derived from the hooks.Event itself.
type BaseInput struct {
	// Event is the source hook event. Required.
	Event *hooks.Event

	// SourceKey is the provider-specific identifier used for
	// content-addressed event IDs and for the SourceKey field on
	// the emitted RawEvent. Usually the transcript reference, with
	// per-provider fallbacks when a transcript is not available.
	SourceKey string

	// Provider is the agent provider name (for example, the value
	// exposed by each agent package as ProviderName).
	Provider string

	// ProviderSessionID identifies the session as the provider sees
	// it. Some providers derive this from the transcript path; some
	// use the hook-supplied session ID directly. The builder treats
	// it as opaque.
	ProviderSessionID string

	// ParentSessionID is optional and currently only set by Claude,
	// which can link subagent sessions back to their parent. Empty
	// for providers that do not track this relationship.
	ParentSessionID string

	// SessionMetaJSON is a pre-serialized JSON object of session
	// metadata. Providers vary in what they include, so the builder
	// does not try to assemble this itself.
	SessionMetaJSON string

	// SourceProjectPath is the repository root associated with the
	// event, usually decoded from the transcript path with a CWD
	// fallback.
	SourceProjectPath string
}

// ComputeEventID returns a deterministic SHA-256 hex digest derived
// from the source key and stable hook context. Using ToolUseID (or
// TurnID when ToolUseID is empty) as the stable key means replayed
// hook deliveries for the same step produce the same event ID, and
// downstream INSERT OR IGNORE semantics suppress duplicates without
// the broker needing a separate deduplication pass.
//
// The format of the hashed input is:
//
//	sourceKey + ":hook:" + HookPhase + ":" + ToolName + ":" + StableKey
//
// where StableKey is ToolUseID if non-empty, otherwise TurnID.
func ComputeEventID(sourceKey string, event *hooks.Event) string {
	h := sha256.New()
	h.Write([]byte(sourceKey))
	stableKey := event.ToolUseID
	if stableKey == "" {
		stableKey = event.TurnID
	}
	_, _ = fmt.Fprintf(h, ":hook:%s:%s:%s", event.Type.HookPhase(), event.ToolName, stableKey)
	return hex.EncodeToString(h.Sum(nil))
}

// BaseRawEvent constructs the broker.RawEvent envelope fields that
// every provider wires the same way: event ID, source key, provider
// name, timestamps, session identifiers, metadata, project path, and
// model. The caller fills in the per-event fields (Kind, Role,
// Summary, PayloadHash, ProvenanceHash, ToolUsesJSON, TurnID,
// ToolUseID, ToolName, EventSource, FilePaths) on the returned value.
func BaseRawEvent(in BaseInput) broker.RawEvent {
	return broker.RawEvent{
		EventID:           ComputeEventID(in.SourceKey, in.Event),
		SourceKey:         in.SourceKey,
		Provider:          in.Provider,
		Timestamp:         in.Event.Timestamp,
		ProviderSessionID: in.ProviderSessionID,
		ParentSessionID:   in.ParentSessionID,
		SessionStartedAt:  in.Event.Timestamp,
		SessionMetaJSON:   in.SessionMetaJSON,
		SourceProjectPath: in.SourceProjectPath,
		Model:             in.Event.Model,
	}
}
