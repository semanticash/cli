package sqlite

import "database/sql"

// NullStr returns a valid sql.NullString if s is non-empty, otherwise null.
func NullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// NullInt64 returns a valid sql.NullInt64 if v > 0, otherwise null.
func NullInt64(v int64) sql.NullInt64 {
	if v <= 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: v, Valid: true}
}
