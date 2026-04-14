package sqlite

import (
	"fmt"
	"net/url"
	"path/filepath"
	"time"
)

func sqliteDSN(dbPath string, opts OpenOptions) string {
	busyMS := int(opts.BusyTimeout / time.Millisecond)
	if busyMS < 0 {
		busyMS = 0
	}

	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyMS))
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", fmt.Sprintf("synchronous(%s)", opts.Synchronous))

	// Use "file:path?query" here.
	// Avoid url.URL: on Windows drive-letter paths it can add extra "//".
	return "file:" + filepath.ToSlash(dbPath) + "?" + q.Encode()
}
