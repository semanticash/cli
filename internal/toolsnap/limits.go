package toolsnap

import "fmt"

// Default capture limits bound per-snapshot work and storage.
const (
	// DefaultMaxCandidatePaths bounds nominated paths per snapshot.
	DefaultMaxCandidatePaths = 5000
	// DefaultMaxBytesRead bounds newly read file content per snapshot.
	DefaultMaxBytesRead = 128 << 20
)

// Partial reason strings are persisted and must remain stable.
const (
	ReasonFileLimit       = "file_limit"
	ReasonByteLimit       = "byte_limit"
	ReasonUnsupportedPath = "unsupported_path"
	ReasonMalformedStatus = "malformed_status"
)

// PartialError reports a capture that must degrade to partial
// evidence with a stable reason instead of guessing.
type PartialError struct {
	Reason string
	Detail string
}

func (e *PartialError) Error() string {
	if e.Detail == "" {
		return "toolsnap: partial capture: " + e.Reason
	}
	return fmt.Sprintf("toolsnap: partial capture: %s: %s", e.Reason, e.Detail)
}
