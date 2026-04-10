package implementations

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/store/impldb"
)

// ImplementationMeta is the structured content of implementations.metadata_json.
// Unknown keys in the JSON are preserved across read/write cycles.
type ImplementationMeta struct {
	Summary                string `json:"summary,omitempty"`
	TitleSource            string `json:"title_source,omitempty"`              // "auto" or "manual"
	SummarySource          string `json:"summary_source,omitempty"`            // "auto" or "manual"
	GeneratedRepoCount     int    `json:"generated_repo_count,omitempty"`      // repo count at last auto-generation
	GeneratedAt            int64  `json:"generated_at,omitempty"`              // unix ms of last auto-generation
	GenerationInProgressAt int64  `json:"generation_in_progress_at,omitempty"` // unix ms, set before spawning

	// extra holds unknown keys from the JSON so they survive round-trips.
	extra map[string]json.RawMessage
}

const (
	SourceAuto   = "auto"
	SourceManual = "manual"

	// InProgressTTL is how long a generation_in_progress_at marker is
	// considered fresh before being treated as stale (crashed process).
	InProgressTTL = 5 * time.Minute
)

// knownMetaKeys lists the keys managed by ImplementationMeta.
// Used to separate known from unknown during serialization.
var knownMetaKeys = map[string]bool{
	"summary":                    true,
	"title_source":               true,
	"summary_source":             true,
	"generated_repo_count":       true,
	"generated_at":               true,
	"generation_in_progress_at":  true,
}

// ReadImplementationMeta parses metadata_json into a structured type,
// preserving unknown keys for round-trip safety.
func ReadImplementationMeta(raw sql.NullString) ImplementationMeta {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return ImplementationMeta{}
	}

	// First pass: unmarshal known fields.
	var meta ImplementationMeta
	_ = json.Unmarshal([]byte(raw.String), &meta)

	// Second pass: capture all keys to find unknowns.
	var allKeys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw.String), &allKeys); err == nil {
		for k, v := range allKeys {
			if !knownMetaKeys[k] {
				if meta.extra == nil {
					meta.extra = make(map[string]json.RawMessage)
				}
				meta.extra[k] = v
			}
		}
	}

	return meta
}

// WriteImplementationMeta serializes metadata back to a sql.NullString,
// preserving unknown keys from the original JSON.
func WriteImplementationMeta(meta ImplementationMeta) (sql.NullString, error) {
	// Marshal known fields into a map.
	known, err := json.Marshal(meta)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("encode implementation metadata: %w", err)
	}

	var merged map[string]json.RawMessage
	if err := json.Unmarshal(known, &merged); err != nil {
		return sql.NullString{}, err
	}

	// Overlay unknown keys.
	for k, v := range meta.extra {
		if _, isKnown := merged[k]; !isKnown {
			merged[k] = v
		}
	}

	// Remove only empty strings and null values. Zero numerics are left
	// for the typed struct's omitempty tags to handle during marshaling.
	for k, v := range merged {
		s := string(v)
		if s == `""` || s == `null` {
			delete(merged, k)
		}
	}

	if len(merged) == 0 {
		return sql.NullString{}, nil
	}

	encoded, err := json.Marshal(merged)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("encode implementation metadata: %w", err)
	}
	return impldb.NullStr(string(encoded)), nil
}

// IsManuallyEdited returns true if either the title or summary was
// manually set by the user. When true, auto-generation should skip.
func (m ImplementationMeta) IsManuallyEdited() bool {
	return m.TitleSource == SourceManual || m.SummarySource == SourceManual
}

// IsGenerationInProgress returns true if a background generation was
// recently started and hasn't completed yet.
func (m ImplementationMeta) IsGenerationInProgress() bool {
	if m.GenerationInProgressAt == 0 {
		return false
	}
	return time.Since(time.UnixMilli(m.GenerationInProgressAt)) < InProgressTTL
}

// implementationSummaryFromMetadata extracts the summary text from metadata_json.
func implementationSummaryFromMetadata(raw sql.NullString) string {
	return ReadImplementationMeta(raw).Summary
}

// implementationMetadataWithSummary updates the summary field in metadata_json,
// preserving all other fields (known and unknown).
func implementationMetadataWithSummary(raw sql.NullString, summary string) (sql.NullString, error) {
	meta := ReadImplementationMeta(raw)
	meta.Summary = strings.TrimSpace(summary)
	return WriteImplementationMeta(meta)
}
