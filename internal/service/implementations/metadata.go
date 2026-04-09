package implementations

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/semanticash/cli/internal/store/impldb"
)

func implementationSummaryFromMetadata(raw sql.NullString) string {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return ""
	}

	var meta struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(raw.String), &meta); err != nil {
		return ""
	}
	return strings.TrimSpace(meta.Summary)
}

func implementationMetadataWithSummary(raw sql.NullString, summary string) (sql.NullString, error) {
	summary = strings.TrimSpace(summary)

	meta := map[string]any{}
	if raw.Valid && strings.TrimSpace(raw.String) != "" {
		if err := json.Unmarshal([]byte(raw.String), &meta); err != nil {
			return sql.NullString{}, fmt.Errorf("parse implementation metadata: %w", err)
		}
	}

	if summary == "" {
		delete(meta, "summary")
	} else {
		meta["summary"] = summary
	}

	if len(meta) == 0 {
		return sql.NullString{}, nil
	}

	encoded, err := json.Marshal(meta)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("encode implementation metadata: %w", err)
	}
	return impldb.NullStr(string(encoded)), nil
}
