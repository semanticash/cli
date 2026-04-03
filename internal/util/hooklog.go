package util

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// AppendActivityLog appends a timestamped log line to .semantica/activity.log.
func AppendActivityLog(semDir, format string, args ...any) {
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "semantica: warning: create activity log dir: %v\n", err)
		return
	}
	path := filepath.Join(semDir, "activity.log")

	line := fmt.Sprintf(format, args...)
	ts := time.Now().Format(time.RFC3339)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "semantica: warning: open activity log: %v\n", err)
		return
	}
	defer func() { _ = f.Close() }()

	if _, err := fmt.Fprintf(f, "%s  %s\n", ts, line); err != nil {
		fmt.Fprintf(os.Stderr, "semantica: warning: write activity log: %v\n", err)
	}
}
