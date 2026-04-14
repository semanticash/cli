package impldb

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
	if opts.TxImmediate {
		q.Add("_txlock", "immediate")
	}

	// Use "file:path?query" format, not "file:///path?query".
	// SQLite URI expects "file:" followed directly by the path.
	// url.URL adds an authority ("//") which breaks on Windows drive paths.
	return "file:" + filepath.ToSlash(dbPath) + "?" + q.Encode()
}
