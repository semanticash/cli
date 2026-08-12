// Package builder provides shared helpers for converting provider hook
// payloads into broker.RawEvent values.
//
// Hash helpers return an empty string when storage or serialization fails.
// Callers still emit the event without the missing blob reference.
package builder

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
)

// BaseInput contains provider-specific fields for a broker.RawEvent.
type BaseInput struct {
	// Event is the source hook event. Required.
	Event *hooks.Event

	// SourceKey identifies the source used for deterministic event IDs.
	SourceKey string

	// Provider is the canonical agent provider name.
	Provider string

	// ProviderSessionID is the provider's opaque session identifier.
	ProviderSessionID string

	// ParentSessionID links a child session to its parent when available.
	ParentSessionID string

	// SessionMetaJSON is a pre-serialized provider metadata object.
	SessionMetaJSON string

	// SourceProjectPath is the repository root associated with the event.
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

// BaseRawEvent constructs the fields shared by provider events. The caller
// fills in fields specific to the event type.
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
