package sqlite

import (
	"fmt"
	"net/url"
	"path/filepath"
	"time"
)

func sqliteDSN(dbPath string, opts OpenOptions) string {
	u := &url.URL{
		Scheme: "file",
		Path:   filepath.ToSlash(dbPath),
	}

	busyMS := int(opts.BusyTimeout / time.Millisecond)
	if busyMS < 0 {
		busyMS = 0
	}

	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyMS))
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", fmt.Sprintf("synchronous(%s)", opts.Synchronous))
	u.RawQuery = q.Encode()

	return u.String()
}
