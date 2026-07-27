package service

import (
	"database/sql"

	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// eventWindow uses insert sequence only to break timestamp ties.
// Legacy boundaries without cursors use timestamps alone.
type eventWindow struct {
	useCursor   bool
	afterCursor int64
	upToCursor  int64
	afterTs     int64
	upToTs      int64
}

func tsWindow(after, upTo int64) eventWindow {
	return eventWindow{afterTs: after, upToTs: upTo}
}

// windowBetween builds (prev, cur], with an optional lower bound.
func windowBetween(prev *sqldb.Checkpoint, cur sqldb.Checkpoint) eventWindow {
	w := eventWindow{upToTs: cur.CreatedAt}
	if prev != nil {
		w.afterTs = prev.CreatedAt
	}
	if cur.EventCursor.Valid && (prev == nil || prev.EventCursor.Valid) {
		w.useCursor = true
		w.upToCursor = cur.EventCursor.Int64
		if prev != nil {
			w.afterCursor = prev.EventCursor.Int64
		}
	}
	return w
}

func (w eventWindow) cursorFlag() int64 {
	if w.useCursor {
		return 1
	}
	return 0
}

func (w eventWindow) cursorAfter() sql.NullInt64 {
	return sql.NullInt64{Int64: w.afterCursor, Valid: w.useCursor}
}

func (w eventWindow) cursorUpTo() sql.NullInt64 {
	return sql.NullInt64{Int64: w.upToCursor, Valid: w.useCursor}
}
